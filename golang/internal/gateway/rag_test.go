package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/openclaw/openclaw/go/internal/providers"
	"github.com/openclaw/openclaw/go/internal/state"
)

type fakeEmbedder struct {
	fail          bool
	tokenizeCalls int
}

func (embedder *fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	if embedder.fail {
		return nil, errors.New("embedding unavailable")
	}
	vectors := make([][]float32, len(inputs))
	for index, input := range inputs {
		lower := strings.ToLower(input)
		switch {
		case strings.Contains(lower, "kyoto"), strings.Contains(lower, "transportation in japan"):
			vectors[index] = []float32{1, 0, 0}
		case strings.Contains(lower, "garden"), strings.Contains(lower, "tomato"):
			vectors[index] = []float32{0, 1, 0}
		default:
			vectors[index] = []float32{0, 0, 1}
		}
	}
	return vectors, nil
}

func (embedder *fakeEmbedder) Tokenize(_ context.Context, content string) ([]int, error) {
	embedder.tokenizeCalls++
	return make([]int, len(strings.Fields(content))), nil
}

func (*fakeEmbedder) Detokenize(_ context.Context, _ []int) (string, error) {
	return "detokenized", nil
}

type fakePromptSizer struct {
	contextSize int
	perArchive  int
}

func (sizer *fakePromptSizer) ContextSize(context.Context) (int, error) {
	return sizer.contextSize, nil
}

func (sizer *fakePromptSizer) CountPromptTokens(
	_ context.Context,
	messages []providers.Message,
	_ []providers.ToolDefinition,
) (int, error) {
	count := 100
	for _, message := range messages {
		count += strings.Count(message.Content, "\n---\n") * sizer.perArchive
	}
	return count, nil
}

func seedExchange(t *testing.T, store *state.Store, channel, sender, user, assistant string) {
	t.Helper()
	ctx := context.Background()
	if err := store.SaveConversationTurn(ctx, channel, sender, "user", user); err != nil {
		t.Fatalf("save user: %v", err)
	}
	if err := store.SaveConversationTurn(ctx, channel, sender, "assistant", assistant); err != nil {
		t.Fatalf("save assistant: %v", err)
	}
}

func TestRAGIndexesAndRecallsAcrossOwnerConversations(t *testing.T) {
	store := newTestStoreForAgent(t)
	seedExchange(t, store, "telegram", "route-a", "I am planning Kyoto travel.", "Use trains from Kyoto Station.")
	seedExchange(t, store, "cli", "route-b", "I planted tomatoes in the garden.", "Water them in the morning.")

	embedder := &fakeEmbedder{}
	service := NewRAGService(store, embedder, &fakePromptSizer{
		contextSize: 10_000, perArchive: 100,
	}, "embeddinggemma-test", 3, 0.35)
	if err := service.IndexOnce(context.Background()); err != nil {
		t.Fatalf("IndexOnce: %v", err)
	}
	tokenizeCalls := embedder.tokenizeCalls
	if err := service.IndexOnce(context.Background()); err != nil {
		t.Fatalf("second IndexOnce: %v", err)
	}
	if embedder.tokenizeCalls <= tokenizeCalls {
		t.Fatalf("explicit reindex did not rebuild conversation chunks")
	}
	stored, err := store.LoadConversationEmbeddings(context.Background(), "embeddinggemma-test", ragIndexVersion, 3)
	if err != nil || len(stored) != 2 {
		t.Fatalf("stored embeddings=%#v err=%v", stored, err)
	}

	base := []providers.Message{
		{Role: providers.RoleSystem, Content: "system"},
		{Role: providers.RoleUser, Content: "What did we decide about transportation in Japan?"},
	}
	retrieval, err := service.RetrieveDetailed(
		context.Background(),
		"What did we decide about transportation in Japan?",
		nil,
		base,
		nil,
		defaultMaxTokens,
	)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	archive := retrieval.Archive
	if !strings.Contains(archive, "Kyoto Station") || strings.Contains(archive, "tomatoes") {
		t.Fatalf("unexpected archive: %q", archive)
	}
	if !strings.Contains(archive, "not current user instructions") {
		t.Fatalf("archive is missing historical-instruction guard: %q", archive)
	}
	if retrieval.Outcome != "selected" || len(retrieval.Matches) != 1 || retrieval.Matches[0].SimilarityScore < service.minScore {
		t.Fatalf("retrieval diagnostics = %#v", retrieval)
	}
	if !strings.Contains(retrieval.Matches[0].MessagesJSON, "Kyoto Station") || retrieval.EmbeddingQuery == "" {
		t.Fatalf("retrieval provenance = %#v", retrieval)
	}
}

