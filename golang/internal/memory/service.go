package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/openclaw/openclaw/go/internal/providers"
	"github.com/openclaw/openclaw/go/internal/state"
	"github.com/openclaw/openclaw/go/internal/vector"
)

const (
	DefaultSearchLimit = 5
	MaxSearchLimit     = 20
	searchTimeout      = 15 * time.Second
	writeTimeout       = 2 * time.Minute
	modelTimeout       = 5 * time.Minute
)

var keywordPattern = regexp.MustCompile(`[\pL\pN_]+`)

type Service struct {
	store      *state.Store
	embedder   providers.Embedder
	indexID    string
	dimensions int
	minScore   float64
	now        func() time.Time
}

type Provenance struct {
	Origin          state.MemoryOrigin
	Source          state.MemorySource
	SourceHistoryID *int64
	SourceTraceID   *int64
}

type PreparedSearch struct {
	Query     string
	FTSQuery  string
	Embedding []float32
	Limit     int
}

func NewService(store *state.Store, embedder providers.Embedder, indexID string, dimensions int, minScore float64, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, embedder: embedder, indexID: indexID, dimensions: dimensions, minScore: minScore, now: now}
}

func (service *Service) PrepareWrite(ctx context.Context, kind state.MemoryKind, content string, provenance Provenance) (state.MemoryWrite, error) {
	content = strings.TrimSpace(content)
	if !state.ValidMemoryKind(kind) {
		return state.MemoryWrite{}, fmt.Errorf("kind must be profile, durable, or daily")
	}
	if content == "" {
		return state.MemoryWrite{}, fmt.Errorf("content must not be empty")
	}
	if provenance.Origin == "" || provenance.Source == "" {
		return state.MemoryWrite{}, fmt.Errorf("memory provenance is required")
	}
	requestCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	vectors, err := service.embedder.Embed(requestCtx, []string{"title: memory | text: " + content})
	cancel()
	if err != nil {
		return state.MemoryWrite{}, fmt.Errorf("embed memory content: %w", err)
	}
	if len(vectors) != 1 {
		return state.MemoryWrite{}, fmt.Errorf("embedding server returned %d vectors, want 1", len(vectors))
	}
	return state.MemoryWrite{
		Kind: kind, Content: content, OriginClass: provenance.Origin, SourceKind: provenance.Source,
		SourceHistoryID: provenance.SourceHistoryID, SourceTraceID: provenance.SourceTraceID,
		EmbeddingModel: service.indexID, Dimensions: service.dimensions, Embedding: vector.Pack(vectors[0]), Now: service.now(),
	}, nil
}

func (service *Service) Search(ctx context.Context, query string, keywords []string, limit int) ([]state.MemorySearchResult, error) {
	prepared, err := service.PrepareSearch(ctx, query, keywords, limit)
	if err != nil {
		return nil, err
	}
	return service.store.SearchMemories(ctx, service.indexID, service.dimensions, prepared.Embedding, prepared.FTSQuery, service.minScore, prepared.Limit)
}

func (service *Service) PrepareSearch(ctx context.Context, query string, keywords []string, limit int) (PreparedSearch, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return PreparedSearch{}, fmt.Errorf("query must not be empty")
	}
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		return PreparedSearch{}, fmt.Errorf("max_results must be at most %d", MaxSearchLimit)
	}
	requestCtx, cancel := context.WithTimeout(ctx, searchTimeout)
	vectors, err := service.embedder.Embed(requestCtx, []string{"task: search result | query: " + query})
	cancel()
	if err != nil {
		return PreparedSearch{}, fmt.Errorf("embed memory query: %w", err)
	}
	if len(vectors) != 1 {
		return PreparedSearch{}, fmt.Errorf("embedding server returned %d vectors, want 1", len(vectors))
	}
	ftsValues := keywords
	if len(ftsValues) == 0 {
		ftsValues = []string{query}
	}
	fts := SafeFTSQuery(ftsValues)
	return PreparedSearch{Query: query, FTSQuery: fts, Embedding: vectors[0], Limit: limit}, nil
}

