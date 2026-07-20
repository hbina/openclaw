package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
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
			{"discord", "user-2", "other", now.Add(time.Hour)},
		} {
			if _, err := tx.AddReminder(ctx, item.channel, item.sender, item.message, ReminderSchedule{Kind: ScheduleAt, At: item.at}, item.at); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed reminders: %v", err)
	}

	listed, err := store.ListReminders("telegram", "user-1")
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

	beforeDelete, err := store.ListReminders("telegram", "user-1")
	if err != nil {
		t.Fatalf("list reminders before delete: %v", err)
	}
	if len(beforeDelete) != 2 {
		t.Fatalf("reminder count before delivery delete = %d, want 2", len(beforeDelete))
	}
	if err := store.CompleteReminder(due[0], now); err != nil {
		t.Fatalf("CompleteReminder: %v", err)
	}

	remaining, err := store.ListReminders("telegram", "user-1")
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
	listed, err := store.ListReminders("telegram", "user-1")
	if err != nil || len(listed) != 1 || listed[0].ID != int(id) {
		t.Fatalf("list recurring reminder: %#v err=%v", listed, err)
	}
	if err := store.CompleteReminder(listed[0], next.Add(time.Minute)); err != nil {
		t.Fatalf("complete recurring reminder: %v", err)
	}
	advanced, err := store.ListReminders("telegram", "user-1")
	want := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	if err != nil || len(advanced) != 1 || !advanced[0].FireAt.Equal(want) {
		t.Fatalf("advanced reminder: %#v err=%v", advanced, err)
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
	for _, content := range []string{"likes espresso", "prefers tea", "espresso after lunch"} {
		if err := store.SaveMemory(ctx, content); err != nil {
			t.Fatalf("SaveMemory(%q): %v", content, err)
		}
	}

	entries, err := store.SearchMemory(ctx, "espresso", 10)
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("memory result count = %d, want 2", len(entries))
	}
	for _, entry := range entries {
		if entry.ID == 0 || entry.Content == "" || entry.CreatedAt.IsZero() {
			t.Fatalf("incomplete memory entry: %#v", entry)
		}
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

	turns, err := store.GetRecentHistory(ctx, "telegram", "u1", 10, 0)
	if err != nil {
		t.Fatalf("GetRecentHistory: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("turn count = %d, want 3", len(turns))
	}
	if turns[0].Content != "hello" || turns[0].ContentType != ContentText {
		t.Fatalf("unexpected first turn: %#v", turns[0])
	}

	// Limit to last 2 turns.
	recent, err := store.GetRecentHistory(ctx, "telegram", "u1", 2, 0)
	if err != nil {
		t.Fatalf("GetRecentHistory(limit=2): %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("limited turn count = %d, want 2", len(recent))
	}
	if recent[0].Content != "hi there" {
		t.Fatalf("unexpected limited first turn: %#v", recent[0])
	}
}

func TestCompactionLifecycle(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// No compaction yet.
	c, err := store.GetLatestCompaction(ctx, "telegram", "u1")
	if err != nil {
		t.Fatalf("GetLatestCompaction (empty): %v", err)
	}
	if c != nil {
		t.Fatalf("expected nil compaction, got %#v", c)
	}

	// Insert a few turns and remember the last ID before the "keep" window.
	for range 5 {
		if err := store.SaveConversationTurn(ctx, "telegram", "u1", "user", "message"); err != nil {
			t.Fatalf("SaveConversationTurn: %v", err)
		}
	}
	allTurns, err := store.GetRecentHistory(ctx, "telegram", "u1", 100, 0)
	if err != nil {
		t.Fatalf("GetRecentHistory (all): %v", err)
	}
	if len(allTurns) != 5 {
		t.Fatalf("expected 5 turns, got %d", len(allTurns))
	}
	firstKeptID := allTurns[3].ID // keep turns 3 and 4, summarize 0-2

	id, err := store.SaveCompaction(ctx, "telegram", "u1", "summary text", 1000, firstKeptID)
	if err != nil {
		t.Fatalf("SaveCompaction: %v", err)
	}
	if id == 0 {
		t.Fatal("SaveCompaction returned id=0")
	}

	// Retrieve and verify.
	loaded, err := store.GetLatestCompaction(ctx, "telegram", "u1")
	if err != nil {
		t.Fatalf("GetLatestCompaction: %v", err)
	}
	if loaded == nil {
		t.Fatal("GetLatestCompaction returned nil after save")
	}
	if loaded.Summary != "summary text" || loaded.FirstKeptID != firstKeptID || loaded.TokensBefore != 1000 {
		t.Fatalf("unexpected compaction: %#v", loaded)
	}

	// Trim history before firstKeptID.
	if err := store.TrimHistoryBefore(ctx, "telegram", "u1", firstKeptID); err != nil {
		t.Fatalf("TrimHistoryBefore: %v", err)
	}
	remaining, err := store.GetRecentHistory(ctx, "telegram", "u1", 100, 0)
	if err != nil {
		t.Fatalf("GetRecentHistory after trim: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("remaining turn count after trim = %d, want 2", len(remaining))
	}
	if remaining[0].ID != firstKeptID {
		t.Fatalf("first remaining id = %d, want %d", remaining[0].ID, firstKeptID)
	}
}

func TestFreshDatabaseDoesNotContainPersonalityDocuments(t *testing.T) {
	store := newTestStore(t)
	if databaseTableExists(t, store, "personality_documents") {
		t.Fatal("fresh database contains personality_documents")
	}
}

func TestOpeningOlderDatabaseDropsPersonalityWithoutAffectingRuntimeState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	ctx := context.Background()
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := store.db.Exec(`CREATE TABLE personality_documents (
		name TEXT PRIMARY KEY, content TEXT NOT NULL, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	); INSERT INTO personality_documents (name, content) VALUES ('SOUL.md', 'legacy soul');`); err != nil {
		t.Fatalf("seed legacy personality table: %v", err)
	}
	if err := store.SaveMemory(ctx, "likes espresso"); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	if err := store.SaveConversationTurn(ctx, "cli", "owner", "user", "hello"); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	if err := store.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.AddReminder(ctx, "cli", "owner", "call home", ReminderSchedule{Kind: ScheduleAt, At: time.Now().Add(time.Hour)}, time.Now().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatalf("seed reminder: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	if databaseTableExists(t, reopened, "personality_documents") {
		t.Fatal("legacy personality_documents table was not dropped")
	}
	reminders, err := reopened.ListReminders("cli", "owner")
	if err != nil || len(reminders) != 1 || reminders[0].Message != "call home" {
		t.Fatalf("reminders after migration: %#v err=%v", reminders, err)
	}
	memories, err := reopened.SearchMemory(ctx, "espresso", 5)
	if err != nil || len(memories) != 1 || memories[0].Content != "likes espresso" {
		t.Fatalf("memory after migration: %#v err=%v", memories, err)
	}
	history, err := reopened.GetRecentHistory(ctx, "cli", "owner", 5, 0)
	if err != nil || len(history) != 1 || history[0].Content != "hello" {
		t.Fatalf("history after migration: %#v err=%v", history, err)
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
