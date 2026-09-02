package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrMaintenanceBusy = errors.New("memory maintenance is already running")

const MaintenanceIndexVersion = 1

type MaintenanceMode string

const (
	MaintenancePreview   MaintenanceMode = "preview"
	MaintenanceApply     MaintenanceMode = "apply"
	MaintenanceScheduled MaintenanceMode = "scheduled"
)

type MaintenanceState struct {
	CheckpointHistoryID int64
	LeaseOwner          string
	LeaseExpiresAt      *time.Time
	NextRunAt           *time.Time
	LastSuccessAt       *time.Time
	UpdatedAt           time.Time
}

type MaintenanceRun struct {
	ID                  int64
	Mode                MaintenanceMode
	Status              string
	Stage               string
	OwnerSenderID       string
	CheckpointHistoryID int64
	HighwaterHistoryID  int64
	ProcessedHistoryID  int64
	CandidateCount      int
	PromotedCount       int
	RejectedCount       int
	EmbeddingModel      string
	Dimensions          int
	IndexVersion        int
	Error               string
	StartedAt           time.Time
	CompletedAt         *time.Time
}

type MaintenanceExchange struct {
	StartHistoryID int64
	EndHistoryID   int64
	ContentType    string
	Content        string
	ObservedAt     time.Time
}

type MaintenanceCandidate struct {
	ID                 int64
	RunID              int64
	Kind               MemoryKind
	Content            string
	ContentHash        string
	OriginClass        MemoryOrigin
	EvidenceHistoryIDs []int64
	ObservedAt         time.Time
	RecurrenceCount    int
	DistinctDayCount   int
	TrustScore         float64
	RecencyScore       float64
	NoveltyScore       float64
	ContradictionScore float64
	ProposedAction     string
	TargetMemoryID     *int64
	Status             string
	DecisionReason     string
	CreatedAt          time.Time
	ResolvedAt         *time.Time
}

type MaintenanceRunStart struct {
	Mode           MaintenanceMode
	OwnerSenderID  string
	LeaseOwner     string
	LeaseDuration  time.Duration
	EmbeddingModel string
	Dimensions     int
	Now            time.Time
}

