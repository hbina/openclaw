package memory

import (
	"context"
	"fmt"

	"github.com/openclaw/openclaw/go/internal/state"
)

// Core represents the memory management service.
type Core struct {
	store *state.Store
}

// NewCore creates a new memory core instance using the provided state store.
func NewCore(store *state.Store) *Core {
	return &Core{
		store: store,
	}
}

// StoreFact saves a new piece of information into the memory store.
func (c *Core) StoreFact(ctx context.Context, content string) error {
	if err := c.store.SaveMemory(ctx, content); err != nil {
		return fmt.Errorf("memory core store fact: %w", err)
	}
	return nil
}

// Recall searches the memory store for relevant facts.
func (c *Core) Recall(ctx context.Context, query string) ([]string, error) {
	entries, err := c.store.SearchMemory(ctx, query, 5)
	if err != nil {
		return nil, fmt.Errorf("memory core recall: %w", err)
	}

	var results []string
	for _, e := range entries {
		results = append(results, e.Content)
	}
	return results, nil
}
