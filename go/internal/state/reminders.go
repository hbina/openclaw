package state

import (
	"fmt"
	"time"
)

type Reminder struct {
	ID        int
	ChannelID string
	SenderID  string
	Message   string
	FireAt    time.Time
	CreatedAt time.Time
}

// AddReminder inserts a new reminder into the database.
func (s *Store) AddReminder(channelID, senderID, message string, fireAt time.Time) error {
	_, err := s.db.Exec(
		"INSERT INTO reminders (channel_id, sender_id, message, fire_at) VALUES (?, ?, ?, ?)",
		channelID, senderID, message, fireAt,
	)
	if err != nil {
		return fmt.Errorf("failed to add reminder: %w", err)
	}
	return nil
}

// ListReminders returns all pending reminders for a specific user.
func (s *Store) ListReminders(channelID, senderID string) ([]Reminder, error) {
	rows, err := s.db.Query(
		"SELECT id, channel_id, sender_id, message, fire_at, created_at FROM reminders WHERE channel_id = ? AND sender_id = ? ORDER BY fire_at ASC",
		channelID, senderID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list reminders: %w", err)
	}
	defer rows.Close()

	var reminders []Reminder
	for rows.Next() {
		var r Reminder
		if err := rows.Scan(&r.ID, &r.ChannelID, &r.SenderID, &r.Message, &r.FireAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		reminders = append(reminders, r)
	}
	return reminders, nil
}

// FetchDueReminders returns all reminders that are due to fire.
func (s *Store) FetchDueReminders() ([]Reminder, error) {
	now := time.Now()
	rows, err := s.db.Query(
		"SELECT id, channel_id, sender_id, message, fire_at, created_at FROM reminders WHERE fire_at <= ?",
		now,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var due []Reminder
	for rows.Next() {
		var r Reminder
		if err := rows.Scan(&r.ID, &r.ChannelID, &r.SenderID, &r.Message, &r.FireAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		due = append(due, r)
	}
	return due, rows.Err()
}

// DeleteReminder removes a reminder after successful delivery.
func (s *Store) DeleteReminder(id int) error {
	result, err := s.db.Exec("DELETE FROM reminders WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to delete reminder: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read deleted reminder count: %w", err)
	}
	if deleted == 0 {
		return fmt.Errorf("reminder %d not found", id)
	}
	return nil
}
