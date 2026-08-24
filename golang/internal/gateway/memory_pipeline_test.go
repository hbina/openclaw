package gateway

import (
	"context"
	"errors"
	"fmt"
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
	query, keywords, fallback, err := agent.planRecall(context.Background(), traceID, "What coffee do I like?", nil)
	if err != nil || fallback || query != "owner coffee preference" || strings.Join(keywords, ",") != "coffee,espresso" {
		t.Fatalf("plan query=%q keywords=%#v fallback=%t err=%v", query, keywords, fallback, err)
	}
	if system := provider.requests[0].Messages[0].Content; !strings.Contains(system, "internal recall planner") || !strings.Contains(system, "exactly once") {
		t.Fatalf("planner prompt=%q", system)
	}
	rendered, _, _, err := agent.rerankRecallDetailed(context.Background(), traceID, "coffee", []state.MemorySearchResult{{Memory: state.Memory{ID: 7, Kind: state.MemoryProfile, Content: "Owner prefers espresso."}}}, nil)
	if err != nil || !strings.Contains(rendered, "Memory ID 7") || !strings.Contains(rendered, "espresso") {
		t.Fatalf("rendered=%q err=%v", rendered, err)
	}
}

func TestRecallPlannerContractFailuresUseRawQuery(t *testing.T) {
	tests := []struct {
		name     string
		response providers.GenerateResponse
		reason   state.RecallPlanContractReason
	}{
		{name: "ordinary text", response: providers.GenerateResponse{Message: providers.Message{Role: providers.RoleAssistant, Content: "I should answer the owner now."}}, reason: state.RecallPlanExpectedSingleCall},
		{name: "missing call", response: providers.GenerateResponse{Message: providers.Message{Role: providers.RoleAssistant}}, reason: state.RecallPlanExpectedSingleCall},
		{name: "extra calls", response: providers.GenerateResponse{Message: providers.Message{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{
			{ID: "plan-1", Type: "function", Function: providers.FunctionCall{Name: "plan_recall", Arguments: `{"semantic_query":"one","keywords":[]}`}},
			{ID: "plan-2", Type: "function", Function: providers.FunctionCall{Name: "plan_recall", Arguments: `{"semantic_query":"two","keywords":[]}`}},
		}}}, reason: state.RecallPlanExpectedSingleCall},
		{name: "prose with call", response: providers.GenerateResponse{Message: providers.Message{Role: providers.RoleAssistant, Content: "answer", ToolCalls: []providers.ToolCall{{ID: "plan-1", Type: "function", Function: providers.FunctionCall{Name: "plan_recall", Arguments: `{"semantic_query":"one","keywords":[]}`}}}}}, reason: state.RecallPlanExpectedSingleCall},
		{name: "wrong tool", response: internalCall("wrong-1", "search_memory", `{}`), reason: state.RecallPlanInvalidCall},
		{name: "wrong call type", response: providers.GenerateResponse{Message: providers.Message{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{{ID: "plan-1", Type: "custom", Function: providers.FunctionCall{Name: "plan_recall", Arguments: `{"semantic_query":"one","keywords":[]}`}}}}}, reason: state.RecallPlanInvalidCall},
		{name: "missing call id", response: providers.GenerateResponse{Message: providers.Message{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{{Type: "function", Function: providers.FunctionCall{Name: "plan_recall", Arguments: `{"semantic_query":"one","keywords":[]}`}}}}}, reason: state.RecallPlanInvalidCall},
		{name: "malformed arguments", response: internalCall("plan-1", "plan_recall", `{`), reason: state.RecallPlanMalformedArguments},
		{name: "missing keywords", response: internalCall("plan-1", "plan_recall", `{"semantic_query":"rewritten"}`), reason: state.RecallPlanMalformedArguments},
		{name: "null keywords", response: internalCall("plan-1", "plan_recall", `{"semantic_query":"rewritten","keywords":null}`), reason: state.RecallPlanMalformedArguments},
		{name: "unknown argument", response: internalCall("plan-1", "plan_recall", `{"semantic_query":"rewritten","keywords":[],"answer":"no"}`), reason: state.RecallPlanMalformedArguments},
		{name: "empty query", response: internalCall("plan-1", "plan_recall", `{"semantic_query":"  ","keywords":[]}`), reason: state.RecallPlanEmptyQuery},
		{name: "too many keywords", response: internalCall("plan-1", "plan_recall", `{"semantic_query":"rewritten","keywords":["1","2","3","4","5","6","7","8","9","10","11","12","13"]}`), reason: state.RecallPlanTooManyKeywords},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &scriptedProvider{responses: []providers.GenerateResponse{test.response}}
			agent, store := newTestAgent(t, provider, nil, "")
			traceID, err := store.StartResponseTrace(context.Background(), state.TraceInput{TriggerType: "chat", ChannelID: "cli", SenderID: "owner", InputJSON: `{}`})
			if err != nil {
				t.Fatal(err)
			}
			query, keywords, fallback, err := agent.planRecall(context.Background(), traceID, "  Owner's exact request  ", nil)
			if err != nil || !fallback || query != "Owner's exact request" || keywords != nil {
				t.Fatalf("query=%q keywords=%#v fallback=%t err=%v", query, keywords, fallback, err)
			}
			report, err := store.GetTraceReport(context.Background(), traceID)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Events) != 1 || report.Events[0]["status"] != "failed" || report.Events[0]["error"] != "recall planner contract: "+string(test.reason) {
				t.Fatalf("planner trace=%#v", report.Events)
			}
			if strings.Contains(report.Events[0]["error"].(string), "Owner's exact request") || strings.Contains(report.Events[0]["error"].(string), "answer the owner") {
				t.Fatalf("planner error leaked content: %#v", report.Events[0])
			}
		})
	}
}

