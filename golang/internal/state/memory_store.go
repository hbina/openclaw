package state

import (
	"context"
	"fmt"
	"time"
)

// MemoryEntry represents a single stored fact or interaction.
type MemoryEntry struct {
	ID        int
	Content   string
	CreatedAt time.Time
}

// SaveMemory stores a new memory entry in the database.
func (s *Store) SaveMemory(ctx context.Context, content string) error {
	query := `INSERT INTO memory_entries (content) VALUES (?)`
	_, err := s.db.ExecContext(ctx, query, content)
	if err != nil {
		return fmt.Errorf("failed to save memory: %w", err)
	}
	return nil
}

// SearchMemory retrieves memories matching a simple keyword search.
func (s *Store) SearchMemory(ctx context.Context, query string, limit int) ([]MemoryEntry, error) {
	// A real implementation would use vector search; this is a basic fallback for the skeleton.
	sqlQuery := `SELECT id, content, created_at FROM memory_entries WHERE content LIKE ? ORDER BY created_at DESC LIMIT ?`
	likeQuery := "%" + query + "%"

	rows, err := s.db.QueryContext(ctx, sqlQuery, likeQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to search memory: %w", err)
	}
	defer rows.Close()

	var results []MemoryEntry
	for rows.Next() {
		var entry MemoryEntry
		if err := rows.Scan(&entry.ID, &entry.Content, &entry.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan memory row: %w", err)
		}
		results = append(results, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory rows iteration error: %w", err)
	}

	return results, nil
}

// SaveMemoryUnique treats an exact duplicate as an idempotent success.
func (tx *Tx) SaveMemoryUnique(ctx context.Context, content string) (bool, error) {
	result, err := tx.tx.ExecContext(ctx,
		`INSERT INTO memory_entries (content) SELECT ? WHERE NOT EXISTS (SELECT 1 FROM memory_entries WHERE content = ?)`,
		content, content,
	)
	if err != nil {
		return false, fmt.Errorf("failed to save memory: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read saved memory count: %w", err)
	}
	return count > 0, nil
}

func (tx *Tx) SearchMemory(ctx context.Context, query string, limit int) ([]MemoryEntry, error) {
	rows, err := tx.tx.QueryContext(ctx,
		`SELECT id, content, created_at FROM memory_entries WHERE content LIKE ? ORDER BY created_at DESC LIMIT ?`,
		"%"+query+"%", limit,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to search memory: %w", err)
	}
	defer rows.Close()

	var results []MemoryEntry
	for rows.Next() {
		var entry MemoryEntry
		if err := rows.Scan(&entry.ID, &entry.Content, &entry.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan memory row: %w", err)
		}
		results = append(results, entry)
	}
	return results, rows.Err()
}
