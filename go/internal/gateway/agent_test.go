package gateway

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw/openclaw/go/internal/channels"
	"github.com/openclaw/openclaw/go/internal/config"
	"github.com/openclaw/openclaw/go/internal/memory"
	"github.com/openclaw/openclaw/go/internal/providers"
	"github.com/openclaw/openclaw/go/internal/state"
)

type recordingProvider struct {
	request *providers.GenerateRequest
}

func (p *recordingProvider) ID() string {
	return "recording"
}

func (p *recordingProvider) Generate(_ context.Context, req *providers.GenerateRequest) (*providers.GenerateResponse, error) {
	p.request = req
	return &providers.GenerateResponse{Content: "assistant reply"}, nil
}

type recordingChannel struct {
	recipient string
	content   string
}

func (c *recordingChannel) ID() string {
	return "test-channel"
}

func (c *recordingChannel) Start(context.Context, channels.Handler) error {
	return nil
}

func (c *recordingChannel) Stop(context.Context) error {
	return nil
}

func (c *recordingChannel) SendMessage(_ context.Context, recipientID, content string) error {
	c.recipient = recipientID
	c.content = content
	return nil
}

func newTestAgent(t *testing.T, prov providers.Provider, ch *recordingChannel, personality string) (*Agent, *state.Store) {
	t.Helper()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "agent.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	registry := channels.NewRegistry()
	if ch != nil {
		registry.Register(ch)
	}
	agent := NewAgent(prov, memory.NewCore(store), registry, store, nil, personality)
	return agent, store
}

func TestAgentHandlesMessage(t *testing.T) {
	provider := &recordingProvider{}
	channel := &recordingChannel{}
	agent, _ := newTestAgent(t, provider, channel, "Be concise and direct.")

	err := agent.HandleMessage(context.Background(), &channels.Message{
		ChannelID: channel.ID(),
		SenderID:  "user-1",
		Content:   "remember espresso",
	})
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if channel.recipient != "user-1" || channel.content != "assistant reply" {
		t.Fatalf("unexpected channel delivery: recipient=%q content=%q", channel.recipient, channel.content)
	}
	if provider.request == nil || provider.request.Model != "default" {
		t.Fatalf("unexpected provider request: %#v", provider.request)
	}
	// First message: [system, user] — no compaction summary, no prior history.
	if len(provider.request.Messages) != 2 {
		t.Fatalf("provider message count = %d, want 2", len(provider.request.Messages))
	}
	sys := provider.request.Messages[0].Content
	if !strings.Contains(sys, "user-1") || !strings.Contains(sys, channel.ID()) {
		t.Fatalf("system prompt missing routing identity: %q", sys)
	}
	if !strings.Contains(sys, "Be concise and direct.") {
		t.Fatalf("system prompt missing personality: %q", sys)
	}
}

func TestAgentHistoryCarriedForward(t *testing.T) {
	provider := &recordingProvider{}
	channel := &recordingChannel{}
	agent, _ := newTestAgent(t, provider, channel, "")

	ctx := context.Background()
	for _, content := range []string{"first message", "second message"} {
		if err := agent.HandleMessage(ctx, &channels.Message{
			ChannelID: channel.ID(),
			SenderID:  "user-1",
			Content:   content,
		}); err != nil {
			t.Fatalf("HandleMessage(%q): %v", content, err)
		}
	}

	// On the second message the provider should see: [system, user(first), assistant(reply), user(second)].
	if len(provider.request.Messages) != 4 {
		t.Fatalf("provider message count after 2nd turn = %d, want 4", len(provider.request.Messages))
	}
	if provider.request.Messages[1].Content != "first message" {
		t.Fatalf("history[0] = %q, want %q", provider.request.Messages[1].Content, "first message")
	}
}

func TestResolveHistoryLimit(t *testing.T) {
	limit := func(v int) *int { return &v }

	tests := []struct {
		name      string
		cfg       func() *config.Config
		channelID string
		senderID  string
		want      int
	}{
		{"nil config falls back to default", func() *config.Config { return nil }, "telegram", "u1", defaultHistoryLimit},
		{"channel limit", func() *config.Config {
			c := &config.Config{}
			c.Channels.Telegram.HistoryLimit = limit(50)
			return c
		}, "telegram", "u1", 50},
		{"dm limit", func() *config.Config {
			c := &config.Config{}
			c.Channels.Telegram.DMHistoryLimit = limit(10)
			return c
		}, "telegram-dm", "u1", 10},
		{"per-DM override", func() *config.Config {
			c := &config.Config{}
			c.Channels.Telegram.DMHistoryLimit = limit(10)
			c.Channels.Telegram.DMs = map[string]config.DMEntry{"u1": {HistoryLimit: limit(5)}}
			return c
		}, "telegram-dm", "u1", 5},
		{"cli channel uses dm limit", func() *config.Config {
			c := &config.Config{}
			c.Channels.Telegram.DMHistoryLimit = limit(15)
			return c
		}, "cli", "u1", defaultHistoryLimit}, // cli doesn't match telegram prefix
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &Agent{cfg: tt.cfg()}
			got := agent.resolveHistoryLimit(tt.channelID, tt.senderID)
			if got != tt.want {
				t.Errorf("resolveHistoryLimit(%q, %q) = %d, want %d", tt.channelID, tt.senderID, got, tt.want)
			}
		})
	}
}

func TestEstimateTokens(t *testing.T) {
	msgs := []providers.Message{
		{Content: "hello world"}, // 11 chars → 2 tokens
		{Content: "foo"},         // 3 chars → 0 tokens (truncated)
	}
	got := estimateTokens(msgs)
	if got != 3 { // (11+3)/4 = 3
		t.Errorf("estimateTokens = %d, want 3", got)
	}
}

func TestShouldCompact(t *testing.T) {
	if shouldCompact(80_000, 100_000, 16_384) {
		t.Error("shouldCompact(80000) should be false")
	}
	if !shouldCompact(90_000, 100_000, 16_384) {
		t.Error("shouldCompact(90000) should be true")
	}
}
