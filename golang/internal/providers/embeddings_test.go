package providers

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddingClientBatchesNormalizesAndPreservesIndexes(t *testing.T) {
	var request struct {
		Model          string   `json:"model"`
		Input          []string `json:"input"`
		EncodingFormat string   `json:"encoding_format"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer local-key" {
			t.Fatalf("authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"data":[
			{"index":1,"embedding":[0,3,4]},
			{"index":0,"embedding":[2,0,0]}
		]}`))
	}))
	defer server.Close()

	client, err := NewEmbeddingClient("local-key", server.URL+"/v1", "default", 3)
	if err != nil {
		t.Fatalf("NewEmbeddingClient: %v", err)
	}
	vectors, err := client.Embed(context.Background(), []string{"one", "two"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if request.Model != "default" || request.EncodingFormat != "float" || len(request.Input) != 2 {
		t.Fatalf("request = %#v", request)
	}
	if vectors[0][0] != 1 || math.Abs(float64(vectors[1][1])-0.6) > 1e-6 ||
		math.Abs(float64(vectors[1][2])-0.8) > 1e-6 {
		t.Fatalf("vectors = %#v", vectors)
	}
}

func TestEmbeddingClientTokenizeAndDetokenize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tokenize":
			_, _ = w.Write([]byte(`{"tokens":[1,2,3]}`))
		case "/detokenize":
			_, _ = w.Write([]byte(`{"content":"restored"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewEmbeddingClient("", server.URL+"/v1", "default", 3)
	if err != nil {
		t.Fatalf("NewEmbeddingClient: %v", err)
	}
	tokens, err := client.Tokenize(context.Background(), "text")
	if err != nil || len(tokens) != 3 {
		t.Fatalf("Tokenize: tokens=%v err=%v", tokens, err)
	}
	content, err := client.Detokenize(context.Background(), tokens)
	if err != nil || content != "restored" {
		t.Fatalf("Detokenize: content=%q err=%v", content, err)
	}
}

func TestEmbeddingClientRejectsInvalidVectors(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "wrong count", body: `{"data":[]}`, want: "count"},
		{name: "duplicate index", body: `{"data":[{"index":0,"embedding":[1,0]},{"index":0,"embedding":[0,1]}]}`, want: "invalid index"},
		{name: "wrong dimensions", body: `{"data":[{"index":0,"embedding":[1]}]}`, want: "dimension"},
		{name: "zero norm", body: `{"data":[{"index":0,"embedding":[0,0]}]}`, want: "zero norm"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := NewEmbeddingClient("", server.URL, "default", 2)
			if err != nil {
				t.Fatalf("NewEmbeddingClient: %v", err)
			}
			inputs := []string{"one"}
			if test.name == "duplicate index" {
				inputs = append(inputs, "two")
			}
			_, err = client.Embed(context.Background(), inputs)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Embed error = %v, want %q", err, test.want)
			}
		})
	}
}
