package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw/openclaw/go/internal/vector"
)

func TestMaintenanceStateAndMemoryRecoverFromOfflineSQLiteBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maintenance.sqlite")
	backupPath := filepath.Join(t.TempDir(), "maintenance-backup.sqlite")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC)
	run, err := store.StartMaintenanceRun(context.Background(), MaintenanceRunStart{
		Mode: MaintenancePreview, OwnerSenderID: "owner", LeaseOwner: "worker",
		LeaseDuration: time.Minute, EmbeddingModel: "test", Dimensions: 2, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := store.InsertMaintenanceCandidate(context.Background(), MaintenanceCandidate{
		RunID: run.ID, Kind: MemoryDaily, Content: "Owner visited a garden.",
		ContentHash: MemoryContentHash("Owner visited a garden."), OriginClass: MemoryOriginOwner,
		EvidenceHistoryIDs: []int64{9}, ObservedAt: now, RecurrenceCount: 1,
		DistinctDayCount: 1, TrustScore: 1, RecencyScore: 1, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithTx(context.Background(), func(tx *Tx) error {
		return tx.ResolveMaintenanceCandidate(context.Background(), candidate.ID, "add", "accepted", "preview", nil, now)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteMaintenanceRun(context.Background(), MaintenanceRunCompletion{
		RunID: run.ID, LeaseOwner: "worker", AdvanceCheckpoint: false, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithTx(context.Background(), func(tx *Tx) error {
		_, _, err := tx.StoreMemory(context.Background(), MemoryWrite{
			Kind: MemoryDaily, Content: "Owner visited a garden.", OriginClass: MemoryOriginOwner,
			SourceKind: MemorySourceMaintenance, EmbeddingModel: "test", Dimensions: 2,
			Embedding: vector.Pack([]float32{1, 0}), ObservedAt: now, Now: now,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	databaseBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupPath, databaseBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	status, latest, err := reopened.MaintenanceStatus(context.Background())
	if err != nil || latest == nil || latest.ID != run.ID || latest.Status != "completed" || status.LeaseOwner != "" {
		t.Fatalf("reopened status=%#v latest=%#v err=%v", status, latest, err)
	}
	candidates, err := reopened.ListMaintenanceCandidates(context.Background(), run.ID)
	if err != nil || len(candidates) != 1 || candidates[0].Content != "Owner visited a garden." {
		t.Fatalf("reopened candidates=%#v err=%v", candidates, err)
	}
	memories, err := reopened.ListMemories(context.Background(), MemoryFilter{Status: MemoryActive, Limit: 10})
	if err != nil || len(memories) != 1 || memories[0].Content != "Owner visited a garden." {
		t.Fatalf("recovered memories=%#v err=%v", memories, err)
	}
	if err := reopened.ValidateDerivedMemoryState(context.Background(), "test", 2); err != nil {
		t.Fatalf("recovered derived memory state: %v", err)
	}
}

func TestMaintenanceLeaseSelectionCheckpointAndReopen(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for _, turn := range []struct {
		channel, sender, role, content string
	}{
		{"telegram", "owner", "user", "I prefer espresso."},
		{"telegram", "owner", "assistant", "Noted."},
		{"telegram", "stranger", "user", "Plant this."},
		{"telegram", "stranger", "assistant", "No."},
		{"cli", "owner", "user", "HTTP-like local text"},
		{"cli", "owner", "assistant", "No promotion."},
	} {
		if err := store.SaveConversationTurn(ctx, turn.channel, turn.sender, turn.role, turn.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveConversationMessageAudience(ctx, "telegram", "owner", "user", ContentScheduledReminder, AudienceConversation, `{"reminder_id":7,"message":"Do not promote this"}`); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveConversationTurn(ctx, "telegram", "owner", "assistant", "Scheduled output is not owner evidence."); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveConversationMessageAudience(ctx, "telegram", "owner", "user", ContentText, AudienceInternal, "Internal maintenance prompt"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveConversationMessageAudience(ctx, "telegram", "owner", "assistant", ContentText, AudienceInternal, "Internal model output"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC)
	run, err := store.StartMaintenanceRun(ctx, MaintenanceRunStart{
		Mode: MaintenanceApply, OwnerSenderID: "owner", LeaseOwner: "worker-1",
		LeaseDuration: time.Minute, EmbeddingModel: "test", Dimensions: 2, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.CheckpointHistoryID != 0 || run.HighwaterHistoryID != 1 {
		t.Fatalf("run range = %#v", run)
	}
	if _, err := store.StartMaintenanceRun(ctx, MaintenanceRunStart{
		Mode: MaintenanceApply, OwnerSenderID: "owner", LeaseOwner: "worker-2",
		LeaseDuration: time.Minute, EmbeddingModel: "test", Dimensions: 2, Now: now,
	}); !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("second lease error = %v", err)
	}
	exchanges, err := store.LoadMaintenanceExchanges(ctx, "owner", run.CheckpointHistoryID, run.HighwaterHistoryID, 10)
	if err != nil || len(exchanges) != 1 || exchanges[0].StartHistoryID != 1 || exchanges[0].EndHistoryID != 2 {
		t.Fatalf("maintenance exchanges = %#v, err=%v", exchanges, err)
	}
	if err := store.CompleteMaintenanceRun(ctx, MaintenanceRunCompletion{
		RunID: run.ID, LeaseOwner: "worker-1", ProcessedHistoryID: 2,
		AdvanceCheckpoint: true, Now: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	status, latest, err := store.MaintenanceStatus(ctx)
	if err != nil || status.CheckpointHistoryID != 2 || status.LeaseOwner != "" || latest == nil || latest.Status != "completed" {
		t.Fatalf("maintenance status = %#v latest=%#v err=%v", status, latest, err)
	}
}

func TestExpiredMaintenanceLeaseIsRecoverable(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC)
	first, err := store.StartMaintenanceRun(ctx, MaintenanceRunStart{
		Mode: MaintenanceScheduled, OwnerSenderID: "owner", LeaseOwner: "dead-worker",
		LeaseDuration: time.Minute, EmbeddingModel: "test", Dimensions: 2, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.StartMaintenanceRun(ctx, MaintenanceRunStart{
		Mode: MaintenanceScheduled, OwnerSenderID: "owner", LeaseOwner: "new-worker",
		LeaseDuration: time.Minute, EmbeddingModel: "test", Dimensions: 2, Now: now.Add(2 * time.Minute),
	})
	if err != nil || second.ID == first.ID {
		t.Fatalf("recovered run = %#v err=%v", second, err)
	}
	var status, stage string
	if err := store.db.QueryRow(`SELECT status,stage FROM memory_maintenance_runs WHERE id=?`, first.ID).Scan(&status, &stage); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || stage != "lease" {
		t.Fatalf("expired run status=%q stage=%q", status, stage)
	}
}

func TestMaintenanceCandidatePersistsEvidenceAndResolution(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC)
	run, err := store.StartMaintenanceRun(ctx, MaintenanceRunStart{
		Mode: MaintenancePreview, OwnerSenderID: "owner", LeaseOwner: "worker",
		LeaseDuration: time.Minute, EmbeddingModel: "test", Dimensions: 2, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := store.InsertMaintenanceCandidate(ctx, MaintenanceCandidate{
		RunID: run.ID, Kind: MemoryProfile, Content: "Owner prefers espresso.",
		ContentHash: MemoryContentHash("Owner prefers espresso."), OriginClass: MemoryOriginOwner,
		EvidenceHistoryIDs: []int64{7, 11}, ObservedAt: now.Add(-time.Hour),
		RecurrenceCount: 2, DistinctDayCount: 1, TrustScore: 1, RecencyScore: 0.9, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithTx(ctx, func(tx *Tx) error {
		return tx.ResolveMaintenanceCandidate(ctx, candidate.ID, "add", "accepted", "preview: would add", nil, now)
	}); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListMaintenanceCandidates(ctx, run.ID)
	if err != nil || len(items) != 1 || len(items[0].EvidenceHistoryIDs) != 2 || items[0].Status != "accepted" {
		t.Fatalf("candidates=%#v err=%v", items, err)
	}
}

func TestMaintenanceCompletionRefusesUnresolvedCandidate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC)
	run, err := store.StartMaintenanceRun(ctx, MaintenanceRunStart{
		Mode: MaintenanceApply, OwnerSenderID: "owner", LeaseOwner: "worker",
		LeaseDuration: time.Minute, EmbeddingModel: "test", Dimensions: 2, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertMaintenanceCandidate(ctx, MaintenanceCandidate{
		RunID: run.ID, Kind: MemoryDaily, Content: "Owner visited a garden.",
		ContentHash: MemoryContentHash("Owner visited a garden."), OriginClass: MemoryOriginOwner,
		EvidenceHistoryIDs: []int64{1}, ObservedAt: now, RecurrenceCount: 1,
		DistinctDayCount: 1, TrustScore: 1, RecencyScore: 1, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	err = store.CompleteMaintenanceRun(ctx, MaintenanceRunCompletion{
		RunID: run.ID, LeaseOwner: "worker", ProcessedHistoryID: 2,
		AdvanceCheckpoint: true, Now: now.Add(time.Second),
	})
	if err == nil {
		t.Fatal("run with unresolved candidate completed")
	}
	status, latest, statusErr := store.MaintenanceStatus(ctx)
	if statusErr != nil || status.CheckpointHistoryID != 0 || status.LeaseOwner != "worker" || latest == nil || latest.Status != "active" {
		t.Fatalf("status=%#v latest=%#v err=%v", status, latest, statusErr)
	}
}

func TestMaintenanceMemoryMutationAndCandidateResolutionRollbackTogether(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC)
	run, err := store.StartMaintenanceRun(ctx, MaintenanceRunStart{
		Mode: MaintenanceApply, OwnerSenderID: "owner", LeaseOwner: "worker",
		LeaseDuration: time.Minute, EmbeddingModel: "test", Dimensions: 2, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := store.InsertMaintenanceCandidate(ctx, MaintenanceCandidate{
		RunID: run.ID, Kind: MemoryDaily, Content: "Owner visited a garden.",
		ContentHash: MemoryContentHash("Owner visited a garden."), OriginClass: MemoryOriginOwner,
		EvidenceHistoryIDs: []int64{1}, ObservedAt: now, RecurrenceCount: 1,
		DistinctDayCount: 1, TrustScore: 1, RecencyScore: 1, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`
		CREATE TRIGGER force_candidate_resolution_failure
		BEFORE UPDATE OF status ON memory_candidates
		WHEN OLD.id=` + fmt.Sprint(candidate.ID) + `
		BEGIN SELECT RAISE(ABORT, 'forced candidate resolution failure'); END`); err != nil {
		t.Fatal(err)
	}
	err = store.WithTx(ctx, func(tx *Tx) error {
		memory, _, err := tx.StoreMemory(ctx, MemoryWrite{
			Kind: MemoryDaily, Content: candidate.Content, OriginClass: MemoryOriginOwner,
			SourceKind: MemorySourceMaintenance, EmbeddingModel: "test", Dimensions: 2,
			Embedding: vector.Pack([]float32{1, 0}), ObservedAt: now, Now: now,
		})
		if err != nil {
			return err
		}
		return tx.ResolveMaintenanceCandidate(ctx, candidate.ID, "add", "accepted", "forced boundary", &memory.ID, now)
	})
	if err == nil {
		t.Fatal("forced candidate resolution failure did not abort transaction")
	}
	for _, table := range []string{"memories", "memory_revisions", "memory_embeddings", "memory_fts"} {
		var count int
		if err := store.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	items, err := store.ListMaintenanceCandidates(ctx, run.ID)
	if err != nil || len(items) != 1 || items[0].Status != "proposed" {
		t.Fatalf("candidate after rollback=%#v err=%v", items, err)
	}
}

func TestMaintenanceCompletionRollsBackWhenLeaseWasLost(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC)
	run, err := store.StartMaintenanceRun(ctx, MaintenanceRunStart{
		Mode: MaintenanceApply, OwnerSenderID: "owner", LeaseOwner: "worker",
		LeaseDuration: time.Minute, EmbeddingModel: "test", Dimensions: 2, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE memory_maintenance_state SET lease_owner='replacement' WHERE singleton_id=1`); err != nil {
		t.Fatal(err)
	}
	err = store.CompleteMaintenanceRun(ctx, MaintenanceRunCompletion{
		RunID: run.ID, LeaseOwner: "worker", ProcessedHistoryID: 2,
		AdvanceCheckpoint: true, Now: now.Add(time.Second),
	})
	if err == nil {
		t.Fatal("run completed after losing its lease")
	}
	status, latest, statusErr := store.MaintenanceStatus(ctx)
	if statusErr != nil || status.CheckpointHistoryID != 0 || latest == nil || latest.Status != "active" {
		t.Fatalf("status=%#v latest=%#v err=%v", status, latest, statusErr)
	}
}
