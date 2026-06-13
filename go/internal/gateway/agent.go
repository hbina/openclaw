package gateway

import (
	"context"
	"fmt"
	"log"

	"github.com/openclaw/openclaw/go/internal/channels"
	"github.com/openclaw/openclaw/go/internal/memory"
	"github.com/openclaw/openclaw/go/internal/providers"
)

// Agent runs the primary interaction loop.
type Agent struct {
	provider   providers.Provider
	memoryCore *memory.Core
	chanReg    *channels.Registry
}

func NewAgent(provider providers.Provider, memoryCore *memory.Core, chanReg *channels.Registry) *Agent {
	return &Agent{
		provider:   provider,
		memoryCore: memoryCore,
		chanReg:    chanReg,
	}
}

// HandleMessage is the callback triggered by any channel receiving a message.
func (a *Agent) HandleMessage(ctx context.Context, msg *channels.Message) error {
	log.Printf("Agent received message from %s [%s]: %s\n", msg.ChannelID, msg.SenderID, msg.Content)

	// 1. Store the incoming fact into memory
	if err := a.memoryCore.StoreFact(ctx, fmt.Sprintf("User %s said: %s", msg.SenderID, msg.Content)); err != nil {
		log.Printf("Failed to store memory: %v", err)
	}

	// 2. Recall relevant context
	contextFacts, _ := a.memoryCore.Recall(ctx, msg.Content)
	var systemPrompt string = fmt.Sprintf("You are a helpful assistant talking to User '%s' on Channel '%s'. When setting reminders, you must explicitly use these exact IDs.\nUse these past facts if relevant:\n", msg.SenderID, msg.ChannelID)
	for _, fact := range contextFacts {
		systemPrompt += "- " + fact + "\n"
	}

	// 3. Query the LLM provider
	req := &providers.GenerateRequest{
		Model: "default", // Should be injected via config
		Messages: []providers.Message{
			{Role: providers.RoleSystem, Content: systemPrompt},
			{Role: providers.RoleUser, Content: msg.Content},
		},
	}

	resp, err := a.provider.Generate(ctx, req)
	if err != nil {
		return fmt.Errorf("agent generation failed: %w", err)
	}

	// 4. Send the response back through the originating channel
	ch, err := a.chanReg.Get(msg.ChannelID)
	if err != nil {
		return fmt.Errorf("channel %s not found: %w", msg.ChannelID, err)
	}

	if err := ch.SendMessage(ctx, msg.SenderID, resp.Content); err != nil {
		return fmt.Errorf("failed to send response: %w", err)
	}

	// 5. Store the assistant's reply into memory
	_ = a.memoryCore.StoreFact(ctx, fmt.Sprintf("Assistant to %s: %s", msg.SenderID, resp.Content))

	return nil
}
