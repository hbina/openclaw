package providers

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ClaudeCLIProvider uses the local 'claude' CLI binary natively without an API key.
type ClaudeCLIProvider struct{}

func NewClaudeCLIProvider() *ClaudeCLIProvider {
	return &ClaudeCLIProvider{}
}

func (p *ClaudeCLIProvider) ID() string {
	return "claude-cli"
}

func (p *ClaudeCLIProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	var promptBuilder strings.Builder
	for _, msg := range req.Messages {
		promptBuilder.WriteString(fmt.Sprintf("%s: %s\n\n", msg.Role, msg.Content))
	}

	// Option A: Execute 'claude -p "system... user..."' per message
	// This uses the CLI non-interactively to generate a response.
	cmd := exec.CommandContext(ctx, "claude", "-p", promptBuilder.String())
	
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = string(exitErr.Stderr)
		}
		return nil, fmt.Errorf("claude cli failed: %w, stderr: %s", err, stderr)
	}

	return &GenerateResponse{
		Content: strings.TrimSpace(string(out)),
	}, nil
}
