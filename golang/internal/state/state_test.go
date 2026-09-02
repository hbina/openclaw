package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/openclaw/go/internal/vector"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}

func listRemindersForTest(t *testing.T, store *Store, channelID, senderID string) ([]Reminder, error) {
	t.Helper()
	var reminders []Reminder
	err := store.WithTx(context.Background(), func(tx *Tx) error {
		var err error
		reminders, err = tx.ListReminders(context.Background(), channelID, senderID)
		return err
	})
	return reminders, err
}

func listTasksForTest(t *testing.T, store *Store, status TaskStatus) ([]Task, error) {
	t.Helper()
	var tasks []Task
	err := store.WithTx(context.Background(), func(tx *Tx) error {
		var err error
		tasks, err = tx.ListTasks(context.Background(), status)
		return err
	})
	return tasks, err
}

func TestTaskLifecycleFiltersDuplicatesAndFinalCompletion(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	started := time.Date(2026, 8, 9, 10, 30, 0, 0, time.FixedZone("MYT", 8*60*60))
	completed := started.Add(2 * time.Hour)

	var first, second Task
	if err := store.WithTx(ctx, func(tx *Tx) error {
		var err error
		first, err = tx.AddTask(ctx, "  Prepare quarterly notes  ", started)
		if err != nil {
			return err
		}
		second, err = tx.AddTask(ctx, "Book dentist", started.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatalf("add tasks: %v", err)
	}
	if first.Description != "Prepare quarterly notes" || !first.StartedAt.Equal(started) || first.Status() != TaskOpen {
		t.Fatalf("first task = %#v", first)
	}

	if err := store.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.AddTask(ctx, " prepare QUARTERLY NOTES ", started.Add(time.Minute))
		return err
	}); err == nil || !strings.Contains(err.Error(), "already has description") {
		t.Fatalf("duplicate open task error = %v", err)
	}

	if err := store.WithTx(ctx, func(tx *Tx) error {
		updated, err := tx.UpdateTask(ctx, second.ID, "Book annual dentist visit")
		if err == nil && updated.Description != "Book annual dentist visit" {
			t.Fatalf("updated task = %#v", updated)
		}
		return err
	}); err != nil {
		t.Fatalf("update task: %v", err)
	}

	if err := store.WithTx(ctx, func(tx *Tx) error {
		done, err := tx.CompleteTask(ctx, first.ID, completed)
		if err == nil && (done.CompletedAt == nil || !done.CompletedAt.Equal(completed) || done.Status() != TaskCompleted) {
			t.Fatalf("completed task = %#v", done)
		}
		return err
	}); err != nil {
		t.Fatalf("complete task: %v", err)
	}

	open, err := listTasksForTest(t, store, "")
	if err != nil || len(open) != 1 || open[0].ID != second.ID {
		t.Fatalf("open tasks = %#v, err=%v", open, err)
	}
	done, err := listTasksForTest(t, store, TaskCompleted)
	if err != nil || len(done) != 1 || done[0].ID != first.ID {
		t.Fatalf("completed tasks = %#v, err=%v", done, err)
	}
	all, err := listTasksForTest(t, store, TaskAll)
	if err != nil || len(all) != 2 {
		t.Fatalf("all tasks = %#v, err=%v", all, err)
	}

	for name, operation := range map[string]func(*Tx) error{
		"update": func(tx *Tx) error {
			_, err := tx.UpdateTask(ctx, first.ID, "Changed")
			return err
		},
		"complete again": func(tx *Tx) error {
			_, err := tx.CompleteTask(ctx, first.ID, completed.Add(time.Hour))
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := store.WithTx(ctx, operation); err == nil {
				t.Fatalf("%s completed task succeeded", name)
			}
		})
	}

	if err := store.WithTx(ctx, func(tx *Tx) error {
		repeated, err := tx.AddTask(ctx, "PREPARE QUARTERLY NOTES", completed.Add(time.Minute))
		if err == nil && repeated.ID == first.ID {
			t.Fatalf("repeated task reused ID %d", repeated.ID)
		}
		return err
	}); err != nil {
		t.Fatalf("reuse completed description: %v", err)
	}

	if err := store.WithTx(ctx, func(tx *Tx) error {
		if _, err := tx.DeleteTask(ctx, first.ID); err != nil {
			return err
		}
		_, err := tx.DeleteTask(ctx, second.ID)
		return err
	}); err != nil {
		t.Fatalf("remove open and completed tasks: %v", err)
	}
}

