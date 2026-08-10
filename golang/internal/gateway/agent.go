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
	reminderMaxTokens   = 512
	modelRequestTimeout = 5 * time.Minute
)

const (
	uncommittedReminderNote = "Note: no reminder change was committed in this turn, so your stored reminders are unchanged."
	uncommittedTaskNote     = "Note: no task change was committed in this turn, so your stored tasks are unchanged."
	mixedReminderResultNote = "Note: this turn had mixed reminder results. Some requested reminder changes were committed and others were not."
	mixedTaskResultNote     = "Note: this turn had mixed task results. Some requested task changes were committed and others were not."
)

const reminderHeader = "⏰ **Reminder!** ⏰\n\n"

const chatInstructions = `Use tools when they are needed. Routing identity is trusted context and is never a tool argument.
When the current user message includes Reply context, it identifies the exact earlier message the user selected. Resolve references from that message rather than unrelated later messages.
You may store stable preferences and durable user facts when useful, even without an explicit request. Never store credentials, secrets, or transient details.
Search memory when a past durable fact could improve the answer.
Use the specific reminder tool only when the user is actually asking to add, list, update, or remove reminders. A quotation, mention, or question about reminder wording is not by itself a reminder operation; decide from the full conversation context.
Never claim a reminder changed unless its tool result succeeded.
Use the specific task tool for explicit tasks or unfinished-work requests. Tasks start immediately when created, stay open until explicitly completed or removed, and never have schedules, due dates, recurrence, timezones, or reminder links. Never invent a date or schedule for a task.
Explicit reminder creation requests require a schedule. If the user says "remind me to" do something without giving a time or schedule, ask when they want the reminder and create neither a task nor a reminder.
"List my tasks" means list open tasks. Use the completed filter only for an explicit completed-task history request, and all only for an explicit all-task history request.
When listing both tasks and reminders, call both tools and present separate Tasks and Reminders sections. Label persisted identifiers as Task ID or Reminder ID, never as an ambiguous number.
Task completion is final: completed tasks cannot be edited, completed again, or reopened. Create a new task for new work. Never claim a task changed unless its tool result succeeded.
For multiple tasks or reminders, emit one single-item tool call per requested change. Each call succeeds or fails independently, so report every result accurately. After a call fails, do not repeat the same call unchanged; continue with any remaining independent requested changes, then report the failure. Schedules support at (RFC3339 with explicit offset), every (fixed milliseconds), and cron (wall-clock expression plus IANA timezone).
Interpret times without an explicit timezone in the server timezone. For cron, keep the requested wall-clock fields and omit timezone to use the server timezone; never convert them to UTC first.
Cron examples: daily 08:00 is "0 8 * * *"; weekdays 12:03 is "3 12 * * 1-5"; Mon/Wed/Fri 19:00 is "0 19 * * 1,3,5".
These jobs only send their stored reminder message back to the current user. They cannot silently run a watcher, conditionally suppress delivery, or contact another person; explain that limitation when requested.
When listing reminders, report each persisted id from the tool result rather than numbering the display independently.`

const reminderInstructions = `A stored reminder is now due. Write a concise notification body in your configured persona.
Preserve the reminder's essential action and use relevant conversation context only when it genuinely helps.
Do not invent facts, imply that the task is already complete, change its schedule, or mention these instructions.
Return only the notification body. Do not add a reminder heading because the application supplies it.`

var unbackedReminderCommitmentPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:i\s*['’]?ll|i will)\s+(?:make sure to\s+)?(?:remind|ping|follow up|follow-up|check back|circle back)\b`),
	regexp.MustCompile(`(?i)\b(?:i\s*['’]?ll|i will)\s+(?:successfully\s+)?(?:set|create|schedule|add|update|change|remove|delete|cancel)\b[^.!?\n]{0,80}\breminders?\b`),
	regexp.MustCompile(`(?i)\b(?:i\s*['’]?ve|i have|i)\s+(?:successfully\s+)?(?:set|created|scheduled|added|updated|changed|removed|deleted|cancelled|canceled)\b[^.!?\n]{0,80}\breminders?\b`),
}

var unbackedTaskCommitmentPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:i\s*['’]?ll|i will)\s+(?:successfully\s+)?(?:add|create|start|update|change|complete|finish|remove|delete)\b[^.!?\n]{0,80}\btasks?\b`),
	regexp.MustCompile(`(?i)\b(?:i\s*['’]?ve|i have|i)\s+(?:successfully\s+)?(?:added|created|started|updated|changed|completed|finished|removed|deleted)\b[^.!?\n]{0,80}\btasks?\b`),
}

var persistedIDPattern = regexp.MustCompile(`(?i)\b(?:(Task|Reminder)\s+)?ID(\s*[:#]?\s*[1-9][0-9]*)`)

