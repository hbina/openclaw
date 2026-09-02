package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/openclaw/openclaw/go/internal/vector"
)

type MemoryKind string

const (
	MemoryProfile MemoryKind = "profile"
	MemoryDurable MemoryKind = "durable"
	MemoryDaily   MemoryKind = "daily"
)

type MemoryStatus string

const (
	MemoryActive  MemoryStatus = "active"
	MemoryDeleted MemoryStatus = "deleted"
)

type MemoryOrigin string

const (
	MemoryOriginOwner     MemoryOrigin = "owner"
	MemoryOriginAgent     MemoryOrigin = "agent"
	MemoryOriginSystem    MemoryOrigin = "system"
	MemoryOriginUntrusted MemoryOrigin = "untrusted"
)

type MemorySource string

const (
	MemorySourceChat        MemorySource = "chat"
	MemorySourceOperator    MemorySource = "operator"
	MemorySourceMaintenance MemorySource = "maintenance"
)

type Memory struct {
	ID              int64        `json:"id"`
	Kind            MemoryKind   `json:"kind"`
	Status          MemoryStatus `json:"status"`
	RevisionID      int64        `json:"revision_id"`
	RevisionNumber  int          `json:"revision_number"`
	Content         string       `json:"content"`
	ContentHash     string       `json:"content_hash"`
	OriginClass     MemoryOrigin `json:"origin_class"`
	SourceKind      MemorySource `json:"source_kind"`
	SourceHistoryID *int64       `json:"source_history_id,omitempty"`
	SourceTraceID   *int64       `json:"source_trace_id,omitempty"`
	ObservedAt      time.Time    `json:"observed_at"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
	DeletedAt       *time.Time   `json:"deleted_at,omitempty"`
}

type MemoryWrite struct {
	Kind            MemoryKind
	Content         string
	OriginClass     MemoryOrigin
	SourceKind      MemorySource
	SourceHistoryID *int64
	SourceTraceID   *int64
	EmbeddingModel  string
	Dimensions      int
	Embedding       []byte
	ObservedAt      time.Time
	Now             time.Time
}

type MemoryFilter struct {
	Kind   *MemoryKind
	Status MemoryStatus
	Limit  int
}

type MemorySearchResult struct {
	Memory
	VectorScore   float64 `json:"vector_score"`
	KeywordScore  float64 `json:"keyword_score"`
	CombinedScore float64 `json:"combined_score"`
	Snippet       string  `json:"snippet"`
}

type MemoryReindexEntry struct {
	MemoryID       int64
	RevisionID     int64
	Content        string
	EmbeddingModel string
	Dimensions     int
	Embedding      []byte
}

type ConversationReindexEntry struct {
	StartHistoryID int64
	EndHistoryID   int64
	Chunks         []ConversationChunk
}

func MemoryContentHash(content string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(content)))
	return hex.EncodeToString(sum[:])
}

func ValidMemoryKind(kind MemoryKind) bool {
	return kind == MemoryProfile || kind == MemoryDurable || kind == MemoryDaily
}

func (tx *Tx) StoreMemory(ctx context.Context, input MemoryWrite) (Memory, bool, error) {
	hash := MemoryContentHash(input.Content)
	existing, err := getMemoryByHash(ctx, tx.tx, hash)
	if err == nil {
		return existing, false, nil
	}
	if err != sql.ErrNoRows {
		return Memory{}, false, err
	}
	now := input.Now.UTC()
	observedAt := input.ObservedAt.UTC()
	if input.ObservedAt.IsZero() {
		observedAt = now
	}
	result, err := tx.tx.ExecContext(ctx, `
		INSERT INTO memories (kind, status, current_content_hash, observed_at, created_at, updated_at)
		VALUES (?, 'active', ?, ?, ?, ?)`, input.Kind, hash, observedAt, now, now)
	if err != nil {
		if existing, lookupErr := getMemoryByHash(ctx, tx.tx, hash); lookupErr == nil {
			return existing, false, nil
		}
		return Memory{}, false, fmt.Errorf("insert memory: %w", err)
	}
	memoryID, err := result.LastInsertId()
	if err != nil {
		return Memory{}, false, fmt.Errorf("read memory id: %w", err)
	}
	revisionID, err := insertMemoryRevision(ctx, tx.tx, memoryID, 1, hash, input, now)
	if err != nil {
		return Memory{}, false, err
	}
	if err := replaceMemoryIndexes(ctx, tx.tx, memoryID, revisionID, input, now); err != nil {
		return Memory{}, false, err
	}
	if _, err := tx.tx.ExecContext(ctx, `UPDATE memories SET current_revision_id=? WHERE id=?`, revisionID, memoryID); err != nil {
		return Memory{}, false, fmt.Errorf("link current memory revision: %w", err)
	}
	memory, err := getMemory(ctx, tx.tx, memoryID)
	return memory, true, err
}

func (tx *Tx) UpdateMemory(ctx context.Context, id int64, input MemoryWrite) (Memory, error) {
	current, err := getMemory(ctx, tx.tx, id)
	if err != nil {
		return Memory{}, err
	}
	if current.Status != MemoryActive {
		return Memory{}, fmt.Errorf("memory %d is deleted", id)
	}
	hash := MemoryContentHash(input.Content)
	if duplicate, lookupErr := getMemoryByHash(ctx, tx.tx, hash); lookupErr == nil && duplicate.ID != id {
		return Memory{}, fmt.Errorf("content is already stored as Memory ID %d", duplicate.ID)
	} else if lookupErr != nil && lookupErr != sql.ErrNoRows {
		return Memory{}, lookupErr
	}
	if input.Kind == "" {
		input.Kind = current.Kind
	}
	now := input.Now.UTC()
	observedAt := input.ObservedAt.UTC()
	if input.ObservedAt.IsZero() {
		observedAt = now
	}
	revisionID, err := insertMemoryRevision(ctx, tx.tx, id, current.RevisionNumber+1, hash, input, now)
	if err != nil {
		return Memory{}, err
	}
	if err := replaceMemoryIndexes(ctx, tx.tx, id, revisionID, input, now); err != nil {
		return Memory{}, err
	}
	if _, err := tx.tx.ExecContext(ctx, `
		UPDATE memories SET kind=?, current_revision_id=?, current_content_hash=?, observed_at=?, updated_at=?
		WHERE id=? AND status='active'`, input.Kind, revisionID, hash, observedAt, now, id); err != nil {
		return Memory{}, fmt.Errorf("update current memory revision: %w", err)
	}
	return getMemory(ctx, tx.tx, id)
}

func (tx *Tx) RemoveMemory(ctx context.Context, id int64, now time.Time) (Memory, error) {
	memory, err := getMemory(ctx, tx.tx, id)
	if err != nil {
		return Memory{}, err
	}
	if memory.Status != MemoryActive {
		return Memory{}, fmt.Errorf("memory %d is already deleted", id)
	}
	if _, err := tx.tx.ExecContext(ctx, `
		UPDATE memories SET status='deleted', current_content_hash=NULL, updated_at=?, deleted_at=? WHERE id=?`,
		now.UTC(), now.UTC(), id); err != nil {
		return Memory{}, fmt.Errorf("delete memory: %w", err)
	}
	if _, err := tx.tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE memory_id=?`, id); err != nil {
		return Memory{}, fmt.Errorf("remove memory FTS row: %w", err)
	}
	if _, err := tx.tx.ExecContext(ctx, `DELETE FROM memory_embeddings WHERE memory_id=?`, id); err != nil {
		return Memory{}, fmt.Errorf("remove memory vector: %w", err)
	}
	return getMemory(ctx, tx.tx, id)
}

