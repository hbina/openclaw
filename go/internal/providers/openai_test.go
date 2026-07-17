package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewOpenAIClientRequiresLocalBaseURL(t *testing.T) {
	if _, err := NewOpenAIClient("", ""); err == nil {
		t.Fatal("expected missing base URL error")
	}
}

func TestOpenAIClientForwardsRequiredToolChoice(t *testing.T) {
	var toolChoice string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ToolChoice string `json:"tool_choice"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		toolChoice = request.ToolChoice
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok","tool_calls":[]},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	client, err := NewOpenAIClient("", server.URL)
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	_, err = client.Generate(context.Background(), &GenerateRequest{
		Model: "default", Tools: []ToolDefinition{{Type: "function"}}, ToolChoice: "required",
	})
	if err != nil || toolChoice != "required" {
		t.Fatalf("Generate: tool_choice=%q err=%v", toolChoice, err)
	}
}

func TestOpenAIClientReportsHTTPAndResponseErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		code int
		want string
	}{
		{"http error", "llama unavailable", http.StatusServiceUnavailable, "status 503"},
		{"malformed response", "{", http.StatusOK, "decode response"},
		{"missing choices", `{"choices":[]}`, http.StatusOK, "no choices"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.code)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := NewOpenAIClient("", server.URL)
			if err != nil {
				t.Fatalf("NewOpenAIClient: %v", err)
			}
			_, err = client.Generate(context.Background(), &GenerateRequest{Model: "default"})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestOpenAIClientStructuredToolContract(t *testing.T) {
	var request struct {
		Model             string           `json:"model"`
		Messages          []openAIMessage  `json:"messages"`
		Tools             []ToolDefinition `json:"tools"`
		ToolChoice        string           `json:"tool_choice"`
		ParallelToolCalls *bool            `json:"parallel_tool_calls"`
		MaxTokens         int              `json:"max_tokens"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer local-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"store_memory","arguments":"{\"content\":\"likes espresso\"}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer server.Close()

	client, err := NewOpenAIClient("local-secret", server.URL+"/")
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	definition := ToolDefinition{Type: "function", Function: FunctionDefinition{
		Name: "store_memory", Parameters: json.RawMessage(`{"type":"object"}`),
	}}
	response, err := client.Generate(context.Background(), &GenerateRequest{
		Model: "default",
		Messages: []Message{
			{Role: RoleUser, Content: "remember espresso"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "old-call", Type: "function", Function: FunctionCall{Name: "store_memory", Arguments: `{}`}}}},
			{Role: RoleTool, ToolCallID: "old-call", Content: `{"stored":true}`},
		},
		Tools:     []ToolDefinition{definition},
		MaxTokens: 4096,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if request.Model != "default" || request.ToolChoice != "auto" {
		t.Fatalf("unexpected request controls: %#v", request)
	}
	if request.ParallelToolCalls == nil || *request.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls = %#v, want false", request.ParallelToolCalls)
	}
	if request.MaxTokens != 4096 {
		t.Fatalf("max_tokens = %d, want 4096", request.MaxTokens)
	}
	if len(request.Tools) != 1 || len(request.Messages) != 3 {
		t.Fatalf("unexpected request payload: %#v", request)
	}
	if request.Messages[1].Content != nil || request.Messages[2].ToolCallID != "old-call" {
		t.Fatalf("structured history was not preserved: %#v", request.Messages)
	}
	if response.FinishReason != "tool_calls" || len(response.Message.ToolCalls) != 1 {
		t.Fatalf("unexpected response: %#v", response)
	}
	if response.Message.ToolCalls[0].ID != "call-1" {
		t.Fatalf("tool call id = %q", response.Message.ToolCalls[0].ID)
	}
}

func TestOpenAIClientOmitsAuthorizationWithoutKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	client, err := NewOpenAIClient("", server.URL)
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	response, err := client.Generate(context.Background(), &GenerateRequest{Model: "default"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Message.Content != "ok" {
		t.Fatalf("content = %q", response.Message.Content)
	}
}

func TestOpenAIClientHonorsContextCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()

	client, err := NewOpenAIClient("", server.URL)
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err = client.Generate(ctx, &GenerateRequest{Model: "default", MaxTokens: 1})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Generate error = %v, want context deadline exceeded", err)
	}
	select {
	case <-started:
	default:
		t.Fatal("request did not reach the test server")
	}
}
