package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type openAIMessage struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type openAIRequest struct {
	Model             string           `json:"model"`
	Messages          []openAIMessage  `json:"messages"`
	Tools             []ToolDefinition `json:"tools,omitempty"`
	ToolChoice        string           `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls,omitempty"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Role      string     `json:"role"`
			Content   *string    `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// OpenAIClient speaks the OpenAI-compatible protocol exposed by llama-server.
// The historical config key remains "openai" so existing local config works.
type OpenAIClient struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

func NewOpenAIClient(apiKey, baseURL string) (*OpenAIClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("local model base URL is required at models.providers.openai.baseUrl")
	}
	return &OpenAIClient{
		apiKey:  strings.TrimSpace(apiKey),
		baseURL: baseURL,
		client:  &http.Client{},
	}, nil
}

func (c *OpenAIClient) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	oReq := openAIRequest{
		Model: req.Model,
		Tools: req.Tools,
	}
	if len(req.Tools) > 0 {
		parallel := false
		oReq.ToolChoice = req.ToolChoice
		if oReq.ToolChoice == "" {
			oReq.ToolChoice = "auto"
		}
		oReq.ParallelToolCalls = &parallel
	}
	for _, message := range req.Messages {
		wire := openAIMessage{
			Role:       string(message.Role),
			ToolCalls:  message.ToolCalls,
			ToolCallID: message.ToolCallID,
		}
		if message.Content != "" || len(message.ToolCalls) == 0 {
			content := message.Content
			wire.Content = &content
		}
		oReq.Messages = append(oReq.Messages, wire)
	}

	bodyBytes, err := json.Marshal(oReq)
	if err != nil {
		return nil, fmt.Errorf("local model marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("local model new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("local model execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("local model unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var oResp openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&oResp); err != nil {
		return nil, fmt.Errorf("local model decode response: %w", err)
	}
	if len(oResp.Choices) == 0 {
		return nil, fmt.Errorf("local model returned no choices")
	}

	choice := oResp.Choices[0]
	message := Message{
		Role:      RoleAssistant,
		ToolCalls: choice.Message.ToolCalls,
	}
	if choice.Message.Content != nil {
		message.Content = *choice.Message.Content
	}
	return &GenerateResponse{Message: message, FinishReason: choice.FinishReason}, nil
}