func (s *Store) GetMemory(ctx context.Context, id int64) (Memory, error) {
	return getMemory(ctx, s.db, id)
}
func (tx *Tx) GetMemory(ctx context.Context, id int64) (Memory, error) {
	return getMemory(ctx, tx.tx, id)
}
func (s *Store) ListMemories(ctx context.Context, filter MemoryFilter) ([]Memory, error) {
	return listMemories(ctx, s.db, filter)
}
func (tx *Tx) ListMemories(ctx context.Context, filter MemoryFilter) ([]Memory, error) {
	return listMemories(ctx, tx.tx, filter)
}

func (s *Store) ListAllActiveMemories(ctx context.Context) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx, memorySelect+` WHERE m.status='active' ORDER BY m.updated_at DESC,m.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list all active memories: %w", err)
	}
	defer rows.Close()
	var memories []Memory
	for rows.Next() {
		memory, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		memories = append(memories, memory)
	}
	return memories, rows.Err()
}

func (s *Store) MemoryCounts(ctx context.Context) (active, deleted int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(sum(CASE WHEN status='active' THEN 1 ELSE 0 END),0),COALESCE(sum(CASE WHEN status='deleted' THEN 1 ELSE 0 END),0) FROM memories`).Scan(&active, &deleted)
	if err != nil {
		return 0, 0, fmt.Errorf("count memories: %w", err)
	}
	return active, deleted, nil
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const memorySelect = `
	SELECT m.id, m.kind, m.status, r.id, r.revision_number, r.content, r.content_hash,
	       r.origin_class, r.source_kind, r.source_history_id, r.source_trace_id,
	       m.observed_at, m.created_at, m.updated_at, m.deleted_at
	FROM memories m JOIN memory_revisions r ON r.id=m.current_revision_id`

