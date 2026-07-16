package gateway

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/openclaw/go/internal/channels"
	"github.com/openclaw/openclaw/go/internal/config"
	"github.com/openclaw/openclaw/go/internal/providers"
	"github.com/openclaw/openclaw/go/internal/state"
)

type recordingProvider struct {
	request *providers.GenerateRequest
}

type scriptedProvider struct {
	responses []providers.GenerateResponse
	requests  []providers.GenerateRequest
}

func (provider *scriptedProvider) Generate(_ context.Context, request *providers.GenerateRequest) (*providers.GenerateResponse, error) {
	copyRequest := *request
	copyRequest.Messages = append([]providers.Message(nil), request.Messages...)
	copyRequest.Tools = append([]providers.ToolDefinition(nil), request.Tools...)
	provider.requests = append(provider.requests, copyRequest)
	if len(provider.responses) == 0 {
		return &providers.GenerateResponse{Message: providers.Message{Role: providers.RoleAssistant, Content: "done"}}, nil
	}
	response := provider.responses[0]
	provider.responses = provider.responses[1:]
	return &response, nil
}

func (p *recordingProvider) Generate(_ context.Context, req *providers.GenerateRequest) (*providers.GenerateResponse, error) {
	p.request = req
	return &providers.GenerateResponse{Message: providers.Message{Role: providers.RoleAssistant, Content: "assistant reply"}}, nil
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

func newTestAgent(t *testing.T, prov providers.Provider, ch *recordingChannel, soul string) (*Agent, *state.Store) {
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
	if soul == "" {
		soul = "Be helpful."
	}
	cfg := &config.Config{}
	cfg.Agents.Defaults.Soul = soul
	cfg.Agents.Defaults.Identity = "Your name is Test."
	agent := NewAgent(prov, registry, store, cfg, time.UTC)
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
	if system := provider.request.Messages[0].Content; !strings.Contains(system, "The current server time is") ||
		!strings.Contains(system, "(UTC)") || !strings.Contains(system, "Reference UTC time is") {
		t.Fatalf("system prompt does not expose server and UTC time: %q", system)
	}
	if description := provider.request.Tools[0].Function.Description; !strings.Contains(description, "server timezone is UTC") {
		t.Fatalf("reminder tool does not expose server timezone: %q", description)
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
		t.Fatalf("system prompt missing soul: %q", sys)
	}
	if !strings.Contains(sys, "Soul:\nBe concise and direct.\n\nIdentity:\nYour name is Test.\n") {
		t.Fatalf("system prompt missing exact persona sections: %q", sys)
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

func TestAgentUsesStartupPersonaSnapshot(t *testing.T) {
	provider := &recordingProvider{}
	store := newTestStoreForAgent(t)
	cfg := &config.Config{}
	cfg.Agents.Defaults.Soul = "Be warm and direct."
	cfg.Agents.Defaults.Identity = "Your name is Jet."
	agent := NewAgent(provider, channels.NewRegistry(), store, cfg, time.UTC)
	cfg.Agents.Defaults.Soul = "Changed after startup."
	cfg.Agents.Defaults.Identity = "Your name is Other."
	ctx := context.Background()
	if _, err := agent.Chat(ctx, "cli", "user-1", "Who are you?"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	systemPrompt := provider.request.Messages[0].Content
	if !strings.Contains(systemPrompt, "Soul:\nBe warm and direct.\n\nIdentity:\nYour name is Jet.\n") ||
		strings.Contains(systemPrompt, "Changed after startup") || strings.Contains(systemPrompt, "Your name is Other") {
		t.Fatalf("system prompt did not preserve startup persona: %q", systemPrompt)
	}
}

func newTestStoreForAgent(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "agent.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
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

func TestAgentExecutesAndReplaysStructuredToolCalls(t *testing.T) {
	toolCall := providers.ToolCall{
		ID:   "memory-1",
		Type: "function",
		Function: providers.FunctionCall{
			Name:      "store_memory",
			Arguments: `{"content":"The user prefers espresso."}`,
		},
	}
	provider := &scriptedProvider{responses: []providers.GenerateResponse{
		{Message: providers.Message{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{toolCall}}, FinishReason: "tool_calls"},
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "I'll remember that."}, FinishReason: "stop"},
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "You prefer espresso."}, FinishReason: "stop"},
	}}
	agent, store := newTestAgent(t, provider, nil, "")
	ctx := context.Background()

	reply, err := agent.Chat(ctx, "cli", "user-1", "I prefer espresso")
	if err != nil || reply != "I'll remember that." {
		t.Fatalf("first Chat: reply=%q err=%v", reply, err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider request count = %d, want 2", len(provider.requests))
	}
	followup := provider.requests[1]
	if len(followup.Tools) != 3 {
		t.Fatalf("tool definition count = %d, want 3", len(followup.Tools))
	}
	if len(followup.Messages) != 4 {
		t.Fatalf("follow-up message count = %d, want 4", len(followup.Messages))
	}
	if followup.Messages[2].ToolCalls[0].ID != "memory-1" || followup.Messages[3].ToolCallID != "memory-1" {
		t.Fatalf("tool call correlation was not retained: %#v", followup.Messages)
	}

	reply, err = agent.Chat(ctx, "cli", "user-1", "What coffee do I prefer?")
	if err != nil || reply != "You prefer espresso." {
		t.Fatalf("second Chat: reply=%q err=%v", reply, err)
	}
	replay := provider.requests[2].Messages
	foundCall, foundResult := false, false
	for _, message := range replay {
		foundCall = foundCall || len(message.ToolCalls) == 1 && message.ToolCalls[0].ID == "memory-1"
		foundResult = foundResult || message.Role == providers.RoleTool && message.ToolCallID == "memory-1"
	}
	if !foundCall || !foundResult {
		t.Fatalf("structured history was not replayed: %#v", replay)
	}

	history, err := store.GetRecentHistory(ctx, "cli", "user-1", 20, 0)
	if err != nil || len(history) != 6 {
		t.Fatalf("history=%#v err=%v", history, err)
	}
	if history[1].ContentType != state.ContentToolCall || history[2].ContentType != state.ContentToolResult {
		t.Fatalf("structured rows missing: %#v", history)
	}
}

func TestAgentReturnsToolValidationErrorToModel(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{
		{Message: providers.Message{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{{
			ID: "bad-1", Type: "function", Function: providers.FunctionCall{
				Name: "manage_reminders", Arguments: `{"action":"list","sender_id":"other"}`,
			},
		}}}},
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "I couldn't schedule that."}},
	}}
	agent, _ := newTestAgent(t, provider, nil, "")
	if _, err := agent.Chat(context.Background(), "cli", "user-1", "remind me"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	toolMessage := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if toolMessage.Role != providers.RoleTool || !strings.Contains(toolMessage.Content, "unknown field") {
		t.Fatalf("model did not receive validation error: %#v", toolMessage)
	}
}

