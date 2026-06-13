package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	Messages  []anthropicMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
}

type anthropicResponse struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

type AnthropicClient struct {
	apiKey string
	client *http.Client
}

func NewAnthropicClient(apiKey string) *AnthropicClient {
	return &AnthropicClient{
		apiKey: apiKey,
		client: &http.Client{},
	}
}

func (c *AnthropicClient) ID() string {
	return "anthropic"
}

func (c *AnthropicClient) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	aReq := anthropicRequest{
		Model:     req.Model,
		MaxTokens: 4096,
	}
	
	for _, m := range req.Messages {
		if m.Role == RoleSystem {
			aReq.System = m.Content
			continue
		}
		aReq.Messages = append(aReq.Messages, anthropicMessage{
			Role:    string(m.Role),
			Content: m.Content,
		})
	}

	bodyBytes, err := json.Marshal(aReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("anthropic new request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anthropic unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var aResp anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&aResp); err != nil {
		return nil, fmt.Errorf("anthropic decode response: %w", err)
	}

	if len(aResp.Content) == 0 {
		return nil, fmt.Errorf("anthropic no content returned")
	}

	return &GenerateResponse{
		Content: aResp.Content[0].Text,
	}, nil
}
