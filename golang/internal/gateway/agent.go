package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/openclaw/openclaw/go/internal/channels"
	"github.com/openclaw/openclaw/go/internal/config"
	"github.com/openclaw/openclaw/go/internal/providers"
	"github.com/openclaw/openclaw/go/internal/state"
	"github.com/openclaw/openclaw/go/internal/tools"
)

const (
	defaultContextWindow = 100_000 // estimated token capacity for the configured model
	defaultReserveTokens = 16_384  // tokens reserved for compaction summary + next response
	defaultHistoryLimit  = 20      // turns to load when no config override is present
	maxToolRounds        = 4
	defaultMaxTokens     = 4_096
	compactionMaxTokens  = 2_048
	modelRequestTimeout  = 5 * time.Minute
)

const uncommittedReminderNote = "Note: no reminder change was committed in this turn, so your stored reminders are unchanged."

var unbackedReminderCommitmentPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:i\s*['’]?ll|i will)\s+(?:make sure to\s+)?(?:remind|ping|follow up|follow-up|check back|circle back)\b`),
	regexp.MustCompile(`(?i)\b(?:i\s*['’]?ll|i will)\s+(?:successfully\s+)?(?:set|create|schedule|add|update|change|remove|delete|cancel)\b[^.!?\n]{0,80}\breminders?\b`),
	regexp.MustCompile(`(?i)\b(?:i\s*['’]?ve|i have|i)\s+(?:successfully\s+)?(?:set|created|scheduled|added|updated|changed|removed|deleted|cancelled|canceled)\b[^.!?\n]{0,80}\breminders?\b`),
}

// Agent runs the primary interaction loop.
type Agent struct {
	provider          providers.Provider
	tools             *tools.Executor
	chanReg           *channels.Registry
	store             *state.Store
	cfg               *config.Config
	soul              string
	identity          string
	location          *time.Location
	conversationLocks *conversationLockManager
}

func NewAgent(
	provider providers.Provider,
	chanReg *channels.Registry,
	store *state.Store,
	cfg *config.Config,
	location *time.Location,
) *Agent {
	if location == nil {
		location = time.Local
	}
	var soul, identity string
	if cfg != nil {
		soul = cfg.Agents.Defaults.Soul
		identity = cfg.Agents.Defaults.Identity
	}
	return &Agent{
		provider:          provider,
		tools:             tools.NewExecutor(store, time.Now, location),
		chanReg:           chanReg,
		store:             store,
		cfg:               cfg,
		soul:              soul,
		identity:          identity,
		location:          location,
		conversationLocks: newConversationLockManager(),
	}
}