func (service *Service) SearchWithModel(ctx context.Context, model providers.Provider, query string, limit int) ([]state.MemorySearchResult, error) {
	planTool := providers.ToolDefinition{Type: "function", Function: providers.FunctionDefinition{Name: "plan_memory_search", Description: "Plan one local memory search.", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"semantic_query":{"type":"string"},"keywords":{"type":"array","maxItems":12,"items":{"type":"string"}}},"required":["semantic_query","keywords"]}`)}}
	modelCtx, modelCancel := context.WithTimeout(ctx, modelTimeout)
	response, err := model.Generate(modelCtx, &providers.GenerateRequest{Model: "default", MaxTokens: 256, ToolChoice: "required", Tools: []providers.ToolDefinition{planTool}, Messages: []providers.Message{{Role: providers.RoleSystem, Content: "Formulate a semantic query and literal keywords for searching the owner's local memory. Always call plan_memory_search."}, {Role: providers.RoleUser, Content: query}}})
	modelCancel()
	if err != nil {
		return nil, fmt.Errorf("plan memory search: %w", err)
	}
	call, err := oneTool(response, "plan_memory_search")
	if err != nil {
		return nil, err
	}
	var plan struct {
		SemanticQuery string   `json:"semantic_query"`
		Keywords      []string `json:"keywords"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &plan); err != nil {
		return nil, fmt.Errorf("decode memory search plan: %w", err)
	}
	candidates, err := service.Search(ctx, plan.SemanticQuery, plan.Keywords, MaxSearchLimit)
	if err != nil {
		return nil, err
	}
	selectTool := providers.ToolDefinition{Type: "function", Function: providers.FunctionDefinition{Name: "select_memories", Description: "Select relevant Memory IDs in relevance order.", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"memory_ids":{"type":"array","maxItems":20,"items":{"type":"integer","minimum":1}}},"required":["memory_ids"]}`)}}
	payload, _ := json.Marshal(map[string]any{"query": query, "candidates": candidates, "max_results": limit})
	modelCtx, modelCancel = context.WithTimeout(ctx, modelTimeout)
	response, err = model.Generate(modelCtx, &providers.GenerateRequest{Model: "default", MaxTokens: 256, ToolChoice: "required", Tools: []providers.ToolDefinition{selectTool}, Messages: []providers.Message{{Role: providers.RoleSystem, Content: "Select only memories that materially answer the query. Always call select_memories, including with an empty list."}, {Role: providers.RoleUser, Content: string(payload)}}})
	modelCancel()
	if err != nil {
		return nil, fmt.Errorf("rerank memory search: %w", err)
	}
	call, err = oneTool(response, "select_memories")
	if err != nil {
		return nil, err
	}
	var selected struct {
		IDs []int64 `json:"memory_ids"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &selected); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		return nil, fmt.Errorf("max_results must be at most %d", MaxSearchLimit)
	}
	byID := map[int64]state.MemorySearchResult{}
	for _, item := range candidates {
		byID[item.ID] = item
	}
	result := make([]state.MemorySearchResult, 0, min(limit, len(selected.IDs)))
	seen := map[int64]struct{}{}
	for _, id := range selected.IDs {
		if len(result) == limit {
			break
		}
		if _, ok := seen[id]; ok {
			return nil, fmt.Errorf("model selected duplicate Memory ID %d", id)
		}
		seen[id] = struct{}{}
		item, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("model selected unknown Memory ID %d", id)
		}
		result = append(result, item)
	}
	return result, nil
}

func oneTool(response *providers.GenerateResponse, name string) (providers.ToolCall, error) {
	if response == nil || len(response.Message.ToolCalls) != 1 || strings.TrimSpace(response.Message.Content) != "" {
		return providers.ToolCall{}, fmt.Errorf("model did not return exactly one %s call", name)
	}
	call := response.Message.ToolCalls[0]
	if call.Type != "function" || call.Function.Name != name || call.ID == "" {
		return providers.ToolCall{}, fmt.Errorf("model returned invalid %s call", name)
	}
	return call, nil
}

func SafeFTSQuery(values []string) string {
	seen := map[string]struct{}{}
	terms := make([]string, 0, len(values))
	for _, value := range values {
		for _, term := range keywordPattern.FindAllString(strings.ToLower(value), -1) {
			if len([]rune(term)) < 2 {
				continue
			}
			if _, ok := seen[term]; ok {
				continue
			}
			seen[term] = struct{}{}
			terms = append(terms, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
			if len(terms) == 16 {
				return strings.Join(terms, " OR ")
			}
		}
	}
	return strings.Join(terms, " OR ")
}

func (service *Service) Store() *state.Store { return service.store }
func (service *Service) IndexID() string     { return service.indexID }
func (service *Service) Dimensions() int     { return service.dimensions }
func (service *Service) MinScore() float64   { return service.minScore }

func (service *Service) PrepareReindex(ctx context.Context) ([]state.MemoryReindexEntry, error) {
	active, err := service.store.ListAllActiveMemories(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]state.MemoryReindexEntry, 0, len(active))
	for start := 0; start < len(active); start += 16 {
		end := min(start+16, len(active))
		inputs := make([]string, end-start)
		for index, item := range active[start:end] {
			inputs[index] = "title: memory | text: " + item.Content
		}
		requestCtx, cancel := context.WithTimeout(ctx, writeTimeout)
		vectors, err := service.embedder.Embed(requestCtx, inputs)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("embed memory reindex batch: %w", err)
		}
		if len(vectors) != len(inputs) {
			return nil, fmt.Errorf("memory reindex embedding count mismatch")
		}
		for index, item := range active[start:end] {
			entries = append(entries, state.MemoryReindexEntry{MemoryID: item.ID, RevisionID: item.RevisionID, Content: item.Content, EmbeddingModel: service.indexID, Dimensions: service.dimensions, Embedding: vector.Pack(vectors[index])})
		}
	}
	return entries, nil
}
