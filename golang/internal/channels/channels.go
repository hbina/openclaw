package channels

import (
	"context"
	"fmt"
)

// Message represents an inbound message from a channel.
type Message struct {
	ChannelID string
	SenderID  string
	Content   string
}

// Handler is a callback function for processing incoming messages.
type Handler func(ctx context.Context, msg *Message) error

// Channel defines the interface for all external integrations.
type Channel interface {
	// ID returns the canonical identifier (for example, "telegram").
	ID() string
	// Start connects to the service and begins listening for messages.
	Start(ctx context.Context, handler Handler) error
	// Stop disconnects from the service cleanly.
	Stop(ctx context.Context) error
	// SendMessage sends a text message to a specific recipient on this channel.
	SendMessage(ctx context.Context, recipientID string, content string) error
}

// Registry holds all initialized channels.
type Registry struct {
	channels map[string]Channel
}

// NewRegistry creates a new channel registry.
func NewRegistry() *Registry {
	return &Registry{
		channels: make(map[string]Channel),
	}
}

// Register adds a channel to the registry.
func (r *Registry) Register(ch Channel) {
	r.channels[ch.ID()] = ch
}

// Get returns a channel by its ID.
func (r *Registry) Get(id string) (Channel, error) {
	ch, ok := r.channels[id]
	if !ok {
		return nil, fmt.Errorf("channel %q not found", id)
	}
	return ch, nil
}

// StartAll starts all registered channels.
func (r *Registry) StartAll(ctx context.Context, handler Handler) error {
	for id, ch := range r.channels {
		if err := ch.Start(ctx, handler); err != nil {
			return fmt.Errorf("failed to start channel %s: %w", id, err)
		}
	}
	return nil
}

// StopAll stops all registered channels.
func (r *Registry) StopAll(ctx context.Context) error {
	var firstErr error
	for id, ch := range r.channels {
		if err := ch.Stop(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("failed to stop channel %s: %w", id, err)
		}
	}
	return firstErr
}
