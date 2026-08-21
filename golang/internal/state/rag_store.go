package state

import (
	"context"
	"fmt"
)

type ConversationChunk struct {
	PartIndex      int
	ContentHash    string
	EmbeddingModel string
	Dimensions     int
	IndexVersion   int
	Embedding      []byte
}

type ConversationEmbedding struct {
	ID             int
	StartHistoryID int
	EndHistoryID   int
	Embedding      []byte
}

func (tx *Tx) SaveConversationChunks(ctx context.Context, startHistoryID, endHistoryID int64, chunks []ConversationChunk) error {
	if startHistoryID < 1 || endHistoryID < startHistoryID || len(chunks) == 0 {
		return fmt.Errorf("complete conversation chunk range is required")
	}
	for _, chunk := range chunks {
		if len(chunk.Embedding) == 0 {
			return fmt.Errorf("conversation chunk %d has no embedding", chunk.PartIndex)
		}
		if _, err := tx.tx.ExecContext(ctx, `
			INSERT INTO conversation_chunks
			(start_history_id, end_history_id, part_index, content_hash, embedding_model, dimensions, index_version, embedding)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, startHistoryID, endHistoryID, chunk.PartIndex,
			chunk.ContentHash, chunk.EmbeddingModel, chunk.Dimensions, chunk.IndexVersion, chunk.Embedding); err != nil {
			return fmt.Errorf("insert conversation chunk: %w", err)
		}
	}
	return nil
}

func (s *Store) LoadConversationEmbeddings(ctx context.Context, model string, version, dimensions int) ([]ConversationEmbedding, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, start_history_id, end_history_id, embedding
		FROM conversation_chunks
		WHERE embedding_model=? AND index_version=? AND dimensions=?
		ORDER BY id ASC`, model, version, dimensions)
	if err != nil {
		return nil, fmt.Errorf("load conversation embeddings: %w", err)
	}
	defer rows.Close()
	var embeddings []ConversationEmbedding
	for rows.Next() {
		var item ConversationEmbedding
		if err := rows.Scan(&item.ID, &item.StartHistoryID, &item.EndHistoryID, &item.Embedding); err != nil {
			return nil, fmt.Errorf("scan conversation embedding: %w", err)
		}
		embeddings = append(embeddings, item)
	}
	return embeddings, rows.Err()
}

func (s *Store) CountConversationIndexGaps(ctx context.Context, model string, version, dimensions int) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		WITH completed AS (
			SELECT u.id AS start_id,
			       (SELECT MAX(a.id) FROM conversation_history a
			        WHERE a.channel_id=u.channel_id AND a.sender_id=u.sender_id AND a.audience='conversation'
			          AND a.id>u.id AND a.role='assistant' AND a.content_type='text'
			          AND NOT EXISTS (SELECT 1 FROM conversation_history next_u
			                          WHERE next_u.channel_id=u.channel_id AND next_u.sender_id=u.sender_id
			                            AND next_u.audience='conversation' AND next_u.role='user'
			                            AND next_u.id>u.id AND next_u.id<a.id)) AS end_id
			FROM conversation_history u
			WHERE u.audience='conversation' AND u.role='user'
		)
		SELECT count(*) FROM completed c
		WHERE c.end_id IS NOT NULL AND NOT EXISTS (
			SELECT 1 FROM conversation_chunks x WHERE x.start_history_id=c.start_id AND x.end_history_id=c.end_id
			AND x.embedding_model=? AND x.index_version=? AND x.dimensions=?)`, model, version, dimensions).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count conversation index gaps: %w", err)
	}
	return count, nil
}

func (s *Store) ReplaceConversationIndexes(ctx context.Context, entries []ConversationReindexEntry) error {
	return s.WithTx(ctx, func(tx *Tx) error {
		if _, err := tx.tx.ExecContext(ctx, `DELETE FROM conversation_chunks`); err != nil {
			return err
		}
		for _, item := range entries {
			if err := tx.SaveConversationChunks(ctx, item.StartHistoryID, item.EndHistoryID, item.Chunks); err != nil {
				return err
			}
		}
		return nil
	})
}
