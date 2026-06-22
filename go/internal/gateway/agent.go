package gateway

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/openclaw/openclaw/go/internal/channels"
	"github.com/openclaw/openclaw/go/internal/config"
	"github.com/openclaw/openclaw/go/internal/memory"
	"github.com/openclaw/openclaw/go/internal/providers"
	"github.com/openclaw/openclaw/go/internal/state"
)

const (
	defaultContextWindow = 100_000 // estimated token capacity for the configured model
	defaultReserveTokens = 16_384  // tokens reserved for compaction summary + next response
	defaultHistoryLimit  = 20      // turns to load when no config override is present
)

// Agent runs the primary interaction loop.
type Agent struct {
	provider    providers.Provider
	memoryCore  *memory.Core
	chanReg     *channels.Registry
	store       *state.Store
	cfg         *config.Config
	personality string
}

func NewAgent(
	provider providers.Provider,
	memoryCore *memory.Core,
	chanReg *channels.Registry,
	store *state.Store,
	cfg *config.Config,
	personality string,
) *Agent {
	return &Agent{
		provider:    provider,
		memoryCore:  memoryCore,
		chanReg:     chanReg,
		store:       store,
		cfg:         cfg,
		personality: personality,
	}
}

// resolveHistoryLimit returns the configured turn limit for the given channel and sender.
// channelIDs ending in "-dm" or the "cli" virtual channel use DMHistoryLimit;
// all others use HistoryLimit. Falls back to defaultHistoryLimit when unconfigured.
func (a *Agent) resolveHistoryLimit(channelID, senderID string) int {
	isDM := channelID == "cli" || strings.HasSuffix(channelID, "-dm")

	var entry *config.ChannelEntry
	if a.cfg != nil {
		switch {
		case strings.HasPrefix(channelID, "telegram"):
			e := a.cfg.Channels.Telegram
			entry = &e
		case strings.HasPrefix(channelID, "whatsapp"):
			e := a.cfg.Channels.WhatsApp
			entry = &e
		case strings.HasPrefix(channelID, "discord"):
			e := a.cfg.Channels.Discord
			entry = &e
		}
	}

	if entry == nil {
		return defaultHistoryLimit
	}

	if isDM {
		if dm, ok := entry.DMs[senderID]; ok && dm.HistoryLimit != nil {
			return *dm.HistoryLimit
		}
		if entry.DMHistoryLimit != nil {
			return *entry.DMHistoryLimit
		}
	} else {
		if entry.HistoryLimit != nil {
			return *entry.HistoryLimit
		}
	}
	return defaultHistoryLimit
}

// estimateTokens approximates token count using the 4-chars-per-token heuristic,
// matching the Node.js fallback in the upstream implementation.
func estimateTokens(messages []providers.Message) int {
	total := 0
	for _, m := range messages {
		total += len(m.Content)
	}
	return total / 4
}

func shouldCompact(totalTokens, contextWindow, reserveTokens int) bool {
	return totalTokens > contextWindow-reserveTokens
}

const summarizationSystemPrompt = `You are a conversation summarization assistant for a personal AI secretary bot.
Produce a concise summary of the conversation below. Include:
- What the user has asked for or is trying to accomplish
- Reminders that have been set and their details (time, message)
- Key facts or preferences the user has shared about themselves
- Ongoing topics or unresolved questions
Be brief and factual. This summary will be injected as context for future turns.`

// runCompaction summarizes older turns, saves the compaction record, and trims the
// history table. It is called after each exchange when the estimated context exceeds
// defaultContextWindow - defaultReserveTokens tokens.
func (a *Agent) runCompaction(ctx context.Context, channelID, senderID string) error {
	// Load the full un-limited history for this session to find the cut point.
	allHistory, err := a.store.GetRecentHistory(ctx, channelID, senderID, 100_000, 0)
	if err != nil {
		return fmt.Errorf("compaction: load history: %w", err)
	}
	if len(allHistory) < 3 {
		return nil // too few turns to compact meaningfully
	}

	// Find the cut point: keep the last keepRecentChars of content,
	// summarize everything before it.
	const keepRecentChars = defaultReserveTokens * 4
	keepFrom := 0
	accumulated := 0
	for i := len(allHistory) - 1; i >= 0; i-- {
		accumulated += len(allHistory[i].Content)
		if accumulated > keepRecentChars {
			keepFrom = i + 1
			break
		}
	}
	if keepFrom == 0 || keepFrom >= len(allHistory) {
		return nil // entire history fits within the keep window
	}

	// Build the message list for the summarization call.
	summaryMessages := make([]providers.Message, 0, len(allHistory)+1)
	summaryMessages = append(summaryMessages, providers.Message{
		Role:    providers.RoleSystem,
		Content: summarizationSystemPrompt,
	})
	for _, t := range allHistory {
		role := providers.RoleUser
		if t.Role == "assistant" {
			role = providers.RoleAssistant
		}
		summaryMessages = append(summaryMessages, providers.Message{Role: role, Content: t.Content})
	}

	resp, err := a.provider.Generate(ctx, &providers.GenerateRequest{
		Model:    "default",
		Messages: summaryMessages,
	})
	if err != nil {
		return fmt.Errorf("compaction: summarization: %w", err)
	}

	firstKeptID := allHistory[keepFrom].ID
	tokensBefore := estimateTokens(summaryMessages)

	if _, err := a.store.SaveCompaction(ctx, channelID, senderID, resp.Content, tokensBefore, firstKeptID); err != nil {
		return fmt.Errorf("compaction: save: %w", err)
	}
	if err := a.store.TrimHistoryBefore(ctx, channelID, senderID, firstKeptID); err != nil {
		return fmt.Errorf("compaction: trim: %w", err)
	}
	log.Printf("Compacted %s/%s: summarized %d turns, retained from history id %d", channelID, senderID, keepFrom, firstKeptID)
	return nil
}

