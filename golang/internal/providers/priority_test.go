package providers

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPriorityGateRunsWaitingForegroundBeforeNextBackground(t *testing.T) {
	gate := NewPriorityGate()
	releaseBackground, err := gate.acquire(context.Background(), BackgroundWork)
	if err != nil {
		t.Fatal(err)
	}
	order := make(chan string, 2)
	foregroundStarted := make(chan struct{})
	go func() {
		close(foregroundStarted)
		release, acquireErr := gate.acquire(context.Background(), ForegroundWork)
		if acquireErr != nil {
			order <- "foreground-error"
			return
		}
		order <- "foreground"
		release()
	}()
	<-foregroundStarted

	gate.mu.Lock()
	for gate.waitingForeground != 1 {
		changed := gate.changed
		gate.mu.Unlock()
		select {
		case <-changed:
		case <-time.After(time.Second):
			t.Fatal("foreground did not enter the priority queue")
		}
		gate.mu.Lock()
	}
	gate.mu.Unlock()

	go func() {
		release, acquireErr := gate.acquire(context.Background(), BackgroundWork)
		if acquireErr != nil {
			order <- "background-error"
			return
		}
		order <- "background"
		release()
	}()
	releaseBackground()
	if first := <-order; first != "foreground" {
		t.Fatalf("first admitted work=%q", first)
	}
	if second := <-order; second != "background" {
		t.Fatalf("second admitted work=%q", second)
	}
}

func TestPriorityGateCancellationDoesNotBlockBackground(t *testing.T) {
	gate := NewPriorityGate()
	releaseBackground, err := gate.acquire(context.Background(), BackgroundWork)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, acquireErr := gate.acquire(ctx, ForegroundWork)
		done <- acquireErr
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire error=%v", err)
	}
	releaseBackground()
	release, err := gate.acquire(context.Background(), BackgroundWork)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

type blockingProvider struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (provider *blockingProvider) Generate(context.Context, *GenerateRequest) (*GenerateResponse, error) {
	provider.once.Do(func() { close(provider.started) })
	<-provider.release
	return &GenerateResponse{}, nil
}

func TestPriorityProviderReleasesGateAfterCall(t *testing.T) {
	gate := NewPriorityGate()
	base := &blockingProvider{started: make(chan struct{}), release: make(chan struct{})}
	background := NewPriorityProvider(base, gate, BackgroundWork)
	foreground := NewPriorityProvider(&blockingProvider{started: make(chan struct{}), release: make(chan struct{})}, gate, ForegroundWork)
	backgroundDone := make(chan struct{})
	go func() {
		_, _ = background.Generate(context.Background(), &GenerateRequest{})
		close(backgroundDone)
	}()
	<-base.started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := foreground.Generate(ctx, &GenerateRequest{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("foreground wait error=%v", err)
	}
	close(base.release)
	select {
	case <-backgroundDone:
	case <-time.After(time.Second):
		t.Fatal("background provider did not release the gate")
	}
}

func TestPriorityProviderPreservesOpenAIWireContract(t *testing.T) {
	base, err := NewOpenAIClient("", "http://127.0.0.1:8080/v1")
	if err != nil {
		t.Fatal(err)
	}
	provider := NewPriorityProvider(base, NewPriorityGate(), ForegroundWork)
	request := &GenerateRequest{
		Model:    "default",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	}
	body, err := MarshalGenerateRequest(provider, request)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["messages"]; !ok {
		t.Fatalf("priority-wrapped payload is missing messages: %s", body)
	}
	if _, ok := payload["Messages"]; ok {
		t.Fatalf("priority-wrapped payload leaked Go field name: %s", body)
	}
	if !IsLocalOpenAIProvider(provider) {
		t.Fatal("priority wrapper hid the retained local provider capability")
	}
}