func (s *Store) StartMaintenanceRun(ctx context.Context, input MaintenanceRunStart) (MaintenanceRun, error) {
	if input.Mode != MaintenancePreview && input.Mode != MaintenanceApply && input.Mode != MaintenanceScheduled {
		return MaintenanceRun{}, fmt.Errorf("invalid maintenance mode %q", input.Mode)
	}
	if input.OwnerSenderID == "" || input.LeaseOwner == "" || input.LeaseDuration <= 0 || input.EmbeddingModel == "" || input.Dimensions <= 0 {
		return MaintenanceRun{}, fmt.Errorf("complete maintenance run identity and index contract are required")
	}
	now := input.Now.UTC()
	var run MaintenanceRun
	err := s.WithTx(ctx, func(tx *Tx) error {
		var (
			checkpoint int64
			leaseOwner string
			leaseUntil sql.NullTime
		)
		if err := tx.tx.QueryRowContext(ctx, `
			SELECT checkpoint_history_id, lease_owner, lease_expires_at
			FROM memory_maintenance_state WHERE singleton_id=1`).Scan(&checkpoint, &leaseOwner, &leaseUntil); err != nil {
			return fmt.Errorf("load maintenance lease: %w", err)
		}
		if leaseOwner != "" && leaseUntil.Valid && leaseUntil.Time.After(now) {
			return ErrMaintenanceBusy
		}
		if leaseOwner != "" {
			if _, err := tx.tx.ExecContext(ctx, `
				UPDATE memory_maintenance_runs
				SET status='failed', stage='lease', error='maintenance lease expired before completion', completed_at=?
				WHERE status='active'`, now); err != nil {
				return fmt.Errorf("retire expired maintenance run: %w", err)
			}
		}
		var highwater int64
		if err := tx.tx.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(u.id), 0)
			FROM conversation_history u
			WHERE u.channel_id='telegram' AND u.sender_id=? AND u.audience='conversation'
			  AND u.role='user' AND u.content_type IN ('text','inbound_message')
			  AND EXISTS (
				SELECT 1 FROM conversation_history a
				WHERE a.channel_id=u.channel_id AND a.sender_id=u.sender_id
				  AND a.audience='conversation' AND a.role='assistant' AND a.content_type='text'
				  AND a.id>u.id
				  AND NOT EXISTS (
					SELECT 1 FROM conversation_history n
					WHERE n.channel_id=u.channel_id AND n.sender_id=u.sender_id
					  AND n.audience='conversation' AND n.role='user' AND n.id>u.id AND n.id<a.id
				  )
			  )`, input.OwnerSenderID).Scan(&highwater); err != nil {
			return fmt.Errorf("find maintenance highwater: %w", err)
		}
		expires := now.Add(input.LeaseDuration)
		result, err := tx.tx.ExecContext(ctx, `
			UPDATE memory_maintenance_state
			SET lease_owner=?, lease_expires_at=?, updated_at=?
			WHERE singleton_id=1 AND (lease_owner='' OR lease_expires_at IS NULL OR lease_expires_at<=?)`,
			input.LeaseOwner, expires, now, now)
		if err != nil {
			return fmt.Errorf("claim maintenance lease: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return ErrMaintenanceBusy
		}
		result, err = tx.tx.ExecContext(ctx, `
			INSERT INTO memory_maintenance_runs
			(mode,status,stage,owner_sender_id,checkpoint_history_id,highwater_history_id,
			 processed_history_id,embedding_model,dimensions,index_version,started_at)
			VALUES (?, 'active', 'select', ?, ?, ?, ?, ?, ?, ?, ?)`,
			input.Mode, input.OwnerSenderID, checkpoint, highwater, checkpoint,
			input.EmbeddingModel, input.Dimensions, MaintenanceIndexVersion, now)
		if err != nil {
			return fmt.Errorf("start maintenance run: %w", err)
		}
		runID, err := result.LastInsertId()
		if err != nil {
			return err
		}
		run = MaintenanceRun{
			ID: runID, Mode: input.Mode, Status: "active", Stage: "select",
			OwnerSenderID: input.OwnerSenderID, CheckpointHistoryID: checkpoint,
			HighwaterHistoryID: highwater, ProcessedHistoryID: checkpoint,
			EmbeddingModel: input.EmbeddingModel, Dimensions: input.Dimensions,
			IndexVersion: MaintenanceIndexVersion, StartedAt: now,
		}
		return nil
	})
	return run, err
}

func (s *Store) RefreshMaintenanceLease(ctx context.Context, leaseOwner string, leaseDuration time.Duration, stage string, runID int64, now time.Time) error {
	if leaseOwner == "" || leaseDuration <= 0 {
		return fmt.Errorf("maintenance lease identity is required")
	}
	now = now.UTC()
	return s.WithTx(ctx, func(tx *Tx) error {
		result, err := tx.tx.ExecContext(ctx, `
			UPDATE memory_maintenance_state SET lease_expires_at=?, updated_at=?
			WHERE singleton_id=1 AND lease_owner=?`, now.Add(leaseDuration), now, leaseOwner)
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return fmt.Errorf("memory maintenance lease was lost")
		}
		if _, err := tx.tx.ExecContext(ctx, `UPDATE memory_maintenance_runs SET stage=? WHERE id=? AND status='active'`, stage, runID); err != nil {
			return err
		}
		return nil
	})
}

func (s *Store) LoadMaintenanceExchanges(ctx context.Context, ownerSenderID string, afterID, highwaterID int64, limit int) ([]MaintenanceExchange, error) {
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("maintenance batch limit must be between 1 and 100")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id,
		       (SELECT MAX(a.id) FROM conversation_history a
		        WHERE a.channel_id=u.channel_id AND a.sender_id=u.sender_id
		          AND a.audience='conversation' AND a.role='assistant' AND a.content_type='text'
		          AND a.id>u.id
		          AND NOT EXISTS (
				SELECT 1 FROM conversation_history n
				WHERE n.channel_id=u.channel_id AND n.sender_id=u.sender_id
				  AND n.audience='conversation' AND n.role='user' AND n.id>u.id AND n.id<a.id
		          )) AS end_id,
		       u.content_type, u.content, u.created_at
		FROM conversation_history u
		WHERE u.channel_id='telegram' AND u.sender_id=? AND u.audience='conversation'
		  AND u.role='user' AND u.content_type IN ('text','inbound_message')
		  AND u.id>? AND u.id<=?
		  AND EXISTS (
			SELECT 1 FROM conversation_history a
			WHERE a.channel_id=u.channel_id AND a.sender_id=u.sender_id
			  AND a.audience='conversation' AND a.role='assistant' AND a.content_type='text'
			  AND a.id>u.id
			  AND NOT EXISTS (
				SELECT 1 FROM conversation_history n
				WHERE n.channel_id=u.channel_id AND n.sender_id=u.sender_id
				  AND n.audience='conversation' AND n.role='user' AND n.id>u.id AND n.id<a.id
			  )
		  )
		ORDER BY u.id ASC LIMIT ?`, ownerSenderID, afterID, highwaterID, limit)
	if err != nil {
		return nil, fmt.Errorf("load maintenance exchanges: %w", err)
	}
	defer rows.Close()
	var exchanges []MaintenanceExchange
	for rows.Next() {
		var item MaintenanceExchange
		if err := rows.Scan(&item.StartHistoryID, &item.EndHistoryID, &item.ContentType, &item.Content, &item.ObservedAt); err != nil {
			return nil, fmt.Errorf("scan maintenance exchange: %w", err)
		}
		exchanges = append(exchanges, item)
	}
	return exchanges, rows.Err()
}

