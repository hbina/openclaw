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
	if err := store.AddReminder("telegram", "user-1", "due", now.Add(-time.Minute)); err != nil {
		t.Fatalf("add due reminder: %v", err)
	}
	if err := store.AddReminder("telegram", "user-1", "future", now.Add(time.Hour)); err != nil {
		t.Fatalf("add future reminder: %v", err)
	}
	if err := store.AddReminder("discord", "user-2", "other", now.Add(time.Hour)); err != nil {
		t.Fatalf("add other reminder: %v", err)
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
	if err := store.DeleteReminder(due[0].ID); err != nil {
		t.Fatalf("DeleteReminder: %v", err)
	}

	remaining, err := store.ListReminders("telegram", "user-1")
	if err != nil {
		t.Fatalf("list remaining reminders: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Message != "future" {
		t.Fatalf("unexpected remaining reminders: %#v", remaining)
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

func TestLoadPersonality(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for _, document := range []PersonalityDocument{
		{Name: "IDENTITY.md", Content: "Name: Claw"},
		{Name: "SOUL.md", Content: "Be direct."},
	} {
		_, err := store.db.ExecContext(ctx, `
			INSERT INTO personality_documents (name, content) VALUES (?, ?)
		`, document.Name, document.Content)
		if err != nil {
			t.Fatalf("insert personality document: %v", err)
		}
	}

	documents, err := store.LoadPersonality(ctx)
	if err != nil {
		t.Fatalf("LoadPersonality: %v", err)
	}
	if len(documents) != 2 {
		t.Fatalf("personality document count = %d, want 2", len(documents))
	}
	if documents[0].Name != "SOUL.md" || documents[1].Name != "IDENTITY.md" {
		t.Fatalf("unexpected personality order: %#v", documents)
	}
}
