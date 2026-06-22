package providers

import (
	"context"
	"testing"
)

type stubProvider struct {
	id string
}

func (p *stubProvider) ID() string {
	return p.id
}

func (p *stubProvider) Generate(context.Context, *GenerateRequest) (*GenerateResponse, error) {
	return &GenerateResponse{Content: "ok"}, nil
}

func TestRegistryRegisterAndGet(t *testing.T) {
	registry := NewRegistry()
	provider := &stubProvider{id: "test"}
	registry.Register(provider)

	got, err := registry.Get("test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != provider {
		t.Fatal("registry returned a different provider")
	}
}

func TestRegistryRejectsUnknownProvider(t *testing.T) {
	registry := NewRegistry()
	if _, err := registry.Get("missing"); err == nil {
		t.Fatal("expected missing provider error")
	}
}