func (s *Store) InsertMaintenanceCandidate(ctx context.Context, candidate MaintenanceCandidate) (MaintenanceCandidate, error) {
	evidence, err := json.Marshal(candidate.EvidenceHistoryIDs)
	if err != nil {
		return MaintenanceCandidate{}, err
	}
	now := candidate.CreatedAt.UTC()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_candidates
		(run_id,kind,content,content_hash,origin_class,evidence_history_ids,observed_at,
		 recurrence_count,distinct_day_count,trust_score,recency_score,novelty_score,
		 contradiction_score,proposed_action,status,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,'pending','proposed',?)`,
		candidate.RunID, candidate.Kind, candidate.Content, candidate.ContentHash,
		candidate.OriginClass, string(evidence), candidate.ObservedAt.UTC(),
		candidate.RecurrenceCount, candidate.DistinctDayCount, candidate.TrustScore,
		candidate.RecencyScore, candidate.NoveltyScore, candidate.ContradictionScore, now)
	if err != nil {
		return MaintenanceCandidate{}, fmt.Errorf("insert maintenance candidate: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return MaintenanceCandidate{}, err
	}
	candidate.ID = id
	candidate.ProposedAction = "pending"
	candidate.Status = "proposed"
	candidate.CreatedAt = now
	return candidate, nil
}

func (s *Store) ScoreMaintenanceCandidate(ctx context.Context, id int64, novelty, contradiction float64) error {
	if novelty < 0 || novelty > 1 || contradiction < 0 || contradiction > 1 {
		return fmt.Errorf("maintenance candidate scores must be between 0 and 1")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE memory_candidates SET novelty_score=?, contradiction_score=?
		WHERE id=? AND status='proposed'`, novelty, contradiction, id)
	if err != nil {
		return fmt.Errorf("score maintenance candidate: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return fmt.Errorf("maintenance candidate %d is not proposed", id)
	}
	return nil
}

