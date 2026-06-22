package state

import (
	"context"
	"fmt"
)

// PersonalityDocument is a workspace document that defines the agent's identity or voice.
type PersonalityDocument struct {
	Name    string
	Content string
}

// LoadPersonality returns personality documents in stable prompt order.
func (s *Store) LoadPersonality(ctx context.Context) ([]PersonalityDocument, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, content
		FROM personality_documents
		ORDER BY CASE name WHEN 'SOUL.md' THEN 0 WHEN 'IDENTITY.md' THEN 1 ELSE 2 END, name
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to load personality: %w", err)
	}
	defer rows.Close()

	var documents []PersonalityDocument
	for rows.Next() {
		var document PersonalityDocument
		if err := rows.Scan(&document.Name, &document.Content); err != nil {
			return nil, fmt.Errorf("failed to scan personality document: %w", err)
		}
		documents = append(documents, document)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("personality rows iteration error: %w", err)
	}
	return documents, nil
}
