package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/openclaw/go/internal/channels"
	"github.com/openclaw/openclaw/go/internal/state"
)

func TestReminderPollInterval(t *testing.T) {
	if reminderPollInterval != time.Minute {
		t.Fatalf("reminder poll interval = %s, want %s", reminderPollInterval, time.Minute)
	}
}

func TestGatewayChatReturnsCompletedTraceID(t *testing.T) {
	agent, store := newTestAgent(t, &recordingProvider{}, nil, "Be concise.")
	gateway := NewGateway(agent, channels.NewRegistry(), store)
	request := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{"sender_id":"owner","message":"hello"}`))
	response := httptest.NewRecorder()
	gateway.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	traceID := response.Header().Get("X-OpenClaw-Trace-ID")
	if traceID == "" {
		t.Fatal("missing trace id header")
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["reply"] != "assistant reply" {
		t.Fatalf("body=%#v", body)
	}
	items, err := store.ListResponseTraces(context.Background(), state.TraceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "completed" {
		t.Fatalf("traces=%#v", items)
	}
}

func TestGatewayHealth(t *testing.T) {
	store, err := state.NewStore(filepath.Join(t.TempDir(), "gateway.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	t.Setenv("PORT", "19001")
	gateway := NewGateway(nil, channels.NewRegistry(), store)
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()

	gateway.server.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d", response.Code)
	}
	if response.Body.String() != "OK" {
		t.Fatalf("health body = %q", response.Body.String())
	}
	if gateway.server.Addr != ":19001" {
		t.Fatalf("gateway address = %q", gateway.server.Addr)
	}
}
