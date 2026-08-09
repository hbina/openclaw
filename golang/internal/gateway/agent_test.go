package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
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

type failingProvider struct {
	err error
}

type scriptedProvider struct {
	responses []providers.GenerateResponse
	requests  []providers.GenerateRequest
}

type blockingFirstProvider struct {
	calls        atomic.Int32
	started      chan int
	releaseFirst <-chan struct{}
}

func (provider *blockingFirstProvider) Generate(ctx context.Context, _ *providers.GenerateRequest) (*providers.GenerateResponse, error) {
	call := int(provider.calls.Add(1))
	provider.started <- call
	if call == 1 && provider.releaseFirst != nil {
		select {
		case <-provider.releaseFirst:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &providers.GenerateResponse{
		Message: providers.Message{Role: providers.RoleAssistant, Content: fmt.Sprintf("reply-%d", call)},
	}, nil
}

type timeoutFirstProvider struct {
	calls atomic.Int32
}

func (provider *timeoutFirstProvider) Generate(ctx context.Context, _ *providers.GenerateRequest) (*providers.GenerateResponse, error) {
	if provider.calls.Add(1) == 1 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &providers.GenerateResponse{
		Message: providers.Message{Role: providers.RoleAssistant, Content: "recovered"},
	}, nil
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

func (p *failingProvider) Generate(context.Context, *providers.GenerateRequest) (*providers.GenerateResponse, error) {
	return nil, p.err
}

type recordingChannel struct {
	recipient string
	content   string
	err       error
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
	if c.err != nil {
		return c.err
	}
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
	agent := NewAgent(prov, registry, store, cfg, time.UTC, &fakeEmbedder{})
	return agent, store
}

func chat(agent *Agent, ctx context.Context, channelID, senderID, content string) (string, error) {
	return agent.Chat(ctx, ChatInput{
		ChannelID: channelID,
		SenderID:  senderID,
		Content:   content,
	})
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
	if provider.request.ToolChoice != "auto" || provider.request.MaxTokens != defaultMaxTokens {
		t.Fatalf("unexpected provider controls: %#v", provider.request)
	}
	if system := provider.request.Messages[0].Content; !strings.Contains(system, "The current server time is") ||
		!strings.Contains(system, "(UTC)") || !strings.Contains(system, "Reference UTC time is") {
		t.Fatalf("system prompt does not expose server and UTC time: %q", system)
	}
	if description := provider.request.Tools[0].Function.Description; !strings.Contains(description, "server timezone is UTC") {
		t.Fatalf("reminder tool does not expose server timezone: %q", description)
	}
	// First message: [system, user] — no prior history.
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

func TestAgentRendersAndPersistsTelegramReplyContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reply.sqlite")
	cfg := &config.Config{}
	cfg.Agents.Defaults.Soul = "Be helpful."
	cfg.Agents.Defaults.Identity = "Your name is Test."

	store, err := state.NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	firstProvider := &recordingProvider{}
	firstAgent := NewAgent(firstProvider, channels.NewRegistry(), store, cfg, time.UTC, &fakeEmbedder{})
	input := ChatInput{
		ChannelID: "telegram",
		SenderID:  "100",
		Content:   "Can you move that to 4?",
		Reply: &channels.ReplyContext{
			MessageID:    "1842",
			Author:       channels.ReplyAuthorAssistant,
			Body:         "The appointment is at 3 PM.",
			SelectedText: "3 PM",
		},
	}
	if _, err := firstAgent.Chat(context.Background(), input); err != nil {
		t.Fatalf("first Chat: %v", err)
	}

	const wantRendered = "Reply context:\n" +
		"Author: assistant\n" +
		"Message:\nThe appointment is at 3 PM.\n\n" +
		"Selected text:\n3 PM\n\n" +
		"Current user message:\nCan you move that to 4?"
	if got := firstProvider.request.Messages[1].Content; got != wantRendered {
		t.Fatalf("rendered reply context:\n%s\nwant:\n%s", got, wantRendered)
	}
	if system := firstProvider.request.Messages[0].Content; !strings.Contains(system, "exact earlier message the user selected") ||
		!strings.Contains(system, "rather than unrelated later messages") {
		t.Fatalf("system prompt does not explain reply precedence: %q", system)
	}
	if strings.Contains(strings.ToLower(firstProvider.request.Messages[1].Content), "untrusted") ||
		strings.Contains(firstProvider.request.Messages[1].Content, "1842") {
		t.Fatalf("model-visible reply context contains transport-only metadata: %q", firstProvider.request.Messages[1].Content)
	}

	history, err := store.GetConversationHistory(context.Background(), "telegram", "100")
	if err != nil || len(history) != 2 {
		t.Fatalf("history=%#v err=%v", history, err)
	}
	if history[0].ContentType != state.ContentInboundMessage {
		t.Fatalf("user content type = %q, want %q", history[0].ContentType, state.ContentInboundMessage)
	}
	var persisted persistedInboundMessage
	if err := json.Unmarshal([]byte(history[0].Content), &persisted); err != nil {
		t.Fatalf("decode persisted inbound message: %v", err)
	}
	if persisted.Content != input.Content || persisted.Reply == nil ||
		persisted.Reply.MessageID != "1842" || persisted.Reply.SelectedText != "3 PM" {
		t.Fatalf("persisted inbound message = %#v", persisted)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	reopened, err := state.NewStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	secondProvider := &recordingProvider{}
	secondAgent := NewAgent(secondProvider, channels.NewRegistry(), reopened, cfg, time.UTC, &fakeEmbedder{})
	if _, err := chat(secondAgent, context.Background(), "telegram", "100", "What did I move?"); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if got := secondProvider.request.Messages[1].Content; got != wantRendered {
		t.Fatalf("replayed reply context:\n%s\nwant:\n%s", got, wantRendered)
	}
	if got := secondProvider.request.Messages[3].Content; got != "What did I move?" {
		t.Fatalf("ordinary follow-up changed: %q", got)
	}
}

func TestAgentRendersUnavailableReplyContent(t *testing.T) {
	provider := &recordingProvider{}
	agent, _ := newTestAgent(t, provider, nil, "")
	_, err := agent.Chat(context.Background(), ChatInput{
		ChannelID: "telegram",
		SenderID:  "100",
		Content:   "What is this?",
		Reply: &channels.ReplyContext{
			MessageID:          "90",
			Author:             channels.ReplyAuthorUser,
			ContentUnavailable: true,
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	want := "Reply context:\nAuthor: user\nMessage:\n" +
		"[non-text Telegram message; content unavailable]\n\n" +
		"Current user message:\nWhat is this?"
	if got := provider.request.Messages[1].Content; got != want {
		t.Fatalf("rendered unavailable context = %q, want %q", got, want)
	}
}

func TestAgentRejectsInvalidReplyAuthorBeforePersistence(t *testing.T) {
	provider := &recordingProvider{}
	agent, store := newTestAgent(t, provider, nil, "")
	_, err := agent.Chat(context.Background(), ChatInput{
		ChannelID: "telegram",
		SenderID:  "100",
		Content:   "hello",
		Reply: &channels.ReplyContext{
			Author: "invalid",
			Body:   "source",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid reply author") {
		t.Fatalf("Chat error = %v, want invalid reply author", err)
	}
	history, historyErr := store.GetConversationHistory(context.Background(), "telegram", "100")
	if historyErr != nil || len(history) != 0 {
		t.Fatalf("history=%#v err=%v", history, historyErr)
	}
	if provider.request != nil {
		t.Fatalf("provider called for invalid reply context: %#v", provider.request)
	}
}

func TestAgentLoadsTwoRecentCompleteExchanges(t *testing.T) {
	provider := &recordingProvider{}
	agent, store := newTestAgent(t, provider, nil, "")
	ctx := context.Background()

	const historyRows = 24
	for i := range historyRows {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if err := store.SaveConversationTurn(ctx, "cli", "user-1", role, fmt.Sprintf("history-%02d", i)); err != nil {
			t.Fatalf("SaveConversationTurn(%d): %v", i, err)
		}
	}

	if _, err := chat(agent, ctx, "cli", "user-1", "current message"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// The provider receives the system prompt, the latest two complete exchanges,
	// and the current message. Older exchanges are supplied only by semantic recall.
	if got, want := len(provider.request.Messages), 6; got != want {
		t.Fatalf("provider message count = %d, want %d", got, want)
	}
	if got := provider.request.Messages[1].Content; got != "history-20" {
		t.Fatalf("first recent message = %q, want %q", got, "history-20")
	}
}

func TestAgentDoesNotSummarizeOrTrimLargeHistory(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{{
		Message: providers.Message{Role: providers.RoleAssistant, Content: "reply"},
	}}}
	agent, store := newTestAgent(t, provider, nil, "")
	ctx := context.Background()
	for _, turn := range []struct {
		role    string
		content string
	}{
		{role: "user", content: strings.Repeat("old question ", 15_000)},
		{role: "assistant", content: strings.Repeat("old answer ", 15_000)},
		{role: "user", content: "most recent stored question"},
	} {
		if err := store.SaveConversationTurn(ctx, "cli", "user-1", turn.role, turn.content); err != nil {
			t.Fatalf("SaveConversationTurn: %v", err)
		}
	}

	if _, err := chat(agent, ctx, "cli", "user-1", "current question"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider request count = %d, want one normal generation", len(provider.requests))
	}
	history, err := store.GetConversationHistory(ctx, "cli", "user-1")
	if err != nil {
		t.Fatalf("GetConversationHistory: %v", err)
	}
	if len(history) != 5 {
		t.Fatalf("history row count = %d, want all 5 rows retained", len(history))
	}
}

func TestAgentUsesStartupPersonaSnapshot(t *testing.T) {
	provider := &recordingProvider{}
	store := newTestStoreForAgent(t)
	cfg := &config.Config{}
	cfg.Agents.Defaults.Soul = "Be warm and direct."
	cfg.Agents.Defaults.Identity = "Your name is Jet."
	agent := NewAgent(provider, channels.NewRegistry(), store, cfg, time.UTC, &fakeEmbedder{})
	cfg.Agents.Defaults.Soul = "Changed after startup."
	cfg.Agents.Defaults.Identity = "Your name is Other."
	ctx := context.Background()
	if _, err := chat(agent, ctx, "cli", "user-1", "Who are you?"); err != nil {
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

func listRemindersForTest(t *testing.T, store *state.Store, channelID, senderID string) ([]state.Reminder, error) {
	t.Helper()
	var reminders []state.Reminder
	err := store.WithTx(context.Background(), func(tx *state.Tx) error {
		var err error
		reminders, err = tx.ListReminders(context.Background(), channelID, senderID)
		return err
	})
	return reminders, err
}

func listTasksForTest(t *testing.T, store *state.Store, status state.TaskStatus) ([]state.Task, error) {
	t.Helper()
	var tasks []state.Task
	err := store.WithTx(context.Background(), func(tx *state.Tx) error {
		var err error
		tasks, err = tx.ListTasks(context.Background(), status)
		return err
	})
	return tasks, err
}

func addDueReminderForTest(t *testing.T, store *state.Store, channelID, senderID, message string) state.Reminder {
	t.Helper()
	ctx := context.Background()
	fireAt := time.Now().Add(-time.Minute)
	var id int64
	err := store.WithTx(ctx, func(tx *state.Tx) error {
		var err error
		id, err = tx.AddReminder(ctx, channelID, senderID, message, state.ReminderSchedule{
			Kind: state.ScheduleAt,
			At:   fireAt,
		}, fireAt)
		return err
	})
	if err != nil {
		t.Fatalf("add due reminder: %v", err)
	}
	return state.Reminder{
		ID: int(id), ChannelID: channelID, SenderID: senderID, Message: message,
		Schedule: state.ReminderSchedule{Kind: state.ScheduleAt, At: fireAt}, FireAt: fireAt, Enabled: true,
	}
}

func TestAgentDeliversContextualReminderAndRecordsExchange(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{{
		Message: providers.Message{Role: providers.RoleAssistant, Content: "Warmly remember to bring the charger for today's workday. 🦞"},
	}}}
	channel := &recordingChannel{}
	agent, store := newTestAgent(t, provider, channel, "Be warm and familiar.")
	for _, exchange := range []struct{ user, assistant string }{
		{"first question", "first answer"},
		{"I am heading to work tomorrow.", "Your desk setup is nearly ready."},
		{"I packed my laptop.", "The charger is the remaining item."},
	} {
		seedExchange(t, store, channel.ID(), "owner", exchange.user, exchange.assistant)
	}
	reminder := addDueReminderForTest(t, store, channel.ID(), "owner", "Bring phone charger to work")

	if err := agent.DeliverReminder(context.Background(), reminder); err != nil {
		t.Fatalf("DeliverReminder: %v", err)
	}
	wantNotification := reminderHeader + "Warmly remember to bring the charger for today's workday. 🦞"
	if channel.recipient != "owner" || channel.content != wantNotification {
		t.Fatalf("delivery recipient=%q content=%q", channel.recipient, channel.content)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(provider.requests))
	}
	request := provider.requests[0]
	if request.Model != "default" || request.MaxTokens != reminderMaxTokens || len(request.Tools) != 0 || request.ToolChoice != "" {
		t.Fatalf("reminder provider controls: %#v", request)
	}
	if len(request.Messages) != 6 {
		t.Fatalf("reminder message count = %d, want system + two recent exchanges + event", len(request.Messages))
	}
	system := request.Messages[0].Content
	for _, want := range []string{"Be warm and familiar.", "Your name is Test.", "A stored reminder is now due", "Return only the notification body"} {
		if !strings.Contains(system, want) {
			t.Fatalf("reminder system prompt missing %q: %s", want, system)
		}
	}
	current := request.Messages[len(request.Messages)-1]
	if current.Role != providers.RoleUser || !strings.Contains(current.Content, "Bring phone charger to work") ||
		!strings.Contains(current.Content, reminder.FireAt.In(time.UTC).Format(time.RFC3339)) {
		t.Fatalf("scheduled event message: %#v", current)
	}
	remaining, err := listRemindersForTest(t, store, channel.ID(), "owner")
	if err != nil || len(remaining) != 0 {
		t.Fatalf("remaining reminders=%#v err=%v", remaining, err)
	}
	history, err := store.GetConversationHistory(context.Background(), channel.ID(), "owner")
	if err != nil || len(history) != 8 {
		t.Fatalf("history rows=%d err=%v", len(history), err)
	}
	scheduled, delivered := history[len(history)-2], history[len(history)-1]
	if scheduled.ContentType != state.ContentScheduledReminder || scheduled.Role != "user" ||
		delivered.ContentType != state.ContentText || delivered.Role != "assistant" || delivered.Content != wantNotification {
		t.Fatalf("delivery transcript scheduled=%#v delivered=%#v", scheduled, delivered)
	}
	var payload persistedScheduledReminder
	if err := json.Unmarshal([]byte(scheduled.Content), &payload); err != nil || payload.ReminderID != reminder.ID || payload.Message != reminder.Message {
		t.Fatalf("scheduled payload=%#v err=%v", payload, err)
	}
}

func TestAgentReminderUsesSemanticRecall(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{{
		Message: providers.Message{Role: providers.RoleAssistant, Content: "Time to check the Kyoto train plan."},
	}}}
	channel := &recordingChannel{}
	agent, store := newTestAgent(t, provider, channel, "")
	seedExchange(t, store, "telegram", "another-route", "I am planning Kyoto travel.", "Use trains from Kyoto Station.")
	seedExchange(t, store, channel.ID(), "owner", "I planted tomatoes.", "Water them tomorrow.")
	seedExchange(t, store, channel.ID(), "owner", "The garden is tidy.", "Everything is ready.")
	embedder := &fakeEmbedder{}
	agent.rag = NewRAGService(store, embedder, &fakePromptSizer{contextSize: 10_000}, "embeddinggemma-test", 3, 0.35)
	if err := agent.rag.IndexOnce(context.Background()); err != nil {
		t.Fatalf("IndexOnce: %v", err)
	}
	reminder := addDueReminderForTest(t, store, channel.ID(), "owner", "Review transportation in Japan")

	if err := agent.DeliverReminder(context.Background(), reminder); err != nil {
		t.Fatalf("DeliverReminder: %v", err)
	}
	request := provider.requests[0]
	if len(request.Tools) != 0 || request.ToolChoice != "" {
		t.Fatalf("semantic reminder exposed tools: %#v", request)
	}
	joined := ""
	for _, message := range request.Messages {
		joined += "\n" + message.Content
	}
	if !strings.Contains(joined, "Kyoto Station") || !strings.Contains(joined, "The garden is tidy") {
		t.Fatalf("reminder context missing recalled or recent exchange: %s", joined)
	}
}

func TestAgentReminderFallsBackWhenContextOrModelFails(t *testing.T) {
	for _, test := range []struct {
		name  string
		agent func(*testing.T, *recordingChannel) (*Agent, *state.Store)
	}{
		{
			name: "model",
			agent: func(t *testing.T, channel *recordingChannel) (*Agent, *state.Store) {
				return newTestAgent(t, &failingProvider{err: errors.New("model unavailable")}, channel, "")
			},
		},
		{
			name: "semantic recall",
			agent: func(t *testing.T, channel *recordingChannel) (*Agent, *state.Store) {
				provider := &recordingProvider{}
				agent, store := newTestAgent(t, provider, channel, "")
				agent.rag = NewRAGService(store, &fakeEmbedder{fail: true}, &fakePromptSizer{contextSize: 10_000}, "embeddinggemma-test", 3, 0.35)
				return agent, store
			},
		},
		{
			name: "empty output",
			agent: func(t *testing.T, channel *recordingChannel) (*Agent, *state.Store) {
				return newTestAgent(t, &scriptedProvider{responses: []providers.GenerateResponse{{
					Message: providers.Message{Role: providers.RoleAssistant, Content: "   "},
				}}}, channel, "")
			},
		},
		{
			name: "unexpected tool call",
			agent: func(t *testing.T, channel *recordingChannel) (*Agent, *state.Store) {
				response := providers.GenerateResponse{Message: providers.Message{
					Role: providers.RoleAssistant,
					ToolCalls: []providers.ToolCall{{
						ID:       "unexpected",
						Type:     "function",
						Function: providers.FunctionCall{Name: "manage_reminders", Arguments: `{"action":"list"}`},
					}},
				}}
				return newTestAgent(t, &scriptedProvider{responses: []providers.GenerateResponse{response}}, channel, "")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			channel := &recordingChannel{}
			agent, store := test.agent(t, channel)
			reminder := addDueReminderForTest(t, store, channel.ID(), "owner", "Bring phone charger to work")
			if err := agent.DeliverReminder(context.Background(), reminder); err != nil {
				t.Fatalf("DeliverReminder: %v", err)
			}
			if channel.content != reminderHeader+reminder.Message {
				t.Fatalf("fallback notification = %q", channel.content)
			}
			history, err := store.GetConversationHistory(context.Background(), channel.ID(), "owner")
			if err != nil || len(history) != 2 || history[1].Content != channel.content {
				t.Fatalf("fallback history=%#v err=%v", history, err)
			}
		})
	}
}

func TestAgentReminderSendFailurePreservesDueStateAndTranscript(t *testing.T) {
	channel := &recordingChannel{err: errors.New("telegram unavailable")}
	agent, store := newTestAgent(t, &recordingProvider{}, channel, "")
	reminder := addDueReminderForTest(t, store, channel.ID(), "owner", "Bring phone charger to work")

	err := agent.DeliverReminder(context.Background(), reminder)
	if err == nil || !strings.Contains(err.Error(), "telegram unavailable") {
		t.Fatalf("DeliverReminder error = %v", err)
	}
	remaining, listErr := listRemindersForTest(t, store, channel.ID(), "owner")
	if listErr != nil || len(remaining) != 1 || remaining[0].ID != reminder.ID {
		t.Fatalf("remaining reminders=%#v err=%v", remaining, listErr)
	}
	history, historyErr := store.GetConversationHistory(context.Background(), channel.ID(), "owner")
	if historyErr != nil || len(history) != 0 {
		t.Fatalf("history=%#v err=%v", history, historyErr)
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

	reply, err := chat(agent, ctx, "cli", "user-1", "I prefer espresso")
	if err != nil || reply != "I'll remember that." {
		t.Fatalf("first Chat: reply=%q err=%v", reply, err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider request count = %d, want 2", len(provider.requests))
	}
	followup := provider.requests[1]
	if len(followup.Tools) != 4 {
		t.Fatalf("tool definition count = %d, want 4", len(followup.Tools))
	}
	if len(followup.Messages) != 4 {
		t.Fatalf("follow-up message count = %d, want 4", len(followup.Messages))
	}
	if followup.Messages[2].ToolCalls[0].ID != "memory-1" || followup.Messages[3].ToolCallID != "memory-1" {
		t.Fatalf("tool call correlation was not retained: %#v", followup.Messages)
	}

	reply, err = chat(agent, ctx, "cli", "user-1", "What coffee do I prefer?")
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

	history, err := store.GetConversationHistory(ctx, "cli", "user-1")
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
	if _, err := chat(agent, context.Background(), "cli", "user-1", "remind me"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	toolMessage := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if toolMessage.Role != providers.RoleTool || !strings.Contains(toolMessage.Content, "unknown field") {
		t.Fatalf("model did not receive validation error: %#v", toolMessage)
	}
}

func TestReminderRequestUsesAutomaticUnifiedReminderTool(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{
		{Message: providers.Message{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{{
			ID: "list-required", Type: "function", Function: providers.FunctionCall{
				Name: "manage_reminders", Arguments: `{"action":"list"}`,
			},
		}}}},
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "No reminders."}},
	}}
	agent, _ := newTestAgent(t, provider, nil, "")
	reply, err := chat(agent, context.Background(), "cli", "user-1", "Please list my reminders")
	if err != nil || reply != "No reminders." {
		t.Fatalf("Chat: reply=%q err=%v", reply, err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(provider.requests))
	}
	first := provider.requests[0]
	if first.ToolChoice != "auto" || len(first.Tools) != 4 || first.Tools[0].Function.Name != "manage_reminders" || first.Tools[1].Function.Name != "manage_tasks" {
		t.Fatalf("first reminder request did not expose automatic unified tool: %#v", first)
	}
	if first.MaxTokens != defaultMaxTokens || provider.requests[1].ToolChoice != "auto" || len(provider.requests[1].Tools) != 4 {
		t.Fatalf("follow-up request controls: %#v", provider.requests[1])
	}
}

func TestQuotedReminderTextDoesNotForceToolUse(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{{
		Message: providers.Message{
			Role:    providers.RoleAssistant,
			Content: "It sounds supportive, but it could be more direct.",
		},
	}}}
	agent, store := newTestAgent(t, provider, nil, "")
	content := `The message says "I will keep the reminder active." What do you think about this?`
	reply, err := chat(agent, context.Background(), "cli", "user-1", content)
	if err != nil || reply != "It sounds supportive, but it could be more direct." {
		t.Fatalf("Chat: reply=%q err=%v", reply, err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider request count = %d, want 1", len(provider.requests))
	}
	request := provider.requests[0]
	if request.ToolChoice != "auto" || len(request.Tools) != 4 {
		t.Fatalf("quoted reminder text forced tool controls: %#v", request)
	}
	reminders, err := listRemindersForTest(t, store, "cli", "user-1")
	if err != nil || len(reminders) != 0 {
		t.Fatalf("reminders=%#v err=%v", reminders, err)
	}
	history, err := store.GetConversationHistory(context.Background(), "cli", "user-1")
	if err != nil || len(history) != 2 || history[0].ContentType != state.ContentInboundMessage || history[1].ContentType != state.ContentText {
		t.Fatalf("history=%#v err=%v", history, err)
	}
}

func TestAgentWarnsAboutUncommittedReminderClaim(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{{
		Message: providers.Message{
			Role:    providers.RoleAssistant,
			Content: "I have scheduled a reminder for Tuesday.",
		},
	}}}
	agent, store := newTestAgent(t, provider, nil, "")
	reply, err := chat(agent, context.Background(), "cli", "user-1", "Please remind me Tuesday")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.HasSuffix(reply, uncommittedReminderNote) {
		t.Fatalf("reply missing uncommitted reminder note: %q", reply)
	}
	history, err := store.GetConversationHistory(context.Background(), "cli", "user-1")
	if err != nil || len(history) != 2 || history[1].Content != reply {
		t.Fatalf("stored warning reply history=%#v err=%v", history, err)
	}
}

func TestAgentDoesNotWarnAfterCommittedReminderMutation(t *testing.T) {
	call := providers.ToolCall{
		ID:   "add-reminder",
		Type: "function",
		Function: providers.FunctionCall{
			Name:      "manage_reminders",
			Arguments: `{"action":"add","items":[{"message":"Workout","schedule":{"kind":"at","at":"2099-01-02T19:00:00Z"}}]}`,
		},
	}
	provider := &scriptedProvider{responses: []providers.GenerateResponse{
		{Message: providers.Message{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{call}}},
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "I have scheduled the reminder. ID 1."}},
	}}
	agent, store := newTestAgent(t, provider, nil, "")
	reply, err := chat(agent, context.Background(), "cli", "user-1", "Please remind me Tuesday")
	if err != nil || reply != "I have scheduled the reminder. Reminder ID 1." {
		t.Fatalf("Chat: reply=%q err=%v", reply, err)
	}
	reminders, err := listRemindersForTest(t, store, "cli", "user-1")
	if err != nil || len(reminders) != 1 {
		t.Fatalf("reminders=%#v err=%v", reminders, err)
	}
}

func TestRejectedReminderMutationCannotBackSuccessClaim(t *testing.T) {
	call := providers.ToolCall{
		ID:   "bad-add",
		Type: "function",
		Function: providers.FunctionCall{
			Name:      "manage_reminders",
			Arguments: `{"action":"add","unexpected":true}`,
		},
	}
	provider := &scriptedProvider{responses: []providers.GenerateResponse{
		{Message: providers.Message{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{call}}},
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "I have added the reminder."}},
	}}
	agent, store := newTestAgent(t, provider, nil, "")
	reply, err := chat(agent, context.Background(), "cli", "user-1", "Please add a reminder")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.HasSuffix(reply, uncommittedReminderNote) {
		t.Fatalf("reply missing uncommitted reminder note: %q", reply)
	}
	reminders, err := listRemindersForTest(t, store, "cli", "user-1")
	if err != nil || len(reminders) != 0 {
		t.Fatalf("reminders=%#v err=%v", reminders, err)
	}
}

func TestReminderDiscussionDoesNotReceiveCommitmentWarning(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{{
		Message: providers.Message{
			Role:    providers.RoleAssistant,
			Content: "The quoted reminder wording sounds supportive.",
		},
	}}}
	agent, _ := newTestAgent(t, provider, nil, "")
	reply, err := chat(agent, context.Background(), "cli", "user-1", "What do you think of this reminder wording?")
	if err != nil || strings.Contains(reply, uncommittedReminderNote) {
		t.Fatalf("Chat: reply=%q err=%v", reply, err)
	}
}

func TestUnbackedReminderCommitmentDetection(t *testing.T) {
	tests := []struct {
		content string
		want    bool
	}{
		{"I'll remind you tomorrow.", true},
		{"I will create a reminder for Tuesday.", true},
		{"I've removed the reminder.", true},
		{"I have updated your reminders.", true},
		{"The quoted reminder wording sounds supportive.", false},
		{"You asked whether an existing reminder is useful.", false},
		{"I couldn't schedule that reminder.", false},
		{"I have scheduled a reminder.\n\n" + uncommittedReminderNote, false},
	}
	for _, test := range tests {
		if got := hasUnbackedReminderCommitment(test.content); got != test.want {
			t.Errorf("hasUnbackedReminderCommitment(%q) = %v, want %v", test.content, got, test.want)
		}
	}
}

func TestAgentTaskMutationBacksSuccessClaim(t *testing.T) {
	call := providers.ToolCall{
		ID: "task-add", Type: "function",
		Function: providers.FunctionCall{Name: "manage_tasks", Arguments: `{"action":"add","descriptions":["Prepare launch notes"]}`},
	}
	provider := &scriptedProvider{responses: []providers.GenerateResponse{
		{Message: providers.Message{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{call}}},
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "I have created the task. ID: 1."}},
	}}
	agent, store := newTestAgent(t, provider, nil, "")
	reply, err := chat(agent, context.Background(), "telegram", "owner", "Add a task to prepare launch notes")
	if err != nil || reply != "I have created the task. Task ID: 1." || strings.Contains(reply, uncommittedTaskNote) {
		t.Fatalf("Chat: reply=%q err=%v", reply, err)
	}
	tasks, err := listTasksForTest(t, store, state.TaskOpen)
	if err != nil || len(tasks) != 1 || tasks[0].Description != "Prepare launch notes" {
		t.Fatalf("tasks=%#v err=%v", tasks, err)
	}
	reminders, err := listRemindersForTest(t, store, "telegram", "owner")
	if err != nil || len(reminders) != 0 {
		t.Fatalf("task request created reminders=%#v err=%v", reminders, err)
	}
}