func getMemory(ctx context.Context, q queryer, id int64) (Memory, error) {
	memory, err := scanMemory(q.QueryRowContext(ctx, memorySelect+` WHERE m.id=?`, id))
	if err == sql.ErrNoRows {
		return Memory{}, fmt.Errorf("memory %d not found", id)
	}
	return memory, err
}

func getMemoryByHash(ctx context.Context, q queryer, hash string) (Memory, error) {
	return scanMemory(q.QueryRowContext(ctx, memorySelect+` WHERE m.status='active' AND m.current_content_hash=?`, hash))
}

type rowScanner interface{ Scan(...any) error }

func scanMemory(row rowScanner) (Memory, error) {
	var memory Memory
	var sourceHistory, sourceTrace sql.NullInt64
	var deleted sql.NullTime
	if err := row.Scan(&memory.ID, &memory.Kind, &memory.Status, &memory.RevisionID, &memory.RevisionNumber,
		&memory.Content, &memory.ContentHash, &memory.OriginClass, &memory.SourceKind, &sourceHistory,
		&sourceTrace, &memory.ObservedAt, &memory.CreatedAt, &memory.UpdatedAt, &deleted); err != nil {
		return Memory{}, err
	}
	if sourceHistory.Valid {
		memory.SourceHistoryID = &sourceHistory.Int64
	}
	if sourceTrace.Valid {
		memory.SourceTraceID = &sourceTrace.Int64
	}
	if deleted.Valid {
		memory.DeletedAt = &deleted.Time
	}
	return memory, nil
}

func listMemories(ctx context.Context, q queryer, filter MemoryFilter) ([]Memory, error) {
	status := filter.Status
	if status == "" {
		status = MemoryActive
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	where := []string{}
	args := []any{}
	if status != "all" {
		where = append(where, "m.status=?")
		args = append(args, status)
	}
	if filter.Kind != nil {
		where = append(where, "m.kind=?")
		args = append(args, *filter.Kind)
	}
	query := memorySelect
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY m.updated_at DESC, m.id ASC LIMIT ?"
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list memories: %w", err)
	}
	defer rows.Close()
	var memories []Memory
	for rows.Next() {
		memory, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}
		memories = append(memories, memory)
	}
	return memories, rows.Err()
}

