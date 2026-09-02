package providers

import (
	"context"
	"fmt"
	"sync"
)

type WorkPriority int

const (
	ForegroundWork WorkPriority = iota
	BackgroundWork
)

// PriorityGate gives queued owner-facing work precedence between background
// calls. A call already in flight is allowed to finish; callers remain bounded
// by their own contexts.
type PriorityGate struct {
	mu                sync.Mutex
	changed           chan struct{}
	activeForeground  int
	waitingForeground int
	backgroundActive  bool
}

func NewPriorityGate() *PriorityGate {
	return &PriorityGate{changed: make(chan struct{})}
}

func (gate *PriorityGate) acquire(ctx context.Context, priority WorkPriority) (func(), error) {
	if gate == nil {
		return func() {}, nil
	}
	if priority != ForegroundWork && priority != BackgroundWork {
		return nil, fmt.Errorf("invalid work priority %d", priority)
	}
	gate.mu.Lock()
	if priority == ForegroundWork {
		gate.waitingForeground++
		gate.signalLocked()
	}
	for !gate.available(priority) {
		changed := gate.changed
		gate.mu.Unlock()
		select {
		case <-ctx.Done():
			gate.mu.Lock()
			if priority == ForegroundWork {
				gate.waitingForeground--
				gate.signalLocked()
			}
			gate.mu.Unlock()
			return nil, ctx.Err()
		case <-changed:
		}
		gate.mu.Lock()
	}
	if priority == ForegroundWork {
		gate.waitingForeground--
		gate.activeForeground++
	} else {
		gate.backgroundActive = true
	}
	gate.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			gate.mu.Lock()
			if priority == ForegroundWork {
				gate.activeForeground--
			} else {
				gate.backgroundActive = false
			}
			gate.signalLocked()
			gate.mu.Unlock()
		})
	}, nil
}

func (gate *PriorityGate) available(priority WorkPriority) bool {
	if priority == ForegroundWork {
		return !gate.backgroundActive
	}
	return !gate.backgroundActive && gate.activeForeground == 0 && gate.waitingForeground == 0
}

func (gate *PriorityGate) signalLocked() {
	close(gate.changed)
	gate.changed = make(chan struct{})
}

type PriorityProvider struct {
	base     Provider
	gate     *PriorityGate
	priority WorkPriority
}

func NewPriorityProvider(base Provider, gate *PriorityGate, priority WorkPriority) *PriorityProvider {
	return &PriorityProvider{base: base, gate: gate, priority: priority}
}

func (provider *PriorityProvider) Generate(ctx context.Context, request *GenerateRequest) (*GenerateResponse, error) {
	release, err := provider.gate.acquire(ctx, provider.priority)
	if err != nil {
		return nil, err
	}
	defer release()
	return provider.base.Generate(ctx, request)
}

// MarshalGenerateRequest preserves the base provider's exact wire contract.
// Tracing prepares the request before Generate acquires the priority gate, so
// falling back to json.Marshal on this wrapper would emit Go field names such
// as "Messages" instead of the OpenAI-compatible "messages" field.
func (provider *PriorityProvider) MarshalGenerateRequest(request *GenerateRequest) ([]byte, error) {
	return MarshalGenerateRequest(provider.base, request)
}

func (provider *PriorityProvider) unwrapProvider() Provider {
	return provider.base
}

func (provider *PriorityProvider) ContextSize(ctx context.Context) (int, error) {
	sizer, ok := provider.base.(PromptSizer)
	if !ok {
		return 0, fmt.Errorf("provider does not support prompt sizing")
	}
	release, err := provider.gate.acquire(ctx, provider.priority)
	if err != nil {
		return 0, err
	}
	defer release()
	return sizer.ContextSize(ctx)
}

func (provider *PriorityProvider) CountPromptTokens(ctx context.Context, messages []Message, tools []ToolDefinition) (int, error) {
	sizer, ok := provider.base.(PromptSizer)
	if !ok {
		return 0, fmt.Errorf("provider does not support prompt sizing")
	}
	release, err := provider.gate.acquire(ctx, provider.priority)
	if err != nil {
		return 0, err
	}
	defer release()
	return sizer.CountPromptTokens(ctx, messages, tools)
}

type PriorityEmbedder struct {
	base     Embedder
	gate     *PriorityGate
	priority WorkPriority
}

func NewPriorityEmbedder(base Embedder, gate *PriorityGate, priority WorkPriority) *PriorityEmbedder {
	return &PriorityEmbedder{base: base, gate: gate, priority: priority}
}

func (embedder *PriorityEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	release, err := embedder.gate.acquire(ctx, embedder.priority)
	if err != nil {
		return nil, err
	}
	defer release()
	return embedder.base.Embed(ctx, inputs)
}

func (embedder *PriorityEmbedder) Tokenize(ctx context.Context, content string) ([]int, error) {
	release, err := embedder.gate.acquire(ctx, embedder.priority)
	if err != nil {
		return nil, err
	}
	defer release()
	return embedder.base.Tokenize(ctx, content)
}

func (embedder *PriorityEmbedder) Detokenize(ctx context.Context, tokens []int) (string, error) {
	release, err := embedder.gate.acquire(ctx, embedder.priority)
	if err != nil {
		return "", err
	}
	defer release()
	return embedder.base.Detokenize(ctx, tokens)
}