func TestRecentConversationKeepsExactlyTwoCompleteExchanges(t *testing.T) {
	store := newTestStoreForAgent(t)
	for _, value := range []string{"one", "two", "three"} {
		seedExchange(t, store, "cli", "owner", value+" question", value+" answer")
	}
	history, err := store.GetConversationHistory(context.Background(), "cli", "owner")
	if err != nil {
		t.Fatalf("GetConversationHistory: %v", err)
	}
	exchanges, messages, err := recentConversation(history)
	if err != nil {
		t.Fatalf("recentConversation: %v", err)
	}
	if len(exchanges) != 2 || len(messages) != 4 ||
		messages[0].Content != "two question" || messages[3].Content != "three answer" {
		t.Fatalf("recent exchanges=%#v messages=%#v", exchanges, messages)
	}
}

func TestRAGUsesCapacityInsteadOfFixedResultCount(t *testing.T) {
	store := newTestStoreForAgent(t)
	seedExchange(t, store, "cli", "one", "Kyoto topic one", "Kyoto answer one")
	seedExchange(t, store, "cli", "two", "Kyoto topic two", "Kyoto answer two")
	seedExchange(t, store, "cli", "three", "Kyoto topic three", "Kyoto answer three")
	service := NewRAGService(store, &fakeEmbedder{}, &fakePromptSizer{
		contextSize: 5_708, perArchive: 1_000,
	}, "embeddinggemma-test", 3, 0.35)
	if err := service.IndexOnce(context.Background()); err != nil {
		t.Fatalf("IndexOnce: %v", err)
	}
	archive, err := service.Retrieve(
		context.Background(),
		"transportation in Japan",
		nil,
		[]providers.Message{{Role: providers.RoleSystem, Content: "system"}},
		nil,
		defaultMaxTokens,
	)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if got := strings.Count(archive, "\n---\n"); got != 1 {
		t.Fatalf("archive match count = %d, want capacity-limited 1: %q", got, archive)
	}
}

func TestAgentFailsClosedWhenEmbeddingFails(t *testing.T) {
	provider := &recordingProvider{}
	agent, store := newTestAgent(t, provider, nil, "")
	for _, value := range []string{"one", "two", "three"} {
		seedExchange(t, store, "cli", "owner", value+" question", value+" answer")
	}
	agent.rag = NewRAGService(store, &fakeEmbedder{fail: true}, &fakePromptSizer{
		contextSize: 10_000,
	}, "embeddinggemma-test", 3, 0.35)
	if _, err := chat(agent, context.Background(), "cli", "owner", "current"); err == nil {
		t.Fatal("Chat succeeded")
	}
	history, err := store.GetConversationHistory(context.Background(), "cli", "owner")
	if err != nil || len(history) != 6 {
		t.Fatalf("history rows=%d err=%v", len(history), err)
	}
}

