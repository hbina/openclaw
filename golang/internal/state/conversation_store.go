package state

import (
	"context"
	"fmt"
	"time"
)

const (
	ContentText              = "text"
	ContentInboundMessage    = "inbound_message"
	ContentScheduledReminder = "scheduled_reminder"
	ContentToolCall          = "tool_call"
	ContentToolResult        = "tool_result"
)

const (
	AudienceConversation = "conversation"
	AudienceInternal     = "internal"
)

type ConversationTurn struct {
	ID          int
	ChannelID   string
	SenderID    string
	Role        string
	ContentType string
	Audience    string
	Content     string // plain text or a JSON-encoded structured payload
	CreatedAt   time.Time
}

// SaveConversationMessage is the canonical write path for conversation history.
func (s *Store) SaveConversationMessage(ctx context.Context, channelID, senderID, role, contentType, content string) error {
	_, err := s.SaveConversationMessageID(ctx, channelID, senderID, role, contentType, content)
	return err
}

func (s *Store) SaveConversationMessageAudience(ctx context.Context, channelID, senderID, role, contentType, audience, content string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conversation_history (channel_id, sender_id, role, content_type, audience, content) VALUES (?, ?, ?, ?, ?, ?)`,
		channelID, senderID, role, contentType, audience, content,
	)
	if err != nil {
		return fmt.Errorf("failed to save conversation message: %w", err)
	}
	return nil
}

// SaveConversationMessageID persists a structured turn and returns its stable
// history identifier for provenance links.
func (s *Store) SaveConversationMessageID(ctx context.Context, channelID, senderID, role, contentType, content string) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO conversation_history (channel_id, sender_id, role, content_type, content) VALUES (?, ?, ?, ?, ?)`,
		channelID, senderID, role, contentType, content,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to save conversation message: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read conversation message id: %w", err)
	}
	return id, nil
}

func (tx *Tx) SaveConversationMessage(ctx context.Context, channelID, senderID, role, contentType, content string) error {
	return tx.SaveConversationMessageAudience(ctx, channelID, senderID, role, contentType, AudienceConversation, content)
}

func (tx *Tx) SaveConversationMessageAudience(ctx context.Context, channelID, senderID, role, contentType, audience, content string) error {
	_, err := tx.tx.ExecContext(ctx,
		`INSERT INTO conversation_history (channel_id, sender_id, role, content_type, audience, content) VALUES (?, ?, ?, ?, ?, ?)`,
		channelID, senderID, role, contentType, audience, content,
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
		SELECT id, channel_id, sender_id, role, content_type, audience, content, created_at
		FROM conversation_history
		WHERE channel_id = ? AND sender_id = ? AND audience = 'conversation'
		ORDER BY id ASC
	`, channelID, senderID)
	if err != nil {
		return nil, fmt.Errorf("failed to load conversation history: %w", err)
	}
	defer rows.Close()

	var turns []ConversationTurn
	for rows.Next() {
		var t ConversationTurn
		if err := rows.Scan(&t.ID, &t.ChannelID, &t.SenderID, &t.Role, &t.ContentType, &t.Audience, &t.Content, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan conversation turn: %w", err)
		}
		turns = append(turns, t)
	}
	return turns, rows.Err()
}

func (s *Store) GetAllConversationHistory(ctx context.Context) ([]ConversationTurn, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, channel_id, sender_id, role, content_type, audience, content, created_at
		FROM conversation_history
		WHERE audience = 'conversation'
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
			&turn.ContentType, &turn.Audience, &turn.Content, &turn.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan conversation turn: %w", err)
		}
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}
