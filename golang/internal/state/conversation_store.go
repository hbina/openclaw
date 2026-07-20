package state

import (
	"context"
	"fmt"
)

const (
	ContentText       = "text"
	ContentToolCall   = "tool_call"
	ContentToolResult = "tool_result"
)

type ConversationTurn struct {
	ID          int
	Role        string
	ContentType string
	Content     string // plain text, or JSON-encoded payload for tool_call / tool_result
}

// SaveConversationMessage is the canonical write path for conversation history.
func (s *Store) SaveConversationMessage(ctx context.Context, channelID, senderID, role, contentType, content string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conversation_history (channel_id, sender_id, role, content_type, content) VALUES (?, ?, ?, ?, ?)`,
		channelID, senderID, role, contentType, content,
	)
	if err != nil {
		return fmt.Errorf("failed to save conversation message: %w", err)
	}
	return nil
}

func (tx *Tx) SaveConversationMessage(ctx context.Context, channelID, senderID, role, contentType, content string) error {
	_, err := tx.tx.ExecContext(ctx,
		`INSERT INTO conversation_history (channel_id, sender_id, role, content_type, content) VALUES (?, ?, ?, ?, ?)`,
		channelID, senderID, role, contentType, content,
	)
	if err != nil {
		return fmt.Errorf("failed to save conversation message: %w", err)
	}
	return nil
}

// SaveConversationTurn saves a plain-text user or assistant turn.
func (s *Store) SaveConversationTurn(ctx context.Context, channelID, senderID, role, content string) error {
	return s.SaveConversationMessage(ctx, channelID, senderID, role, ContentText, content)
}

// GetRecentHistory returns the last `limit` turns for a sender in oldest-first order,
// including only rows after firstKeptID (0 = no lower bound).
func (s *Store) GetRecentHistory(ctx context.Context, channelID, senderID string, limit, firstKeptID int) ([]ConversationTurn, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, role, content_type, content FROM (
			SELECT id, role, content_type, content
			FROM conversation_history
			WHERE channel_id = ? AND sender_id = ? AND id >= ?
			ORDER BY id DESC
			LIMIT ?
		) ORDER BY id ASC
	`, channelID, senderID, firstKeptID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to load conversation history: %w", err)
	}
	defer rows.Close()

	var turns []ConversationTurn
	for rows.Next() {
		var t ConversationTurn
		if err := rows.Scan(&t.ID, &t.Role, &t.ContentType, &t.Content); err != nil {
			return nil, fmt.Errorf("failed to scan conversation turn: %w", err)
		}
		turns = append(turns, t)
	}
	return turns, rows.Err()
}
