package gateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/openclaw/openclaw/go/internal/channels"
	"github.com/openclaw/openclaw/go/internal/state"
)

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