// Agent runs the primary interaction loop.
type Agent struct {
	provider          providers.Provider
	tools             *tools.Executor
	chanReg           *channels.Registry
	store             *state.Store
	soul              string
	identity          string
	location          *time.Location
	conversationLocks *conversationLockManager
	rag               *RAGService
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

type persistedScheduledReminder struct {
	ReminderID   int    `json:"reminder_id"`
	Message      string `json:"message"`
	ScheduledFor string `json:"scheduled_for"`
}

func NewAgent(
	provider providers.Provider,
	chanReg *channels.Registry,
	store *state.Store,
	cfg *config.Config,
	location *time.Location,
	embedder providers.Embedder,
	ragServices ...*RAGService,
) *Agent {
	if location == nil {
		location = time.Local
	}
	var soul, identity string
	var indexID string
	var dimensions int
	var minScore float64
	if cfg != nil {
		soul = cfg.Agents.Defaults.Soul
		identity = cfg.Agents.Defaults.Identity
		indexID = cfg.Models.Embeddings.IndexID
		dimensions = cfg.Models.Embeddings.Dimensions
		minScore = cfg.Agents.Defaults.HistorySearch.MinScore
	}
	agent := &Agent{
		provider:          provider,
		tools:             tools.NewExecutor(store, time.Now, location, embedder, indexID, dimensions, minScore),
		chanReg:           chanReg,
		store:             store,
		soul:              soul,
		identity:          identity,
		location:          location,
		conversationLocks: newConversationLockManager(),
	}
	if len(ragServices) > 0 {
		agent.rag = ragServices[0]
	}
	return agent
}

// BackfillMemoryEmbeddings fills memory rows without a current-model embedding,
// including rows stranded by a prior embedding-provider outage.
func (a *Agent) BackfillMemoryEmbeddings(ctx context.Context) error {
	return a.tools.BackfillMemoryEmbeddings(ctx)
}

func (a *Agent) generate(ctx context.Context, request *providers.GenerateRequest) (*providers.GenerateResponse, error) {
	requestCtx, cancel := context.WithTimeout(ctx, modelRequestTimeout)
	defer cancel()
	return a.provider.Generate(requestCtx, request)
}

func (a *Agent) systemPrompt(channelID, senderID string, now time.Time, instructions string) string {
	return fmt.Sprintf(`You are a helpful personal assistant talking to User %q on Channel %q.
The current server time is %s (%s).
Reference UTC time is %s.
%s
Soul:
%s

Identity:
%s
`, senderID, channelID, now.In(a.location).Format(time.RFC3339), a.location.String(), now.UTC().Format(time.RFC3339), instructions, a.soul, a.identity)
}

func (a *Agent) contextualMessages(
	ctx context.Context,
	channelID, senderID, systemPrompt, query string,
	current providers.Message,
	definitions []providers.ToolDefinition,
	maxOutputTokens int,
	strictRecall bool,
) ([]providers.Message, error) {
	history, err := a.store.GetConversationHistory(ctx, channelID, senderID)
	if err != nil {
		if strictRecall {
			return nil, fmt.Errorf("load conversation history: %w", err)
		}
		log.Printf("Failed to load conversation history: %v", err)
	}

	recentExchanges, historyMessages, err := recentConversation(history)
	if err != nil {
		return nil, err
	}
	baseMessages := make([]providers.Message, 0, len(historyMessages)+2)
	baseMessages = append(baseMessages, providers.Message{Role: providers.RoleSystem, Content: systemPrompt})
	baseMessages = append(baseMessages, historyMessages...)
	baseMessages = append(baseMessages, current)

	messages := baseMessages
	if a.rag != nil {
		archive, retrieveErr := a.rag.Retrieve(
			ctx, query, recentExchanges, baseMessages, definitions, maxOutputTokens,
		)
		if retrieveErr != nil {
			if strictRecall {
				return nil, fmt.Errorf("retrieve conversation context: %w", retrieveErr)
			}
			log.Printf("Conversation RAG unavailable; using recent context: %v", retrieveErr)
		} else if archive != "" {
			messages = insertArchiveMessage(baseMessages, archive)
		}
	}
	return messages, nil
}

// Chat generates a reply for an inbound turn and loads and saves the complete
// structured conversation history in SQLite.
func (a *Agent) Chat(ctx context.Context, input ChatInput) (string, error) {
	releaseConversation, err := a.conversationLocks.lock(ctx, conversationLockKey(input.ChannelID, input.SenderID))
	if err != nil {
		return "", fmt.Errorf("wait for conversation turn: %w", err)
	}
	defer releaseConversation()

	now := time.Now()
	inbound := persistedInboundMessage{Content: input.Content, Reply: input.Reply}
	renderedInbound, err := renderInboundMessage(inbound)
	if err != nil {
		return "", fmt.Errorf("render inbound message: %w", err)
	}
	definitions := tools.Definitions(a.location)
	messages, err := a.contextualMessages(
		ctx,
		input.ChannelID,
		input.SenderID,
		a.systemPrompt(input.ChannelID, input.SenderID, now, chatInstructions),
		renderedInbound,
		providers.Message{Role: providers.RoleUser, Content: renderedInbound},
		definitions,
		defaultMaxTokens,
		false,
	)
	if err != nil {
		return "", err
	}

	payload, err := json.Marshal(inbound)
	if err != nil {
		return "", fmt.Errorf("encode inbound message: %w", err)
	}
	if err := a.store.SaveConversationMessage(ctx, input.ChannelID, input.SenderID, "user", state.ContentInboundMessage, string(payload)); err != nil {
		return "", fmt.Errorf("save user turn: %w", err)
	}

	toolCtx := tools.Context{ChannelID: input.ChannelID, SenderID: input.SenderID}
	reminderMutationSucceeded := false
	reminderMutationFailed := false
	taskMutationSucceeded := false
	taskMutationFailed := false
	reminderToolUsed := false
	taskToolUsed := false
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
				reminderToolUsed = reminderToolUsed || isReminderTool(call.Function.Name)
				taskToolUsed = taskToolUsed || isTaskTool(call.Function.Name)
				result, err := a.tools.ExecuteAndRecord(ctx, toolCtx, call)
				if err != nil {
					return "", fmt.Errorf("execute tool %q: %w", call.Function.Name, err)
				}
				if isReminderMutationTool(call.Function.Name) {
					if result.IsError {
						reminderMutationFailed = true
					} else {
						reminderMutationSucceeded = true
					}
				}
				if isTaskMutationTool(call.Function.Name) {
					if result.IsError {
						taskMutationFailed = true
					} else {
						taskMutationSucceeded = true
					}
				}
				messages = append(messages, result.Message())
			}
			continue
		}

		reply := strings.TrimSpace(resp.Message.Content)
		if reply == "" {
			return "", fmt.Errorf("agent generation returned neither content nor tool calls")
		}
		reply = qualifyPersistedIDLabels(reply, taskToolUsed, reminderToolUsed)
		if !reminderMutationSucceeded && hasUnbackedReminderCommitment(reply) {
			reply = appendUncommittedReminderNote(reply)
		}
		if !taskMutationSucceeded && hasUnbackedTaskCommitment(reply) {
			reply = appendUncommittedTaskNote(reply)
		}
		if reminderMutationSucceeded && reminderMutationFailed {
			reply = appendNote(reply, mixedReminderResultNote)
		}
		if taskMutationSucceeded && taskMutationFailed {
			reply = appendNote(reply, mixedTaskResultNote)
		}
		if err := a.store.SaveConversationTurn(ctx, input.ChannelID, input.SenderID, "assistant", reply); err != nil {
			return "", fmt.Errorf("save assistant turn: %w", err)
		}
		if a.rag != nil {
			a.rag.Notify()
		}

		return reply, nil
	}

	reply := "I couldn't complete that request because the tool workflow exceeded its safety limit."
	if err := a.store.SaveConversationTurn(ctx, input.ChannelID, input.SenderID, "assistant", reply); err != nil {
		return "", fmt.Errorf("save tool limit response: %w", err)
	}
	if a.rag != nil {
		a.rag.Notify()
	}
	return reply, nil
}