func TestTaskBatchTransactionRollsBack(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 9, 1, 0, 0, 0, time.UTC)
	err := store.WithTx(ctx, func(tx *Tx) error {
		if _, err := tx.AddTask(ctx, "first", now); err != nil {
			return err
		}
		_, err := tx.AddTask(ctx, " FIRST ", now)
		return err
	})
	if err == nil {
		t.Fatal("duplicate batch succeeded")
	}
	tasks, listErr := listTasksForTest(t, store, TaskAll)
	if listErr != nil || len(tasks) != 0 {
		t.Fatalf("rolled-back tasks = %#v, err=%v", tasks, listErr)
	}
}

func TestTasksPersistAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	started := time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC)
	if err := store.WithTx(context.Background(), func(tx *Tx) error {
		_, err := tx.AddTask(context.Background(), "survive restart", started)
		return err
	}); err != nil {
		t.Fatalf("add task: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	tasks, err := listTasksForTest(t, reopened, TaskOpen)
	if err != nil || len(tasks) != 1 || tasks[0].Description != "survive restart" || !tasks[0].StartedAt.Equal(started) {
		t.Fatalf("reopened tasks = %#v, err=%v", tasks, err)
	}
}

func TestReminderLifecycle(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	ctx := context.Background()
	if err := store.WithTx(ctx, func(tx *Tx) error {
		for _, item := range []struct {
			channel, sender, message string
			at                       time.Time
		}{
			{"telegram", "user-1", "due", now.Add(-time.Minute)},
			{"telegram", "user-1", "future", now.Add(time.Hour)},
			{"cli", "user-2", "other", now.Add(time.Hour)},
		} {
			if _, err := tx.AddReminder(ctx, item.channel, item.sender, item.message, ReminderSchedule{Kind: ScheduleAt, At: item.at}, item.at); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed reminders: %v", err)
	}

	listed, err := listRemindersForTest(t, store, "telegram", "user-1")
	if err != nil {
		t.Fatalf("ListReminders: %v", err)
	}
	if len(listed) != 2 || listed[0].Message != "due" || listed[1].Message != "future" {
		t.Fatalf("unexpected reminder list: %#v", listed)
	}

	due, err := store.FetchDueReminders()
	if err != nil {
		t.Fatalf("FetchDueReminders: %v", err)
	}
	if len(due) != 1 || due[0].Message != "due" {
		t.Fatalf("unexpected due reminders: %#v", due)
	}

	beforeDelete, err := listRemindersForTest(t, store, "telegram", "user-1")
	if err != nil {
		t.Fatalf("list reminders before delete: %v", err)
	}
	if len(beforeDelete) != 2 {
		t.Fatalf("reminder count before delivery delete = %d, want 2", len(beforeDelete))
	}
	if err := store.CompleteReminder(due[0], now); err != nil {
		t.Fatalf("CompleteReminder: %v", err)
	}

	remaining, err := listRemindersForTest(t, store, "telegram", "user-1")
	if err != nil {
		t.Fatalf("list remaining reminders: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Message != "future" {
		t.Fatalf("unexpected remaining reminders: %#v", remaining)
	}
}

func TestRecurringReminderAdvancesAfterDelivery(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	schedule := ReminderSchedule{Kind: ScheduleCron, CronExpr: "0 8 * * 1-5", Timezone: "Asia/Kuala_Lumpur"}
	next, err := NextReminderRun(schedule, now)
	if err != nil || !next.Equal(time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("NextReminderRun: next=%v err=%v", next, err)
	}
	var id int64
	if err := store.WithTx(ctx, func(tx *Tx) error {
		id, err = tx.AddReminder(ctx, "telegram", "user-1", "briefing", schedule, next)
		return err
	}); err != nil {
		t.Fatalf("add recurring reminder: %v", err)
	}
	listed, err := listRemindersForTest(t, store, "telegram", "user-1")
	if err != nil || len(listed) != 1 || listed[0].ID != int(id) {
		t.Fatalf("list recurring reminder: %#v err=%v", listed, err)
	}
	if err := store.CompleteReminder(listed[0], next.Add(time.Minute)); err != nil {
		t.Fatalf("complete recurring reminder: %v", err)
	}
	advanced, err := listRemindersForTest(t, store, "telegram", "user-1")
	want := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	if err != nil || len(advanced) != 1 || !advanced[0].FireAt.Equal(want) {
		t.Fatalf("advanced reminder: %#v err=%v", advanced, err)
	}
}

func TestCompleteReminderDeliveryRollsBackTranscriptWhenCompletionFails(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	schedule := ReminderSchedule{Kind: ScheduleCron, CronExpr: "0 8 * * *", Timezone: "UTC"}
	var id int64
	if err := store.WithTx(ctx, func(tx *Tx) error {
		var err error
		id, err = tx.AddReminder(ctx, "telegram", "owner", "briefing", schedule, now.Add(-time.Hour))
		return err
	}); err != nil {
		t.Fatalf("add reminder: %v", err)
	}
	reminders, err := listRemindersForTest(t, store, "telegram", "owner")
	if err != nil || len(reminders) != 1 {
		t.Fatalf("reminders=%#v err=%v", reminders, err)
	}
	stale := reminders[0]
	stale.FireAt = stale.FireAt.Add(-time.Minute)
	err = store.CompleteReminderDelivery(ctx, stale, now, `{"reminder_id":1}`, "delivered")
	if err == nil || !strings.Contains(err.Error(), "changed while delivering") {
		t.Fatalf("CompleteReminderDelivery error = %v", err)
	}
	history, err := store.GetConversationHistory(ctx, "telegram", "owner")
	if err != nil || len(history) != 0 {
		t.Fatalf("rolled-back history=%#v err=%v", history, err)
	}
	remaining, err := listRemindersForTest(t, store, "telegram", "owner")
	if err != nil || len(remaining) != 1 || remaining[0].ID != int(id) {
		t.Fatalf("remaining reminders=%#v err=%v", remaining, err)
	}
}

func TestCompleteReminderDeliveryAdvancesRecurringAndRecordsTranscript(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	schedule := ReminderSchedule{Kind: ScheduleCron, CronExpr: "0 8 * * *", Timezone: "UTC"}
	fireAt := now.Add(-4 * time.Hour)
	if err := store.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.AddReminder(ctx, "telegram", "owner", "briefing", schedule, fireAt)
		return err
	}); err != nil {
		t.Fatalf("add reminder: %v", err)
	}
	reminders, err := listRemindersForTest(t, store, "telegram", "owner")
	if err != nil || len(reminders) != 1 {
		t.Fatalf("reminders=%#v err=%v", reminders, err)
	}
	if err := store.CompleteReminderDelivery(ctx, reminders[0], now, `{"reminder_id":1}`, "delivered briefing"); err != nil {
		t.Fatalf("CompleteReminderDelivery: %v", err)
	}
	advanced, err := listRemindersForTest(t, store, "telegram", "owner")
	wantNext := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	if err != nil || len(advanced) != 1 || !advanced[0].FireAt.Equal(wantNext) {
		t.Fatalf("advanced reminder=%#v err=%v", advanced, err)
	}
	history, err := store.GetConversationHistory(ctx, "telegram", "owner")
	if err != nil || len(history) != 2 || history[0].ContentType != ContentScheduledReminder || history[1].Content != "delivered briefing" {
		t.Fatalf("delivery history=%#v err=%v", history, err)
	}
}

func TestNextReminderRunRejectsOverflowingInterval(t *testing.T) {
	_, err := NextReminderRun(ReminderSchedule{Kind: ScheduleEvery, EveryMS: int64(^uint64(0) >> 1)}, time.Now())
	if err == nil {
		t.Fatal("expected overflowing interval to fail")
	}
}

func TestNextReminderRunSupportsAnchoredEveryAndSixFieldCron(t *testing.T) {
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		schedule ReminderSchedule
		want     time.Time
	}{
		{
			name: "anchored every",
			schedule: ReminderSchedule{
				Kind: ScheduleEvery, EveryMS: int64(time.Hour / time.Millisecond),
				AnchorAt: now.Add(-30 * time.Minute),
			},
			want: now.Add(30 * time.Minute),
		},
		{
			name: "future every anchor",
			schedule: ReminderSchedule{
				Kind: ScheduleEvery, EveryMS: int64(time.Hour / time.Millisecond),
				AnchorAt: now.Add(2 * time.Hour),
			},
			want: now.Add(2 * time.Hour),
		},
		{
			name: "six field cron",
			schedule: ReminderSchedule{
				Kind: ScheduleCron, CronExpr: "30 0 8 * * *", Timezone: "Asia/Kuala_Lumpur",
			},
			want: time.Date(2026, 7, 15, 0, 0, 30, 0, time.UTC),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := NextReminderRun(test.schedule, now)
			if err != nil || !got.Equal(test.want) {
				t.Fatalf("NextReminderRun: got=%v want=%v err=%v", got, test.want, err)
			}
		})
	}
}