func TestRecallPlannerServiceErrorsRemainFatal(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "provider", err: errors.New("provider unavailable")},
		{name: "cancellation", err: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent, store := newTestAgent(t, &failingProvider{err: test.err}, nil, "")
			traceID, err := store.StartResponseTrace(context.Background(), state.TraceInput{TriggerType: "chat", ChannelID: "cli", SenderID: "owner", InputJSON: `{}`})
			if err != nil {
				t.Fatal(err)
			}
			_, _, fallback, err := agent.planRecall(context.Background(), traceID, "hello", nil)
			if err == nil || fallback || !errors.Is(err, test.err) {
				t.Fatalf("fallback=%t err=%v", fallback, err)
			}
		})
	}
}

func TestChatPlannerFallbackSkipsEmptySelectorAndCompletesTrace(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "Planner prose must stay internal."}},
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "Hello from the assistant."}},
	}}
	agent, store := newTestAgent(t, provider, nil, "")
	agent.modelMemory = true
	agent.rag = NewRAGService(store, &fakeEmbedder{}, &fakePromptSizer{contextSize: 10_000}, "embeddinggemma-test", 3, 0.35)

	reply, err := chat(agent, context.Background(), "cli", "fallback-owner", "  Hello  ")
	if err != nil || reply != "Hello from the assistant." {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests=%d, want planner and assistant", len(provider.requests))
	}
	for _, request := range provider.requests {
		for _, message := range request.Messages {
			if request.ToolChoice != "required" && strings.Contains(message.Content, "Planner prose must stay internal") {
				t.Fatalf("planner prose leaked into owner-facing generation: %#v", request.Messages)
			}
		}
	}
	history, err := store.GetConversationHistory(context.Background(), "cli", "fallback-owner")
	if err != nil || len(history) != 2 || history[1].Content != reply {
		t.Fatalf("history=%#v err=%v", history, err)
	}
	indexed, err := store.LoadConversationEmbeddings(context.Background(), "embeddinggemma-test", ragIndexVersion, 3)
	if err != nil || len(indexed) != 1 {
		t.Fatalf("indexed conversation=%#v err=%v", indexed, err)
	}
	traces, err := store.ListResponseTraces(context.Background(), state.TraceFilter{})
	if err != nil || len(traces) != 1 || traces[0].Status != "completed" {
		t.Fatalf("traces=%#v err=%v", traces, err)
	}
	report, err := store.GetTraceReport(context.Background(), traces[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := eventKindsAndStatuses(report); got != "llm:failed,rag:succeeded,llm:succeeded,output:succeeded,delivery:succeeded" {
		t.Fatalf("trace events=%s; report=%#v", got, report.Events)
	}
	rag := report.Events[1]["detail"].(map[string]any)
	if rag["embedding_query"] != "task: search result | query: Hello" || rag["selected_count"].(float64) != 0 {
		t.Fatalf("fallback RAG detail=%#v", rag)
	}
}

func TestChatDoesNotRunPostResponseCurator(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{
		internalCall("plan-1", "plan_recall", `{"semantic_query":"simple greeting","keywords":[]}`),
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "Hello."}},
	}}
	agent, store := newTestAgent(t, provider, nil, "")
	agent.modelMemory = true
	agent.rag = NewRAGService(store, &fakeEmbedder{}, &fakePromptSizer{contextSize: 10_000}, "embeddinggemma-test", 3, 0.35)

	reply, err := chat(agent, context.Background(), "cli", "owner", "Hello")
	if err != nil || reply != "Hello." {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests=%d, want recall planner and main assistant only", len(provider.requests))
	}
	if provider.requests[0].ToolChoice != "required" || provider.requests[1].ToolChoice != "auto" {
		t.Fatalf("provider requests=%#v", provider.requests)
	}
	traces, err := store.ListResponseTraces(context.Background(), state.TraceFilter{})
	if err != nil || len(traces) != 1 || traces[0].Status != "completed" {
		t.Fatalf("traces=%#v err=%v", traces, err)
	}
	report, err := store.GetTraceReport(context.Background(), traces[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := eventKindsAndStatuses(report); got != "llm:succeeded,tool:succeeded,rag:succeeded,llm:succeeded,output:succeeded,delivery:succeeded" {
		t.Fatalf("trace events=%s; report=%#v", got, report.Events)
	}
}

func TestReminderPlannerFallbackUsesRawQuery(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "I cannot plan this."}},
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "Bring the charger."}},
	}}
	channel := &recordingChannel{}
	agent, store := newTestAgent(t, provider, channel, "")
	agent.modelMemory = true
	agent.rag = NewRAGService(store, &fakeEmbedder{}, &fakePromptSizer{contextSize: 10_000}, "embeddinggemma-test", 3, 0.35)
	reminder := addDueReminderForTest(t, store, channel.ID(), "owner", "  Bring phone charger  ")

	if err := agent.DeliverReminder(context.Background(), reminder); err != nil {
		t.Fatal(err)
	}
	if channel.content != reminderHeader+"Bring the charger." || len(provider.requests) != 2 {
		t.Fatalf("content=%q requests=%d", channel.content, len(provider.requests))
	}
	traces, err := store.ListResponseTraces(context.Background(), state.TraceFilter{})
	if err != nil || len(traces) != 1 || traces[0].Status != "completed" {
		t.Fatalf("traces=%#v err=%v", traces, err)
	}
	report, err := store.GetTraceReport(context.Background(), traces[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Events[0]["status"] != "failed" || report.Events[1]["detail"].(map[string]any)["embedding_query"] != "task: search result | query: Bring phone charger" {
		t.Fatalf("fallback reminder trace=%#v", report.Events)
	}
}

func TestReminderFallbackStillRequiresSelectorForCandidates(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.GenerateResponse{
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "planner prose"}},
		{Message: providers.Message{Role: providers.RoleAssistant, Content: "selector prose"}},
	}}
	channel := &recordingChannel{}
	agent, store := newTestAgent(t, provider, channel, "")
	agent.modelMemory = true
	seedExchange(t, store, "telegram", "other-route", "Plan transportation in Japan.", "Use Kyoto Station.")
	agent.rag = NewRAGService(store, &fakeEmbedder{}, &fakePromptSizer{contextSize: 10_000}, "embeddinggemma-test", 3, 0.35)
	if err := agent.rag.IndexOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	reminder := addDueReminderForTest(t, store, channel.ID(), "owner", "Review transportation in Japan")

	err := agent.DeliverReminder(context.Background(), reminder)
	if err == nil || !strings.Contains(err.Error(), "rerank recall") {
		t.Fatalf("DeliverReminder error=%v", err)
	}
	if channel.content != "" || len(provider.requests) != 2 || provider.requests[1].Tools[0].Function.Name != "select_recall_evidence" {
		t.Fatalf("content=%q requests=%#v", channel.content, provider.requests)
	}
	due, dueErr := store.FetchDueReminders()
	if dueErr != nil || len(due) != 1 || due[0].ID != reminder.ID {
		t.Fatalf("due=%#v err=%v", due, dueErr)
	}
}

func eventKindsAndStatuses(report state.TraceReport) string {
	parts := make([]string, 0, len(report.Events))
	for _, event := range report.Events {
		parts = append(parts, fmt.Sprintf("%s:%s", event["kind"], event["status"]))
	}
	return strings.Join(parts, ",")
}