func (a *Agent) generate(ctx context.Context, request *providers.GenerateRequest) (*providers.GenerateResponse, error) {
	requestCtx, cancel := context.WithTimeout(ctx, modelRequestTimeout)
	defer cancel()
	return a.provider.Generate(requestCtx, request)
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
		for _, call := range m.ToolCalls {
			total += len(call.ID) + len(call.Function.Name) + len(call.Function.Arguments)
		}
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
	// Retained history must start at a user turn, never in the middle of an
	// assistant tool-call/result sequence.
	for keepFrom < len(allHistory) && (allHistory[keepFrom].ContentType != state.ContentText || allHistory[keepFrom].Role != "user") {
		keepFrom++
	}
	if keepFrom >= len(allHistory) {
		return nil
	}

	// Build the message list for the summarization call.
	summaryMessages := make([]providers.Message, 0, keepFrom+1)
	summaryMessages = append(summaryMessages, providers.Message{
		Role:    providers.RoleSystem,
		Content: summarizationSystemPrompt,
	})
	for _, t := range allHistory[:keepFrom] {
		role := providers.RoleUser
		if t.Role == "assistant" {
			role = providers.RoleAssistant
		}
		summaryMessages = append(summaryMessages, providers.Message{Role: role, Content: summaryContent(t)})
	}

	resp, err := a.generate(ctx, &providers.GenerateRequest{
		Model:     "default",
		Messages:  summaryMessages,
		MaxTokens: compactionMaxTokens,
	})
	if err != nil {
		return fmt.Errorf("compaction: summarization: %w", err)
	}

	firstKeptID := allHistory[keepFrom].ID
	tokensBefore := estimateTokens(summaryMessages)

	if strings.TrimSpace(resp.Message.Content) == "" {
		return fmt.Errorf("compaction: summarization returned empty content")
	}
	if _, err := a.store.SaveCompaction(ctx, channelID, senderID, resp.Message.Content, tokensBefore, firstKeptID); err != nil {
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
	releaseConversation, err := a.conversationLocks.lock(ctx, conversationLockKey(channelID, senderID))
	if err != nil {
		return "", fmt.Errorf("wait for conversation turn: %w", err)
	}
	defer releaseConversation()

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

	now := time.Now()
	systemPrompt := fmt.Sprintf(`You are a helpful personal assistant talking to User %q on Channel %q.
The current server time is %s (%s).
Reference UTC time is %s.
Use tools when they are needed. Routing identity is trusted context and is never a tool argument.
You may store stable preferences and durable user facts when useful, even without an explicit request. Never store credentials, secrets, or transient details.
Search memory when a past durable fact could improve the answer.
Use manage_reminders only when the user is actually asking to add, list, update, or remove reminders. A quotation, mention, or question about reminder wording is not by itself a reminder operation; decide from the full conversation context.
Never claim a reminder changed unless its tool result succeeded.
Use one batch add for multiple reminders. Schedules support at (RFC3339 with explicit offset), every (fixed milliseconds), and cron (wall-clock expression plus IANA timezone).
Interpret times without an explicit timezone in the server timezone. For cron, keep the requested wall-clock fields and omit timezone to use the server timezone; never convert them to UTC first.
Cron examples: daily 08:00 is "0 8 * * *"; weekdays 12:03 is "3 12 * * 1-5"; Mon/Wed/Fri 19:00 is "0 19 * * 1,3,5".
These jobs only send their stored reminder message back to the current user. They cannot silently run a watcher, conditionally suppress delivery, or contact another person; explain that limitation when requested.
When listing reminders, report each persisted id from the tool result rather than numbering the display independently.
Soul:
%s

Identity:
%s
`, senderID, channelID, now.In(a.location).Format(time.RFC3339), a.location.String(), now.UTC().Format(time.RFC3339), a.soul, a.identity)

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

	historyMessages, err := reconstructHistory(history)
	if err != nil {
		return "", err
	}
	messages = append(messages, historyMessages...)
	messages = append(messages, providers.Message{Role: providers.RoleUser, Content: content})

	if err := a.store.SaveConversationTurn(ctx, channelID, senderID, "user", content); err != nil {
		return "", fmt.Errorf("save user turn: %w", err)
	}

	toolCtx := tools.Context{ChannelID: channelID, SenderID: senderID}
	definitions := tools.Definitions(a.location)
	reminderMutationSucceeded := false
	for round := 0; round < maxToolRounds; round++ {
		resp, err := a.generate(ctx, &providers.GenerateRequest{
			Model:      "default",
			Messages:   messages,
			Tools:      definitions,
			ToolChoice: "auto",
			MaxTokens:  defaultMaxTokens,
		})
		if err != nil {
			return "", fmt.Errorf("agent generation failed: %w", err)
		}

		if len(resp.Message.ToolCalls) > 0 {
			assistantMessage := resp.Message
			assistantMessage.Role = providers.RoleAssistant
			payload, err := json.Marshal(assistantMessage)
			if err != nil {
				return "", fmt.Errorf("encode assistant tool calls: %w", err)
			}
			if err := a.store.SaveConversationMessage(ctx, channelID, senderID, "assistant", state.ContentToolCall, string(payload)); err != nil {
				return "", fmt.Errorf("save assistant tool calls: %w", err)
			}
			messages = append(messages, assistantMessage)

			for _, call := range assistantMessage.ToolCalls {
				result, err := a.tools.ExecuteAndRecord(ctx, toolCtx, call)
				if err != nil {
					return "", fmt.Errorf("execute tool %q: %w", call.Function.Name, err)
				}
				if isSuccessfulReminderMutation(call, result) {
					reminderMutationSucceeded = true
				}
				messages = append(messages, result.Message())
			}
			continue
		}

		reply := strings.TrimSpace(resp.Message.Content)
		if reply == "" {
			return "", fmt.Errorf("agent generation returned neither content nor tool calls")
		}
		if !reminderMutationSucceeded && hasUnbackedReminderCommitment(reply) {
			reply = appendUncommittedReminderNote(reply)
		}
		if err := a.store.SaveConversationTurn(ctx, channelID, senderID, "assistant", reply); err != nil {
			return "", fmt.Errorf("save assistant turn: %w", err)
		}

		totalTokens := estimateTokens(messages) + len(reply)/4
		if shouldCompact(totalTokens, defaultContextWindow, defaultReserveTokens) {
			if err := a.runCompaction(ctx, channelID, senderID); err != nil {
				log.Printf("Compaction failed for %s/%s: %v", channelID, senderID, err)
			}
		}
		return reply, nil
	}

	reply := "I couldn't complete that request because the tool workflow exceeded its safety limit."
	if err := a.store.SaveConversationTurn(ctx, channelID, senderID, "assistant", reply); err != nil {
		return "", fmt.Errorf("save tool limit response: %w", err)
	}
	return reply, nil
}

func isSuccessfulReminderMutation(call providers.ToolCall, result tools.Result) bool {
	if call.Function.Name != "manage_reminders" || result.IsError {
		return false
	}
	var arguments struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &arguments); err != nil {
		return false
	}
	switch arguments.Action {
	case "add", "update", "remove":
		return true
	default:
		return false
	}
}

