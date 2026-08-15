package state

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
)

type ScheduleKind string

const (
	ScheduleAt    ScheduleKind = "at"
	ScheduleEvery ScheduleKind = "every"
	ScheduleCron  ScheduleKind = "cron"
)

type ReminderSchedule struct {
	Kind     ScheduleKind
	At       time.Time
	EveryMS  int64
	AnchorAt time.Time
	CronExpr string
	Timezone string
}

type Reminder struct {
	ID        int
	ChannelID string
	SenderID  string
	Message   string
	Schedule  ReminderSchedule
	FireAt    time.Time
	Enabled   bool
}

var cronParser = cron.NewParser(
	cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

func NextReminderRun(schedule ReminderSchedule, now time.Time) (time.Time, error) {
	switch schedule.Kind {
	case ScheduleAt:
		if !schedule.At.After(now) {
			return time.Time{}, fmt.Errorf("at must be in the future")
		}
		return schedule.At, nil
	case ScheduleEvery:
		if schedule.EveryMS < 1 {
			return time.Time{}, fmt.Errorf("every_ms must be a positive integer")
		}
		if schedule.EveryMS > math.MaxInt64/int64(time.Millisecond) {
			return time.Time{}, fmt.Errorf("every_ms is too large")
		}
		interval := time.Duration(schedule.EveryMS) * time.Millisecond
		anchor := schedule.AnchorAt
		if anchor.IsZero() {
			anchor = now
		}
		if now.Before(anchor) {
			return anchor, nil
		}
		steps := now.Sub(anchor)/interval + 1
		return anchor.Add(steps * interval), nil
	case ScheduleCron:
		expr := strings.TrimSpace(schedule.CronExpr)
		if expr == "" {
			return time.Time{}, fmt.Errorf("expr is required for cron schedules")
		}
		location := time.Local
		if tz := strings.TrimSpace(schedule.Timezone); tz != "" {
			var err error
			location, err = time.LoadLocation(tz)
			if err != nil {
				return time.Time{}, fmt.Errorf("invalid IANA timezone %q: %w", tz, err)
			}
		}
		parsed, err := cronParser.Parse("CRON_TZ=" + location.String() + " " + expr)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid cron expression: %w", err)
		}
		next := parsed.Next(now)
		if next.IsZero() {
			return time.Time{}, fmt.Errorf("cron expression has no future run")
		}
		return next, nil
	default:
		return time.Time{}, fmt.Errorf("unsupported schedule kind %q", schedule.Kind)
	}
}

