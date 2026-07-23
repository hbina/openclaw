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
	maxToolRounds       = 4
	defaultMaxTokens    = 4_096
	modelRequestTimeout = 5 * time.Minute
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

// ChatInput is the canonical inbound turn passed to the agent.
type ChatInput struct {
	ChannelID string
	SenderID  string
	Content   string
	Reply     *channels.ReplyContext
}

type persistedInboundMessage struct {
	Content string                 `json:"content"`
	Reply   *channels.ReplyContext `json:"reply,omitempty"`
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

// Chat generates a reply for an inbound turn and loads and saves the complete
// structured conversation history in SQLite.
func (a *Agent) Chat(ctx context.Context, input ChatInput) (string, error) {
	releaseConversation, err := a.conversationLocks.lock(ctx, conversationLockKey(input.ChannelID, input.SenderID))
	if err != nil {
		return "", fmt.Errorf("wait for conversation turn: %w", err)
	}
	defer releaseConversation()

	history, err := a.store.GetConversationHistory(ctx, input.ChannelID, input.SenderID, 0)
	if err != nil {
		log.Printf("Failed to load conversation history: %v", err)
	}

	now := time.Now()
	systemPrompt := fmt.Sprintf(`You are a helpful personal assistant talking to User %q on Channel %q.
The current server time is %s (%s).
Reference UTC time is %s.
Use tools when they are needed. Routing identity is trusted context and is never a tool argument.
When the current user message includes Reply context, it identifies the exact earlier message the user selected. Resolve references from that message rather than unrelated later messages.
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
`, input.SenderID, input.ChannelID, now.In(a.location).Format(time.RFC3339), a.location.String(), now.UTC().Format(time.RFC3339), a.soul, a.identity)

	messages := []providers.Message{
		{Role: providers.RoleSystem, Content: systemPrompt},
	}

	historyMessages, err := reconstructHistory(history)
	if err != nil {
		return "", err
	}
	messages = append(messages, historyMessages...)

	inbound := persistedInboundMessage{Content: input.Content, Reply: input.Reply}
	renderedInbound, err := renderInboundMessage(inbound)
	if err != nil {
		return "", fmt.Errorf("render inbound message: %w", err)
	}
	messages = append(messages, providers.Message{Role: providers.RoleUser, Content: renderedInbound})

	payload, err := json.Marshal(inbound)
	if err != nil {
		return "", fmt.Errorf("encode inbound message: %w", err)
	}
	if err := a.store.SaveConversationMessage(ctx, input.ChannelID, input.SenderID, "user", state.ContentInboundMessage, string(payload)); err != nil {
		return "", fmt.Errorf("save user turn: %w", err)
	}

	toolCtx := tools.Context{ChannelID: input.ChannelID, SenderID: input.SenderID}
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
			if err := a.store.SaveConversationMessage(ctx, input.ChannelID, input.SenderID, "assistant", state.ContentToolCall, string(payload)); err != nil {
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
		if err := a.store.SaveConversationTurn(ctx, input.ChannelID, input.SenderID, "assistant", reply); err != nil {
			return "", fmt.Errorf("save assistant turn: %w", err)
		}

		return reply, nil
	}

	reply := "I couldn't complete that request because the tool workflow exceeded its safety limit."
	if err := a.store.SaveConversationTurn(ctx, input.ChannelID, input.SenderID, "assistant", reply); err != nil {
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

func renderInboundMessage(inbound persistedInboundMessage) (string, error) {
	if inbound.Reply == nil {
		return inbound.Content, nil
	}

	var author string
	switch inbound.Reply.Author {
	case channels.ReplyAuthorUser:
		author = "user"
	case channels.ReplyAuthorAssistant:
		author = "assistant"
	case channels.ReplyAuthorOther:
		author = "other"
	default:
		return "", fmt.Errorf("invalid reply author %q", inbound.Reply.Author)
	}

	body := strings.TrimSpace(inbound.Reply.Body)
	if body == "" {
		if !inbound.Reply.ContentUnavailable {
			return "", fmt.Errorf("reply body is empty without content-unavailable marker")
		}
		body = "[non-text Telegram message; content unavailable]"
	} else if inbound.Reply.ContentUnavailable {
		return "", fmt.Errorf("reply body conflicts with content-unavailable marker")
	}

	var rendered strings.Builder
	fmt.Fprintf(&rendered, "Reply context:\nAuthor: %s\nMessage:\n%s", author, body)
	if selectedText := strings.TrimSpace(inbound.Reply.SelectedText); selectedText != "" {
		fmt.Fprintf(&rendered, "\n\nSelected text:\n%s", selectedText)
	}
	fmt.Fprintf(&rendered, "\n\nCurrent user message:\n%s", inbound.Content)
	return rendered.String(), nil
}

func historyMessage(turn state.ConversationTurn) (providers.Message, error) {
	switch turn.ContentType {
	case state.ContentText:
		role := providers.MessageRole(turn.Role)
		if role != providers.RoleUser && role != providers.RoleAssistant && role != providers.RoleSystem {
			return providers.Message{}, fmt.Errorf("invalid text role %q", turn.Role)
		}
		return providers.Message{Role: role, Content: turn.Content}, nil
	case state.ContentInboundMessage:
		if turn.Role != "user" {
			return providers.Message{}, fmt.Errorf("invalid inbound message role %q", turn.Role)
		}
		var inbound persistedInboundMessage
		if err := json.Unmarshal([]byte(turn.Content), &inbound); err != nil {
			return providers.Message{}, fmt.Errorf("decode inbound message: %w", err)
		}
		content, err := renderInboundMessage(inbound)
		if err != nil {
			return providers.Message{}, err
		}
		return providers.Message{Role: providers.RoleUser, Content: content}, nil
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
	for start < len(turns) && !isUserTurn(turns[start]) {
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

func isUserTurn(turn state.ConversationTurn) bool {
	return turn.Role == "user" &&
		(turn.ContentType == state.ContentText || turn.ContentType == state.ContentInboundMessage)
}

// HandleMessage is the callback triggered by any channel receiving a message.
func (a *Agent) HandleMessage(ctx context.Context, msg *channels.Message) error {
	log.Printf("Agent received message from %s [%s]: %s\n", msg.ChannelID, msg.SenderID, msg.Content)

	reply, err := a.Chat(ctx, ChatInput{
		ChannelID: msg.ChannelID,
		SenderID:  msg.SenderID,
		Content:   msg.Content,
		Reply:     msg.Reply,
	})
	if err != nil {
		return err
	}

	ch, err := a.chanReg.Get(msg.ChannelID)
	if err != nil {
		return fmt.Errorf("channel %s not found: %w", msg.ChannelID, err)
	}

	return ch.SendMessage(ctx, msg.SenderID, reply)
}