func hasUnbackedReminderCommitment(content string) bool {
	if strings.Contains(strings.ToLower(content), strings.ToLower(uncommittedReminderNote)) {
		return false
	}
	for _, pattern := range unbackedReminderCommitmentPatterns {
		if pattern.MatchString(content) {
			return true
		}
	}
	return false
}

func appendUncommittedReminderNote(content string) string {
	return strings.TrimSpace(content) + "\n\n" + uncommittedReminderNote
}

func historyMessage(turn state.ConversationTurn) (providers.Message, error) {
	switch turn.ContentType {
	case state.ContentText:
		role := providers.MessageRole(turn.Role)
		if role != providers.RoleUser && role != providers.RoleAssistant && role != providers.RoleSystem {
			return providers.Message{}, fmt.Errorf("invalid text role %q", turn.Role)
		}
		return providers.Message{Role: role, Content: turn.Content}, nil
	case state.ContentToolCall:
		var message providers.Message
		if err := json.Unmarshal([]byte(turn.Content), &message); err != nil {
			return providers.Message{}, fmt.Errorf("decode tool call: %w", err)
		}
		if message.Role != providers.RoleAssistant || len(message.ToolCalls) == 0 {
			return providers.Message{}, fmt.Errorf("invalid assistant tool-call payload")
		}
		return message, nil
	case state.ContentToolResult:
		var result tools.Result
		if err := json.Unmarshal([]byte(turn.Content), &result); err != nil {
			return providers.Message{}, fmt.Errorf("decode tool result: %w", err)
		}
		if result.ToolCallID == "" {
			return providers.Message{}, fmt.Errorf("tool result is missing tool_call_id")
		}
		return result.Message(), nil
	default:
		return providers.Message{}, fmt.Errorf("unknown content type %q", turn.ContentType)
	}
}

func reconstructHistory(turns []state.ConversationTurn) ([]providers.Message, error) {
	start := 0
	for start < len(turns) && (turns[start].ContentType != state.ContentText || turns[start].Role != "user") {
		start++
	}
	turns = turns[start:]
	messages := make([]providers.Message, 0, len(turns))
	for index := 0; index < len(turns); index++ {
		message, err := historyMessage(turns[index])
		if err != nil {
			return nil, fmt.Errorf("load structured history row %d: %w", turns[index].ID, err)
		}
		if len(message.ToolCalls) == 0 {
			if message.Role == providers.RoleTool {
				return nil, fmt.Errorf("load structured history row %d: tool result without assistant call", turns[index].ID)
			}
			messages = append(messages, message)
			continue
		}

		callIDs := make(map[string]struct{}, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			callIDs[call.ID] = struct{}{}
		}
		if len(turns)-index-1 < len(callIDs) {
			break // an interrupted final tool sequence is not replayable
		}
		sequence := []providers.Message{message}
		complete := true
		for offset := 1; offset <= len(callIDs); offset++ {
			result, err := historyMessage(turns[index+offset])
			if err != nil || result.Role != providers.RoleTool {
				complete = false
				break
			}
			if _, ok := callIDs[result.ToolCallID]; !ok {
				complete = false
				break
			}
			delete(callIDs, result.ToolCallID)
			sequence = append(sequence, result)
		}
		if !complete || len(callIDs) != 0 {
			break
		}
		messages = append(messages, sequence...)
		index += len(sequence) - 1
	}
	return messages, nil
}

func summaryContent(turn state.ConversationTurn) string {
	message, err := historyMessage(turn)
	if err != nil {
		return turn.Content
	}
	if len(message.ToolCalls) > 0 {
		parts := make([]string, 0, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			parts = append(parts, fmt.Sprintf("%s(%s)", call.Function.Name, call.Function.Arguments))
		}
		return "Assistant called tools: " + strings.Join(parts, ", ")
	}
	if message.Role == providers.RoleTool {
		return "Tool result: " + message.Content
	}
	return message.Content
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