func TestMemorySearch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const model = "test-embedding-model"
	const dims = 2

	seeds := []struct {
		content string
		vector  []float32
	}{
		{"likes espresso", []float32{1, 0}},
		{"prefers tea", []float32{0, 1}},
		{"espresso after lunch", []float32{1, 0}},
	}
	for _, seed := range seeds {
		if err := store.WithTx(ctx, func(tx *Tx) error {
			_, _, err := tx.StoreMemory(ctx, MemoryWrite{Kind: MemoryDurable, Content: seed.content, OriginClass: MemoryOriginOwner, SourceKind: MemorySourceOperator, EmbeddingModel: model, Dimensions: dims, Embedding: vector.Pack(seed.vector), Now: time.Now()})
			return err
		}); err != nil {
			t.Fatalf("SaveMemoryUnique(%q): %v", seed.content, err)
		}
	}

	var entries []MemorySearchResult
	if err := store.WithTx(ctx, func(tx *Tx) error {
		var err error
		entries, err = tx.SearchMemories(ctx, model, dims, []float32{1, 0}, `"espresso"`, 0.5, 10)
		return err
	}); err != nil {
		t.Fatalf("SearchMemoryByVector: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("memory result count = %d, want 2", len(entries))
	}
	for _, entry := range entries {
		if entry.ID == 0 || entry.Content == "" {
			t.Fatalf("incomplete memory entry: %#v", entry)
		}
	}
}