func TestFailedTaskMutationCannotBackSuccessClaim(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{
		{Message: providers.Message{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{{
			ID: "bad-task", Type: "function",
			Function: providers.FunctionCall{Name: "manage_tasks", Arguments: `{"action":"add","descriptions":["task"],"due_at":"tomorrow"}`},
		}}}},
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "I have added the task."}},
	}}
	agent, store := newTestAgent(t, provider, nil, "")
	reply, err := chat(agent, context.Background(), "cli", "owner", "Add a task")
	if err != nil || !strings.HasSuffix(reply, uncommittedTaskNote) {
		t.Fatalf("Chat: reply=%q err=%v", reply, err)
	}
	tasks, err := listTasksForTest(t, store, state.TaskAll)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("tasks=%#v err=%v", tasks, err)
	}
}

func TestNoTimeReminderClarificationCreatesNoState(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{{Message: providers.Message{
		Role: providers.RoleAssistant, Content: "When would you like me to remind you?",
	}}}}
	agent, store := newTestAgent(t, provider, nil, "")
	reply, err := chat(agent, context.Background(), "cli", "owner", "Remind me to submit the form")
	if err != nil || reply != "When would you like me to remind you?" || len(provider.requests) != 1 {
		t.Fatalf("Chat: reply=%q requests=%d err=%v", reply, len(provider.requests), err)
	}
	if !strings.Contains(provider.requests[0].Messages[0].Content, "create neither a task nor a reminder") {
		t.Fatalf("system prompt lacks no-time reminder rule: %q", provider.requests[0].Messages[0].Content)
	}
	tasks, taskErr := listTasksForTest(t, store, state.TaskAll)
	reminders, reminderErr := listRemindersForTest(t, store, "cli", "owner")
	if taskErr != nil || reminderErr != nil || len(tasks) != 0 || len(reminders) != 0 {
		t.Fatalf("tasks=%#v reminders=%#v taskErr=%v reminderErr=%v", tasks, reminders, taskErr, reminderErr)
	}
}

