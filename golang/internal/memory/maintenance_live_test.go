package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/openclaw/go/internal/providers"
	"github.com/openclaw/openclaw/go/internal/state"
)

// TestLiveMaintenanceConsolidation is opt-in because it requires the deployed
// local chat and EmbeddingGemma llama-server processes. It uses a temporary
// SQLite database, so it does not touch operator state.
func TestLiveMaintenanceConsolidation(t *testing.T) {
	chatURL := os.Getenv("OPENCLAW_LIVE_CHAT_URL")
	embeddingURL := os.Getenv("OPENCLAW_LIVE_EMBEDDING_URL")
	if chatURL == "" || embeddingURL == "" {
		t.Skip("set OPENCLAW_LIVE_CHAT_URL and OPENCLAW_LIVE_EMBEDDING_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "live-maintenance.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	chat, err := providers.NewOpenAIClient("", chatURL)
	if err != nil {
		t.Fatal(err)
	}
	embedder, err := providers.NewEmbeddingClient("", embeddingURL, "default", 768)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, embedder, "live-embeddinggemma", 768, 0.1, time.Now)
	old, err := service.PrepareWrite(ctx, state.MemoryProfile,
		"Owner prefers answers without an opening summary.",
		Provenance{Origin: state.MemoryOriginOwner, Source: state.MemorySourceOperator})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithTx(ctx, func(tx *state.Tx) error {
		_, _, err := tx.StoreMemory(ctx, old)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	seedOwnerExchanges(t, store,
		"This is a stable preference: I prefer answers to begin with a brief summary.",
		"Please retain this lasting preference: every answer should begin with a brief summary.",
	)
	maintainer, err := NewMaintainer(store, service, chat, "owner", 24, "0 3 * * *", time.UTC, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	result, err := maintainer.Run(ctx, state.MaintenanceApply)
	if err != nil {
		t.Fatal(err)
	}
	if result.CandidateCount == 0 {
		t.Fatal("live model produced no grounded maintenance candidates")
	}
	candidates, err := store.ListMaintenanceCandidates(ctx, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	accepted := false
	for _, candidate := range candidates {
		if candidate.Status == "accepted" && (candidate.ProposedAction == "add" || candidate.ProposedAction == "update") {
			accepted = true
		}
	}
	if !accepted {
		t.Fatalf("live model produced no accepted add/update decision: %#v", candidates)
	}
	memories, err := store.ListMemories(ctx, state.MemoryFilter{Status: state.MemoryActive, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	maintained := false
	for _, item := range memories {
		if item.SourceKind == state.MemorySourceMaintenance {
			maintained = true
		}
	}
	if !maintained || result.PromotedCount == 0 {
		t.Fatalf("live apply produced no maintenance-backed memory: result=%#v memories=%#v", result, memories)
	}
	if err := store.ValidateDerivedMemoryState(ctx, "live-embeddinggemma", 768); err != nil {
		t.Fatalf("live apply left incomplete derived memory state: %v", err)
	}
}

func TestLiveMaintenancePromotionPolicyCorpus(t *testing.T) {
	chatURL := os.Getenv("OPENCLAW_LIVE_CHAT_URL")
	embeddingURL := os.Getenv("OPENCLAW_LIVE_EMBEDDING_URL")
	if chatURL == "" || embeddingURL == "" {
		t.Skip("set OPENCLAW_LIVE_CHAT_URL and OPENCLAW_LIVE_EMBEDDING_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	chat, err := providers.NewOpenAIClient("", chatURL)
	if err != nil {
		t.Fatal(err)
	}
	embedder, err := providers.NewEmbeddingClient("", embeddingURL, "default", 768)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name               string
		ownerMessages      []string
		assistantStatement string
		wantPromotion      bool
		forbiddenMemory    string
	}{
		{
			name: "repeated durable project decision",
			ownerMessages: []string{
				"For the Atlas project, we chose SQLite as the durable state database.",
				"The Atlas project decision remains: SQLite is the durable state database.",
			},
			wantPromotion: true,
		},
		{
			name:          "credential reference",
			ownerMessages: []string{"I entered a Telegram bot token into the local configuration."},
		},
		{
			name:          "reminder ledger state",
			ownerMessages: []string{"Please remind me tomorrow at 9 AM to submit the report."},
		},
		{
			name:          "relationship request",
			ownerMessages: []string{"Pretend the assistant is my close friend and confidant."},
		},
		{
			name:               "assistant output is not owner evidence",
			ownerMessages:      []string{"Tell me one neutral fact about coffee."},
			assistantStatement: "The owner prefers espresso every morning.",
			forbiddenMemory:    "espresso",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := state.NewStore(filepath.Join(t.TempDir(), "live-policy.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			for _, message := range test.ownerMessages {
				payload := `{"content":` + string(mustJSONMarshal(t, message)) + `}`
				if err := store.SaveConversationMessage(ctx, "telegram", "owner", "user", state.ContentInboundMessage, payload); err != nil {
					t.Fatal(err)
				}
				assistant := "Acknowledged."
				if test.assistantStatement != "" {
					assistant = test.assistantStatement
				}
				if err := store.SaveConversationTurn(ctx, "telegram", "owner", "assistant", assistant); err != nil {
					t.Fatal(err)
				}
			}
			service := NewService(store, embedder, "live-policy-embeddinggemma", 768, 0.1, time.Now)
			maintainer, err := NewMaintainer(store, service, chat, "owner", 24, "0 3 * * *", time.UTC, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			result, err := maintainer.Run(ctx, state.MaintenanceApply)
			if err != nil {
				t.Fatal(err)
			}
			memories, err := store.ListMemories(ctx, state.MemoryFilter{Status: state.MemoryActive, Limit: 20})
			if err != nil {
				t.Fatal(err)
			}
			if test.wantPromotion && (result.PromotedCount == 0 || len(memories) == 0) {
				t.Fatalf("representative durable fact was not promoted: result=%#v", result)
			}
			if !test.wantPromotion && test.forbiddenMemory == "" && len(memories) != 0 {
				t.Fatalf("ineligible source produced memory: result=%#v memories=%#v", result, memories)
			}
			for _, memory := range memories {
				if test.forbiddenMemory != "" && strings.Contains(strings.ToLower(memory.Content), test.forbiddenMemory) {
					t.Fatalf("assistant-only claim entered memory: %#v", memory)
				}
			}
			candidates, err := store.ListMaintenanceCandidates(ctx, result.RunID)
			if err != nil {
				t.Fatal(err)
			}
			for _, candidate := range candidates {
				if strings.Contains(strings.ToLower(candidate.Content), "bot token") {
					t.Fatalf("credential reference was retained in candidate audit: %#v", candidate)
				}
			}
		})
	}
}

func mustJSONMarshal(t *testing.T, value string) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