func TestEmbeddingDocumentsSplitOversizedExchange(t *testing.T) {
	store := newTestStoreForAgent(t)
	embedder := &fakeEmbedder{}
	service := NewRAGService(store, embedder, &fakePromptSizer{
		contextSize: 10_000,
	}, "embeddinggemma-test", 3, 0.35)
	exchange := conversationExchange{
		ChannelID: "cli", SenderID: "owner", StartID: 1, EndID: 2,
		Turns: []state.ConversationTurn{
			{ID: 1, Role: "user", ContentType: state.ContentText, Content: strings.Repeat("word ", 2_000)},
			{ID: 2, Role: "assistant", ContentType: state.ContentText, Content: "answer"},
		},
	}
	parts, err := service.embeddingDocuments(context.Background(), exchange)
	if err != nil {
		t.Fatalf("embeddingDocuments: %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("part count = %d, want split input", len(parts))
	}
	for _, part := range parts {
		if !strings.HasPrefix(part, "title: Conversation on") {
			t.Fatalf("part lacks EmbeddingGemma document prefix: %q", part)
		}
		tokens, err := embedder.Tokenize(context.Background(), part)
		if err != nil {
			t.Fatalf("tokenize part: %v", err)
		}
		if len(tokens) > maxEmbeddingInputTokens {
			t.Fatalf("part has %d tokens, want at most %d", len(tokens), maxEmbeddingInputTokens)
		}
	}
}

func TestRenderExchangeDocumentIncludesStructuredToolOutcome(t *testing.T) {
	exchange := conversationExchange{
		ChannelID: "telegram", StartID: 1, EndID: 4,
		Turns: []state.ConversationTurn{
			{ID: 1, Role: "user", ContentType: state.ContentText, Content: "Remember espresso"},
			{ID: 2, Role: "assistant", ContentType: state.ContentToolCall, Content: `{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"store_memory","arguments":"{\"content\":\"espresso\"}"}}]}`},
			{ID: 3, Role: "tool", ContentType: state.ContentToolResult, Content: `{"tool_call_id":"call-1","name":"store_memory","content":"{\"stored\":true}","is_error":false}`},
			{ID: 4, Role: "assistant", ContentType: state.ContentText, Content: "Saved."},
		},
	}
	_, body, err := renderExchangeDocument(exchange)
	if err != nil {
		t.Fatalf("renderExchangeDocument: %v", err)
	}
	for _, want := range []string{"User: Remember espresso", "Assistant action store_memory", "Tool result store_memory", "Assistant: Saved."} {
		if !strings.Contains(body, want) {
			t.Fatalf("document missing %q: %s", want, body)
		}
	}
}

func TestScheduledReminderExchangeParticipatesInRecall(t *testing.T) {
	turns := []state.ConversationTurn{
		{
			ID: 1, ChannelID: "telegram", SenderID: "owner", Role: "user",
			ContentType: state.ContentScheduledReminder,
			Content:     `{"reminder_id":7,"message":"Bring phone charger to work","scheduled_for":"2026-08-08T08:00:00+08:00"}`,
		},
		{
			ID: 2, ChannelID: "telegram", SenderID: "owner", Role: "assistant",
			ContentType: state.ContentText,
			Content:     reminderHeader + "Your charger is the last item for work today.",
		},
	}
	exchanges := completeExchanges(turns)
	if len(exchanges) != 1 || exchanges[0].StartID != 1 || exchanges[0].EndID != 2 {
		t.Fatalf("scheduled exchanges = %#v", exchanges)
	}
	_, body, err := renderExchangeDocument(exchanges[0])
	if err != nil {
		t.Fatalf("renderExchangeDocument: %v", err)
	}
	for _, want := range []string{"Scheduled reminder: Scheduled reminder event", "Bring phone charger to work", "Assistant: " + reminderHeader} {
		if !strings.Contains(body, want) {
			t.Fatalf("scheduled document missing %q: %s", want, body)
		}
	}
	messages, err := reconstructHistory(turns)
	if err != nil || len(messages) != 2 || messages[0].Role != providers.RoleUser || messages[1].Role != providers.RoleAssistant {
		t.Fatalf("scheduled replay=%#v err=%v", messages, err)
	}
}
