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
			_, err := tx.SaveMemoryUnique(ctx, seed.content, model, dims, vector.Pack(seed.vector))
			return err
		}); err != nil {
			t.Fatalf("SaveMemoryUnique(%q): %v", seed.content, err)
		}
	}

	var entries []MemoryEntry
	if err := store.WithTx(ctx, func(tx *Tx) error {
		var err error
		entries, err = tx.SearchMemoryByVector(ctx, model, dims, []float32{1, 0}, 0.5, 10)
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
	chunk := ConversationChunkKey{
		StartHistoryID: 1, EndHistoryID: 2, PartIndex: 0,
		ContentHash: "hash", EmbeddingModel: "embeddinggemma-v1",
		Dimensions: 3, IndexVersion: 1,
	}
	due, attempts, err := store.PrepareConversationChunk(ctx, chunk, time.Now())
	if err != nil || !due || attempts != 0 {
		t.Fatalf("PrepareConversationChunk: due=%v attempts=%d err=%v", due, attempts, err)
	}
	blob := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	if err := store.SaveConversationChunkEmbedding(ctx, chunk, blob); err != nil {
		t.Fatalf("SaveConversationChunkEmbedding: %v", err)
	}
	due, _, err = store.PrepareConversationChunk(ctx, chunk, time.Now())
	if err != nil || due {
		t.Fatalf("prepared completed chunk: due=%v err=%v", due, err)
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

func TestFreshDatabaseUsesOnlyCanonicalTables(t *testing.T) {
	store := newTestStore(t)
	for _, table := range []string{"memory_entries", "reminders", "conversation_history", "conversation_chunks"} {
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