func TestMemoryLedgerRevisionsAndDeletionRetainAudit(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC)
	write := MemoryWrite{Kind: MemoryProfile, Content: "Owner prefers espresso.", OriginClass: MemoryOriginOwner, SourceKind: MemorySourceOperator, EmbeddingModel: "test", Dimensions: 2, Embedding: vector.Pack([]float32{1, 0}), Now: now}
	var item Memory
	if err := store.WithTx(ctx, func(tx *Tx) error { var err error; item, _, err = tx.StoreMemory(ctx, write); return err }); err != nil {
		t.Fatalf("StoreMemory: %v", err)
	}
	stableID := item.ID
	write.Content = "Owner now prefers tea."
	write.Kind = MemoryDurable
	write.Embedding = vector.Pack([]float32{0, 1})
	write.Now = now.Add(time.Hour)
	if err := store.WithTx(ctx, func(tx *Tx) error { var err error; item, err = tx.UpdateMemory(ctx, stableID, write); return err }); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	if item.ID != stableID || item.RevisionNumber != 2 || item.Kind != MemoryDurable {
		t.Fatalf("updated memory=%#v", item)
	}
	var revisionCount int
	if err := store.db.QueryRow(`SELECT count(*) FROM memory_revisions WHERE memory_id=?`, stableID).Scan(&revisionCount); err != nil || revisionCount != 2 {
		t.Fatalf("revision count=%d err=%v", revisionCount, err)
	}
	if err := store.WithTx(ctx, func(tx *Tx) error {
		var err error
		item, err = tx.RemoveMemory(ctx, stableID, now.Add(2*time.Hour))
		return err
	}); err != nil {
		t.Fatalf("RemoveMemory: %v", err)
	}
	if item.Status != MemoryDeleted || item.DeletedAt == nil {
		t.Fatalf("deleted memory=%#v", item)
	}
	results, err := store.SearchMemories(ctx, "test", 2, []float32{0, 1}, `"tea"`, 0, 10)
	if err != nil || len(results) != 0 {
		t.Fatalf("deleted search results=%#v err=%v", results, err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM memory_revisions WHERE memory_id=?`, stableID).Scan(&revisionCount); err != nil || revisionCount != 2 {
		t.Fatalf("retained revisions=%d err=%v", revisionCount, err)
	}
}

func TestConversationHistory(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Save a few turns and verify they round-trip.
	for _, turn := range []struct{ role, content string }{
		{"user", "hello"},
		{"assistant", "hi there"},
		{"user", "how are you"},
	} {
		if err := store.SaveConversationTurn(ctx, "telegram", "u1", turn.role, turn.content); err != nil {
			t.Fatalf("SaveConversationTurn: %v", err)
		}
	}

	turns, err := store.GetConversationHistory(ctx, "telegram", "u1")
	if err != nil {
		t.Fatalf("GetConversationHistory: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("turn count = %d, want 3", len(turns))
	}
	if turns[0].Content != "hello" || turns[0].ContentType != ContentText {
		t.Fatalf("unexpected first turn: %#v", turns[0])
	}
}

func TestConversationChunkIndexPersistsAndIsVersionScoped(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	blob := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	err := store.WithTx(ctx, func(tx *Tx) error {
		return tx.SaveConversationChunks(ctx, 1, 2, []ConversationChunk{{PartIndex: 0, ContentHash: "hash", EmbeddingModel: "embeddinggemma-v1", Dimensions: 3, IndexVersion: 1, Embedding: blob}})
	})
	if err != nil {
		t.Fatalf("SaveConversationChunks: %v", err)
	}
	active, err := store.LoadConversationEmbeddings(ctx, "embeddinggemma-v1", 1, 3)
	if err != nil || len(active) != 1 || string(active[0].Embedding) != string(blob) {
		t.Fatalf("active embeddings=%#v err=%v", active, err)
	}
	other, err := store.LoadConversationEmbeddings(ctx, "embeddinggemma-v2", 1, 3)
	if err != nil || len(other) != 0 {
		t.Fatalf("other-model embeddings=%#v err=%v", other, err)
	}
}

func TestConversationIndexGapValidation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.SaveConversationTurn(ctx, "cli", "owner", "user", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveConversationTurn(ctx, "cli", "owner", "assistant", "hi"); err != nil {
		t.Fatal(err)
	}
	gaps, err := store.CountConversationIndexGaps(ctx, "test", 1, 2)
	if err != nil || gaps != 1 {
		t.Fatalf("unindexed gaps=%d err=%v", gaps, err)
	}
	if err := store.WithTx(ctx, func(tx *Tx) error {
		return tx.SaveConversationChunks(ctx, 1, 2, []ConversationChunk{{PartIndex: 0, ContentHash: "hash", EmbeddingModel: "test", Dimensions: 2, IndexVersion: 1, Embedding: vector.Pack([]float32{1, 0})}})
	}); err != nil {
		t.Fatal(err)
	}
	gaps, err = store.CountConversationIndexGaps(ctx, "test", 1, 2)
	if err != nil || gaps != 0 {
		t.Fatalf("indexed gaps=%d err=%v", gaps, err)
	}
}

func TestFreshDatabaseUsesOnlyCanonicalTables(t *testing.T) {
	store := newTestStore(t)
	for _, table := range []string{"memories", "memory_revisions", "memory_embeddings", "memory_fts", "memory_rag_matches", "memory_maintenance_state", "memory_maintenance_runs", "memory_candidates", "reminders", "tasks", "conversation_history", "conversation_chunks", "response_traces", "trace_events", "rag_retrievals", "rag_matches", "llm_calls", "tool_executions", "response_outputs", "delivery_attempts"} {
		if !databaseTableExists(t, store, table) {
			t.Errorf("fresh database is missing %s", table)
		}
	}
	for _, table := range []string{"agent_state", "conversation_compactions", "personality_documents"} {
		if databaseTableExists(t, store, table) {
			t.Errorf("fresh database contains removed table %s", table)
		}
	}
}

func TestStoreRejectsMissingCanonicalTaskIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-task-index.sqlite")
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := store.db.Exec(`DROP INDEX idx_tasks_open_description`); err != nil {
		t.Fatalf("drop task index: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if _, err := NewStore(path); err == nil || !strings.Contains(err.Error(), "task duplicate-prevention index is missing") {
		t.Fatalf("NewStore error = %v", err)
	}
}

func TestStoreRejectsMissingCanonicalProvenanceIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-trace-index.sqlite")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP INDEX idx_delivery_provider_message`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path); err == nil || !strings.Contains(err.Error(), "idx_delivery_provider_message") {
		t.Fatalf("NewStore error=%v", err)
	}
}

func TestStoreRejectsNonCanonicalSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "noncanonical.sqlite")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE agent_state (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create removed table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}

	if _, err := NewStore(path); err == nil || !strings.Contains(err.Error(), "do not match canonical tables") {
		t.Fatalf("NewStore error = %v, want canonical-schema rejection", err)
	}

	reopened, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("reopen fixture: %v", err)
	}
	defer reopened.Close()
	var tables int
	if err := reopened.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil {
		t.Fatalf("count fixture tables: %v", err)
	}
	if tables != 1 {
		t.Fatalf("rejected database was mutated to %d tables, want 1", tables)
	}
}

func databaseTableExists(t *testing.T, store *Store, name string) bool {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count); err != nil {
		t.Fatalf("query table %q: %v", name, err)
	}
	return count != 0
}
