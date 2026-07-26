package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type ConversationChunkKey struct {
	StartHistoryID int
	EndHistoryID   int
	PartIndex      int
	ContentHash    string
	EmbeddingModel string
	Dimensions     int
	IndexVersion   int
}

type ConversationEmbedding struct {
	ID             int
	StartHistoryID int
	EndHistoryID   int
	Embedding      []byte
}

func (s *Store) ConversationExchangeIndexed(
	ctx context.Context,
	model string,
	version, dimensions, startHistoryID, endHistoryID int,
) (bool, error) {
	var total, embedded int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*), count(embedding)
		FROM conversation_chunks
		WHERE embedding_model = ? AND index_version = ? AND dimensions = ?
		  AND start_history_id = ? AND end_history_id = ?
	`, model, version, dimensions, startHistoryID, endHistoryID).Scan(&total, &embedded); err != nil {
		return false, fmt.Errorf("check indexed conversation exchange: %w", err)
	}
	return total > 0 && total == embedded, nil
}

func (s *Store) PrepareConversationChunk(ctx context.Context, chunk ConversationChunkKey, now time.Time) (bool, int, error) {
	var hash string
	var embedding []byte
	var attempts int
	var retryAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT content_hash, embedding, attempts, retry_at
		FROM conversation_chunks
		WHERE embedding_model = ? AND index_version = ?
		  AND start_history_id = ? AND end_history_id = ? AND part_index = ?
	`, chunk.EmbeddingModel, chunk.IndexVersion, chunk.StartHistoryID, chunk.EndHistoryID, chunk.PartIndex).
		Scan(&hash, &embedding, &attempts, &retryAt)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO conversation_chunks (
				start_history_id, end_history_id, part_index,
				content_hash, embedding_model, dimensions, index_version
			) VALUES (?, ?, ?, ?, ?, ?, ?)
		`, chunk.StartHistoryID, chunk.EndHistoryID,
			chunk.PartIndex, chunk.ContentHash, chunk.EmbeddingModel, chunk.Dimensions, chunk.IndexVersion)
		if err != nil {
			return false, 0, fmt.Errorf("insert conversation chunk: %w", err)
		}
		return true, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("load conversation chunk state: %w", err)
	}
	if hash != chunk.ContentHash {
		_, err = s.db.ExecContext(ctx, `
			UPDATE conversation_chunks
			SET content_hash = ?, dimensions = ?, embedding = NULL,
			    attempts = 0, retry_at = NULL
			WHERE embedding_model = ? AND index_version = ?
			  AND start_history_id = ? AND end_history_id = ? AND part_index = ?
		`, chunk.ContentHash, chunk.Dimensions,
			chunk.EmbeddingModel, chunk.IndexVersion, chunk.StartHistoryID, chunk.EndHistoryID, chunk.PartIndex)
		if err != nil {
			return false, 0, fmt.Errorf("reset changed conversation chunk: %w", err)
		}
		return true, 0, nil
	}
	if len(embedding) > 0 {
		return false, attempts, nil
	}
	if retryAt.Valid && retryAt.Time.After(now) {
		return false, attempts, nil
	}
	return true, attempts, nil
}

func (s *Store) SaveConversationChunkEmbedding(ctx context.Context, chunk ConversationChunkKey, embedding []byte) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE conversation_chunks
		SET embedding = ?, attempts = 0, retry_at = NULL
		WHERE embedding_model = ? AND index_version = ?
		  AND start_history_id = ? AND end_history_id = ? AND part_index = ?
		  AND content_hash = ?
	`, embedding, chunk.EmbeddingModel, chunk.IndexVersion, chunk.StartHistoryID,
		chunk.EndHistoryID, chunk.PartIndex, chunk.ContentHash)
	if err != nil {
		return fmt.Errorf("save conversation chunk embedding: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read embedding update result: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("conversation chunk changed before embedding commit")
	}
	return nil
}

func (s *Store) RecordConversationChunkFailure(
	ctx context.Context,
	chunk ConversationChunkKey,
	attempts int,
	retryAt time.Time,
) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE conversation_chunks
		SET attempts = ?, retry_at = ?
		WHERE embedding_model = ? AND index_version = ?
		  AND start_history_id = ? AND end_history_id = ? AND part_index = ?
		  AND content_hash = ?
	`, attempts, retryAt, chunk.EmbeddingModel, chunk.IndexVersion,
		chunk.StartHistoryID, chunk.EndHistoryID, chunk.PartIndex, chunk.ContentHash)
	if err != nil {
		return fmt.Errorf("record conversation chunk failure: %w", err)
	}
	return nil
}

func (s *Store) LoadConversationEmbeddings(
	ctx context.Context,
	model string,
	version, dimensions int,
) ([]ConversationEmbedding, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, start_history_id, end_history_id, embedding
		FROM conversation_chunks
		WHERE embedding_model = ? AND index_version = ? AND dimensions = ? AND embedding IS NOT NULL
		ORDER BY id ASC
	`, model, version, dimensions)
	if err != nil {
		return nil, fmt.Errorf("load conversation embeddings: %w", err)
	}
	defer rows.Close()
	var embeddings []ConversationEmbedding
	for rows.Next() {
		var item ConversationEmbedding
		if err := rows.Scan(
			&item.ID, &item.StartHistoryID, &item.EndHistoryID, &item.Embedding,
		); err != nil {
			return nil, fmt.Errorf("scan conversation embedding: %w", err)
		}
		embeddings = append(embeddings, item)
	}
	return embeddings, rows.Err()
}

func (s *Store) CountPendingConversationChunks(ctx context.Context, model string, version int) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*)
		FROM conversation_chunks
		WHERE embedding_model = ? AND index_version = ? AND embedding IS NULL
	`, model, version).Scan(&count); err != nil {
		return 0, fmt.Errorf("count pending conversation chunks: %w", err)
	}
	return count, nil
}

func (s *Store) PruneStaleConversationChunks(ctx context.Context, model string, version int) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM conversation_chunks
		WHERE embedding_model <> ? OR index_version <> ?
	`, model, version)
	if err != nil {
		return fmt.Errorf("prune stale conversation chunks: %w", err)
	}
	return nil
}