func TestAgentCombinedTaskAndReminderListingUsesSeparateTools(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{
		{Message: providers.Message{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{
			{ID: "list-reminders", Type: "function", Function: providers.FunctionCall{Name: "manage_reminders", Arguments: `{"action":"list"}`}},
			{ID: "list-tasks", Type: "function", Function: providers.FunctionCall{Name: "manage_tasks", Arguments: `{"action":"list"}`}},
		}}},
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "Tasks\n- Task ID: 1 — Prepare report\n\nReminders\n- None"}},
	}}
	agent, _ := newTestAgent(t, provider, nil, "")
	reply, err := chat(agent, context.Background(), "cli", "owner", "List my tasks and reminders")
	if err != nil || !strings.Contains(reply, "Tasks\n") || !strings.Contains(reply, "Reminders\n") {
		t.Fatalf("Chat: reply=%q err=%v", reply, err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("requests=%d, want 2", len(provider.requests))
	}
	calls := provider.requests[1].Messages[len(provider.requests[1].Messages)-3].ToolCalls
	if len(calls) != 2 || calls[0].Function.Name != "manage_reminders" || calls[1].Function.Name != "manage_tasks" {
		t.Fatalf("combined listing calls=%#v", calls)
	}
}

func TestAgentCompletedTaskHistoryUsesExplicitFilter(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{
		{Message: providers.Message{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{{
			ID: "completed-tasks", Type: "function",
			Function: providers.FunctionCall{Name: "manage_tasks", Arguments: `{"action":"list","status":"completed"}`},
		}}}},
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "No completed tasks."}},
	}}
	agent, _ := newTestAgent(t, provider, nil, "")
	if _, err := chat(agent, context.Background(), "cli", "owner", "Show my completed task history"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	toolCall := provider.requests[1].Messages[len(provider.requests[1].Messages)-2].ToolCalls[0]
	if toolCall.Function.Name != "manage_tasks" || !strings.Contains(toolCall.Function.Arguments, `"status":"completed"`) {
		t.Fatalf("completed history call=%#v", toolCall)
	}
}

