package gateway

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/openclaw/openclaw/go/internal/channels"
)

// Gateway exposes the HTTP health and orchestration endpoints.
type Gateway struct {
	agent   *Agent
	chanReg *channels.Registry
	server  *http.Server
}

func NewGateway(agent *Agent, chanReg *channels.Registry) *Gateway {
	mux := http.NewServeMux()
	g := &Gateway{
		agent:   agent,
		chanReg: chanReg,
	}

	mux.HandleFunc("/healthz", g.healthCheck)

	port := os.Getenv("PORT")
	if port == "" {
		port = "18789"
	}

	g.server = &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	return g
}

func (g *Gateway) Start(ctx context.Context) error {
	log.Printf("Starting Gateway on port %s...\n", g.server.Addr)
	
	// Start channels
	if err := g.chanReg.StartAll(ctx, g.agent.HandleMessage); err != nil {
		return err
	}

	// Start HTTP Server asynchronously
	go func() {
		if err := g.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Gateway server error: %v", err)
		}
	}()

	return nil
}

func (g *Gateway) Stop(ctx context.Context) error {
	log.Println("Stopping Gateway...")
	
	// Stop channels
	if err := g.chanReg.StopAll(ctx); err != nil {
		log.Printf("Error stopping channels: %v", err)
	}

	// Give the HTTP server a brief period to shutdown gracefully
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	
	return g.server.Shutdown(shutdownCtx)
}

func (g *Gateway) healthCheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
