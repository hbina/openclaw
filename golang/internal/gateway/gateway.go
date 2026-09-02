package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/openclaw/openclaw/go/internal/channels"
	"github.com/openclaw/openclaw/go/internal/memory"
	"github.com/openclaw/openclaw/go/internal/state"
)

const reminderPollInterval = time.Minute

// Gateway exposes the HTTP health and orchestration endpoints.
type Gateway struct {
	agent             *Agent
	chanReg           *channels.Registry
	store             *state.Store
	maintainer        *memory.Maintainer
	maintenanceCancel context.CancelFunc
	maintenanceDone   chan struct{}
	server            *http.Server
}

func NewGateway(agent *Agent, chanReg *channels.Registry, store *state.Store, maintainers ...*memory.Maintainer) *Gateway {
	mux := http.NewServeMux()
	g := &Gateway{
		agent:   agent,
		chanReg: chanReg,
		store:   store,
	}
	if len(maintainers) > 0 {
		g.maintainer = maintainers[0]
	}

	mux.HandleFunc("/healthz", g.healthCheck)
	mux.HandleFunc("/chat", g.chat)

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

	// Start Reminder Loop
	go g.startReminderLoop(ctx)
	if g.maintainer != nil {
		maintenanceCtx, cancel := context.WithCancel(ctx)
		g.maintenanceCancel = cancel
		g.maintenanceDone = make(chan struct{})
		go func() {
			defer close(g.maintenanceDone)
			g.maintainer.RunLoop(maintenanceCtx)
		}()
	}

	return nil
}

func (g *Gateway) startReminderLoop(ctx context.Context) {
	ticker := time.NewTicker(reminderPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			due, err := g.store.FetchDueReminders()
			if err != nil {
				log.Printf("Error fetching due reminders: %v", err)
				continue
			}

			for _, r := range due {
				if err := g.agent.DeliverReminder(ctx, r); err != nil {
					log.Printf("Failed to deliver reminder %d: %v", r.ID, err)
				}
			}
		}
	}
}

func (g *Gateway) Stop(ctx context.Context) error {
	log.Println("Stopping Gateway...")
	if g.maintenanceCancel != nil {
		g.maintenanceCancel()
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Stop channels
	if err := g.chanReg.StopAll(ctx); err != nil {
		log.Printf("Error stopping channels: %v", err)
	}

	serverErr := g.server.Shutdown(shutdownCtx)
	if g.maintenanceDone != nil {
		select {
		case <-g.maintenanceDone:
		case <-shutdownCtx.Done():
			return errors.Join(serverErr, fmt.Errorf("memory maintenance did not stop before shutdown deadline: %w", shutdownCtx.Err()))
		}
	}
	return serverErr
}

func (g *Gateway) healthCheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (g *Gateway) chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		SenderID string `json:"sender_id"`
		Message  string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.Message == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}
	if body.SenderID == "" {
		body.SenderID = "cli-user"
	}

	prepared, err := g.agent.PrepareChat(r.Context(), ChatInput{
		ChannelID: "cli",
		SenderID:  body.SenderID,
		Content:   body.Message,
	})
	if err != nil {
		if prepared.TraceID != 0 {
			w.Header().Set("X-OpenClaw-Trace-ID", fmt.Sprintf("%d", prepared.TraceID))
		}
		log.Printf("chat endpoint error: %v", err)
		http.Error(w, "agent error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer prepared.Release()

	deliveryID, err := g.store.PrepareDelivery(r.Context(), prepared.TraceID, prepared.OutputEventID, "cli", body.SenderID, prepared.Content)
	if err != nil {
		log.Printf("trace %d prepare HTTP delivery: %v", prepared.TraceID, err)
		_ = g.store.FinishTrace(context.Background(), prepared.TraceID, "failed", "delivery", err)
		http.Error(w, "agent error", http.StatusInternalServerError)
		return
	}
	if err := g.store.MarkDeliveryAttempting(r.Context(), deliveryID); err != nil {
		log.Printf("trace %d mark HTTP delivery: %v", prepared.TraceID, err)
		_ = g.store.FinishTrace(context.Background(), prepared.TraceID, "failed", "delivery", err)
		http.Error(w, "agent error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-OpenClaw-Trace-ID", fmt.Sprintf("%d", prepared.TraceID))
	if err := json.NewEncoder(w).Encode(map[string]string{"reply": prepared.Content}); err != nil {
		_ = g.store.FailDelivery(context.Background(), prepared.TraceID, deliveryID, err)
		return
	}
	if err := g.store.CompleteDeliveryIndexed(context.Background(), prepared.TraceID, deliveryID, "http", "cli", body.SenderID, prepared.Content, prepared.StartHistoryID, prepared.Chunks); err != nil {
		log.Printf("trace %d HTTP response sent but finalization failed: %v", prepared.TraceID, err)
		return
	}
	log.Printf("Response trace %d delivered over HTTP", prepared.TraceID)
}