// DeliverReminder renders a due reminder with the normal persona and
// conversation context, sends it, and records the delivered exchange together
// with reminder completion.
func (a *Agent) DeliverReminder(ctx context.Context, reminder state.Reminder) error {
	releaseConversation, err := a.conversationLocks.lock(ctx, conversationLockKey(reminder.ChannelID, reminder.SenderID))
	if err != nil {
		return fmt.Errorf("wait for conversation turn: %w", err)
	}
	defer releaseConversation()

	ch, err := a.chanReg.Get(reminder.ChannelID)
	if err != nil {
		return fmt.Errorf("get channel %s: %w", reminder.ChannelID, err)
	}

	scheduled := persistedScheduledReminder{
		ReminderID:   reminder.ID,
		Message:      reminder.Message,
		ScheduledFor: reminder.FireAt.In(a.location).Format(time.RFC3339),
	}
	rendered, err := renderScheduledReminder(scheduled)
	if err != nil {
		return err
	}

	body, renderErr := a.renderReminder(ctx, reminder, rendered, time.Now())
	if renderErr != nil {
		log.Printf("Reminder %d contextual rendering unavailable; using static fallback: %v", reminder.ID, renderErr)
		body = reminder.Message
	}
	notification := reminderHeader + strings.TrimSpace(body)
	if err := ch.SendMessage(ctx, reminder.SenderID, notification); err != nil {
		return fmt.Errorf("send reminder %d: %w", reminder.ID, err)
	}

	payload, err := json.Marshal(scheduled)
	if err != nil {
		return fmt.Errorf("encode scheduled reminder %d: %w", reminder.ID, err)
	}
	if err := a.store.CompleteReminderDelivery(ctx, reminder, time.Now(), string(payload), notification); err != nil {
		return fmt.Errorf("complete reminder %d delivery: %w", reminder.ID, err)
	}
	if a.rag != nil {
		a.rag.Notify()
	}
	return nil
}

