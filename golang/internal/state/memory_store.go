package state

import (
	"context"
	"fmt"
	"sort"

	"github.com/openclaw/openclaw/go/internal/vector"
)

// MemoryEntry represents a single stored fact or interaction.
type MemoryEntry struct {
	ID      int
	Content string
}

// SaveMemoryUnique treats an exact duplicate as an idempotent success and
// stores the embedding alongside the new row.
func (tx *Tx) SaveMemoryUnique(ctx context.Context, content, embeddingModel string, dimensions int, embedding []byte) (bool, error) {
	result, err := tx.tx.ExecContext(ctx,
		`INSERT INTO memory_entries (content, embedding_model, dimensions, embedding)
		 SELECT ?, ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM memory_entries WHERE content = ?)`,
		content, embeddingModel, dimensions, embedding, content,
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

// SearchMemoryByVector scores every embedded memory entry for the current
// embedding model/dimensions against query by dot product (equivalent to
// cosine similarity, since Embed implementations return unit-length
// vectors), keeping matches at or above minScore and returning at most limit
// entries, highest score first.
func (tx *Tx) SearchMemoryByVector(ctx context.Context, embeddingModel string, dimensions int, query []float32, minScore float64, limit int) ([]MemoryEntry, error) {
	rows, err := tx.tx.QueryContext(ctx,
		`SELECT id, content, embedding FROM memory_entries
		 WHERE embedding_model = ? AND dimensions = ? AND embedding IS NOT NULL`,
		embeddingModel, dimensions,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to search memory: %w", err)
	}
	defer rows.Close()

	type scoredEntry struct {
		entry MemoryEntry
		score float64
	}
	var candidates []scoredEntry
	for rows.Next() {
		var entry MemoryEntry
		var raw []byte
		if err := rows.Scan(&entry.ID, &entry.Content, &raw); err != nil {
			return nil, fmt.Errorf("failed to scan memory row: %w", err)
		}
		stored, err := vector.Unpack(raw, dimensions)
		if err != nil {
			return nil, fmt.Errorf("decode memory embedding %d: %w", entry.ID, err)
		}
		score := vector.Dot(query, stored)
		if score >= minScore {
			candidates = append(candidates, scoredEntry{entry: entry, score: score})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory rows iteration error: %w", err)
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].entry.ID < candidates[j].entry.ID
		}
		return candidates[i].score > candidates[j].score
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	results := make([]MemoryEntry, len(candidates))
	for index, candidate := range candidates {
		results[index] = candidate.entry
	}
	return results, nil
}

// MemoryEntriesMissingEmbedding returns memory entries that have no embedding
// yet, or whose stored embedding belongs to a different embedding model or
// dimensions than currently configured.
func (s *Store) MemoryEntriesMissingEmbedding(ctx context.Context, embeddingModel string, dimensions int) ([]MemoryEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content FROM memory_entries
		 WHERE embedding IS NULL OR embedding_model <> ? OR dimensions <> ?`,
		embeddingModel, dimensions,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list unembedded memory: %w", err)
	}
	defer rows.Close()

	var results []MemoryEntry
	for rows.Next() {
		var entry MemoryEntry
		if err := rows.Scan(&entry.ID, &entry.Content); err != nil {
			return nil, fmt.Errorf("failed to scan memory row: %w", err)
		}
		results = append(results, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory rows iteration error: %w", err)
	}
	return results, nil
}

// SaveMemoryEmbedding backfills the embedding for an existing memory entry.
func (s *Store) SaveMemoryEmbedding(ctx context.Context, id int, embeddingModel string, dimensions int, embedding []byte) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE memory_entries SET embedding_model = ?, dimensions = ?, embedding = ? WHERE id = ?`,
		embeddingModel, dimensions, embedding, id,
	)
	if err != nil {
		return fmt.Errorf("failed to save memory embedding: %w", err)
	}
	return nil
}
