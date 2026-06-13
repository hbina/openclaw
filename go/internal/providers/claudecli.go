package providers

import (
	"context"
	"fmt"
	"os"
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
	// Create a temporary settings JSON for MCP
	mcpSettings := `{
		"mcpServers": {
			"openclaw-cron": {
				"command": "/app/openclaw",
				"args": ["mcp-server"]
			}
		}
	}`
	settingsPath := "/tmp/claude-settings.json"
	os.WriteFile(settingsPath, []byte(mcpSettings), 0644)

	// Build the command execution. We use "-p" to execute non-interactively and return text.
	cmdArgs := []string{"-p", promptBuilder.String(), "--mcp-config", settingsPath, "--permission-mode", "auto"}
	cmd := exec.CommandContext(ctx, "claude", cmdArgs...)

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