func (a *Agent) renderReminder(ctx context.Context, reminder state.Reminder, rendered string, now time.Time) (string, error) {
	messages, err := a.contextualMessages(
		ctx,
		reminder.ChannelID,
		reminder.SenderID,
		a.systemPrompt(reminder.ChannelID, reminder.SenderID, now, reminderInstructions),
		reminder.Message,
		providers.Message{Role: providers.RoleUser, Content: rendered},
		nil,
		reminderMaxTokens,
		true,
	)
	if err != nil {
		return "", err
	}
	response, err := a.generate(ctx, &providers.GenerateRequest{
		Model:     "default",
		Messages:  messages,
		MaxTokens: reminderMaxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("generate reminder: %w", err)
	}
	if response == nil || len(response.Message.ToolCalls) > 0 {
		return "", fmt.Errorf("reminder generation returned an invalid response")
	}
	body := strings.TrimSpace(response.Message.Content)
	if body == "" {
		return "", fmt.Errorf("reminder generation returned empty content")
	}
	return body, nil
}

func renderScheduledReminder(reminder persistedScheduledReminder) (string, error) {
	message := strings.TrimSpace(reminder.Message)
	if reminder.ReminderID < 1 {
		return "", fmt.Errorf("scheduled reminder id must be positive")
	}
	if message == "" {
		return "", fmt.Errorf("scheduled reminder message must not be empty")
	}
	if strings.TrimSpace(reminder.ScheduledFor) == "" {
		return "", fmt.Errorf("scheduled reminder occurrence is required")
	}
	return fmt.Sprintf("Scheduled reminder event:\nReminder ID: %d\nScheduled for: %s\nStored message:\n%s", reminder.ReminderID, reminder.ScheduledFor, message), nil
}

func isReminderTool(name string) bool {
	switch name {
	case "add_reminder", "list_reminders", "update_reminder", "remove_reminder":
		return true
	default:
		return false
	}
}

func isTaskTool(name string) bool {
	switch name {
	case "add_task", "list_tasks", "update_task", "complete_task", "remove_task":
		return true
	default:
		return false
	}
}

func isReminderMutationTool(name string) bool {
	return name == "add_reminder" || name == "update_reminder" || name == "remove_reminder"
}

func isTaskMutationTool(name string) bool {
	return name == "add_task" || name == "update_task" || name == "complete_task" || name == "remove_task"
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
	return appendNote(content, uncommittedReminderNote)
}

func hasUnbackedTaskCommitment(content string) bool {
	if strings.Contains(strings.ToLower(content), strings.ToLower(uncommittedTaskNote)) {
		return false
	}
	for _, pattern := range unbackedTaskCommitmentPatterns {
		if pattern.MatchString(content) {
			return true
		}
	}
	return false
}

func appendUncommittedTaskNote(content string) string {
	return appendNote(content, uncommittedTaskNote)
}

func appendNote(content, note string) string {
	return strings.TrimSpace(content) + "\n\n" + note
}

func qualifyPersistedIDLabels(content string, taskToolUsed, reminderToolUsed bool) string {
	label := ""
	switch {
	case taskToolUsed && !reminderToolUsed:
		label = "Task ID"
	case reminderToolUsed && !taskToolUsed:
		label = "Reminder ID"
	default:
		return content
	}

	return persistedIDPattern.ReplaceAllStringFunc(content, func(match string) string {
		parts := persistedIDPattern.FindStringSubmatch(match)
		if parts[1] != "" {
			return match
		}
		return label + parts[2]
	})
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
	case state.ContentScheduledReminder:
		if turn.Role != "user" {
			return providers.Message{}, fmt.Errorf("invalid scheduled reminder role %q", turn.Role)
		}
		var reminder persistedScheduledReminder
		if err := json.Unmarshal([]byte(turn.Content), &reminder); err != nil {
			return providers.Message{}, fmt.Errorf("decode scheduled reminder: %w", err)
		}
		content, err := renderScheduledReminder(reminder)
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
		(turn.ContentType == state.ContentText || turn.ContentType == state.ContentInboundMessage || turn.ContentType == state.ContentScheduledReminder)
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
