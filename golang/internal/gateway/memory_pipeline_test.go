package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/openclaw/openclaw/go/internal/providers"
	"github.com/openclaw/openclaw/go/internal/state"
)

func internalCall(id, name, arguments string) providers.GenerateResponse {
	return providers.GenerateResponse{Message: providers.Message{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{{ID: id, Type: "function", Function: providers.FunctionCall{Name: name, Arguments: arguments}}}}, FinishReason: "tool_calls"}
}

func TestModelRecallPlannerAndRerankerUseStructuredCalls(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{
		internalCall("plan-1", "plan_recall", `{"semantic_query":"owner coffee preference","keywords":["coffee","espresso"]}`),
		internalCall("rank-1", "select_recall_evidence", `{"memory_ids":[7],"conversation_ids":[]}`),
	}}
	agent, store := newTestAgent(t, provider, nil, "")
	traceID, err := store.StartResponseTrace(context.Background(), state.TraceInput{TriggerType: "chat", ChannelID: "cli", SenderID: "owner", InputJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	query, keywords, err := agent.planRecall(context.Background(), traceID, "What coffee do I like?", nil)
	if err != nil || query != "owner coffee preference" || len(keywords) != 2 {
		t.Fatalf("plan query=%q keywords=%#v err=%v", query, keywords, err)
	}
	rendered, _, _, err := agent.rerankRecallDetailed(context.Background(), traceID, "coffee", []state.MemorySearchResult{{Memory: state.Memory{ID: 7, Kind: state.MemoryProfile, Content: "Owner prefers espresso."}}}, nil)
	if err != nil || !strings.Contains(rendered, "Memory ID 7") || !strings.Contains(rendered, "espresso") {
		t.Fatalf("rendered=%q err=%v", rendered, err)
	}
}

func TestMemoryCuratorStoresProactiveMemoryAndStops(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{
		internalCall("curate-1", "store_memory", `{"kind":"profile","content":"Owner prefers espresso."}`),
		internalCall("curate-2", "finish_memory_curation", `{}`),
	}}
	agent, store := newTestAgent(t, provider, nil, "")
	traceID, err := store.StartResponseTrace(context.Background(), state.TraceInput{TriggerType: "chat", ChannelID: "cli", SenderID: "owner", InputJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	mutated, err := agent.curateMemories(context.Background(), traceID, 0, "cli", "owner", "I prefer espresso", "Understood.")
	if err != nil || !mutated {
		t.Fatalf("mutated=%t err=%v", mutated, err)
	}
	items, err := store.ListMemories(context.Background(), state.MemoryFilter{Status: state.MemoryActive, Limit: 20})
	if err != nil || len(items) != 1 || items[0].Kind != state.MemoryProfile || items[0].Content != "Owner prefers espresso." {
		t.Fatalf("memories=%#v err=%v", items, err)
	}
	allHistory, err := store.GetAllConversationHistory(context.Background())
	if err != nil || len(allHistory) != 0 {
		t.Fatalf("internal curator transcript leaked into visible history: %#v err=%v", allHistory, err)
	}
}
