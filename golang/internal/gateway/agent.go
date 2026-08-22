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
	"github.com/openclaw/openclaw/go/internal/memory"
	"github.com/openclaw/openclaw/go/internal/providers"
	"github.com/openclaw/openclaw/go/internal/state"
	"github.com/openclaw/openclaw/go/internal/tools"
)

const (
	defaultMaxTokens    = 4_096
	reminderMaxTokens   = 512
	modelRequestTimeout = 5 * time.Minute
)

const (
	uncommittedReminderNote = "Note: no reminder change was committed in this turn, so your stored reminders are unchanged."
	uncommittedTaskNote     = "Note: no task change was committed in this turn, so your stored tasks are unchanged."
	mixedReminderResultNote = "Note: this turn had mixed reminder results. Some requested reminder changes were committed and others were not."
	mixedTaskResultNote     = "Note: this turn had mixed task results. Some requested task changes were committed and others were not."
	uncommittedMemoryNote   = "Note: no memory change was committed in this turn, so your stored memories are unchanged."
	mixedMemoryResultNote   = "Note: this turn had mixed memory results. Some requested memory changes were committed and others were not."
)

const reminderHeader = "⏰ **Reminder!** ⏰\n\n"

const chatInstructions = `Use tools when they are needed. Routing identity is trusted context and is never a tool argument.
When the current user message includes Reply context, it identifies the exact earlier message the user selected. Resolve references from that message rather than unrelated later messages.
You may store one concise profile, durable, or daily memory when persistence is material to the current response. Profile covers enduring owner identity, preferences, and relationships; durable covers reusable facts, decisions, and project context; daily covers episodic context likely to matter soon. Never store credentials, secrets, greetings, speculation, or routine transient details. A separate curator also reviews the completed exchange.
Search memory when a past owner fact could improve the answer. Update the existing Memory ID when a remembered fact changes; remove memory only when the owner explicitly asks to forget it.
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

var unbackedMemoryCommitmentPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:i\s*['’]?ll|i will)\s+(?:remember|save|store|forget)\b`),
	regexp.MustCompile(`(?i)\b(?:i\s*['’]?ve|i have|i)\s+(?:remembered|saved|stored|updated|forgotten|removed)\b`),
}

var persistedIDPattern = regexp.MustCompile(`(?i)\b(?:(Task|Reminder|Memory)\s+)?ID(\s*[:#]?\s*[1-9][0-9]*)`)

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
	memory            *memory.Service
	embedder          providers.Embedder
	modelMemory       bool
}

// ChatInput is the canonical inbound turn passed to the agent.
type ChatInput struct {
	ChannelID string
	SenderID  string
	MessageID string
	Content   string
	Reply     *channels.ReplyContext
}

type PreparedResponse struct {
	TraceID        int64
	OutputEventID  int64
	ChannelID      string
	SenderID       string
	Content        string
	StartHistoryID int64
	Chunks         []state.ConversationChunk
	release        func()
}

func (response *PreparedResponse) Release() {
	if response.release != nil {
		response.release()
		response.release = nil
	}
}

type outputTransformation struct {
	Name   string `json:"name"`
	Before string `json:"before"`
	After  string `json:"after"`
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
		memory:            memory.NewService(store, embedder, indexID, dimensions, minScore, time.Now),
		embedder:          embedder,
	}
	_, agent.modelMemory = provider.(*providers.OpenAIClient)
	if len(ragServices) > 0 {
		agent.rag = ragServices[0]
	}
	return agent
}

