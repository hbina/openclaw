package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/openclaw/go/internal/providers"
	"github.com/openclaw/openclaw/go/internal/state"
)

const (
	testEmbeddingModel      = "test-embedding-model"
	testEmbeddingDimensions = 2
	testMinScore            = 0.5
)

// fakeEmbedder maps "espresso"/"coffee"-flavored text to one unit vector and
// everything else to an orthogonal vector, so tests can prove search_memory
// matches by vector similarity rather than literal substring overlap.
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	vectors := make([][]float32, len(inputs))
	for index, input := range inputs {
		lower := strings.ToLower(input)
		if strings.Contains(lower, "espresso") || strings.Contains(lower, "coffee") {
			vectors[index] = []float32{1, 0}
		} else {
			vectors[index] = []float32{0, 1}
		}
	}
	return vectors, nil
}

func (fakeEmbedder) Tokenize(_ context.Context, content string) ([]int, error) {
	return make([]int, len(strings.Fields(content))), nil
}

func (fakeEmbedder) Detokenize(_ context.Context, _ []int) (string, error) {
	return "detokenized", nil
}

func newTestExecutor(t *testing.T) (*Executor, *state.Store, time.Time) {
	t.Helper()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "tools.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	return NewExecutor(store, func() time.Time { return now }, time.UTC, fakeEmbedder{}, testEmbeddingModel, testEmbeddingDimensions, testMinScore), store, now
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

func call(id, name, arguments string) providers.ToolCall {
	return providers.ToolCall{ID: id, Type: "function", Function: providers.FunctionCall{Name: name, Arguments: arguments}}
}

func TestReminderToolsUseTrustedIdentityAndScopedDelete(t *testing.T) {
	executor, store, now := newTestExecutor(t)
	ctx := context.Background()
	owner := Context{ChannelID: "telegram", SenderID: "owner"}

	added, err := executor.ExecuteAndRecord(ctx, owner, call("add-1", "add_reminder", `{"message":"make coffee","schedule":{"kind":"at","at":"2026-07-14T12:15:00Z"}}`))
	if err != nil || added.IsError {
		t.Fatalf("add reminder: result=%#v err=%v", added, err)
	}
	var addContent struct {
		Added struct {
			ID     int       `json:"id"`
			FireAt time.Time `json:"next_fire_at"`
		} `json:"added"`
	}
	if err := json.Unmarshal([]byte(added.Content), &addContent); err != nil {
		t.Fatalf("decode add result: %v", err)
	}
	if addContent.Added.ID != 1 || !addContent.Added.FireAt.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("added = %#v", addContent.Added)
	}

	attacker := Context{ChannelID: "telegram", SenderID: "other"}
	denied, err := executor.ExecuteAndRecord(ctx, attacker, call("delete-other", "remove_reminder", `{"id":1}`))
	if err != nil || !denied.IsError {
		t.Fatalf("cross-user delete: result=%#v err=%v", denied, err)
	}
	reminders, err := listRemindersForTest(t, store, owner.ChannelID, owner.SenderID)
	if err != nil || len(reminders) != 1 {
		t.Fatalf("owner reminders after denied delete: %#v err=%v", reminders, err)
	}

	deleted, err := executor.ExecuteAndRecord(ctx, owner, call("delete-1", "remove_reminder", `{"id":1}`))
	if err != nil || deleted.IsError {
		t.Fatalf("delete reminder: result=%#v err=%v", deleted, err)
	}
	reminders, err = listRemindersForTest(t, store, owner.ChannelID, owner.SenderID)
	if err != nil || len(reminders) != 0 {
		t.Fatalf("owner reminders after delete: %#v err=%v", reminders, err)
	}
}

