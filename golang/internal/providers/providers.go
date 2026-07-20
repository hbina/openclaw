package providers

import (
	"context"
	"encoding/json"
)

// MessageRole is an OpenAI-compatible chat message role.
type MessageRole string

const (
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleSystem    MessageRole = "system"
	RoleTool      MessageRole = "tool"
)

// FunctionCall is a model-requested function invocation. Arguments remains JSON
// text so validation happens at the trusted tool execution boundary.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// Message represents the structured subset of Chat Completions used by the Go
// agent. Assistant tool calls and their tool results retain exact call IDs.
type Message struct {
	Role       MessageRole `json:"role"`
	Content    string      `json:"content,omitempty"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
}

type FunctionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ToolDefinition struct {
	Type     string             `json:"type"`
	Function FunctionDefinition `json:"function"`
}

type GenerateRequest struct {
	Model      string
	Messages   []Message
	Tools      []ToolDefinition
	ToolChoice string
	MaxTokens  int
}

type GenerateResponse struct {
	Message      Message
	FinishReason string
}

// Provider is retained as a narrow injection seam for the local model client
// and deterministic agent tests.
type Provider interface {
	Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error)
}