func (a *Agent) generate(ctx context.Context, traceID int64, round int, purpose string, request *providers.GenerateRequest) (*providers.GenerateResponse, int64, error) {
	wireRequest, err := providers.MarshalGenerateRequest(a.provider, request)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal model request for trace: %w", err)
	}
	eventID, err := a.store.StartLLMCall(ctx, traceID, round, purpose, string(wireRequest))
	if err != nil {
		return nil, 0, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, modelRequestTimeout)
	defer cancel()
	response, generateErr := a.provider.Generate(requestCtx, request)
	responseJSON := ""
	httpStatus := 0
	finishReason := ""
	if response != nil {
		httpStatus = response.HTTPStatus
		finishReason = response.FinishReason
		if len(response.RawResponse) > 0 {
			responseJSON = string(response.RawResponse)
		} else if encoded, marshalErr := json.Marshal(response); marshalErr == nil {
			responseJSON = string(encoded)
		}
	}
	if err := a.store.FinishLLMCall(ctx, eventID, responseJSON, httpStatus, finishReason, generateErr); err != nil {
		return nil, eventID, fmt.Errorf("finish LLM trace: %w", err)
	}
	return response, eventID, generateErr
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
	traceID int64,
	channelID, senderID, systemPrompt, query string,
	current providers.Message,
	definitions []providers.ToolDefinition,
	maxOutputTokens int,
	strictRecall bool,
) ([]providers.Message, error) {
	history, err := a.store.GetConversationHistory(ctx, channelID, senderID)
	if err != nil {
		return nil, fmt.Errorf("load conversation history: %w", err)
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
		planned := query
		var keywords []string
		if a.modelMemory {
			var planErr error
			planned, keywords, planErr = a.planRecall(ctx, traceID, query, historyMessages)
			if planErr != nil {
				return nil, fmt.Errorf("plan recall: %w", planErr)
			}
		}
		retrieval, memoryMatches, retrieveErr := a.retrieveUnified(ctx, planned, keywords, recentExchanges, baseMessages, definitions, maxOutputTokens)
		if retrieveErr != nil {
			detail := state.RAGTrace{Outcome: retrieval.Outcome, EmbeddingQuery: retrieval.EmbeddingQuery, EmbeddingModel: retrieval.EmbeddingModel, Dimensions: retrieval.Dimensions, IndexVersion: retrieval.IndexVersion, MinimumScore: retrieval.MinimumScore, HistoryHighwaterID: retrieval.HistoryHighwaterID, CandidateCount: retrieval.CandidateCount, ExcludedCount: retrieval.ExcludedCount, QualifiedCount: retrieval.QualifiedCount}
			if err := a.store.RecordRAGTrace(ctx, traceID, detail, retrieveErr); err != nil {
				return nil, fmt.Errorf("record failed retrieval trace: %w", err)
			}
			return nil, fmt.Errorf("retrieve unified context: %w", retrieveErr)
		}
		archive := retrieval.Archive
		selectedMemoryIDs := make([]int64, 0, len(memoryMatches))
		for _, item := range memoryMatches {
			selectedMemoryIDs = append(selectedMemoryIDs, item.ID)
		}
		selectedConversationIDs := make([]string, 0, len(retrieval.Matches))
		for _, item := range retrieval.Matches {
			selectedConversationIDs = append(selectedConversationIDs, fmt.Sprintf("%d:%d", item.StartHistoryID, item.EndHistoryID))
		}
		if a.modelMemory {
			var rerankErr error
			archive, selectedMemoryIDs, selectedConversationIDs, rerankErr = a.rerankRecallDetailed(ctx, traceID, query, memoryMatches, retrieval.Matches)
			if rerankErr != nil {
				return nil, fmt.Errorf("rerank recall: %w", rerankErr)
			}
		}
		detail := state.RAGTrace{
			Outcome: retrieval.Outcome, EmbeddingQuery: retrieval.EmbeddingQuery,
			EmbeddingModel: retrieval.EmbeddingModel, Dimensions: retrieval.Dimensions,
			IndexVersion: retrieval.IndexVersion, MinimumScore: retrieval.MinimumScore,
			HistoryHighwaterID: retrieval.HistoryHighwaterID, CandidateCount: retrieval.CandidateCount,
			ExcludedCount: retrieval.ExcludedCount, QualifiedCount: retrieval.QualifiedCount,
			RenderedArchive: archive,
		}
		memoryByID := map[int64]state.MemorySearchResult{}
		for _, item := range memoryMatches {
			memoryByID[item.ID] = item
		}
		for index, id := range selectedMemoryIDs {
			item := memoryByID[id]
			detail.MemoryMatches = append(detail.MemoryMatches, state.MemoryRAGTraceMatch{Rank: index + 1, MemoryID: item.ID, RevisionID: item.RevisionID, VectorScore: item.VectorScore, KeywordScore: item.KeywordScore, CombinedScore: item.CombinedScore, ContentHash: item.ContentHash})
		}
		conversationByID := map[string]state.RAGTraceMatch{}
		for _, item := range retrieval.Matches {
			conversationByID[fmt.Sprintf("%d:%d", item.StartHistoryID, item.EndHistoryID)] = item
		}
		for index, id := range selectedConversationIDs {
			item := conversationByID[id]
			item.Rank = index + 1
			detail.Matches = append(detail.Matches, item)
		}
		if err := a.store.RecordRAGTrace(ctx, traceID, detail, nil); err != nil {
			return nil, fmt.Errorf("record conversation retrieval trace: %w", err)
		}
		core, coreErr := a.memoryCore(ctx)
		if coreErr != nil {
			return nil, fmt.Errorf("load memory core: %w", coreErr)
		}
		combined := strings.TrimSpace(strings.Join([]string{core, archive}, "\n\n"))
		if combined != "" {
			messages = insertArchiveMessage(baseMessages, combined)
		}
	} else if err := a.store.RecordRAGTrace(ctx, traceID, state.RAGTrace{Outcome: "disabled"}, nil); err != nil {
		return nil, fmt.Errorf("record disabled retrieval trace: %w", err)
	}
	return messages, nil
}