func TestUnbackedTaskCommitmentDetection(t *testing.T) {
	tests := []struct {
		content string
		want    bool
	}{
		{"I have added the task.", true},
		{"I'll complete that task.", true},
		{"I removed your tasks.", true},
		{"The task wording is clear.", false},
		{"I couldn't update the task.", false},
		{"I have completed the task.\n\n" + uncommittedTaskNote, false},
	}
	for _, test := range tests {
		if got := hasUnbackedTaskCommitment(test.content); got != test.want {
			t.Errorf("hasUnbackedTaskCommitment(%q) = %v, want %v", test.content, got, test.want)
		}
	}
}

func TestQualifyPersistedIDLabels(t *testing.T) {
	for _, test := range []struct {
		name                   string
		content                string
		taskUsed, reminderUsed bool
		want                   string
	}{
		{"task bare", "**ID 12** and ID: 13", true, false, "**Task ID 12** and Task ID: 13"},
		{"reminder bare", "ID #7", false, true, "Reminder ID #7"},
		{"already qualified", "Task ID 2 and Reminder ID: 3", true, false, "Task ID 2 and Reminder ID: 3"},
		{"mixed tools unchanged", "ID 4", true, true, "ID 4"},
		{"no tools unchanged", "ID 5", false, false, "ID 5"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := qualifyPersistedIDLabels(test.content, test.taskUsed, test.reminderUsed); got != test.want {
				t.Fatalf("qualifyPersistedIDLabels() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAgentSerializesTurnsWithinConversation(t *testing.T) {
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	provider := &blockingFirstProvider{
		started:      make(chan int, 2),
		releaseFirst: release,
	}
	agent, store := newTestAgent(t, provider, nil, "")
	errs := make(chan error, 2)

	go func() {
		_, err := chat(agent, context.Background(), "telegram", "user-1", "first")
		errs <- err
	}()
	if call := <-provider.started; call != 1 {
		t.Fatalf("first provider call = %d, want 1", call)
	}
	go func() {
		_, err := chat(agent, context.Background(), "telegram", "user-1", "second")
		errs <- err
	}()

	select {
	case call := <-provider.started:
		t.Fatalf("same-conversation call %d started before call 1 completed", call)
	case <-time.After(75 * time.Millisecond):
	}

	close(release)
	released = true
	select {
	case call := <-provider.started:
		if call != 2 {
			t.Fatalf("second provider call = %d, want 2", call)
		}
	case <-time.After(time.Second):
		t.Fatal("second same-conversation call did not start after release")
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("Chat: %v", err)
		}
	}

	history, err := store.GetConversationHistory(context.Background(), "telegram", "user-1")
	if err != nil {
		t.Fatalf("GetConversationHistory: %v", err)
	}
	reconstructed, reconstructErr := reconstructHistory(history)
	if len(history) != 4 || reconstructErr != nil ||
		reconstructed[0].Role != providers.RoleUser || reconstructed[0].Content != "first" ||
		reconstructed[1].Role != providers.RoleAssistant || reconstructed[1].Content != "reply-1" ||
		reconstructed[2].Role != providers.RoleUser || reconstructed[2].Content != "second" ||
		reconstructed[3].Role != providers.RoleAssistant || reconstructed[3].Content != "reply-2" {
		t.Fatalf("conversation history was not serialized: %#v", history)
	}
}

func TestAgentAllowsDifferentConversationsToRunConcurrently(t *testing.T) {
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	provider := &blockingFirstProvider{
		started:      make(chan int, 2),
		releaseFirst: release,
	}
	agent, _ := newTestAgent(t, provider, nil, "")
	errs := make(chan error, 2)

	go func() {
		_, err := chat(agent, context.Background(), "telegram", "user-1", "first")
		errs <- err
	}()
	if call := <-provider.started; call != 1 {
		t.Fatalf("first provider call = %d, want 1", call)
	}
	go func() {
		_, err := chat(agent, context.Background(), "telegram", "user-2", "second")
		errs <- err
	}()

	select {
	case call := <-provider.started:
		if call != 2 {
			t.Fatalf("concurrent provider call = %d, want 2", call)
		}
	case <-time.After(time.Second):
		t.Fatal("different conversation was unnecessarily serialized")
	}
	close(release)
	released = true
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("Chat: %v", err)
		}
	}
}

func TestAgentReleasesConversationLockAfterTimeout(t *testing.T) {
	provider := &timeoutFirstProvider{}
	agent, _ := newTestAgent(t, provider, nil, "")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := chat(agent, ctx, "telegram", "user-1", "first"); err == nil {
		t.Fatal("first Chat succeeded, want timeout")
	}

	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), time.Second)
	defer recoveryCancel()
	reply, err := chat(agent, recoveryCtx, "telegram", "user-1", "second")
	if err != nil || reply != "recovered" {
		t.Fatalf("second Chat: reply=%q err=%v", reply, err)
	}
}

func TestAgentRejectsCanceledContextBeforeTurn(t *testing.T) {
	provider := &timeoutFirstProvider{}
	agent, _ := newTestAgent(t, provider, nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := chat(agent, ctx, "telegram", "user-1", "message"); err == nil {
		t.Fatal("Chat succeeded with a canceled context")
	}
	if calls := provider.calls.Load(); calls != 0 {
		t.Fatalf("provider calls = %d, want 0", calls)
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
	reply, err := chat(agent, context.Background(), "cli", "user-1", "keep listing")
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
	if _, err := chat(agent, context.Background(), "cli", "user-1", "hello"); err != nil {
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