// Chat generates a reply for the given channel+sender, loads and saves conversation
// history in SQLite, injects any compaction summary as context, and triggers
// compaction when the estimated context exceeds the configured threshold.
func (a *Agent) Chat(ctx context.Context, channelID, senderID, content string) (string, error) {
	// Determine the lower-bound history row from the latest compaction (if any).
	compaction, err := a.store.GetLatestCompaction(ctx, channelID, senderID)
	if err != nil {
		log.Printf("Failed to load compaction state: %v", err)
	}
	firstKeptID := 0
	if compaction != nil {
		firstKeptID = compaction.FirstKeptID
	}

	limit := a.resolveHistoryLimit(channelID, senderID)
	history, err := a.store.GetRecentHistory(ctx, channelID, senderID, limit, firstKeptID)
	if err != nil {
		log.Printf("Failed to load conversation history: %v", err)
	}

	systemPrompt := fmt.Sprintf(
		"You are a helpful assistant talking to User '%s' on Channel '%s'. When setting reminders, you must explicitly use these exact IDs.\n",
		senderID, channelID,
	)
	if a.personality != "" {
		systemPrompt += "\nFollow this personality and identity context:\n" + a.personality + "\n"
	}

	messages := []providers.Message{
		{Role: providers.RoleSystem, Content: systemPrompt},
	}

	// Inject the compaction summary as additional system context before history.
	if compaction != nil {
		messages = append(messages, providers.Message{
			Role:    providers.RoleSystem,
			Content: "Previous conversation summary:\n" + compaction.Summary,
		})
	}

	for _, turn := range history {
		role := providers.RoleUser
		if turn.Role == "assistant" {
			role = providers.RoleAssistant
		}
		// ContentToolCall / ContentToolResult turns are stored as JSON payloads.
		// Fall back to the raw content string until the provider layer supports
		// structured tool-call messages natively.
		messages = append(messages, providers.Message{Role: role, Content: turn.Content})
	}
	messages = append(messages, providers.Message{Role: providers.RoleUser, Content: content})

	resp, err := a.provider.Generate(ctx, &providers.GenerateRequest{
		Model:    "default",
		Messages: messages,
	})
	if err != nil {
		return "", fmt.Errorf("agent generation failed: %w", err)
	}

	if err := a.store.SaveConversationTurn(ctx, channelID, senderID, "user", content); err != nil {
		log.Printf("Failed to save user turn: %v", err)
	}
	if err := a.store.SaveConversationTurn(ctx, channelID, senderID, "assistant", resp.Content); err != nil {
		log.Printf("Failed to save assistant turn: %v", err)
	}

	// Estimate context size including the new response and trigger compaction if needed.
	totalTokens := estimateTokens(messages) + len(resp.Content)/4
	if shouldCompact(totalTokens, defaultContextWindow, defaultReserveTokens) {
		if err := a.runCompaction(ctx, channelID, senderID); err != nil {
			log.Printf("Compaction failed for %s/%s: %v", channelID, senderID, err)
		}
	}

	return resp.Content, nil
}

// HandleMessage is the callback triggered by any channel receiving a message.
func (a *Agent) HandleMessage(ctx context.Context, msg *channels.Message) error {
	log.Printf("Agent received message from %s [%s]: %s\n", msg.ChannelID, msg.SenderID, msg.Content)

	reply, err := a.Chat(ctx, msg.ChannelID, msg.SenderID, msg.Content)
	if err != nil {
		return err
	}

	ch, err := a.chanReg.Get(msg.ChannelID)
	if err != nil {
		return fmt.Errorf("channel %s not found: %w", msg.ChannelID, err)
	}

	return ch.SendMessage(ctx, msg.SenderID, reply)
}