func TestReminderRequestRequiresUnifiedReminderTool(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{
		{Message: providers.Message{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{{
			ID: "list-required", Type: "function", Function: providers.FunctionCall{
				Name: "manage_reminders", Arguments: `{"action":"list"}`,
			},
		}}}},
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "No reminders."}},
	}}
	agent, _ := newTestAgent(t, provider, nil, "")
	reply, err := agent.Chat(context.Background(), "cli", "user-1", "Please list my reminders")
	if err != nil || reply != "No reminders." {
		t.Fatalf("Chat: reply=%q err=%v", reply, err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(provider.requests))
	}
	first := provider.requests[0]
	if first.ToolChoice != "required" || len(first.Tools) != 1 || first.Tools[0].Function.Name != "manage_reminders" {
		t.Fatalf("first reminder request did not require unified tool: %#v", first)
	}
	if provider.requests[1].ToolChoice != "auto" || len(provider.requests[1].Tools) != 3 {
		t.Fatalf("follow-up request controls: %#v", provider.requests[1])
	}
}

func TestAgentStopsAfterMaximumToolRounds(t *testing.T) {
	responses := make([]providers.GenerateResponse, maxToolRounds)
	for index := range responses {
		responses[index] = providers.GenerateResponse{Message: providers.Message{
			Role: providers.RoleAssistant,
			ToolCalls: []providers.ToolCall{{
				ID: "list-" + string(rune('a'+index)), Type: "function",
				Function: providers.FunctionCall{Name: "manage_reminders", Arguments: `{"action":"list"}`},
			}},
		}}
	}
	provider := &scriptedProvider{responses: responses}
	agent, _ := newTestAgent(t, provider, nil, "")
	reply, err := agent.Chat(context.Background(), "cli", "user-1", "keep listing")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(provider.requests) != maxToolRounds || !strings.Contains(reply, "safety limit") {
		t.Fatalf("requests=%d reply=%q", len(provider.requests), reply)
	}
}

func TestToolDefinitionsDoNotExposeRoutingIdentity(t *testing.T) {
	provider := &recordingProvider{}
	agent, _ := newTestAgent(t, provider, nil, "")
	if _, err := agent.Chat(context.Background(), "cli", "user-1", "hello"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	payload, err := json.Marshal(provider.request.Tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	if strings.Contains(string(payload), "sender_id") || strings.Contains(string(payload), "channel_id") {
		t.Fatalf("tool schema leaks trusted identity: %s", payload)
	}
}

func TestReconstructHistoryKeepsOnlyCompleteToolSequences(t *testing.T) {
	turns := []state.ConversationTurn{
		{ID: 1, Role: "tool", ContentType: state.ContentToolResult, Content: `{"tool_call_id":"old","name":"search_memory","content":"{}","is_error":false}`},
		{ID: 2, Role: "user", ContentType: state.ContentText, Content: "remember this"},
		{ID: 3, Role: "assistant", ContentType: state.ContentToolCall, Content: `{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"store_memory","arguments":"{}"}}]}`},
		{ID: 4, Role: "tool", ContentType: state.ContentToolResult, Content: `{"tool_call_id":"call-1","name":"store_memory","content":"{\"stored\":true}","is_error":false}`},
		{ID: 5, Role: "assistant", ContentType: state.ContentText, Content: "done"},
	}
	messages, err := reconstructHistory(turns)
	if err != nil {
		t.Fatalf("reconstructHistory: %v", err)
	}
	if len(messages) != 4 || messages[0].Role != providers.RoleUser || messages[2].ToolCallID != "call-1" {
		t.Fatalf("unexpected reconstructed history: %#v", messages)
	}

	incomplete, err := reconstructHistory(turns[:3])
	if err != nil {
		t.Fatalf("reconstruct incomplete history: %v", err)
	}
	if len(incomplete) != 1 || incomplete[0].Role != providers.RoleUser {
		t.Fatalf("incomplete tool sequence was retained: %#v", incomplete)
	}
}
