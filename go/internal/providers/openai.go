package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiRequest struct {
	Model    string          `json:"model"`
	Messages []openaiMessage `json:"messages"`
}

type openaiResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

type OpenAIClient struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

func NewOpenAIClient(apiKey, baseURL string) *OpenAIClient {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAIClient{
		apiKey:  apiKey,
		baseURL: baseURL,
		client:  &http.Client{},
	}
}

func (c *OpenAIClient) ID() string {
	return "openai"
}

func (c *OpenAIClient) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	oReq := openaiRequest{
		Model: req.Model,
	}
	for _, m := range req.Messages {
		oReq.Messages = append(oReq.Messages, openaiMessage{
			Role:    string(m.Role),
			Content: m.Content,
		})
	}

	bodyBytes, err := json.Marshal(oReq)
	if err != nil {
		return nil, fmt.Errorf("openai marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("openai new request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var oResp openaiResponse
	if err := json.NewDecoder(resp.Body).Decode(&oResp); err != nil {
		return nil, fmt.Errorf("openai decode response: %w", err)
	}

	if len(oResp.Choices) == 0 {
		return nil, fmt.Errorf("openai no choices returned")
	}

	return &GenerateResponse{
		Content: oResp.Choices[0].Message.Content,
	}, nil
}