// Chat generates a reply for an inbound turn and loads and saves the complete
// structured conversation history in SQLite.
func (a *Agent) PrepareChat(ctx context.Context, input ChatInput) (prepared PreparedResponse, returnErr error) {
	releaseConversation, err := a.conversationLocks.lock(ctx, conversationLockKey(input.ChannelID, input.SenderID))
	if err != nil {
		return prepared, fmt.Errorf("wait for conversation turn: %w", err)
	}
	prepared.release = releaseConversation
	defer func() {
		if returnErr != nil {
			prepared.Release()
		}
	}()

	now := time.Now()
	inbound := persistedInboundMessage{Content: input.Content, Reply: input.Reply}
	inboundPayload, err := json.Marshal(inbound)
	if err != nil {
		return prepared, fmt.Errorf("encode inbound message: %w", err)
	}
	traceID, err := a.store.StartResponseTrace(ctx, state.TraceInput{
		TriggerType: "chat", ChannelID: input.ChannelID, SenderID: input.SenderID,
		ExternalMessageID: input.MessageID, InputJSON: string(inboundPayload),
	})
	if err != nil {
		return prepared, err
	}
	prepared.TraceID = traceID
	defer func() {
		if returnErr != nil {
			if finishErr := a.store.FinishTrace(context.Background(), traceID, "failed", "generation", returnErr); finishErr != nil {
				log.Printf("Trace %d failure could not be finalized: %v", traceID, finishErr)
			}
		}
	}()
	log.Printf("Response trace %d started for %s [%s]", traceID, input.ChannelID, input.SenderID)
	renderedInbound, err := renderInboundMessage(inbound)
	if err != nil {
		return prepared, fmt.Errorf("render inbound message: %w", err)
	}
	definitions := tools.Definitions(a.location)
	messages, err := a.contextualMessages(
		ctx,
		traceID,
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
		return prepared, err
	}

	historyID, err := a.store.SaveConversationMessageID(ctx, input.ChannelID, input.SenderID, "user", state.ContentInboundMessage, string(inboundPayload))
	if err != nil {
		return prepared, fmt.Errorf("save user turn: %w", err)
	}
	if err := a.store.LinkTraceInbound(ctx, traceID, historyID); err != nil {
		return prepared, err
	}

	toolCtx := tools.Context{ChannelID: input.ChannelID, SenderID: input.SenderID, ResponseTraceID: traceID, SourceHistoryID: historyID}
	reminderMutationSucceeded := false
	reminderMutationFailed := false
	taskMutationSucceeded := false
	taskMutationFailed := false
	reminderToolUsed := false
	taskToolUsed := false
	memoryMutationSucceeded := false
	memoryMutationFailed := false
	memoryToolUsed := false
	for round := 0; ; round++ {
		resp, llmEventID, err := a.generate(ctx, traceID, round+1, "chat", &providers.GenerateRequest{
			Model:      "default",
			Messages:   messages,
			Tools:      definitions,
			ToolChoice: "auto",
			MaxTokens:  defaultMaxTokens,
		})
		if err != nil {
			return prepared, fmt.Errorf("agent generation failed: %w", err)
		}

		if len(resp.Message.ToolCalls) > 0 {
			assistantMessage := resp.Message
			assistantMessage.Role = providers.RoleAssistant
			payload, err := json.Marshal(assistantMessage)
			if err != nil {
				return prepared, fmt.Errorf("encode assistant tool calls: %w", err)
			}
			if err := a.store.SaveConversationMessage(ctx, input.ChannelID, input.SenderID, "assistant", state.ContentToolCall, string(payload)); err != nil {
				return prepared, fmt.Errorf("save assistant tool calls: %w", err)
			}
			messages = append(messages, assistantMessage)

			for _, call := range assistantMessage.ToolCalls {
				toolEventID, traceErr := a.store.StartToolExecution(ctx, traceID, llmEventID, call.ID, call.Function.Name, call.Function.Arguments)
				if traceErr != nil {
					return prepared, traceErr
				}
				reminderToolUsed = reminderToolUsed || isReminderTool(call.Function.Name)
				taskToolUsed = taskToolUsed || isTaskTool(call.Function.Name)
				callToolCtx := toolCtx
				callToolCtx.TraceEventID = toolEventID
				result, err := a.tools.ExecuteAndRecord(ctx, callToolCtx, call)
				if err != nil {
					_ = a.store.FinishToolExecution(ctx, toolEventID, "", true, false, err)
					return prepared, fmt.Errorf("execute tool %q: %w", call.Function.Name, err)
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
				if isMemoryMutationTool(call.Function.Name) {
					if result.IsError {
						memoryMutationFailed = true
					} else {
						memoryMutationSucceeded = true
					}
				}
				memoryToolUsed = memoryToolUsed || isMemoryTool(call.Function.Name)
				messages = append(messages, result.Message())
			}
			continue
		}

		source := resp.Message.Content
		reply := strings.TrimSpace(source)
		transformations := make([]outputTransformation, 0)
		if reply != source {
			transformations = append(transformations, outputTransformation{Name: "trim", Before: source, After: reply})
		}
		if reply == "" {
			return prepared, fmt.Errorf("agent generation returned neither content nor tool calls")
		}
		curated := false
		if a.modelMemory {
			curated, err = a.curateMemories(ctx, traceID, historyID, input.ChannelID, input.SenderID, renderedInbound, reply)
			if err != nil {
				return prepared, fmt.Errorf("curate memory: %w", err)
			}
		}
		memoryMutationSucceeded = memoryMutationSucceeded || curated
		before := reply
		reply = qualifyPersistedIDLabelsForTools(reply, taskToolUsed, reminderToolUsed, memoryToolUsed)
		if reply != before {
			transformations = append(transformations, outputTransformation{Name: "qualify_persisted_ids", Before: before, After: reply})
		}
		if !reminderMutationSucceeded && hasUnbackedReminderCommitment(reply) {
			before = reply
			reply = appendUncommittedReminderNote(reply)
			transformations = append(transformations, outputTransformation{Name: "uncommitted_reminder_note", Before: before, After: reply})
		}
		if !taskMutationSucceeded && hasUnbackedTaskCommitment(reply) {
			before = reply
			reply = appendUncommittedTaskNote(reply)
			transformations = append(transformations, outputTransformation{Name: "uncommitted_task_note", Before: before, After: reply})
		}
		if reminderMutationSucceeded && reminderMutationFailed {
			before = reply
			reply = appendNote(reply, mixedReminderResultNote)
			transformations = append(transformations, outputTransformation{Name: "mixed_reminder_note", Before: before, After: reply})
		}
		if taskMutationSucceeded && taskMutationFailed {
			before = reply
			reply = appendNote(reply, mixedTaskResultNote)
			transformations = append(transformations, outputTransformation{Name: "mixed_task_note", Before: before, After: reply})
		}
		if !memoryMutationSucceeded && hasUnbackedMemoryCommitment(reply) {
			before = reply
			reply = appendNote(reply, uncommittedMemoryNote)
			transformations = append(transformations, outputTransformation{Name: "uncommitted_memory_note", Before: before, After: reply})
		}
		if memoryMutationSucceeded && memoryMutationFailed {
			before = reply
			reply = appendNote(reply, mixedMemoryResultNote)
			transformations = append(transformations, outputTransformation{Name: "mixed_memory_note", Before: before, After: reply})
		}
		transformJSON, err := json.Marshal(transformations)
		if err != nil {
			return prepared, err
		}
		outputEventID, err := a.store.RecordResponseOutput(ctx, traceID, "llm", &llmEventID, source, string(transformJSON), reply)
		if err != nil {
			return prepared, err
		}
		prepared.TraceID = traceID
		prepared.OutputEventID = outputEventID
		prepared.ChannelID = input.ChannelID
		prepared.SenderID = input.SenderID
		prepared.Content = reply
		prepared.StartHistoryID, prepared.Chunks, err = a.prepareCurrentExchange(ctx, input.ChannelID, input.SenderID, reply)
		if err != nil {
			return prepared, fmt.Errorf("index completed exchange: %w", err)
		}
		return prepared, nil
	}

}

func (a *Agent) Chat(ctx context.Context, input ChatInput) (string, error) {
	prepared, err := a.PrepareChat(ctx, input)
	if err != nil {
		return "", err
	}
	defer prepared.Release()
	deliveryID, err := a.store.PrepareDelivery(ctx, prepared.TraceID, prepared.OutputEventID, input.ChannelID, input.SenderID, prepared.Content)
	if err != nil {
		_ = a.store.FinishTrace(context.Background(), prepared.TraceID, "failed", "delivery", err)
		return "", err
	}
	if err := a.store.MarkDeliveryAttempting(ctx, deliveryID); err != nil {
		_ = a.store.FinishTrace(context.Background(), prepared.TraceID, "failed", "delivery", err)
		return "", err
	}
	if err := a.store.CompleteDeliveryIndexed(ctx, prepared.TraceID, deliveryID, "internal", input.ChannelID, input.SenderID, prepared.Content, prepared.StartHistoryID, prepared.Chunks); err != nil {
		return "", err
	}
	return prepared.Content, nil
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

	scheduled := persistedScheduledReminder{
		ReminderID:   reminder.ID,
		Message:      reminder.Message,
		ScheduledFor: reminder.FireAt.In(a.location).Format(time.RFC3339),
	}
	traceInput, err := json.Marshal(scheduled)
	if err != nil {
		return fmt.Errorf("encode reminder trace input: %w", err)
	}
	reminderID := reminder.ID
	traceID, err := a.store.StartResponseTrace(ctx, state.TraceInput{TriggerType: "reminder", ChannelID: reminder.ChannelID, SenderID: reminder.SenderID, ReminderID: &reminderID, InputJSON: string(traceInput)})
	if err != nil {
		return err
	}
	completed := false
	defer func() {
		if !completed {
			log.Printf("Reminder response trace %d did not complete", traceID)
		}
	}()
	ch, err := a.chanReg.Get(reminder.ChannelID)
	if err != nil {
		_ = a.store.FinishTrace(context.Background(), traceID, "failed", "channel", err)
		return fmt.Errorf("get channel %s: %w", reminder.ChannelID, err)
	}
	rendered, err := renderScheduledReminder(scheduled)
	if err != nil {
		_ = a.store.FinishTrace(context.Background(), traceID, "failed", "input", err)
		return err
	}

	body, llmEventID, renderErr := a.renderReminder(ctx, traceID, reminder, rendered, time.Now())
	if renderErr != nil {
		_ = a.store.FinishTrace(context.Background(), traceID, "failed", "generation", renderErr)
		return fmt.Errorf("render reminder %d: %w", reminder.ID, renderErr)
	}
	if a.modelMemory {
		if _, err := a.curateMemories(ctx, traceID, 0, reminder.ChannelID, reminder.SenderID, rendered, body); err != nil {
			_ = a.store.FinishTrace(context.Background(), traceID, "failed", "memory_curate", err)
			return fmt.Errorf("curate reminder memory: %w", err)
		}
	}
	sourceType := "llm"
	sourceContent := body
	notification := reminderHeader + strings.TrimSpace(body)
	payload, err := json.Marshal(scheduled)
	if err != nil {
		return fmt.Errorf("encode scheduled reminder %d: %w", reminder.ID, err)
	}
	chunks, err := a.prepareReminderExchange(ctx, reminder, string(payload), notification)
	if err != nil {
		_ = a.store.FinishTrace(context.Background(), traceID, "failed", "index", err)
		return fmt.Errorf("index reminder %d exchange: %w", reminder.ID, err)
	}
	transforms, _ := json.Marshal([]outputTransformation{{Name: "add_reminder_header", Before: body, After: notification}})
	var sourceEvent *int64
	if llmEventID != 0 {
		sourceEvent = &llmEventID
	}
	outputEventID, err := a.store.RecordResponseOutput(ctx, traceID, sourceType, sourceEvent, sourceContent, string(transforms), notification)
	if err != nil {
		_ = a.store.FinishTrace(context.Background(), traceID, "failed", "output", err)
		return err
	}
	deliveryID, err := a.store.PrepareDelivery(ctx, traceID, outputEventID, reminder.ChannelID, reminder.SenderID, notification)
	if err != nil {
		_ = a.store.FinishTrace(context.Background(), traceID, "failed", "delivery", err)
		return err
	}
	if err := a.store.MarkDeliveryAttempting(ctx, deliveryID); err != nil {
		_ = a.store.FinishTrace(context.Background(), traceID, "failed", "delivery", err)
		return err
	}
	receipt, err := ch.SendMessage(ctx, reminder.SenderID, notification)
	if err != nil {
		_ = a.store.FailDelivery(context.Background(), traceID, deliveryID, err)
		return fmt.Errorf("send reminder %d: %w", reminder.ID, err)
	}

	if err := a.store.CompleteReminderTraceDeliveryIndexed(ctx, reminder, time.Now(), string(payload), notification, traceID, deliveryID, receipt.MessageID, chunks); err != nil {
		return fmt.Errorf("complete reminder %d delivery: %w", reminder.ID, err)
	}
	completed = true
	return nil
}

func (a *Agent) renderReminder(ctx context.Context, traceID int64, reminder state.Reminder, rendered string, now time.Time) (string, int64, error) {
	messages, err := a.contextualMessages(
		ctx,
		traceID,
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
		return "", 0, err
	}
	response, llmEventID, err := a.generate(ctx, traceID, 1, "reminder", &providers.GenerateRequest{
		Model:     "default",
		Messages:  messages,
		MaxTokens: reminderMaxTokens,
	})
	if err != nil {
		return "", llmEventID, fmt.Errorf("generate reminder: %w", err)
	}
	if response == nil || len(response.Message.ToolCalls) > 0 {
		return "", llmEventID, fmt.Errorf("reminder generation returned an invalid response")
	}
	body := strings.TrimSpace(response.Message.Content)
	if body == "" {
		return "", llmEventID, fmt.Errorf("reminder generation returned empty content")
	}
	return body, llmEventID, nil
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

func isMemoryMutationTool(name string) bool {
	return name == "store_memory" || name == "update_memory" || name == "remove_memory"
}

func isMemoryTool(name string) bool {
	switch name {
	case "store_memory", "get_memory", "list_memories", "update_memory", "remove_memory", "search_memory":
		return true
	default:
		return false
	}
}

func hasUnbackedMemoryCommitment(content string) bool {
	if strings.Contains(strings.ToLower(content), strings.ToLower(uncommittedMemoryNote)) {
		return false
	}
	for _, pattern := range unbackedMemoryCommitmentPatterns {
		if pattern.MatchString(content) {
			return true
		}
	}
	return false
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
	return qualifyPersistedIDLabelsForTools(content, taskToolUsed, reminderToolUsed, false)
}

func qualifyPersistedIDLabelsForTools(content string, taskToolUsed, reminderToolUsed, memoryToolUsed bool) string {
	label := ""
	switch {
	case taskToolUsed && !reminderToolUsed && !memoryToolUsed:
		label = "Task ID"
	case reminderToolUsed && !taskToolUsed && !memoryToolUsed:
		label = "Reminder ID"
	case memoryToolUsed && !taskToolUsed && !reminderToolUsed:
		label = "Memory ID"
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

	prepared, err := a.PrepareChat(ctx, ChatInput{
		ChannelID: msg.ChannelID,
		SenderID:  msg.SenderID,
		MessageID: msg.MessageID,
		Content:   msg.Content,
		Reply:     msg.Reply,
	})
	if err != nil {
		return err
	}
	defer prepared.Release()

	ch, err := a.chanReg.Get(msg.ChannelID)
	if err != nil {
		_ = a.store.FinishTrace(context.Background(), prepared.TraceID, "failed", "channel", err)
		return fmt.Errorf("channel %s not found: %w", msg.ChannelID, err)
	}

	deliveryID, err := a.store.PrepareDelivery(ctx, prepared.TraceID, prepared.OutputEventID, msg.ChannelID, msg.SenderID, prepared.Content)
	if err != nil {
		_ = a.store.FinishTrace(context.Background(), prepared.TraceID, "failed", "delivery", err)
		return err
	}
	if err := a.store.MarkDeliveryAttempting(ctx, deliveryID); err != nil {
		_ = a.store.FinishTrace(context.Background(), prepared.TraceID, "failed", "delivery", err)
		return err
	}
	receipt, err := ch.SendMessage(ctx, msg.SenderID, prepared.Content)
	if err != nil {
		_ = a.store.FailDelivery(context.Background(), prepared.TraceID, deliveryID, err)
		return err
	}
	if err := a.store.CompleteDeliveryIndexed(ctx, prepared.TraceID, deliveryID, receipt.MessageID, msg.ChannelID, msg.SenderID, prepared.Content, prepared.StartHistoryID, prepared.Chunks); err != nil {
		return err
	}
	log.Printf("Response trace %d delivered as %s", prepared.TraceID, receipt.MessageID)
	return nil
}
