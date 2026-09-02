package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openclaw/openclaw/go/internal/config"
	"github.com/openclaw/openclaw/go/internal/gateway"
	"github.com/openclaw/openclaw/go/internal/memory"
	"github.com/openclaw/openclaw/go/internal/providers"
	"github.com/openclaw/openclaw/go/internal/state"
)

func runMemoryCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: openclaw memory <status|add|list|get|search|update|remove|reindex|maintenance>")
	}
	switch args[0] {
	case "status":
		return memoryStatus(args[1:], os.Stdout)
	case "add":
		return memoryAdd(args[1:], os.Stdout)
	case "list":
		return memoryList(args[1:], os.Stdout)
	case "get":
		return memoryGet(args[1:], os.Stdout)
	case "search":
		return memorySearch(args[1:], os.Stdout)
	case "update":
		return memoryUpdate(args[1:], os.Stdout)
	case "remove":
		return memoryRemove(args[1:], os.Stdout)
	case "reindex":
		return memoryReindex(args[1:], os.Stdout)
	case "maintenance":
		return memoryMaintenance(args[1:], os.Stdout)
	default:
		return fmt.Errorf("unknown memory command %q", args[0])
	}
}

func memoryMaintenance(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: openclaw memory maintenance <status|preview|run|candidates>")
	}
	switch args[0] {
	case "status":
		set, database := commandFlags("maintenance status")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		store, err := openCommandStore(*database)
		if err != nil {
			return err
		}
		defer store.Close()
		maintenanceState, latest, err := store.MaintenanceStatus(context.Background())
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Memory maintenance checkpoint: history ID %d\n", maintenanceState.CheckpointHistoryID)
		if maintenanceState.NextRunAt != nil {
			fmt.Fprintf(out, "Next scheduled run: %s\n", maintenanceState.NextRunAt.UTC().Format(time.RFC3339))
		}
		if maintenanceState.LeaseOwner != "" {
			fmt.Fprintln(out, "Worker lease: active")
		}
		if latest == nil {
			fmt.Fprintln(out, "Latest run: none")
			return nil
		}
		fmt.Fprintf(out, "Latest run: %d mode=%s status=%s stage=%s range=%d..%d processed=%d candidates=%d promoted=%d rejected=%d\n",
			latest.ID, latest.Mode, latest.Status, latest.Stage, latest.CheckpointHistoryID,
			latest.HighwaterHistoryID, latest.ProcessedHistoryID, latest.CandidateCount,
			latest.PromotedCount, latest.RejectedCount)
		if latest.Error != "" {
			fmt.Fprintf(out, "Latest error: %s\n", latest.Error)
		}
		return nil
	case "candidates":
		set, database := commandFlags("maintenance candidates")
		runID := set.Int64("run-id", 0, "maintenance run ID")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		if *runID <= 0 {
			return fmt.Errorf("--run-id must be positive")
		}
		store, err := openCommandStore(*database)
		if err != nil {
			return err
		}
		defer store.Close()
		candidates, err := store.ListMaintenanceCandidates(context.Background(), *runID)
		if err != nil {
			return err
		}
		for _, candidate := range candidates {
			target := ""
			if candidate.TargetMemoryID != nil {
				target = fmt.Sprintf(" target=%d", *candidate.TargetMemoryID)
			}
			fmt.Fprintf(out, "Candidate %d [%s/%s] action=%s%s evidence=%v scores=trust:%.3f recency:%.3f novelty:%.3f contradiction:%.3f reason=%s\n%s\n",
				candidate.ID, candidate.Kind, candidate.Status, candidate.ProposedAction,
				target, candidate.EvidenceHistoryIDs, candidate.TrustScore, candidate.RecencyScore,
				candidate.NoveltyScore, candidate.ContradictionScore, candidate.DecisionReason, candidate.Content)
		}
		return nil
	case "preview", "run":
		set, database := commandFlags("maintenance " + args[0])
		configDir := set.String("config-dir", "", "configuration directory")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		store, service, cfg, _, err := configuredMemoryFull(*database, *configDir)
		if err != nil {
			return err
		}
		defer store.Close()
		chat, err := configuredChat(*configDir, cfg)
		if err != nil {
			return err
		}
		location, err := time.LoadLocation(cfg.Agents.Defaults.MemoryMaintenance.Timezone)
		if err != nil {
			return err
		}
		maintainer, err := memory.NewMaintainer(store, service, chat, cfg.Channels.Telegram.OwnerUserID,
			cfg.Agents.Defaults.MemoryMaintenance.BatchSize, cfg.Agents.Defaults.MemoryMaintenance.Schedule,
			location, time.Now)
		if err != nil {
			return err
		}
		mode := state.MaintenanceApply
		if args[0] == "preview" {
			mode = state.MaintenancePreview
		}
		result, err := maintainer.Run(context.Background(), mode)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Maintenance run %d (%s) processed through history ID %d: %d candidates, %d promoted, %d rejected.\n",
			result.RunID, result.Mode, result.ProcessedHistoryID, result.CandidateCount,
			result.PromotedCount, result.RejectedCount)
		return nil
	default:
		return fmt.Errorf("unknown memory maintenance command %q", args[0])
	}
}

