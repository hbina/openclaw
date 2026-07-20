package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw/openclaw/go/internal/providers"
	"github.com/openclaw/openclaw/go/internal/state"
)

func newTestExecutor(t *testing.T) (*Executor, *state.Store, time.Time) {
	t.Helper()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "tools.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	return NewExecutor(store, func() time.Time { return now }, time.UTC), store, now
}

func call(id, name, arguments string) providers.ToolCall {
	return providers.ToolCall{ID: id, Type: "function", Function: providers.FunctionCall{Name: name, Arguments: arguments}}
}

func TestReminderToolsUseTrustedIdentityAndScopedDelete(t *testing.T) {
	executor, store, now := newTestExecutor(t)
	ctx := context.Background()
	owner := Context{ChannelID: "telegram", SenderID: "owner"}

	added, err := executor.ExecuteAndRecord(ctx, owner, call("add-1", "manage_reminders", `{"action":"add","items":[{"message":"make coffee","schedule":{"kind":"at","at":"2026-07-14T12:15:00Z"}}]}`))
	if err != nil || added.IsError {
		t.Fatalf("add reminder: result=%#v err=%v", added, err)
	}
	var addContent struct {
		Added []struct {
			ID     int       `json:"id"`
			FireAt time.Time `json:"next_fire_at"`
		} `json:"added"`
	}
	if err := json.Unmarshal([]byte(added.Content), &addContent); err != nil {
		t.Fatalf("decode add result: %v", err)
	}
	if len(addContent.Added) != 1 || !addContent.Added[0].FireAt.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("added = %#v", addContent.Added)
	}

	attacker := Context{ChannelID: "telegram", SenderID: "other"}
	denied, err := executor.ExecuteAndRecord(ctx, attacker, call("delete-other", "manage_reminders", `{"action":"remove","ids":[1]}`))
	if err != nil || !denied.IsError {
		t.Fatalf("cross-user delete: result=%#v err=%v", denied, err)
	}
	reminders, err := store.ListReminders(owner.ChannelID, owner.SenderID)
	if err != nil || len(reminders) != 1 {
		t.Fatalf("owner reminders after denied delete: %#v err=%v", reminders, err)
	}

	deleted, err := executor.ExecuteAndRecord(ctx, owner, call("delete-1", "manage_reminders", `{"action":"remove","ids":[1]}`))
	if err != nil || deleted.IsError {
		t.Fatalf("delete reminder: result=%#v err=%v", deleted, err)
	}
	reminders, err = store.ListReminders(owner.ChannelID, owner.SenderID)
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
		{"missing offset", `{"action":"add","items":[{"message":"call","schedule":{"kind":"at","at":"2026-07-15T12:00:00"}}]}`},
		{"past", `{"action":"add","items":[{"message":"call","schedule":{"kind":"at","at":"2026-07-13T12:00:00Z"}}]}`},
		{"mixed schedule", `{"action":"add","items":[{"message":"call","schedule":{"kind":"at","at":"2026-07-15T12:00:00Z","expr":"0 1 * * *"}}]}`},
		{"unknown identity field", `{"action":"list","sender_id":"other"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := executor.ExecuteAndRecord(ctx, toolCtx, call(test.name, "manage_reminders", test.args))
			if err != nil || !result.IsError {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}

func TestReminderBatchAddUpdateListAndRemove(t *testing.T) {
	executor, store, _ := newTestExecutor(t)
	ctx := context.Background()
	toolCtx := Context{ChannelID: "telegram", SenderID: "owner"}
	batch := `{"action":"add","items":[
		{"message":"weekday check","schedule":{"kind":"cron","expr":"3 12 * * 1-5","timezone":"Asia/Kuala_Lumpur"}},
		{"message":"stretch","schedule":{"kind":"every","every_ms":3600000,"anchor_at":"2026-07-14T13:00:00Z"}}
	]}`
	result, err := executor.ExecuteAndRecord(ctx, toolCtx, call("batch", "manage_reminders", batch))
	if err != nil || result.IsError || !stringsContain(result.Content, `"count":2`) {
		t.Fatalf("batch add: %#v err=%v", result, err)
	}

	updated, err := executor.ExecuteAndRecord(ctx, toolCtx, call("update", "manage_reminders", `{"action":"update","id":1,"patch":{"message":"weekday lunch check","enabled":false}}`))
	if err != nil || updated.IsError || !stringsContain(updated.Content, "weekday lunch check") {
		t.Fatalf("update: %#v err=%v", updated, err)
	}
	listed, err := executor.ExecuteAndRecord(ctx, toolCtx, call("list", "manage_reminders", `{"action":"list"}`))
	if err != nil || listed.IsError || !stringsContain(listed.Content, `"kind":"cron"`) || !stringsContain(listed.Content, `"enabled":false`) {
		t.Fatalf("list: %#v err=%v", listed, err)
	}
	removed, err := executor.ExecuteAndRecord(ctx, toolCtx, call("remove", "manage_reminders", `{"action":"remove","ids":[1,2]}`))
	if err != nil || removed.IsError || !stringsContain(removed.Content, `"count":2`) {
		t.Fatalf("remove: %#v err=%v", removed, err)
	}
	remaining, err := store.ListReminders(toolCtx.ChannelID, toolCtx.SenderID)
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
	executor := NewExecutor(store, func() time.Time { return now }, location)
	toolCtx := Context{ChannelID: "telegram", SenderID: "owner"}

	result, err := executor.ExecuteAndRecord(context.Background(), toolCtx, call("local-cron", "manage_reminders", `{"action":"add","items":[{"message":"morning","schedule":{"kind":"cron","expr":"0 8 * * *"}}]}`))
	if err != nil || result.IsError {
		t.Fatalf("add reminder: result=%#v err=%v", result, err)
	}
	if !stringsContain(result.Content, `"timezone":"Asia/Singapore"`) ||
		!stringsContain(result.Content, `"next_fire_at":"2026-07-15T08:00:00+08:00"`) ||
		!stringsContain(result.Content, `"display_timezone":"Asia/Singapore"`) {
		t.Fatalf("result does not use server timezone: %s", result.Content)
	}

	reminders, err := store.ListReminders(toolCtx.ChannelID, toolCtx.SenderID)
	if err != nil || len(reminders) != 1 || reminders[0].Schedule.Timezone != "Asia/Singapore" {
		t.Fatalf("persisted reminder: %#v err=%v", reminders, err)
	}
}

func TestReminderBatchRollsBackOnInvalidItem(t *testing.T) {
	executor, store, _ := newTestExecutor(t)
	ctx := context.Background()
	toolCtx := Context{ChannelID: "cli", SenderID: "owner"}
	result, err := executor.ExecuteAndRecord(ctx, toolCtx, call("batch-invalid", "manage_reminders", `{"action":"add","items":[
		{"message":"valid","schedule":{"kind":"at","at":"2026-07-15T12:00:00Z"}},
		{"message":"invalid","schedule":{"kind":"cron","expr":"not cron","timezone":"UTC"}}
	]}`))
	if err != nil || !result.IsError {
		t.Fatalf("invalid batch: %#v err=%v", result, err)
	}
	reminders, err := store.ListReminders(toolCtx.ChannelID, toolCtx.SenderID)
	if err != nil || len(reminders) != 0 {
		t.Fatalf("batch was not atomic: %#v err=%v", reminders, err)
	}
}

func TestGlobalMemoryToolsAreIdempotentAndSearchable(t *testing.T) {
	executor, _, _ := newTestExecutor(t)
	ctx := context.Background()
	firstUser := Context{ChannelID: "telegram", SenderID: "first"}
	secondUser := Context{ChannelID: "discord", SenderID: "second"}

	first, err := executor.ExecuteAndRecord(ctx, firstUser, call("store-1", "store_memory", `{"content":"The user prefers espresso."}`))
	if err != nil || first.IsError {
		t.Fatalf("first store: %#v err=%v", first, err)
	}
	duplicate, err := executor.ExecuteAndRecord(ctx, secondUser, call("store-2", "store_memory", `{"content":"The user prefers espresso."}`))
	if err != nil || duplicate.IsError || !stringsContain(duplicate.Content, `"stored":false`) {
		t.Fatalf("duplicate store: %#v err=%v", duplicate, err)
	}
	search, err := executor.ExecuteAndRecord(ctx, secondUser, call("search-1", "search_memory", `{"query":"espresso"}`))
	if err != nil || search.IsError || !stringsContain(search.Content, "The user prefers espresso.") {
		t.Fatalf("global search: %#v err=%v", search, err)
	}
}

func TestPersonalityToolIsNotAvailable(t *testing.T) {
	definitions := Definitions(time.UTC)
	wantNames := []string{"manage_reminders", "store_memory", "search_memory"}
	for _, definition := range definitions {
		if definition.Function.Name == "manage_personality" {
			t.Fatal("manage_personality remains in the model tool catalog")
		}
	}
	if len(definitions) != 3 {
		t.Fatalf("tool definition count = %d, want 3", len(definitions))
	}
	for index, want := range wantNames {
		if got := definitions[index].Function.Name; got != want {
			t.Fatalf("tool definition %d = %q, want %q", index, got, want)
		}
	}

	executor, _, _ := newTestExecutor(t)
	ctx := context.Background()
	toolCtx := Context{ChannelID: "cli", SenderID: "owner"}
	result, err := executor.ExecuteAndRecord(ctx, toolCtx, call("personality", "manage_personality", `{"action":"view"}`))
	if err != nil || !result.IsError || !stringsContain(result.Content, `unknown tool \"manage_personality\"`) {
		t.Fatalf("removed personality tool result: %#v err=%v", result, err)
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
	result, err := executor.ExecuteAndRecord(ctx, toolCtx, call("list-1", "manage_reminders", `{"action":"list"}`))
	if err != nil || result.IsError {
		t.Fatalf("list reminders: %#v err=%v", result, err)
	}
	history, err := store.GetRecentHistory(ctx, toolCtx.ChannelID, toolCtx.SenderID, 10, 0)
	if err != nil || len(history) != 1 {
		t.Fatalf("history=%#v err=%v", history, err)
	}
	if history[0].Role != "tool" || history[0].ContentType != state.ContentToolResult {
		t.Fatalf("unexpected history row: %#v", history[0])
	}
}
