package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/openclaw/openclaw/go/internal/channels"
	"github.com/openclaw/openclaw/go/internal/config"
	"github.com/openclaw/openclaw/go/internal/gateway"
	"github.com/openclaw/openclaw/go/internal/providers"
	"github.com/openclaw/openclaw/go/internal/state"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "trace" {
		if err := runTraceCommand(os.Args[2:]); err != nil {
			log.Fatalf("Trace command failed: %v", err)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "--check-state" {
		store, err := state.NewStore(os.Args[2])
		if err != nil {
			log.Fatalf("State check failed: %v", err)
		}
		if err := store.Close(); err != nil {
			log.Fatalf("State check close failed: %v", err)
		}
		log.Println("State schema check passed")
		return
	}

	dataDir := strings.TrimSpace(os.Getenv("OPENCLAW_DATA_DIR"))
	if dataDir == "" {
		log.Fatal("OPENCLAW_DATA_DIR must be set")
	}
	configDir := strings.TrimSpace(os.Getenv("OPENCLAW_CONFIG_DIR"))
	if configDir == "" {
		log.Fatal("OPENCLAW_CONFIG_DIR must be set")
	}

	log.Println("Starting OpenClaw (Go Core)...")

	// 1. Configuration
	cfg, err := config.LoadConfig(filepath.Join(configDir, "openclaw.json"))
	if err != nil {
		log.Fatalf("Failed to load openclaw.json: %v", err)
	}
	sec, err := config.LoadSecrets(filepath.Join(configDir, "secrets.json"))
	if err != nil {
		log.Printf("Warning: secrets.json not found: %v", err)
		sec = &config.Secrets{}
	}

	// 2. State & Memory
	store, err := state.NewStore(filepath.Join(dataDir, "openclaw-agent.sqlite"))
	if err != nil {
		log.Fatalf("Failed to initialize state store: %v", err)
	}
	defer store.Close()

	// 3. Local OpenAI-compatible provider
	primaryProv, err := providers.NewOpenAIClient(sec.Models.Providers.OpenAI.APIKey, cfg.Models.Providers.OpenAI.BaseURL)
	if err != nil {
		log.Fatalf("Failed to configure local model provider: %v", err)
	}
	embeddingProv, err := providers.NewEmbeddingClient(
		sec.Models.Embeddings.APIKey,
		cfg.Models.Embeddings.BaseURL,
		cfg.Models.Embeddings.Model,
		cfg.Models.Embeddings.Dimensions,
	)
	if err != nil {
		log.Fatalf("Failed to configure local embedding provider: %v", err)
	}

	// 4. Channels
	chanReg := channels.NewRegistry()
	if cfg.Channels.Telegram.Enabled && sec.Channels.Telegram.BotToken != "" {
		tg, err := channels.NewTelegramAdapter(sec.Channels.Telegram.BotToken)
		if err != nil {
			log.Fatalf("Failed to configure Telegram: %v", err)
		}
		chanReg.Register(tg)
	}

	// 5. Agent & Gateway
	serverTimezone := time.Local
	log.Printf("Using server timezone %s", serverTimezone.String())
	rag := gateway.NewRAGService(
		store,
		embeddingProv,
		primaryProv,
		cfg.Models.Embeddings.IndexID,
		cfg.Models.Embeddings.Dimensions,
		cfg.Agents.Defaults.HistorySearch.MinScore,
	)
	agent := gateway.NewAgent(primaryProv, chanReg, store, cfg, serverTimezone, embeddingProv, rag)
	gw := gateway.NewGateway(agent, chanReg, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := agent.BackfillMemoryEmbeddings(ctx); err != nil {
		log.Printf("Warning: memory embedding backfill incomplete: %v", err)
	}
	rag.Start(ctx)

	if err := gw.Start(ctx); err != nil {
		log.Fatalf("Gateway failed to start: %v", err)
	}

	// 6. Graceful Shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down gracefully...")
	if err := gw.Stop(context.Background()); err != nil {
		log.Printf("Gateway shutdown incomplete: %v", err)
	}
	log.Println("OpenClaw stopped.")
}