func commandFlags(name string) (*flag.FlagSet, *string) {
	set := flag.NewFlagSet("memory "+name, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	database := set.String("database", "", "SQLite database path")
	return set, database
}

func openCommandStore(path string) (*state.Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("--database is required")
	}
	return state.NewStore(path)
}

func memoryStatus(args []string, out io.Writer) error {
	set, database := commandFlags("status")
	if err := set.Parse(args); err != nil {
		return err
	}
	store, err := openCommandStore(*database)
	if err != nil {
		return err
	}
	defer store.Close()
	active, deleted, err := store.MemoryCounts(context.Background())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Memory ledger: %d active, %d deleted\n", active, deleted)
	return nil
}

func memoryAdd(args []string, out io.Writer) error {
	set, database := commandFlags("add")
	configDir := set.String("config-dir", "", "configuration directory")
	kindText := set.String("kind", "", "profile, durable, or daily")
	content := set.String("content", "", "memory content")
	if err := set.Parse(args); err != nil {
		return err
	}
	store, service, err := configuredMemory(*database, *configDir)
	if err != nil {
		return err
	}
	defer store.Close()
	write, err := service.PrepareWrite(context.Background(), state.MemoryKind(*kindText), *content, memory.Provenance{Origin: state.MemoryOriginOwner, Source: state.MemorySourceOperator})
	if err != nil {
		return err
	}
	var item state.Memory
	var stored bool
	err = store.WithTx(context.Background(), func(tx *state.Tx) error {
		var err error
		item, stored, err = tx.StoreMemory(context.Background(), write)
		return err
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Memory ID %d (%s, revision %d, stored=%t): %s\n", item.ID, item.Kind, item.RevisionNumber, stored, item.Content)
	return nil
}

func memoryList(args []string, out io.Writer) error {
	set, database := commandFlags("list")
	kindText := set.String("kind", "", "optional kind")
	statusText := set.String("status", "active", "active, deleted, or all")
	limit := set.Int("limit", 20, "maximum results")
	if err := set.Parse(args); err != nil {
		return err
	}
	store, err := openCommandStore(*database)
	if err != nil {
		return err
	}
	defer store.Close()
	if *statusText != "active" && *statusText != "deleted" && *statusText != "all" {
		return fmt.Errorf("invalid --status")
	}
	var kind *state.MemoryKind
	if *kindText != "" {
		value := state.MemoryKind(*kindText)
		if !state.ValidMemoryKind(value) {
			return fmt.Errorf("invalid --kind")
		}
		kind = &value
	}
	items, err := store.ListMemories(context.Background(), state.MemoryFilter{Kind: kind, Status: state.MemoryStatus(*statusText), Limit: *limit})
	if err != nil {
		return err
	}
	for _, item := range items {
		printMemory(out, item)
	}
	return nil
}

func memoryGet(args []string, out io.Writer) error {
	set, database := commandFlags("get")
	id := set.Int64("id", 0, "Memory ID")
	if err := set.Parse(args); err != nil {
		return err
	}
	store, err := openCommandStore(*database)
	if err != nil {
		return err
	}
	defer store.Close()
	item, err := store.GetMemory(context.Background(), *id)
	if err != nil {
		return err
	}
	printMemory(out, item)
	return nil
}

func memorySearch(args []string, out io.Writer) error {
	set, database := commandFlags("search")
	configDir := set.String("config-dir", "", "configuration directory")
	query := set.String("query", "", "search query")
	limit := set.Int("max-results", 5, "maximum results")
	if err := set.Parse(args); err != nil {
		return err
	}
	store, service, cfg, _, err := configuredMemoryFull(*database, *configDir)
	if err != nil {
		return err
	}
	defer store.Close()
	chat, err := configuredChat(*configDir, cfg)
	if err != nil {
		return err
	}
	items, err := service.SearchWithModel(context.Background(), chat, *query, *limit)
	if err != nil {
		return err
	}
	for index, item := range items {
		fmt.Fprintf(out, "%d. Memory ID %d [%s] score %.4f: %s\n", index+1, item.ID, item.Kind, item.CombinedScore, item.Content)
	}
	return nil
}

func memoryUpdate(args []string, out io.Writer) error {
	set, database := commandFlags("update")
	configDir := set.String("config-dir", "", "configuration directory")
	id := set.Int64("id", 0, "Memory ID")
	content := set.String("content", "", "replacement content")
	kindText := set.String("kind", "", "optional replacement kind")
	if err := set.Parse(args); err != nil {
		return err
	}
	store, service, err := configuredMemory(*database, *configDir)
	if err != nil {
		return err
	}
	defer store.Close()
	kind := state.MemoryKind(*kindText)
	preparedKind := kind
	if preparedKind == "" {
		preparedKind = state.MemoryDurable
	}
	write, err := service.PrepareWrite(context.Background(), preparedKind, *content, memory.Provenance{Origin: state.MemoryOriginOwner, Source: state.MemorySourceOperator})
	if err != nil {
		return err
	}
	write.Kind = kind
	var item state.Memory
	err = store.WithTx(context.Background(), func(tx *state.Tx) error {
		var err error
		item, err = tx.UpdateMemory(context.Background(), *id, write)
		return err
	})
	if err != nil {
		return err
	}
	printMemory(out, item)
	return nil
}

func memoryRemove(args []string, out io.Writer) error {
	set, database := commandFlags("remove")
	id := set.Int64("id", 0, "Memory ID")
	if err := set.Parse(args); err != nil {
		return err
	}
	store, err := openCommandStore(*database)
	if err != nil {
		return err
	}
	defer store.Close()
	var item state.Memory
	err = store.WithTx(context.Background(), func(tx *state.Tx) error {
		var err error
		item, err = tx.RemoveMemory(context.Background(), *id, time.Now())
		return err
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Memory ID %d is deleted and no longer recalled; revisions remain available.\n", item.ID)
	return nil
}

func memoryReindex(args []string, out io.Writer) error {
	set, database := commandFlags("reindex")
	configDir := set.String("config-dir", "", "configuration directory")
	if err := set.Parse(args); err != nil {
		return err
	}
	store, service, cfg, embedder, err := configuredMemoryFull(*database, *configDir)
	if err != nil {
		return err
	}
	defer store.Close()
	memoryEntries, err := service.PrepareReindex(context.Background())
	if err != nil {
		return err
	}
	chat, err := configuredChat(*configDir, cfg)
	if err != nil {
		return err
	}
	rag := gateway.NewRAGService(store, embedder, chat, cfg.Models.Embeddings.IndexID, cfg.Models.Embeddings.Dimensions, cfg.Agents.Defaults.HistorySearch.MinScore)
	conversationEntries, err := rag.PrepareReindex(context.Background())
	if err != nil {
		return err
	}
	if err := store.ReplaceDerivedIndexes(context.Background(), memoryEntries, conversationEntries, time.Now()); err != nil {
		return err
	}
	fmt.Fprintf(out, "Reindexed %d active memories and %d completed conversations.\n", len(memoryEntries), len(conversationEntries))
	return nil
}

func configuredMemory(database, configDir string) (*state.Store, *memory.Service, error) {
	store, service, _, _, err := configuredMemoryFull(database, configDir)
	return store, service, err
}

func configuredMemoryFull(database, configDir string) (*state.Store, *memory.Service, *config.Config, providers.Embedder, error) {
	store, err := openCommandStore(database)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if strings.TrimSpace(configDir) == "" {
		store.Close()
		return nil, nil, nil, nil, fmt.Errorf("--config-dir is required")
	}
	cfg, err := config.LoadConfig(filepath.Join(configDir, "openclaw.json"))
	if err != nil {
		store.Close()
		return nil, nil, nil, nil, err
	}
	secrets, _ := config.LoadSecrets(filepath.Join(configDir, "secrets.json"))
	if secrets == nil {
		secrets = &config.Secrets{}
	}
	embedder, err := providers.NewEmbeddingClient(secrets.Models.Embeddings.APIKey, cfg.Models.Embeddings.BaseURL, cfg.Models.Embeddings.Model, cfg.Models.Embeddings.Dimensions)
	if err != nil {
		store.Close()
		return nil, nil, nil, nil, err
	}
	service := memory.NewService(store, embedder, cfg.Models.Embeddings.IndexID, cfg.Models.Embeddings.Dimensions, cfg.Agents.Defaults.HistorySearch.MinScore, time.Now)
	return store, service, cfg, embedder, nil
}

func configuredChat(configDir string, cfg *config.Config) (*providers.OpenAIClient, error) {
	secrets, _ := config.LoadSecrets(filepath.Join(configDir, "secrets.json"))
	if secrets == nil {
		secrets = &config.Secrets{}
	}
	return providers.NewOpenAIClient(secrets.Models.Providers.OpenAI.APIKey, cfg.Models.Providers.OpenAI.BaseURL)
}

func printMemory(out io.Writer, item state.Memory) {
	deleted := ""
	if item.DeletedAt != nil {
		deleted = " deleted=" + item.DeletedAt.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(out, "Memory ID %d [%s/%s] revision %d observed=%s updated=%s%s\n%s\n", item.ID, item.Kind, item.Status, item.RevisionNumber, item.ObservedAt.UTC().Format(time.RFC3339), item.UpdatedAt.UTC().Format(time.RFC3339), deleted, item.Content)
}
