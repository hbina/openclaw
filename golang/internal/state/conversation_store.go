package state

import (
	"context"
	"fmt"
	"time"
)

const (
	ContentText           = "text"
	ContentInboundMessage = "inbound_message"
	ContentToolCall       = "tool_call"
	ContentToolResult     = "tool_result"
)

type ConversationTurn struct {
	ID          int
	ChannelID   string
	SenderID    string
	Role        string
	ContentType string
	Content     string // plain text, or JSON-encoded payload for tool_call / tool_result
	CreatedAt   time.Time
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

// GetConversationHistory returns every turn for a route in oldest-first order.
func (s *Store) GetConversationHistory(ctx context.Context, channelID, senderID string) ([]ConversationTurn, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, channel_id, sender_id, role, content_type, content, created_at
		FROM conversation_history
		WHERE channel_id = ? AND sender_id = ?
		ORDER BY id ASC
	`, channelID, senderID)
	if err != nil {
		return nil, fmt.Errorf("failed to load conversation history: %w", err)
	}
	defer rows.Close()

	var turns []ConversationTurn
	for rows.Next() {
		var t ConversationTurn
		if err := rows.Scan(&t.ID, &t.ChannelID, &t.SenderID, &t.Role, &t.ContentType, &t.Content, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan conversation turn: %w", err)
		}
		turns = append(turns, t)
	}
	return turns, rows.Err()
}

func (s *Store) GetAllConversationHistory(ctx context.Context) ([]ConversationTurn, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, channel_id, sender_id, role, content_type, content, created_at
		FROM conversation_history
		ORDER BY id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to load all conversation history: %w", err)
	}
	defer rows.Close()

	var turns []ConversationTurn
	for rows.Next() {
		var turn ConversationTurn
		if err := rows.Scan(
			&turn.ID, &turn.ChannelID, &turn.SenderID, &turn.Role,
			&turn.ContentType, &turn.Content, &turn.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan conversation turn: %w", err)
		}
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}