func (tx *Tx) ResolveMaintenanceCandidate(ctx context.Context, id int64, action, status, reason string, targetMemoryID *int64, now time.Time) error {
	if action != "noop" && action != "add" && action != "update" && action != "review" && action != "reject" {
		return fmt.Errorf("invalid maintenance candidate action %q", action)
	}
	if status != "accepted" && status != "rejected" && status != "failed" {
		return fmt.Errorf("invalid maintenance candidate status %q", status)
	}
	result, err := tx.tx.ExecContext(ctx, `
		UPDATE memory_candidates
		SET proposed_action=?, target_memory_id=?, status=?, decision_reason=?, resolved_at=?
		WHERE id=? AND status='proposed'`, action, targetMemoryID, status, reason, now.UTC(), id)
	if err != nil {
		return fmt.Errorf("resolve maintenance candidate: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return fmt.Errorf("maintenance candidate %d is not proposed", id)
	}
	return nil
}

type MaintenanceRunCompletion struct {
	RunID              int64
	LeaseOwner         string
	ProcessedHistoryID int64
	CandidateCount     int
	PromotedCount      int
	RejectedCount      int
	AdvanceCheckpoint  bool
	NextRunAt          *time.Time
	Now                time.Time
}

func (s *Store) CompleteMaintenanceRun(ctx context.Context, completion MaintenanceRunCompletion) error {
	now := completion.Now.UTC()
	return s.WithTx(ctx, func(tx *Tx) error {
		var unresolved int
		if err := tx.tx.QueryRowContext(ctx, `
			SELECT count(*) FROM memory_candidates
			WHERE run_id=? AND status NOT IN ('accepted','rejected')`, completion.RunID).Scan(&unresolved); err != nil {
			return err
		}
		if unresolved != 0 {
			return fmt.Errorf("maintenance run %d has %d non-terminal candidates", completion.RunID, unresolved)
		}
		result, err := tx.tx.ExecContext(ctx, `
			UPDATE memory_maintenance_runs
			SET status='completed', stage='complete', processed_history_id=?, candidate_count=?,
			    promoted_count=?, rejected_count=?, completed_at=?
			WHERE id=? AND status='active'`, completion.ProcessedHistoryID, completion.CandidateCount,
			completion.PromotedCount, completion.RejectedCount, now, completion.RunID)
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return fmt.Errorf("maintenance run %d is not active", completion.RunID)
		}
		if completion.AdvanceCheckpoint {
			result, err = tx.tx.ExecContext(ctx, `
				UPDATE memory_maintenance_state
				SET checkpoint_history_id=MAX(checkpoint_history_id, ?), lease_owner='', lease_expires_at=NULL,
				    next_run_at=COALESCE(?, next_run_at), last_success_at=?, updated_at=?
				WHERE singleton_id=1 AND lease_owner=?`, completion.ProcessedHistoryID,
				completion.NextRunAt, now, now, completion.LeaseOwner)
		} else {
			result, err = tx.tx.ExecContext(ctx, `
				UPDATE memory_maintenance_state
				SET lease_owner='', lease_expires_at=NULL, next_run_at=COALESCE(?, next_run_at), updated_at=?
				WHERE singleton_id=1 AND lease_owner=?`, completion.NextRunAt, now, completion.LeaseOwner)
		}
		if err != nil {
			return err
		}
		changed, _ = result.RowsAffected()
		if changed != 1 {
			return fmt.Errorf("memory maintenance lease was lost before completion")
		}
		return nil
	})
}

func (s *Store) FailMaintenanceRun(ctx context.Context, runID int64, leaseOwner, stage, message string, cancelled bool, nextRunAt *time.Time, now time.Time) error {
	status := "failed"
	if cancelled {
		status = "cancelled"
	}
	if len(message) > 1000 {
		message = message[:1000]
	}
	now = now.UTC()
	return s.WithTx(context.WithoutCancel(ctx), func(tx *Tx) error {
		if _, err := tx.tx.ExecContext(context.WithoutCancel(ctx), `
			UPDATE memory_maintenance_runs
			SET status=?, stage=?, error=?, completed_at=?,
			    candidate_count=(SELECT count(*) FROM memory_candidates WHERE run_id=?),
			    promoted_count=CASE WHEN mode='preview' THEN 0 ELSE
			      (SELECT count(*) FROM memory_candidates WHERE run_id=? AND status='accepted' AND proposed_action IN ('add','update')) END,
			    rejected_count=(SELECT count(*) FROM memory_candidates WHERE run_id=? AND status='rejected')
			WHERE id=? AND status='active'`, status, stage, message, now, runID, runID, runID, runID); err != nil {
			return err
		}
		_, err := tx.tx.ExecContext(context.WithoutCancel(ctx), `
			UPDATE memory_maintenance_state
			SET lease_owner='', lease_expires_at=NULL, next_run_at=COALESCE(?, next_run_at), updated_at=?
			WHERE singleton_id=1 AND lease_owner=?`, nextRunAt, now, leaseOwner)
		return err
	})
}