func TestReminderAbsoluteTimeValidation(t *testing.T) {
	executor, _, _ := newTestExecutor(t)
	ctx := context.Background()
	toolCtx := Context{ChannelID: "cli", SenderID: "user"}

	for _, test := range []struct {
		name string
		args string
	}{
		{"missing offset", `{"message":"call","schedule":{"kind":"at","at":"2026-07-15T12:00:00"}}`},
		{"past", `{"message":"call","schedule":{"kind":"at","at":"2026-07-13T12:00:00Z"}}`},
		{"mixed schedule", `{"message":"call","schedule":{"kind":"at","at":"2026-07-15T12:00:00Z","expr":"0 1 * * *"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := executor.ExecuteAndRecord(ctx, toolCtx, call(test.name, "add_reminder", test.args))
			if err != nil || !result.IsError {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
	result, err := executor.ExecuteAndRecord(ctx, toolCtx, call("unknown identity field", "list_reminders", `{"sender_id":"other"}`))
	if err != nil || !result.IsError || !stringsContain(result.Content, "unknown field") {
		t.Fatalf("identity field: result=%#v err=%v", result, err)
	}
}

func TestReminderSingleItemAddUpdateListAndRemove(t *testing.T) {
	executor, store, _ := newTestExecutor(t)
	ctx := context.Background()
	toolCtx := Context{ChannelID: "telegram", SenderID: "owner"}
	for _, step := range []struct{ id, args string }{
		{"cron", `{"message":"weekday check","schedule":{"kind":"cron","expr":"3 12 * * 1-5","timezone":"Asia/Kuala_Lumpur"}}`},
		{"interval", `{"message":"stretch","schedule":{"kind":"every","every_ms":3600000,"anchor_at":"2026-07-14T13:00:00Z"}}`},
	} {
		result, err := executor.ExecuteAndRecord(ctx, toolCtx, call(step.id, "add_reminder", step.args))
		if err != nil || result.IsError {
			t.Fatalf("%s add: %#v err=%v", step.id, result, err)
		}
	}

	updated, err := executor.ExecuteAndRecord(ctx, toolCtx, call("update", "update_reminder", `{"id":1,"message":"weekday lunch check","enabled":false}`))
	if err != nil || updated.IsError || !stringsContain(updated.Content, "weekday lunch check") {
		t.Fatalf("update: %#v err=%v", updated, err)
	}
	listed, err := executor.ExecuteAndRecord(ctx, toolCtx, call("list", "list_reminders", `{}`))
	if err != nil || listed.IsError || !stringsContain(listed.Content, `"kind":"cron"`) || !stringsContain(listed.Content, `"enabled":false`) {
		t.Fatalf("list: %#v err=%v", listed, err)
	}
	for id := 1; id <= 2; id++ {
		removed, err := executor.ExecuteAndRecord(ctx, toolCtx, call(fmt.Sprintf("remove-%d", id), "remove_reminder", fmt.Sprintf(`{"id":%d}`, id)))
		if err != nil || removed.IsError {
			t.Fatalf("remove %d: %#v err=%v", id, removed, err)
		}
	}
	remaining, err := listRemindersForTest(t, store, toolCtx.ChannelID, toolCtx.SenderID)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("remaining reminders: %#v err=%v", remaining, err)
	}
}

func TestReminderCronDefaultsToServerTimezone(t *testing.T) {
	store, err := state.NewStore(filepath.Join(t.TempDir(), "timezone.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	location, err := time.LoadLocation("Asia/Singapore")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	executor := NewExecutor(store, func() time.Time { return now }, location, fakeEmbedder{}, testEmbeddingModel, testEmbeddingDimensions, testMinScore)
	toolCtx := Context{ChannelID: "telegram", SenderID: "owner"}

	result, err := executor.ExecuteAndRecord(context.Background(), toolCtx, call("local-cron", "add_reminder", `{"message":"morning","schedule":{"kind":"cron","expr":"0 8 * * *"}}`))
	if err != nil || result.IsError {
		t.Fatalf("add reminder: result=%#v err=%v", result, err)
	}
	if !stringsContain(result.Content, `"timezone":"Asia/Singapore"`) ||
		!stringsContain(result.Content, `"next_fire_at":"2026-07-15T08:00:00+08:00"`) ||
		!stringsContain(result.Content, `"display_timezone":"Asia/Singapore"`) {
		t.Fatalf("result does not use server timezone: %s", result.Content)
	}

	reminders, err := listRemindersForTest(t, store, toolCtx.ChannelID, toolCtx.SenderID)
	if err != nil || len(reminders) != 1 || reminders[0].Schedule.Timezone != "Asia/Singapore" {
		t.Fatalf("persisted reminder: %#v err=%v", reminders, err)
	}
}

func TestReminderCallsCommitIndependently(t *testing.T) {
	executor, store, _ := newTestExecutor(t)
	ctx := context.Background()
	toolCtx := Context{ChannelID: "cli", SenderID: "owner"}
	for _, step := range []struct {
		id, args  string
		wantError bool
	}{
		{"first", `{"message":"first","schedule":{"kind":"at","at":"2026-07-15T12:00:00Z"}}`, false},
		{"invalid", `{"message":"invalid","schedule":{"kind":"cron","expr":"not cron","timezone":"UTC"}}`, true},
		{"third", `{"message":"third","schedule":{"kind":"at","at":"2026-07-16T12:00:00Z"}}`, false},
	} {
		result, err := executor.ExecuteAndRecord(ctx, toolCtx, call(step.id, "add_reminder", step.args))
		if err != nil || result.IsError != step.wantError {
			t.Fatalf("%s: %#v err=%v", step.id, result, err)
		}
	}
	reminders, err := listRemindersForTest(t, store, toolCtx.ChannelID, toolCtx.SenderID)
	if err != nil || len(reminders) != 2 || reminders[0].Message != "first" || reminders[1].Message != "third" {
		t.Fatalf("independent results: %#v err=%v", reminders, err)
	}
}

func TestGlobalMemoryToolsAreIdempotentAndSearchable(t *testing.T) {
	executor, _, _ := newTestExecutor(t)
	ctx := context.Background()
	firstUser := Context{ChannelID: "telegram", SenderID: "first"}
	secondUser := Context{ChannelID: "cli", SenderID: "second"}

	first, err := executor.ExecuteAndRecord(ctx, firstUser, call("store-1", "store_memory", `{"content":"The user prefers espresso."}`))
	if err != nil || first.IsError {
		t.Fatalf("first store: %#v err=%v", first, err)
	}
	duplicate, err := executor.ExecuteAndRecord(ctx, secondUser, call("store-2", "store_memory", `{"content":"The user prefers espresso."}`))
	if err != nil || duplicate.IsError || !stringsContain(duplicate.Content, `"stored":false`) {
		t.Fatalf("duplicate store: %#v err=%v", duplicate, err)
	}
	distractor, err := executor.ExecuteAndRecord(ctx, firstUser, call("store-3", "store_memory", `{"content":"The user's favorite color is blue."}`))
	if err != nil || distractor.IsError {
		t.Fatalf("distractor store: %#v err=%v", distractor, err)
	}

	// "coffee preference" shares no substring with the stored fact, so a
	// match here only comes from vector similarity, not keyword overlap.
	search, err := executor.ExecuteAndRecord(ctx, secondUser, call("search-1", "search_memory", `{"query":"coffee preference"}`))
	if err != nil || search.IsError || !stringsContain(search.Content, "The user prefers espresso.") {
		t.Fatalf("semantic search: %#v err=%v", search, err)
	}
	if stringsContain(search.Content, "favorite color") {
		t.Fatalf("semantic search matched an unrelated memory: %#v", search)
	}
}

func TestTaskToolsAreGlobalAndPreserveLifecycleTimestamps(t *testing.T) {
	executor, store, now := newTestExecutor(t)
	ctx := context.Background()
	firstRoute := Context{ChannelID: "telegram", SenderID: "owner"}
	secondRoute := Context{ChannelID: "cli", SenderID: "different-route"}

	added, err := executor.ExecuteAndRecord(ctx, firstRoute, call("task-add", "add_task", `{"description":"  Prepare report  "}`))
	if err != nil || added.IsError ||
		!stringsContain(added.Content, `"started_at":"2026-07-14T12:00:00Z"`) ||
		!stringsContain(added.Content, `"completed_at":null`) {
		t.Fatalf("add task: result=%#v err=%v", added, err)
	}
	added, err = executor.ExecuteAndRecord(ctx, firstRoute, call("task-add-2", "add_task", `{"description":"Book dentist"}`))
	if err != nil || added.IsError {
		t.Fatalf("add second task: result=%#v err=%v", added, err)
	}

	listed, err := executor.ExecuteAndRecord(ctx, secondRoute, call("task-list", "list_tasks", `{}`))
	if err != nil || listed.IsError || !stringsContain(listed.Content, "Prepare report") || !stringsContain(listed.Content, `"status":"open"`) {
		t.Fatalf("global task list: result=%#v err=%v", listed, err)
	}

	updated, err := executor.ExecuteAndRecord(ctx, secondRoute, call("task-update", "update_task", `{"id":1,"description":"Prepare final report"}`))
	if err != nil || updated.IsError || !stringsContain(updated.Content, "Prepare final report") {
		t.Fatalf("update task: result=%#v err=%v", updated, err)
	}

	completed, err := executor.ExecuteAndRecord(ctx, firstRoute, call("task-complete", "complete_task", `{"id":1}`))
	if err != nil || completed.IsError || !stringsContain(completed.Content, `"completed_at":"2026-07-14T12:00:00Z"`) ||
		!stringsContain(completed.Content, `"status":"completed"`) {
		t.Fatalf("complete task: result=%#v err=%v", completed, err)
	}

	open, err := executor.ExecuteAndRecord(ctx, firstRoute, call("task-open", "list_tasks", `{}`))
	if err != nil || open.IsError || stringsContain(open.Content, "Prepare final report") || !stringsContain(open.Content, "Book dentist") {
		t.Fatalf("default open list: result=%#v err=%v", open, err)
	}
	history, err := executor.ExecuteAndRecord(ctx, firstRoute, call("task-history", "list_tasks", `{"status":"completed"}`))
	if err != nil || history.IsError || !stringsContain(history.Content, "Prepare final report") || stringsContain(history.Content, "Book dentist") {
		t.Fatalf("completed list: result=%#v err=%v", history, err)
	}
	all, err := executor.ExecuteAndRecord(ctx, firstRoute, call("task-all", "list_tasks", `{"status":"all"}`))
	if err != nil || all.IsError || !stringsContain(all.Content, "Prepare final report") || !stringsContain(all.Content, "Book dentist") {
		t.Fatalf("all list: result=%#v err=%v", all, err)
	}

	for id := 1; id <= 2; id++ {
		removed, err := executor.ExecuteAndRecord(ctx, secondRoute, call(fmt.Sprintf("task-remove-%d", id), "remove_task", fmt.Sprintf(`{"id":%d}`, id)))
		if err != nil || removed.IsError {
			t.Fatalf("remove task %d: result=%#v err=%v", id, removed, err)
		}
	}
	tasks, err := listTasksForTest(t, store, state.TaskAll)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("remaining tasks = %#v, err=%v", tasks, err)
	}

	toolHistory, err := store.GetConversationHistory(ctx, firstRoute.ChannelID, firstRoute.SenderID)
	if err != nil || len(toolHistory) != 6 {
		t.Fatalf("first-route tool history = %#v, err=%v", toolHistory, err)
	}
	for _, turn := range toolHistory {
		if turn.ContentType != state.ContentToolResult {
			t.Fatalf("non-tool-result transcript = %#v", turn)
		}
	}
	_ = now
}

func TestTaskToolRejectsReminderAndScheduleFields(t *testing.T) {
	executor, _, _ := newTestExecutor(t)
	ctx := context.Background()
	toolCtx := Context{ChannelID: "cli", SenderID: "owner"}
	for name, args := range map[string]string{
		"due date":      `{"description":"report","due_at":"2026-07-15T12:00:00Z"}`,
		"schedule":      `{"description":"report","schedule":{"kind":"at"}}`,
		"timezone":      `{"description":"report","timezone":"UTC"}`,
		"reminder link": `{"description":"report","reminder_id":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			result, err := executor.ExecuteAndRecord(ctx, toolCtx, call(name, "add_task", args))
			if err != nil || !result.IsError || !stringsContain(result.Content, "unknown field") {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
	result, err := executor.ExecuteAndRecord(ctx, toolCtx, call("routing", "list_tasks", `{"sender_id":"other"}`))
	if err != nil || !result.IsError || !stringsContain(result.Content, "unknown field") {
		t.Fatalf("routing result=%#v err=%v", result, err)
	}
}

func TestTaskCallsCommitIndependently(t *testing.T) {
	executor, store, _ := newTestExecutor(t)
	ctx := context.Background()
	toolCtx := Context{ChannelID: "cli", SenderID: "owner"}

	for _, step := range []struct {
		id, description string
		wantError       bool
	}{
		{"first", "First", false},
		{"duplicate", " first ", true},
		{"third", "Third", false},
	} {
		result, err := executor.ExecuteAndRecord(ctx, toolCtx, call(step.id, "add_task", fmt.Sprintf(`{"description":%q}`, step.description)))
		if err != nil || result.IsError != step.wantError {
			t.Fatalf("%s: result=%#v err=%v", step.id, result, err)
		}
	}
	open, err := listTasksForTest(t, store, state.TaskOpen)
	if err != nil || len(open) != 2 || open[0].Description != "First" || open[1].Description != "Third" {
		t.Fatalf("independent task results: %#v, err=%v", open, err)
	}
}

func TestCompletedTaskCannotChangeOrCompleteAgain(t *testing.T) {
	executor, _, _ := newTestExecutor(t)
	ctx := context.Background()
	toolCtx := Context{ChannelID: "cli", SenderID: "owner"}
	for _, step := range []struct{ id, name, args string }{
		{"add", "add_task", `{"description":"final task"}`},
		{"complete", "complete_task", `{"id":1}`},
	} {
		result, err := executor.ExecuteAndRecord(ctx, toolCtx, call(step.id, step.name, step.args))
		if err != nil || result.IsError {
			t.Fatalf("%s: result=%#v err=%v", step.id, result, err)
		}
	}
	for name, step := range map[string]struct{ tool, args string }{
		"update":   {"update_task", `{"id":1,"description":"changed"}`},
		"complete": {"complete_task", `{"id":1}`},
	} {
		result, err := executor.ExecuteAndRecord(ctx, toolCtx, call("final-"+name, step.tool, step.args))
		if err != nil || !result.IsError || !stringsContain(result.Content, "completed") {
			t.Fatalf("%s final task: result=%#v err=%v", name, result, err)
		}
	}
}

func TestToolCatalogUsesSinglePurposeTools(t *testing.T) {
	definitions := Definitions(time.UTC)
	wantNames := []string{
		"add_reminder", "list_reminders", "update_reminder", "remove_reminder",
		"add_task", "list_tasks", "update_task", "complete_task", "remove_task",
		"store_memory", "search_memory",
	}
	for _, definition := range definitions {
		if definition.Function.Name == "manage_personality" {
			t.Fatal("manage_personality remains in the model tool catalog")
		}
	}
	if len(definitions) != len(wantNames) {
		t.Fatalf("tool definition count = %d, want %d", len(definitions), len(wantNames))
	}
	for index, want := range wantNames {
		if got := definitions[index].Function.Name; got != want {
			t.Fatalf("tool definition %d = %q, want %q", index, got, want)
		}
		if index < 9 {
			schema := string(definitions[index].Function.Parameters)
			if strings.Contains(schema, `"type":"array"`) || strings.Contains(schema, `"action"`) || strings.Contains(schema, `"patch"`) {
				t.Fatalf("tool %q retains a batch or multifunction field: %s", want, schema)
			}
		}
	}

	executor, _, _ := newTestExecutor(t)
	ctx := context.Background()
	toolCtx := Context{ChannelID: "cli", SenderID: "owner"}
	result, err := executor.ExecuteAndRecord(ctx, toolCtx, call("personality", "manage_personality", `{"action":"view"}`))
	if err != nil || !result.IsError || !stringsContain(result.Content, `unknown tool \"manage_personality\"`) {
		t.Fatalf("removed personality tool result: %#v err=%v", result, err)
	}
	for _, legacy := range []string{"manage_tasks", "manage_reminders"} {
		result, err := executor.ExecuteAndRecord(ctx, toolCtx, call("legacy-"+legacy, legacy, `{}`))
		if err != nil || !result.IsError || !stringsContain(result.Content, `unknown tool \"`+legacy+`\"`) {
			t.Fatalf("legacy tool %q result: %#v err=%v", legacy, result, err)
		}
	}
}

func stringsContain(value, fragment string) bool {
	for i := 0; i+len(fragment) <= len(value); i++ {
		if value[i:i+len(fragment)] == fragment {
			return true
		}
	}
	return false
}

func TestToolResultsPersistAsStructuredHistory(t *testing.T) {
	executor, store, _ := newTestExecutor(t)
	ctx := context.Background()
	toolCtx := Context{ChannelID: "cli", SenderID: "user"}
	result, err := executor.ExecuteAndRecord(ctx, toolCtx, call("list-1", "list_reminders", `{}`))
	if err != nil || result.IsError {
		t.Fatalf("list reminders: %#v err=%v", result, err)
	}
	history, err := store.GetConversationHistory(ctx, toolCtx.ChannelID, toolCtx.SenderID)
	if err != nil || len(history) != 1 {
		t.Fatalf("history=%#v err=%v", history, err)
	}
	if history[0].Role != "tool" || history[0].ContentType != state.ContentToolResult {
		t.Fatalf("unexpected history row: %#v", history[0])
	}
}
