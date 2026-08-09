package state

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type TaskStatus string

const (
	TaskOpen      TaskStatus = "open"
	TaskCompleted TaskStatus = "completed"
	TaskAll       TaskStatus = "all"
)

type Task struct {
	ID          int
	Description string
	StartedAt   time.Time
	CompletedAt *time.Time
}

func (task Task) Status() TaskStatus {
	if task.CompletedAt != nil {
		return TaskCompleted
	}
	return TaskOpen
}

func (tx *Tx) AddTask(ctx context.Context, description string, startedAt time.Time) (Task, error) {
	description = strings.TrimSpace(description)
	if description == "" {
		return Task{}, fmt.Errorf("task description must not be empty")
	}
	if startedAt.IsZero() {
		return Task{}, fmt.Errorf("task start time must not be zero")
	}

	var existingID int
	err := tx.tx.QueryRowContext(ctx, `
		SELECT id FROM tasks
		WHERE completed_at IS NULL AND lower(trim(description)) = lower(trim(?))
	`, description).Scan(&existingID)
	if err == nil {
		return Task{}, fmt.Errorf("open task %d already has description %q", existingID, description)
	}
	if err != sql.ErrNoRows {
		return Task{}, fmt.Errorf("check duplicate open task: %w", err)
	}

	result, err := tx.tx.ExecContext(ctx,
		`INSERT INTO tasks (description, started_at, completed_at) VALUES (?, ?, NULL)`,
		description, startedAt,
	)
	if err != nil {
		return Task{}, fmt.Errorf("add task: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Task{}, fmt.Errorf("read task id: %w", err)
	}
	return Task{ID: int(id), Description: description, StartedAt: startedAt}, nil
}

const taskColumns = `id, description, started_at, completed_at`

func scanTask(scanner interface{ Scan(...any) error }) (Task, error) {
	var task Task
	var completedAt sql.NullTime
	if err := scanner.Scan(&task.ID, &task.Description, &task.StartedAt, &completedAt); err != nil {
		return Task{}, err
	}
	if completedAt.Valid {
		completed := completedAt.Time
		task.CompletedAt = &completed
	}
	return task, nil
}

func (tx *Tx) ListTasks(ctx context.Context, status TaskStatus) ([]Task, error) {
	where := ""
	switch status {
	case "", TaskOpen:
		where = " WHERE completed_at IS NULL"
	case TaskCompleted:
		where = " WHERE completed_at IS NOT NULL"
	case TaskAll:
	default:
		return nil, fmt.Errorf("task status must be open, completed, or all")
	}
	rows, err := tx.tx.QueryContext(ctx, "SELECT "+taskColumns+" FROM tasks"+where+" ORDER BY started_at ASC, id ASC")
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}
	return tasks, nil
}

func (tx *Tx) GetTask(ctx context.Context, id int) (Task, error) {
	task, err := scanTask(tx.tx.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id = ?", id))
	if err == sql.ErrNoRows {
		return Task{}, fmt.Errorf("task %d not found", id)
	}
	if err != nil {
		return Task{}, fmt.Errorf("get task %d: %w", id, err)
	}
	return task, nil
}

func (tx *Tx) UpdateTask(ctx context.Context, id int, description string) (Task, error) {
	description = strings.TrimSpace(description)
	if description == "" {
		return Task{}, fmt.Errorf("task description must not be empty")
	}
	task, err := tx.GetTask(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if task.CompletedAt != nil {
		return Task{}, fmt.Errorf("task %d is completed and cannot be updated", id)
	}

	var duplicateID int
	err = tx.tx.QueryRowContext(ctx, `
		SELECT id FROM tasks
		WHERE id <> ? AND completed_at IS NULL
			AND lower(trim(description)) = lower(trim(?))
	`, id, description).Scan(&duplicateID)
	if err == nil {
		return Task{}, fmt.Errorf("open task %d already has description %q", duplicateID, description)
	}
	if err != sql.ErrNoRows {
		return Task{}, fmt.Errorf("check duplicate open task: %w", err)
	}

	result, err := tx.tx.ExecContext(ctx,
		`UPDATE tasks SET description = ? WHERE id = ? AND completed_at IS NULL`,
		description, id,
	)
	if err != nil {
		return Task{}, fmt.Errorf("update task %d: %w", id, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return Task{}, fmt.Errorf("read updated task count: %w", err)
	}
	if updated != 1 {
		return Task{}, fmt.Errorf("task %d is not open", id)
	}
	task.Description = description
	return task, nil
}

func (tx *Tx) CompleteTask(ctx context.Context, id int, completedAt time.Time) (Task, error) {
	if completedAt.IsZero() {
		return Task{}, fmt.Errorf("task completion time must not be zero")
	}
	task, err := tx.GetTask(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if task.CompletedAt != nil {
		return Task{}, fmt.Errorf("task %d is already completed", id)
	}
	result, err := tx.tx.ExecContext(ctx,
		`UPDATE tasks SET completed_at = ? WHERE id = ? AND completed_at IS NULL`,
		completedAt, id,
	)
	if err != nil {
		return Task{}, fmt.Errorf("complete task %d: %w", id, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return Task{}, fmt.Errorf("read completed task count: %w", err)
	}
	if updated != 1 {
		return Task{}, fmt.Errorf("task %d is not open", id)
	}
	task.CompletedAt = &completedAt
	return task, nil
}

func (tx *Tx) DeleteTask(ctx context.Context, id int) (Task, error) {
	task, err := tx.GetTask(ctx, id)
	if err != nil {
		return Task{}, err
	}
	result, err := tx.tx.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id)
	if err != nil {
		return Task{}, fmt.Errorf("remove task %d: %w", id, err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return Task{}, fmt.Errorf("read removed task count: %w", err)
	}
	if deleted != 1 {
		return Task{}, fmt.Errorf("task %d not found", id)
	}
	return task, nil
}