func (s *Store) MaintenanceStatus(ctx context.Context) (MaintenanceState, *MaintenanceRun, error) {
	var result MaintenanceState
	var leaseUntil, nextRun, lastSuccess sql.NullTime
	if err := s.db.QueryRowContext(ctx, `
		SELECT checkpoint_history_id,lease_owner,lease_expires_at,next_run_at,last_success_at,updated_at
		FROM memory_maintenance_state WHERE singleton_id=1`).Scan(
		&result.CheckpointHistoryID, &result.LeaseOwner, &leaseUntil, &nextRun, &lastSuccess, &result.UpdatedAt); err != nil {
		return MaintenanceState{}, nil, fmt.Errorf("load maintenance state: %w", err)
	}
	if leaseUntil.Valid {
		result.LeaseExpiresAt = &leaseUntil.Time
	}
	if nextRun.Valid {
		result.NextRunAt = &nextRun.Time
	}
	if lastSuccess.Valid {
		result.LastSuccessAt = &lastSuccess.Time
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id,mode,status,stage,owner_sender_id,checkpoint_history_id,highwater_history_id,
		       processed_history_id,candidate_count,promoted_count,rejected_count,embedding_model,
		       dimensions,index_version,error,started_at,completed_at
		FROM memory_maintenance_runs ORDER BY id DESC LIMIT 1`)
	var run MaintenanceRun
	var completed sql.NullTime
	if err := row.Scan(&run.ID, &run.Mode, &run.Status, &run.Stage, &run.OwnerSenderID,
		&run.CheckpointHistoryID, &run.HighwaterHistoryID, &run.ProcessedHistoryID,
		&run.CandidateCount, &run.PromotedCount, &run.RejectedCount, &run.EmbeddingModel,
		&run.Dimensions, &run.IndexVersion, &run.Error, &run.StartedAt, &completed); err != nil {
		if err == sql.ErrNoRows {
			return result, nil, nil
		}
		return MaintenanceState{}, nil, err
	}
	if completed.Valid {
		run.CompletedAt = &completed.Time
	}
	return result, &run, nil
}

func (s *Store) SetMaintenanceNextRun(ctx context.Context, next time.Time, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE memory_maintenance_state SET next_run_at=?, updated_at=? WHERE singleton_id=1`,
		next.UTC(), now.UTC())
	if err != nil {
		return fmt.Errorf("set next maintenance run: %w", err)
	}
	return nil
}

func (s *Store) ListMaintenanceCandidates(ctx context.Context, runID int64) ([]MaintenanceCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id,run_id,kind,content,content_hash,origin_class,evidence_history_ids,observed_at,
		       recurrence_count,distinct_day_count,trust_score,recency_score,novelty_score,
		       contradiction_score,proposed_action,target_memory_id,status,
		       decision_reason,created_at,resolved_at
		FROM memory_candidates WHERE run_id=? ORDER BY id ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("list maintenance candidates: %w", err)
	}
	defer rows.Close()
	var candidates []MaintenanceCandidate
	for rows.Next() {
		var (
			candidate MaintenanceCandidate
			evidence  string
			target    sql.NullInt64
			resolved  sql.NullTime
		)
		if err := rows.Scan(&candidate.ID, &candidate.RunID, &candidate.Kind, &candidate.Content,
			&candidate.ContentHash, &candidate.OriginClass, &evidence, &candidate.ObservedAt,
			&candidate.RecurrenceCount, &candidate.DistinctDayCount, &candidate.TrustScore,
			&candidate.RecencyScore, &candidate.NoveltyScore, &candidate.ContradictionScore, &candidate.ProposedAction,
			&target, &candidate.Status, &candidate.DecisionReason, &candidate.CreatedAt, &resolved); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(evidence), &candidate.EvidenceHistoryIDs); err != nil {
			return nil, fmt.Errorf("decode maintenance candidate %d evidence: %w", candidate.ID, err)
		}
		if target.Valid {
			candidate.TargetMemoryID = &target.Int64
		}
		if resolved.Valid {
			candidate.ResolvedAt = &resolved.Time
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}
