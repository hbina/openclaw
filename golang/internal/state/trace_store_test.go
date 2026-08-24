package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestResponseTracePersistsCompleteDiagnosticChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.sqlite")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	traceID, err := store.StartResponseTrace(ctx, TraceInput{TriggerType: "chat", ChannelID: "telegram", SenderID: "owner", ExternalMessageID: "in-7", InputJSON: `{"content":"why?"}`})
	if err != nil {
		t.Fatal(err)
	}
	historyID, err := store.SaveConversationMessageID(ctx, "telegram", "owner", "user", ContentInboundMessage, `{"content":"why?"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LinkTraceInbound(ctx, traceID, historyID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRAGTrace(ctx, traceID, RAGTrace{Outcome: "selected", EmbeddingQuery: "query", EmbeddingModel: "embed-v1", Dimensions: 3, IndexVersion: 1, MinimumScore: .35, HistoryHighwaterID: int(historyID), CandidateCount: 2, QualifiedCount: 1, RenderedArchive: "archive", Matches: []RAGTraceMatch{{Rank: 1, StartHistoryID: int(historyID), EndHistoryID: int(historyID), SimilarityScore: .8, ContentHash: "hash", MessagesJSON: `[{"role":"user","content":"why?"}]`}}}, nil); err != nil {
		t.Fatal(err)
	}
	llmID, err := store.StartLLMCall(ctx, traceID, 1, "chat", `{"messages":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishLLMCall(ctx, llmID, `{"choices":[]}`, 200, "stop", nil); err != nil {
		t.Fatal(err)
	}
	outputID, err := store.RecordResponseOutput(ctx, traceID, "llm", &llmID, "raw", `[]`, "final")
	if err != nil {
		t.Fatal(err)
	}
	deliveryID, err := store.PrepareDelivery(ctx, traceID, outputID, "telegram", "owner", "final")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeliveryAttempting(ctx, deliveryID); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteDelivery(ctx, traceID, deliveryID, "out-9", "telegram", "owner", "final"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenReadOnlyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	items, err := readOnly.ListResponseTraces(ctx, TraceFilter{ExternalMessageID: "out-9"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "completed" || items[0].FinalContent != "final" {
		t.Fatalf("summaries=%#v", items)
	}
	report, err := readOnly.GetTraceReport(ctx, traceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Events) != 4 {
		t.Fatalf("events=%#v", report.Events)
	}
	if _, err := readOnly.StartResponseTrace(ctx, TraceInput{TriggerType: "chat", ChannelID: "x", SenderID: "x", InputJSON: `{}`}); err == nil {
		t.Fatal("read-only trace store accepted a write")
	}
}

func TestFailedLLMTraceRetainsMalformedProviderBody(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	traceID, err := store.StartResponseTrace(ctx, TraceInput{TriggerType: "chat", ChannelID: "cli", SenderID: "owner", InputJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	callID, err := store.StartLLMCall(ctx, traceID, 1, "chat", `{"messages":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishLLMCall(ctx, callID, "{", 200, "", errors.New("decode response")); err != nil {
		t.Fatal(err)
	}
	report, err := store.GetTraceReport(ctx, traceID)
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Events[0]["status"]; got != "failed" {
		t.Fatalf("status=%v", got)
	}
}

func TestFailRecallPlanCallRetainsRequestAndResponse(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	traceID, err := store.StartResponseTrace(ctx, TraceInput{TriggerType: "chat", ChannelID: "cli", SenderID: "owner", InputJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	callID, err := store.StartLLMCall(ctx, traceID, 1, "recall_plan", `{"request":"exact"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishLLMCall(ctx, callID, `{"response":"exact"}`, 200, "stop", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.FailRecallPlanCall(ctx, callID, RecallPlanExpectedSingleCall); err != nil {
		t.Fatal(err)
	}
	report, err := store.GetTraceReport(ctx, traceID)
	if err != nil {
		t.Fatal(err)
	}
	event := report.Events[0]
	detail := event["detail"].(map[string]any)
	if event["status"] != "failed" || event["error"] != "recall planner contract: expected exactly one structured plan_recall call" ||
		detail["request_json"] != `{"request":"exact"}` || detail["response_json"] != `{"response":"exact"}` {
		t.Fatalf("event=%#v", event)
	}
	if err := store.FailRecallPlanCall(ctx, callID, RecallPlanContractReason("owner content")); err == nil {
		t.Fatal("unsupported contract reason was accepted")
	}
}
