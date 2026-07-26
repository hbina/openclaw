package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
)

const maxEmbeddingResponseBytes = 16 << 20

type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
	Tokenize(ctx context.Context, content string) ([]int, error)
	Detokenize(ctx context.Context, tokens []int) (string, error)
}

type PromptSizer interface {
	ContextSize(ctx context.Context) (int, error)
	CountPromptTokens(ctx context.Context, messages []Message, tools []ToolDefinition) (int, error)
}

type EmbeddingClient struct {
	apiKey     string
	baseURL    string
	serverURL  string
	model      string
	dimensions int
	client     *http.Client
}

func NewEmbeddingClient(apiKey, baseURL, model string, dimensions int) (*EmbeddingClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("embedding base URL is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("invalid embedding base URL")
	}
	serverURL := baseURL
	if strings.HasSuffix(serverURL, "/v1") {
		serverURL = strings.TrimSuffix(serverURL, "/v1")
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("embedding model is required")
	}
	if dimensions <= 0 {
		return nil, fmt.Errorf("embedding dimensions must be positive")
	}
	return &EmbeddingClient{
		apiKey: strings.TrimSpace(apiKey), baseURL: baseURL, serverURL: serverURL,
		model: strings.TrimSpace(model), dimensions: dimensions, client: &http.Client{},
	}, nil
}

func (c *EmbeddingClient) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, fmt.Errorf("embedding input must not be empty")
	}
	var response struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := c.doJSON(ctx, http.MethodPost, c.baseURL+"/embeddings", map[string]any{
		"model": c.model, "input": inputs, "encoding_format": "float",
	}, &response); err != nil {
		return nil, err
	}
	if len(response.Data) != len(inputs) {
		return nil, fmt.Errorf("embedding response count %d does not match input count %d", len(response.Data), len(inputs))
	}
	vectors := make([][]float32, len(inputs))
	seen := make([]bool, len(inputs))
	for _, item := range response.Data {
		if item.Index < 0 || item.Index >= len(inputs) || seen[item.Index] {
			return nil, fmt.Errorf("embedding response contains invalid index %d", item.Index)
		}
		if len(item.Embedding) != c.dimensions {
			return nil, fmt.Errorf("embedding dimension %d does not match configured %d", len(item.Embedding), c.dimensions)
		}
		var norm float64
		for _, value := range item.Embedding {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, fmt.Errorf("embedding contains a non-finite value")
			}
			norm += value * value
		}
		if norm == 0 {
			return nil, fmt.Errorf("embedding has zero norm")
		}
		norm = math.Sqrt(norm)
		vector := make([]float32, c.dimensions)
		for index, value := range item.Embedding {
			vector[index] = float32(value / norm)
		}
		vectors[item.Index] = vector
		seen[item.Index] = true
	}
	return vectors, nil
}

func (c *EmbeddingClient) Tokenize(ctx context.Context, content string) ([]int, error) {
	var response struct {
		Tokens []int `json:"tokens"`
	}
	if err := c.doJSON(ctx, http.MethodPost, c.serverURL+"/tokenize", map[string]any{
		"content": content, "add_special": false, "parse_special": true,
	}, &response); err != nil {
		return nil, err
	}
	return response.Tokens, nil
}

func (c *EmbeddingClient) Detokenize(ctx context.Context, tokens []int) (string, error) {
	var response struct {
		Content string `json:"content"`
	}
	if err := c.doJSON(ctx, http.MethodPost, c.serverURL+"/detokenize", map[string]any{
		"tokens": tokens,
	}, &response); err != nil {
		return "", err
	}
	return response.Content, nil
}

func (c *EmbeddingClient) doJSON(ctx context.Context, method, endpoint string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("marshal local embedding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create local embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("execute local embedding request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("local embedding server status %d: %s", resp.StatusCode, strings.TrimSpace(string(message)))
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxEmbeddingResponseBytes))
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode local embedding response: %w", err)
	}
	return nil
}
