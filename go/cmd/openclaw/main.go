package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/openclaw/openclaw/go/internal/channels"
	"github.com/openclaw/openclaw/go/internal/config"
	"github.com/openclaw/openclaw/go/internal/gateway"
	"github.com/openclaw/openclaw/go/internal/memory"
	"github.com/openclaw/openclaw/go/internal/providers"
	"github.com/openclaw/openclaw/go/internal/state"
)

func loadPersonalityPrompt(ctx context.Context, store *state.Store) string {
	documents, err := store.LoadPersonality(ctx)
	if err != nil {
		log.Printf("Warning: failed to load personality: %v", err)
		return ""
	}

	var prompt strings.Builder
	for _, document := range documents {
		if prompt.Len() > 0 {
			prompt.WriteString("\n\n")
		}
		fmt.Fprintf(&prompt, "## %s\n%s", document.Name, document.Content)
	}
	return prompt.String()
}

func main() {
	dataDir := strings.TrimSpace(os.Getenv("OPENCLAW_DATA_DIR"))
	if dataDir == "" {
		log.Fatal("OPENCLAW_DATA_DIR must be set")
	}
	configDir := strings.TrimSpace(os.Getenv("OPENCLAW_CONFIG_DIR"))
	if configDir == "" {
		log.Fatal("OPENCLAW_CONFIG_DIR must be set")
	}

	if len(os.Args) > 1 && os.Args[1] == "mcp-server" {
		dbPath := filepath.Join(dataDir, "openclaw-agent.sqlite")
		store, err := state.NewStore(dbPath)
		if err != nil {
			log.Fatalf("Failed to open DB for MCP: %v", err)
		}
		providers.RunMCPServer(store)
		return
	}

	log.Println("Starting OpenClaw (Go Core)...")

	// 1. Configuration
	// For skeleton, we use dummy paths. In production these are injected via ENV.
	cfg, err := config.LoadConfig(filepath.Join(configDir, "openclaw.json"))
	if err != nil {
		log.Printf("Warning: openclaw.json not found, using defaults: %v", err)
		cfg = &config.Config{}
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
	memCore := memory.NewCore(store)

	// 3. Providers
	provReg := providers.NewRegistry()
	if sec.Models.Providers.OpenAI.APIKey != "" {
		provReg.Register(providers.NewOpenAIClient(sec.Models.Providers.OpenAI.APIKey, cfg.Models.Providers.OpenAI.BaseURL))
	}
	if sec.Models.Providers.Anthropic.APIKey != "" {
		provReg.Register(providers.NewAnthropicClient(sec.Models.Providers.Anthropic.APIKey))
	}
	provReg.Register(providers.NewClaudeCLIProvider())

	primaryProviderID := "openai" // default
	if cfg.Agents.Defaults.Model.Primary != "" {
		parts := strings.SplitN(cfg.Agents.Defaults.Model.Primary, "/", 2)
		primaryProviderID = parts[0]
	}

	primaryProv, err := provReg.Get(primaryProviderID)
	if err != nil {
		log.Printf("Warning: Primary provider %q not found, falling back", primaryProviderID)
		primaryProv, err = provReg.Get("claude-cli")
		if err != nil {
			primaryProv, err = provReg.Get("openai")
			if err != nil {
				log.Println("Warning: No providers registered. Agent will fail to reply.")
			}
		}
	}

	// 4. Channels
	chanReg := channels.NewRegistry()
	if cfg.Channels.Telegram.Enabled && sec.Channels.Telegram.BotToken != "" {
		tg, err := channels.NewTelegramAdapter(sec.Channels.Telegram.BotToken)
		if err == nil {
			chanReg.Register(tg)
		}
	}
	if cfg.Channels.Discord.Enabled && sec.Channels.Discord.BotToken != "" {
		dc, err := channels.NewDiscordAdapter(sec.Channels.Discord.BotToken)
		if err == nil {
			chanReg.Register(dc)
		}
	}
	if cfg.Channels.WhatsApp.Enabled {
		wa, err := channels.NewWhatsAppAdapter(
			context.Background(),
			filepath.Join(dataDir, "whatsapp-store.sqlite"),
		)
		if err == nil {
			chanReg.Register(wa)
		}
	}

	// 5. Agent & Gateway
	agent := gateway.NewAgent(primaryProv, memCore, chanReg, store, loadPersonalityPrompt(context.Background(), store))
	gw := gateway.NewGateway(agent, chanReg, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := gw.Start(ctx); err != nil {
		log.Fatalf("Gateway failed to start: %v", err)
	}

	// 6. Graceful Shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down gracefully...")
	gw.Stop(context.Background())
	log.Println("OpenClaw stopped.")
	os.Exit(0)
}
