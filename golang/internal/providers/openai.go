package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	MaxTokens         int              `json:"max_tokens,omitempty"`
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
	apiKey    string
	baseURL   string
	serverURL string
	client    *http.Client
}

func NewOpenAIClient(apiKey, baseURL string) (*OpenAIClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("local model base URL is required at models.providers.openai.baseUrl")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("invalid local model base URL")
	}
	serverURL := baseURL
	if before, ok := strings.CutSuffix(serverURL, "/v1"); ok {
		serverURL = before
	}
	return &OpenAIClient{
		apiKey:    strings.TrimSpace(apiKey),
		baseURL:   baseURL,
		serverURL: serverURL,
		client:    &http.Client{},
	}, nil
}

func (c *OpenAIClient) ContextSize(ctx context.Context) (int, error) {
	var response struct {
		DefaultGenerationSettings struct {
			ContextSize int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := c.doLocalJSON(ctx, http.MethodGet, c.serverURL+"/props", nil, &response); err != nil {
		return 0, err
	}
	if response.DefaultGenerationSettings.ContextSize <= 0 {
		return 0, fmt.Errorf("local model reported invalid context size")
	}
	return response.DefaultGenerationSettings.ContextSize, nil
}

func (c *OpenAIClient) CountPromptTokens(ctx context.Context, messages []Message, tools []ToolDefinition) (int, error) {
	wireMessages := make([]openAIMessage, 0, len(messages))
	for _, message := range messages {
		wire := openAIMessage{
			Role: string(message.Role), ToolCalls: message.ToolCalls, ToolCallID: message.ToolCallID,
		}
		if message.Content != "" || len(message.ToolCalls) == 0 {
			content := message.Content
			wire.Content = &content
		}
		wireMessages = append(wireMessages, wire)
	}
	var templated struct {
		Prompt string `json:"prompt"`
	}
	if err := c.doLocalJSON(ctx, http.MethodPost, c.serverURL+"/apply-template", map[string]any{
		"messages": wireMessages,
	}, &templated); err != nil {
		return 0, err
	}
	toolJSON, err := json.Marshal(tools)
	if err != nil {
		return 0, fmt.Errorf("marshal tool definitions for token count: %w", err)
	}
	var tokenized struct {
		Tokens []int `json:"tokens"`
	}
	if err := c.doLocalJSON(ctx, http.MethodPost, c.serverURL+"/tokenize", map[string]any{
		"content":       templated.Prompt + "\n" + string(toolJSON),
		"add_special":   false,
		"parse_special": true,
	}, &tokenized); err != nil {
		return 0, err
	}
	return len(tokenized.Tokens), nil
}

func (c *OpenAIClient) doLocalJSON(ctx context.Context, method, endpoint string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("marshal local model request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("create local model request: %w", err)
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("execute local model request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("local model status %d: %s", resp.StatusCode, strings.TrimSpace(string(message)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(output); err != nil {
		return fmt.Errorf("decode local model response: %w", err)
	}
	return nil
}

func (c *OpenAIClient) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	oReq := openAIRequest{
		Model:     req.Model,
		Tools:     req.Tools,
		MaxTokens: req.MaxTokens,
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