func (tx *Tx) AddReminder(ctx context.Context, channelID, senderID, message string, schedule ReminderSchedule, fireAt time.Time) (int64, error) {
	var anchor any
	if !schedule.AnchorAt.IsZero() {
		anchor = schedule.AnchorAt
	}
	result, err := tx.tx.ExecContext(ctx, `
		INSERT INTO reminders
		(channel_id, sender_id, message, fire_at, schedule_kind, every_ms, anchor_at, cron_expr, timezone, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		channelID, senderID, message, fireAt, schedule.Kind, schedule.EveryMS, anchor, schedule.CronExpr, schedule.Timezone,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to add reminder: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read reminder id: %w", err)
	}
	return id, nil
}

const reminderColumns = `id, channel_id, sender_id, message, fire_at, schedule_kind, every_ms, anchor_at, cron_expr, timezone, enabled`

func scanReminder(scanner interface{ Scan(...any) error }) (Reminder, error) {
	var reminder Reminder
	var kind string
	var anchor sql.NullTime
	var enabled int
	err := scanner.Scan(&reminder.ID, &reminder.ChannelID, &reminder.SenderID, &reminder.Message, &reminder.FireAt,
		&kind, &reminder.Schedule.EveryMS, &anchor, &reminder.Schedule.CronExpr, &reminder.Schedule.Timezone, &enabled)
	if err != nil {
		return Reminder{}, err
	}
	reminder.Schedule.Kind = ScheduleKind(kind)
	reminder.Schedule.At = reminder.FireAt
	if anchor.Valid {
		reminder.Schedule.AnchorAt = anchor.Time
	}
	reminder.Enabled = enabled != 0
	return reminder, nil
}

func (tx *Tx) ListReminders(ctx context.Context, channelID, senderID string) ([]Reminder, error) {
	rows, err := tx.tx.QueryContext(ctx, "SELECT "+reminderColumns+" FROM reminders WHERE channel_id = ? AND sender_id = ? ORDER BY fire_at ASC", channelID, senderID)
	if err != nil {
		return nil, fmt.Errorf("failed to list reminders: %w", err)
	}
	return collectReminders(rows)
}

func collectReminders(rows *sql.Rows) ([]Reminder, error) {
	defer rows.Close()
	var reminders []Reminder
	for rows.Next() {
		reminder, err := scanReminder(rows)
		if err != nil {
			return nil, fmt.Errorf("scan reminder: %w", err)
		}
		reminders = append(reminders, reminder)
	}
	return reminders, rows.Err()
}

func (tx *Tx) GetReminderForUser(ctx context.Context, id int, channelID, senderID string) (Reminder, error) {
	reminder, err := scanReminder(tx.tx.QueryRowContext(ctx, "SELECT "+reminderColumns+" FROM reminders WHERE id = ? AND channel_id = ? AND sender_id = ?", id, channelID, senderID))
	if err == sql.ErrNoRows {
		return Reminder{}, fmt.Errorf("reminder %d not found", id)
	}
	if err != nil {
		return Reminder{}, fmt.Errorf("get reminder: %w", err)
	}
	return reminder, nil
}

func (tx *Tx) UpdateReminderForUser(ctx context.Context, reminder Reminder) error {
	var anchor any
	if !reminder.Schedule.AnchorAt.IsZero() {
		anchor = reminder.Schedule.AnchorAt
	}
	result, err := tx.tx.ExecContext(ctx, `UPDATE reminders SET message = ?, fire_at = ?, schedule_kind = ?, every_ms = ?, anchor_at = ?, cron_expr = ?, timezone = ?, enabled = ? WHERE id = ? AND channel_id = ? AND sender_id = ?`,
		reminder.Message, reminder.FireAt, reminder.Schedule.Kind, reminder.Schedule.EveryMS, anchor, reminder.Schedule.CronExpr, reminder.Schedule.Timezone, reminder.Enabled, reminder.ID, reminder.ChannelID, reminder.SenderID)
	if err != nil {
		return fmt.Errorf("failed to update reminder: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read updated reminder count: %w", err)
	}
	if updated == 0 {
		return fmt.Errorf("reminder %d not found", reminder.ID)
	}
	return nil
}

func (s *Store) FetchDueReminders() ([]Reminder, error) {
	rows, err := s.db.Query("SELECT "+reminderColumns+" FROM reminders WHERE enabled = 1 AND fire_at <= ? ORDER BY fire_at ASC", time.Now())
	if err != nil {
		return nil, fmt.Errorf("fetch due reminders: %w", err)
	}
	return collectReminders(rows)
}

// CompleteReminder deletes one-shots and advances recurring reminders only
// after successful delivery, so failed sends remain due for retry.
func (s *Store) CompleteReminder(reminder Reminder, now time.Time) error {
	return s.WithTx(context.Background(), func(tx *Tx) error {
		return tx.completeReminder(context.Background(), reminder, now)
	})
}

// CompleteReminderDelivery records the scheduled turn and delivered assistant
// text together with the matching reminder advancement or deletion.
func (s *Store) CompleteReminderDelivery(ctx context.Context, reminder Reminder, now time.Time, scheduledContent, notification string) error {
	return s.WithTx(ctx, func(tx *Tx) error {
		if err := tx.SaveConversationMessage(ctx, reminder.ChannelID, reminder.SenderID, "user", ContentScheduledReminder, scheduledContent); err != nil {
			return err
		}
		if err := tx.SaveConversationMessage(ctx, reminder.ChannelID, reminder.SenderID, "assistant", ContentText, notification); err != nil {
			return err
		}
		return tx.completeReminder(ctx, reminder, now)
	})
}

// CompleteReminderTraceDelivery atomically records the delivered reminder,
// advances its schedule, and finalizes the matching provenance trace.
func (s *Store) CompleteReminderTraceDelivery(ctx context.Context, reminder Reminder, now time.Time, scheduledContent, notification string, traceID, deliveryEventID int64, providerMessageID string) error {
	return s.WithTx(ctx, func(tx *Tx) error {
		if err := tx.SaveConversationMessage(ctx, reminder.ChannelID, reminder.SenderID, "user", ContentScheduledReminder, scheduledContent); err != nil {
			return err
		}
		result, err := tx.tx.ExecContext(ctx, `INSERT INTO conversation_history (channel_id, sender_id, role, content_type, content) VALUES (?, ?, 'assistant', ?, ?)`, reminder.ChannelID, reminder.SenderID, ContentText, notification)
		if err != nil {
			return err
		}
		historyID, err := result.LastInsertId()
		if err != nil {
			return err
		}
		if err := tx.completeReminder(ctx, reminder, now); err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx, `UPDATE delivery_attempts SET provider_message_id=?, conversation_history_id=?, accepted_at=CURRENT_TIMESTAMP WHERE event_id=?`, providerMessageID, historyID, deliveryEventID); err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx, `UPDATE trace_events SET status='succeeded', completed_at=CURRENT_TIMESTAMP WHERE id=?`, deliveryEventID); err != nil {
			return err
		}
		_, err = tx.tx.ExecContext(ctx, `UPDATE response_traces SET status='completed', completed_at=CURRENT_TIMESTAMP WHERE id=?`, traceID)
		return err
	})
}

func (tx *Tx) completeReminder(ctx context.Context, reminder Reminder, now time.Time) error {
	if reminder.Schedule.Kind == ScheduleAt {
		return tx.deleteReminder(ctx, reminder.ID)
	}
	next, err := NextReminderRun(reminder.Schedule, now)
	if err != nil {
		return fmt.Errorf("advance reminder %d: %w", reminder.ID, err)
	}
	result, err := tx.tx.ExecContext(ctx, "UPDATE reminders SET fire_at = ? WHERE id = ? AND fire_at = ?", next, reminder.ID, reminder.FireAt)
	if err != nil {
		return fmt.Errorf("advance reminder %d: %w", reminder.ID, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read advanced reminder count: %w", err)
	}
	if updated == 0 {
		return fmt.Errorf("reminder %d changed while delivering", reminder.ID)
	}
	return nil
}

func (tx *Tx) deleteReminder(ctx context.Context, id int) error {
	result, err := tx.tx.ExecContext(ctx, "DELETE FROM reminders WHERE id = ?", id)
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

func (tx *Tx) DeleteReminderForUser(ctx context.Context, id int, channelID, senderID string) error {
	result, err := tx.tx.ExecContext(ctx, "DELETE FROM reminders WHERE id = ? AND channel_id = ? AND sender_id = ?", id, channelID, senderID)
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

func ScheduleDescription(schedule ReminderSchedule) map[string]any {
	result := map[string]any{"kind": schedule.Kind}
	switch schedule.Kind {
	case ScheduleAt:
		result["at"] = schedule.At.Format(time.RFC3339)
	case ScheduleEvery:
		result["every_ms"] = schedule.EveryMS
		if !schedule.AnchorAt.IsZero() {
			result["anchor_at"] = schedule.AnchorAt.Format(time.RFC3339)
		}
	case ScheduleCron:
		result["expr"] = schedule.CronExpr
		if schedule.Timezone != "" {
			result["timezone"] = schedule.Timezone
		}
	}
	return result
}
