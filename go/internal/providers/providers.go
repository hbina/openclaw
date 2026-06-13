package providers

import (
	"context"
	"fmt"
)

// MessageRole defines the role of the message sender.
type MessageRole string

const (
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleSystem    MessageRole = "system"
)

// Message represents a generic chat message.
type Message struct {
	Role    MessageRole
	Content string
}

// GenerateRequest represents the input to a provider model.
type GenerateRequest struct {
	Model    string
	Messages []Message
}

// GenerateResponse represents the output from a provider model.
type GenerateResponse struct {
	Content string
}

// Provider defines the interface that all model providers must implement.
type Provider interface {
	// Generate replies to the message history.
	Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error)
	// ID returns the canonical provider ID (e.g., "openai", "anthropic").
	ID() string
}

// Factory defines a function capable of initializing a Provider.
type Factory func() (Provider, error)

// Registry holds the initialized providers.
type Registry struct {
	providers map[string]Provider
}

// NewRegistry creates a new provider registry.
func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[string]Provider),
	}
}

// Register registers a provider implementation.
func (r *Registry) Register(p Provider) {
	r.providers[p.ID()] = p
}

// Get returns a provider by its ID.
func (r *Registry) Get(id string) (Provider, error) {
	p, ok := r.providers[id]
	if !ok {
		return nil, fmt.Errorf("provider %q not found", id)
	}
	return p, nil
}