func (tx *Tx) SearchMemories(ctx context.Context, embeddingModel string, dimensions int, queryVector []float32, ftsQuery string, minVector float64, limit int) ([]MemorySearchResult, error) {
	return searchMemories(ctx, tx.tx, embeddingModel, dimensions, queryVector, ftsQuery, minVector, limit)
}
func (s *Store) SearchMemories(ctx context.Context, embeddingModel string, dimensions int, queryVector []float32, ftsQuery string, minVector float64, limit int) ([]MemorySearchResult, error) {
	return searchMemories(ctx, s.db, embeddingModel, dimensions, queryVector, ftsQuery, minVector, limit)
}

func searchMemories(ctx context.Context, q queryer, embeddingModel string, dimensions int, queryVector []float32, ftsQuery string, minVector float64, limit int) ([]MemorySearchResult, error) {
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	candidates := map[int64]*MemorySearchResult{}
	rows, err := q.QueryContext(ctx, memorySelect+`
		JOIN memory_embeddings e ON e.memory_id=m.id AND e.revision_id=r.id
		WHERE m.status='active' AND e.embedding_model=? AND e.dimensions=?`, embeddingModel, dimensions)
	if err != nil {
		return nil, fmt.Errorf("load memory vectors: %w", err)
	}
	for rows.Next() {
		memory, err := scanMemory(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		var packed []byte
		if err := q.QueryRowContext(ctx, `SELECT embedding FROM memory_embeddings WHERE memory_id=? AND revision_id=? AND embedding_model=?`, memory.ID, memory.RevisionID, embeddingModel).Scan(&packed); err != nil {
			_ = rows.Close()
			return nil, err
		}
		stored, err := vector.Unpack(packed, dimensions)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("decode memory %d embedding: %w", memory.ID, err)
		}
		score := vector.Dot(queryVector, stored)
		if score >= minVector {
			copy := MemorySearchResult{Memory: memory, VectorScore: score, Snippet: memory.Content}
			candidates[memory.ID] = &copy
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(ftsQuery) != "" {
		ftsRows, err := q.QueryContext(ctx, `
			SELECT memory_id, revision_id, snippet(memory_fts, 0, '[', ']', ' … ', 24), bm25(memory_fts)
			FROM memory_fts WHERE memory_fts MATCH ? ORDER BY bm25(memory_fts) LIMIT 24`, ftsQuery)
		if err != nil {
			return nil, fmt.Errorf("search memory FTS: %w", err)
		}
		ordinal := 0
		for ftsRows.Next() {
			var memoryID, revisionID int64
			var snippet string
			var rank float64
			if err := ftsRows.Scan(&memoryID, &revisionID, &snippet, &rank); err != nil {
				_ = ftsRows.Close()
				return nil, err
			}
			candidate := candidates[memoryID]
			if candidate == nil {
				memory, err := getMemory(ctx, q, memoryID)
				if err != nil || memory.Status != MemoryActive || memory.RevisionID != revisionID {
					continue
				}
				candidate = &MemorySearchResult{Memory: memory}
				candidates[memoryID] = candidate
			}
			_ = rank // raw BM25 establishes ordering; rank position normalizes its scale.
			candidate.KeywordScore = 1 / float64(ordinal+1)
			ordinal++
			candidate.Snippet = snippet
		}
		if err := ftsRows.Close(); err != nil {
			return nil, err
		}
	}
	results := make([]MemorySearchResult, 0, len(candidates))
	for _, candidate := range candidates {
		candidate.CombinedScore = .65*candidate.VectorScore + .35*candidate.KeywordScore
		results = append(results, *candidate)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].CombinedScore == results[j].CombinedScore {
			if results[i].UpdatedAt.Equal(results[j].UpdatedAt) {
				return results[i].ID < results[j].ID
			}
			return results[i].UpdatedAt.After(results[j].UpdatedAt)
		}
		return results[i].CombinedScore > results[j].CombinedScore
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func insertMemoryRevision(ctx context.Context, tx *sql.Tx, memoryID int64, revision int, hash string, input MemoryWrite, now time.Time) (int64, error) {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO memory_revisions
		(memory_id, revision_number, content, content_hash, origin_class, source_kind, source_history_id, source_trace_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, memoryID, revision, strings.TrimSpace(input.Content), hash,
		input.OriginClass, input.SourceKind, input.SourceHistoryID, input.SourceTraceID, now)
	if err != nil {
		return 0, fmt.Errorf("insert memory revision: %w", err)
	}
	return result.LastInsertId()
}

func replaceMemoryIndexes(ctx context.Context, tx *sql.Tx, memoryID, revisionID int64, input MemoryWrite, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE memory_id=?`, memoryID); err != nil {
		return fmt.Errorf("replace memory FTS row: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_fts(content, memory_id, revision_id) VALUES (?, ?, ?)`, strings.TrimSpace(input.Content), memoryID, revisionID); err != nil {
		return fmt.Errorf("insert memory FTS row: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_embeddings WHERE memory_id=?`, memoryID); err != nil {
		return fmt.Errorf("replace memory vector: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_embeddings(memory_id, revision_id, embedding_model, dimensions, embedding, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, memoryID, revisionID, input.EmbeddingModel, input.Dimensions, input.Embedding, now); err != nil {
		return fmt.Errorf("insert memory vector: %w", err)
	}
	return nil
}

func (s *Store) ValidateDerivedMemoryState(ctx context.Context, embeddingModel string, dimensions int) error {
	var missing int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM memories m
		WHERE m.status='active' AND (
			m.current_revision_id IS NULL OR
			(SELECT count(*) FROM memory_embeddings e WHERE e.memory_id=m.id AND e.revision_id=m.current_revision_id AND e.embedding_model=? AND e.dimensions=?) <> 1 OR
			(SELECT count(*) FROM memory_fts f WHERE f.memory_id=m.id AND f.revision_id=m.current_revision_id) <> 1
		)`, embeddingModel, dimensions).Scan(&missing); err != nil {
		return fmt.Errorf("validate active memory indexes: %w", err)
	}
	if missing != 0 {
		return fmt.Errorf("%d active memories have incomplete derived indexes; run memory reindex", missing)
	}
	var inactiveRows int
	if err := s.db.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM memory_embeddings e JOIN memories m ON m.id=e.memory_id WHERE m.status<>'active') +
		       (SELECT count(*) FROM memory_fts f JOIN memories m ON m.id=f.memory_id WHERE m.status<>'active')`).Scan(&inactiveRows); err != nil {
		return fmt.Errorf("validate inactive memory indexes: %w", err)
	}
	if inactiveRows != 0 {
		return fmt.Errorf("deleted memories remain indexed; run memory reindex")
	}
	return nil
}

func (s *Store) ReplaceDerivedIndexes(ctx context.Context, memories []MemoryReindexEntry, conversations []ConversationReindexEntry, now time.Time) error {
	return s.WithTx(ctx, func(tx *Tx) error {
		for _, statement := range []string{`DELETE FROM memory_fts`, `DELETE FROM memory_embeddings`, `DELETE FROM conversation_chunks`} {
			if _, err := tx.tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("clear derived index: %w", err)
			}
		}
		for _, item := range memories {
			if _, err := tx.tx.ExecContext(ctx, `INSERT INTO memory_fts(content,memory_id,revision_id) VALUES (?,?,?)`, item.Content, item.MemoryID, item.RevisionID); err != nil {
				return err
			}
			if _, err := tx.tx.ExecContext(ctx, `INSERT INTO memory_embeddings(memory_id,revision_id,embedding_model,dimensions,embedding,created_at) VALUES (?,?,?,?,?,?)`, item.MemoryID, item.RevisionID, item.EmbeddingModel, item.Dimensions, item.Embedding, now.UTC()); err != nil {
				return err
			}
		}
		for _, item := range conversations {
			if err := tx.SaveConversationChunks(ctx, item.StartHistoryID, item.EndHistoryID, item.Chunks); err != nil {
				return err
			}
		}
		return nil
	})
}
