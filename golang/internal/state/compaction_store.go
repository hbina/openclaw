package state

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Compaction records a summarization of older conversation turns.
type Compaction struct {
	ID           int
	ChannelID    string
	SenderID     string
	Summary      string
	TokensBefore int
	FirstKeptID  int // all history rows with id >= FirstKeptID are retained
	CreatedAt    time.Time
}

// SaveCompaction persists a new compaction record and returns its ID.
func (s *Store) SaveCompaction(ctx context.Context, channelID, senderID, summary string, tokensBefore, firstKeptID int) (int, error) {
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO conversation_compactions (channel_id, sender_id, summary, tokens_before, first_kept_id) VALUES (?, ?, ?, ?, ?)`,
		channelID, senderID, summary, tokensBefore, firstKeptID,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to save compaction: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get compaction id: %w", err)
	}
	return int(id), nil
}

// GetLatestCompaction returns the most recent compaction for the given channel+sender,
// or nil if none exists.
func (s *Store) GetLatestCompaction(ctx context.Context, channelID, senderID string) (*Compaction, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, channel_id, sender_id, summary, tokens_before, first_kept_id, created_at
		 FROM conversation_compactions
		 WHERE channel_id = ? AND sender_id = ?
		 ORDER BY created_at DESC
		 LIMIT 1`,
		channelID, senderID,
	)

	var c Compaction
	err := row.Scan(&c.ID, &c.ChannelID, &c.SenderID, &c.Summary, &c.TokensBefore, &c.FirstKeptID, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load compaction: %w", err)
	}
	return &c, nil
}

// TrimHistoryBefore deletes conversation_history rows with id < firstKeptID
// for the given channel+sender pair.
func (s *Store) TrimHistoryBefore(ctx context.Context, channelID, senderID string, firstKeptID int) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM conversation_history WHERE channel_id = ? AND sender_id = ? AND id < ?`,
		channelID, senderID, firstKeptID,
	)
	if err != nil {
		return fmt.Errorf("failed to trim history: %w", err)
	}
	return nil
}
