package gateway

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/openclaw/openclaw/go/internal/channels"
	"github.com/openclaw/openclaw/go/internal/state"
)

const reminderPollInterval = time.Minute

// Gateway exposes the HTTP health and orchestration endpoints.
type Gateway struct {
	agent   *Agent
	chanReg *channels.Registry
	store   *state.Store
	server  *http.Server
}

func NewGateway(agent *Agent, chanReg *channels.Registry, store *state.Store) *Gateway {
	mux := http.NewServeMux()
	g := &Gateway{
		agent:   agent,
		chanReg: chanReg,
		store:   store,
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
				ch, err := g.chanReg.Get(r.ChannelID)
				if err != nil {
					log.Printf("Failed to get channel %s for reminder: %v", r.ChannelID, err)
					continue
				}
				notification := "⏰ **Reminder!** ⏰\n\n" + r.Message
				if err := ch.SendMessage(ctx, r.SenderID, notification); err != nil {
					log.Printf("Failed to deliver reminder to %s: %v", r.SenderID, err)
					continue
				}
				if err := g.store.CompleteReminder(r, time.Now()); err != nil {
					log.Printf("Failed to mark reminder %d delivered: %v", r.ID, err)
				}
			}
		}
	}
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

	reply, err := g.agent.Chat(r.Context(), "cli", body.SenderID, body.Message)
	if err != nil {
		log.Printf("chat endpoint error: %v", err)
		http.Error(w, "agent error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"reply": reply})
}
