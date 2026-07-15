package state

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const (
	SoulDocumentName     = "SOUL.md"
	IdentityDocumentName = "IDENTITY.md"

	DefaultSoul = `# SOUL.md - Who You Are

You are not a generic chatbot. You are a capable local personal assistant with continuity, judgment, and a point of view.

## Core Truths

- Be genuinely helpful, not performatively helpful. Skip canned praise and get to the point.
- Have opinions. You may disagree, prefer one approach, and find things interesting or dull.
- Be resourceful before asking. Use available context and tools, then ask only when genuinely blocked.
- Earn trust through competence. Be careful with external actions and bold with safe internal work.
- Treat access to the user's life as private and personal.

## Vibe

Warm, direct, curious, quietly confident, and occasionally playful. Concise for simple matters; thoughtful when nuance matters. Never sound like a corporate support bot.

## Boundaries

- Private things stay private.
- Ask before consequential external actions.
- Do not pretend an action succeeded when a tool did not confirm it.
- You are not the user's voice.
`

	DefaultIdentity = `# IDENTITY.md - Who You Are

- **Name:** OpenClaw
- **Creature:** A local AI familiar and personal assistant
- **Vibe:** Warm, sharp, resourceful, and calmly opinionated
- **Emoji:** 🦞

When asked who you are or what your name is, answer from this identity directly and naturally.
`
)

// PersonalityDocument is a workspace document that defines the agent's identity or voice.
type PersonalityDocument struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

func isPersonalityDocumentName(name string) bool {
	return name == SoulDocumentName || name == IdentityDocumentName
}

// EnsureDefaultPersonality seeds first-run identity without overwriting any
// user-edited document already stored in SQLite.
func (s *Store) EnsureDefaultPersonality(ctx context.Context) error {
	for _, document := range []PersonalityDocument{
		{Name: SoulDocumentName, Content: DefaultSoul},
		{Name: IdentityDocumentName, Content: DefaultIdentity},
	} {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO personality_documents (name, content)
			VALUES (?, ?)
			ON CONFLICT(name) DO NOTHING
		`, document.Name, document.Content); err != nil {
			return fmt.Errorf("seed %s: %w", document.Name, err)
		}
	}
	return nil
}

// LoadPersonality returns personality documents in stable prompt order.
func (s *Store) LoadPersonality(ctx context.Context) ([]PersonalityDocument, error) {
	return loadPersonality(ctx, s.db)
}

func (tx *Tx) LoadPersonality(ctx context.Context) ([]PersonalityDocument, error) {
	return loadPersonality(ctx, tx.tx)
}

type personalityQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadPersonality(ctx context.Context, querier personalityQuerier) ([]PersonalityDocument, error) {
	rows, err := querier.QueryContext(ctx, `
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

func (tx *Tx) UpdatePersonality(ctx context.Context, name, content string) error {
	name = strings.TrimSpace(name)
	content = strings.TrimSpace(content)
	if !isPersonalityDocumentName(name) {
		return fmt.Errorf("document must be %s or %s", SoulDocumentName, IdentityDocumentName)
	}
	if content == "" {
		return fmt.Errorf("content must not be empty")
	}
	_, err := tx.tx.ExecContext(ctx, `
		INSERT INTO personality_documents (name, content, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(name) DO UPDATE SET content = excluded.content, updated_at = CURRENT_TIMESTAMP
	`, name, content)
	if err != nil {
		return fmt.Errorf("update %s: %w", name, err)
	}
	return nil
}
