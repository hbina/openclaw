package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/openclaw/openclaw/go/internal/memory"
	"github.com/openclaw/openclaw/go/internal/providers"
	"github.com/openclaw/openclaw/go/internal/state"
)

type Context struct {
	ChannelID       string
	SenderID        string
	TraceEventID    int64
	ResponseTraceID int64
	SourceHistoryID int64
	Audience        string
}

type Result struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error"`
}

func (result Result) Message() providers.Message {
	return providers.Message{
		Role:       providers.RoleTool,
		Content:    result.Content,
		ToolCallID: result.ToolCallID,
	}
}

type Executor struct {
	store      *state.Store
	now        func() time.Time
	location   *time.Location
	embedder   providers.Embedder
	indexID    string
	dimensions int
	minScore   float64
	memory     *memory.Service
}

func NewExecutor(
	store *state.Store,
	now func() time.Time,
	location *time.Location,
	embedder providers.Embedder,
	indexID string,
	dimensions int,
	minScore float64,
) *Executor {
	if now == nil {
		now = time.Now
	}
	if location == nil {
		location = time.Local
	}
	return &Executor{
		store: store, now: now, location: location,
		embedder: embedder, indexID: indexID, dimensions: dimensions, minScore: minScore,
		memory: memory.NewService(store, embedder, indexID, dimensions, minScore, now),
	}
}

// memoryToolInput is the trimmed text and embedding vector resolved before a
// store_memory/search_memory tool call opens its SQL transaction, since the
// embedding call is network I/O and shouldn't happen while a transaction is
// held open.
type memoryToolInput struct {
	write   *state.MemoryWrite
	search  *memory.PreparedSearch
	id      int64
	kind    *state.MemoryKind
	status  state.MemoryStatus
	limit   int
	mutated bool
}

func (executor *Executor) prepareUpdateMemory(ctx context.Context, raw string) (*memoryToolInput, error) {
	var args struct {
		ID      int64             `json:"id"`
		Content string            `json:"content"`
		Kind    *state.MemoryKind `json:"kind"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return nil, err
	}
	if args.ID < 1 {
		return nil, fmt.Errorf("id must be positive")
	}
	kind := state.MemoryKind("")
	if args.Kind != nil {
		kind = *args.Kind
	}
	write, err := executor.memory.PrepareWrite(ctx, kindOrDurable(kind), args.Content, memory.Provenance{Origin: state.MemoryOriginAgent, Source: state.MemorySourceChat})
	if err != nil {
		return nil, err
	}
	write.Kind = kind
	return &memoryToolInput{id: args.ID, write: &write}, nil
}

func kindOrDurable(kind state.MemoryKind) state.MemoryKind {
	if kind == "" {
		return state.MemoryDurable
	}
	return kind
}

func prepareMemoryRead(raw string, list bool) (*memoryToolInput, error) {
	if list {
		var args struct {
			Kind   *state.MemoryKind  `json:"kind"`
			Status state.MemoryStatus `json:"status"`
			Limit  int                `json:"limit"`
		}
		if err := decodeArguments(raw, &args); err != nil {
			return nil, err
		}
		if args.Kind != nil && !state.ValidMemoryKind(*args.Kind) {
			return nil, fmt.Errorf("invalid memory kind")
		}
		if args.Status != "" && args.Status != state.MemoryActive && args.Status != state.MemoryDeleted && args.Status != "all" {
			return nil, fmt.Errorf("invalid memory status")
		}
		if args.Limit < 0 || args.Limit > 100 {
			return nil, fmt.Errorf("limit must be between 1 and 100")
		}
		return &memoryToolInput{kind: args.Kind, status: args.Status, limit: args.Limit}, nil
	}
	var args struct {
		ID int64 `json:"id"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return nil, err
	}
	if args.ID < 1 {
		return nil, fmt.Errorf("id must be positive")
	}
	return &memoryToolInput{id: args.ID}, nil
}

func (executor *Executor) prepareStoreMemory(ctx context.Context, raw string) (*memoryToolInput, error) {
	var args struct {
		Content string           `json:"content"`
		Kind    state.MemoryKind `json:"kind"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return nil, err
	}
	write, err := executor.memory.PrepareWrite(ctx, args.Kind, args.Content, memory.Provenance{Origin: state.MemoryOriginAgent, Source: state.MemorySourceChat})
	if err != nil {
		return nil, err
	}
	return &memoryToolInput{write: &write}, nil
}

func (executor *Executor) prepareSearchMemory(ctx context.Context, raw string) (*memoryToolInput, error) {
	var args struct {
		Query      string            `json:"query"`
		Kind       *state.MemoryKind `json:"kind"`
		MaxResults int               `json:"max_results"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return nil, err
	}
	prepared, err := executor.memory.PrepareSearch(ctx, args.Query, nil, args.MaxResults)
	if err != nil {
		return nil, err
	}
	return &memoryToolInput{search: &prepared, kind: args.Kind}, nil
}

func Definitions(location *time.Location) []providers.ToolDefinition {
	if location == nil {
		location = time.Local
	}
	timezone := location.String()
	schedule := fmt.Sprintf(`{
		"type":"object","additionalProperties":false,
		"properties":{
			"kind":{"type":"string","enum":["at","every","cron"]},
			"at":{"type":"string","description":"Future RFC3339 timestamp with explicit UTC offset for kind=at."},
			"every_ms":{"type":"integer","minimum":1,"description":"Fixed interval milliseconds for kind=every."},
			"anchor_at":{"type":"string","description":"Optional RFC3339 interval anchor for kind=every."},
			"expr":{"type":"string","description":"Five- or six-field cron expression in timezone wall-clock time for kind=cron."},
			"timezone":{"type":"string","description":"IANA timezone for kind=cron; omit to use server timezone %s."}
		},"required":["kind"]
	}`, timezone)
	return []providers.ToolDefinition{
		definition("add_reminder", "Add one reminder for the current user. The server timezone is "+timezone+"; omit cron timezone to use it.", fmt.Sprintf(`{
			"type":"object","additionalProperties":false,
			"properties":{"message":{"type":"string"},"schedule":%s},
			"required":["message","schedule"]
		}`, schedule)),
		definition("list_reminders", "List reminders for the current user.", `{
			"type":"object","additionalProperties":false,"properties":{}
		}`),
		definition("update_reminder", "Update one reminder for the current user. Supply at least one of message, enabled, or schedule.", fmt.Sprintf(`{
			"type":"object","additionalProperties":false,"minProperties":2,
			"properties":{"id":{"type":"integer","minimum":1},"message":{"type":"string"},"enabled":{"type":"boolean"},"schedule":%s},
			"required":["id"]
		}`, schedule)),
		definition("remove_reminder", "Remove one reminder for the current user.", `{
			"type":"object","additionalProperties":false,
			"properties":{"id":{"type":"integer","minimum":1}},"required":["id"]
		}`),
		definition("add_task", "Add one owner-global task. Tasks start immediately and have no schedule.", `{
			"type":"object","additionalProperties":false,
			"properties":{"description":{"type":"string"}},"required":["description"]
		}`),
		definition("list_tasks", "List owner-global tasks. The status defaults to open.", `{
			"type":"object","additionalProperties":false,
			"properties":{"status":{"type":"string","enum":["open","completed","all"]}}
		}`),
		definition("update_task", "Replace the description of one open owner-global task.", `{
			"type":"object","additionalProperties":false,
			"properties":{"id":{"type":"integer","minimum":1},"description":{"type":"string"}},"required":["id","description"]
		}`),
		definition("complete_task", "Complete one open owner-global task. Completion is final.", `{
			"type":"object","additionalProperties":false,
			"properties":{"id":{"type":"integer","minimum":1}},"required":["id"]
		}`),
		definition("remove_task", "Remove one owner-global task.", `{
			"type":"object","additionalProperties":false,
			"properties":{"id":{"type":"integer","minimum":1}},"required":["id"]
		}`),
		definition("store_memory", "Store one profile, durable, or daily memory.", `{
			"type":"object","additionalProperties":false,
			"properties":{"content":{"type":"string","description":"One concise standalone fact."},"kind":{"type":"string","enum":["profile","durable","daily"]}},"required":["content","kind"]
		}`),
		definition("get_memory", "Get one memory by its stable Memory ID.", `{
			"type":"object","additionalProperties":false,"properties":{"id":{"type":"integer","minimum":1}},"required":["id"]
		}`),
		definition("list_memories", "List memories. Status defaults to active and limit defaults to 20.", `{
			"type":"object","additionalProperties":false,"properties":{"kind":{"type":"string","enum":["profile","durable","daily"]},"status":{"type":"string","enum":["active","deleted","all"]},"limit":{"type":"integer","minimum":1,"maximum":100}}
		}`),
		definition("update_memory", "Create a new revision of one active memory, retaining its stable Memory ID.", `{
			"type":"object","additionalProperties":false,"properties":{"id":{"type":"integer","minimum":1},"content":{"type":"string"},"kind":{"type":"string","enum":["profile","durable","daily"]}},"required":["id","content"]
		}`),
		definition("remove_memory", "Stop one memory from being recalled. Audit revisions are retained.", `{
			"type":"object","additionalProperties":false,"properties":{"id":{"type":"integer","minimum":1}},"required":["id"]
		}`),
		definition("search_memory", "Hybrid keyword and semantic search over active memories.", `{
			"type":"object","additionalProperties":false,
			"properties":{"query":{"type":"string"},"kind":{"type":"string","enum":["profile","durable","daily"]},"max_results":{"type":"integer","minimum":1,"maximum":20}},"required":["query"]
		}`),
	}
}

func definition(name, description, schema string) providers.ToolDefinition {
	return providers.ToolDefinition{
		Type: "function",
		Function: providers.FunctionDefinition{
			Name:        name,
			Description: description,
			Parameters:  json.RawMessage(schema),
		},
	}
}

// ExecuteAndRecord validates trusted tool input, executes it, and commits the
// state change and matching tool-result transcript row together.
func (executor *Executor) ExecuteAndRecord(ctx context.Context, toolCtx Context, call providers.ToolCall) (Result, error) {
	result := Result{ToolCallID: call.ID, Name: call.Function.Name}
	validationError := ""
	if strings.TrimSpace(toolCtx.ChannelID) == "" || strings.TrimSpace(toolCtx.SenderID) == "" {
		validationError = "trusted channel and sender identity are required"
	} else if call.ID == "" {
		validationError = "tool call id is required"
	} else if call.Type != "function" {
		validationError = fmt.Sprintf("unsupported tool call type %q", call.Type)
	}

	// Embedding calls are network I/O and must not happen while a SQL
	// transaction is held open, so resolve them before store.WithTx below.
	var memoryInput *memoryToolInput
	if validationError == "" {
		var prepareErr error
		switch call.Function.Name {
		case "store_memory":
			memoryInput, prepareErr = executor.prepareStoreMemory(ctx, call.Function.Arguments)
		case "get_memory", "remove_memory":
			memoryInput, prepareErr = prepareMemoryRead(call.Function.Arguments, false)
		case "list_memories":
			memoryInput, prepareErr = prepareMemoryRead(call.Function.Arguments, true)
		case "update_memory":
			memoryInput, prepareErr = executor.prepareUpdateMemory(ctx, call.Function.Arguments)
		case "search_memory":
			memoryInput, prepareErr = executor.prepareSearchMemory(ctx, call.Function.Arguments)
		}
		if prepareErr != nil {
			validationError = prepareErr.Error()
		}
	}

	if validationError != "" {
		result.Content = errorJSON(validationError)
		result.IsError = true
		return result, executor.recordResult(ctx, toolCtx, result)
	}

	err := executor.store.WithTx(ctx, func(tx *state.Tx) error {
		content, err := executor.execute(ctx, tx, toolCtx, call, memoryInput)
		if err != nil {
			return err
		}
		result.Content = content
		if err := saveResult(ctx, tx, toolCtx, result); err != nil {
			return err
		}
		payload, err := json.Marshal(result)
		if err != nil {
			return err
		}
		committed := toolMutation(call.Function.Name)
		if isMemoryMutationTool(call.Function.Name) {
			committed = memoryInput != nil && memoryInput.mutated
		}
		return tx.FinishToolExecution(ctx, toolCtx.TraceEventID, string(payload), result.IsError, committed)
	})
	if err == nil {
		return result, nil
	}

	result.Content = errorJSON(err.Error())
	result.IsError = true
	if recordErr := executor.recordResult(ctx, toolCtx, result); recordErr != nil {
		return Result{}, fmt.Errorf("execute tool %q: %v; record error result: %w", call.Function.Name, err, recordErr)
	}
	return result, nil
}

func (executor *Executor) execute(ctx context.Context, tx *state.Tx, toolCtx Context, call providers.ToolCall, memoryInput *memoryToolInput) (string, error) {
	switch call.Function.Name {
	case "add_reminder":
		return executor.addReminder(ctx, tx, toolCtx, call.Function.Arguments)
	case "list_reminders":
		return executor.listReminders(ctx, tx, toolCtx, call.Function.Arguments)
	case "update_reminder":
		return executor.updateReminder(ctx, tx, toolCtx, call.Function.Arguments)
	case "remove_reminder":
		return executor.removeReminder(ctx, tx, toolCtx, call.Function.Arguments)
	case "add_task":
		return executor.addTask(ctx, tx, call.Function.Arguments)
	case "list_tasks":
		return executor.listTasks(ctx, tx, call.Function.Arguments)
	case "update_task":
		return executor.updateTask(ctx, tx, call.Function.Arguments)
	case "complete_task":
		return executor.completeTask(ctx, tx, call.Function.Arguments)
	case "remove_task":
		return executor.removeTask(ctx, tx, call.Function.Arguments)
	case "store_memory":
		memoryWrite := *memoryInput.write
		if toolCtx.SourceHistoryID > 0 {
			memoryWrite.SourceHistoryID = &toolCtx.SourceHistoryID
		}
		if toolCtx.ResponseTraceID > 0 {
			memoryWrite.SourceTraceID = &toolCtx.ResponseTraceID
		}
		storedMemory, stored, err := tx.StoreMemory(ctx, memoryWrite)
		if err != nil {
			return "", err
		}
		memoryInput.mutated = stored
		return marshalContent(map[string]any{"memory": storedMemory, "stored": stored})

	case "get_memory":
		storedMemory, err := tx.GetMemory(ctx, memoryInput.id)
		if err != nil {
			return "", err
		}
		return marshalContent(map[string]any{"memory": storedMemory})

	case "list_memories":
		memories, err := tx.ListMemories(ctx, state.MemoryFilter{Kind: memoryInput.kind, Status: memoryInput.status, Limit: memoryInput.limit})
		if err != nil {
			return "", err
		}
		return marshalContent(map[string]any{"memories": memories})

	case "update_memory":
		memoryWrite := *memoryInput.write
		if toolCtx.SourceHistoryID > 0 {
			memoryWrite.SourceHistoryID = &toolCtx.SourceHistoryID
		}
		if toolCtx.ResponseTraceID > 0 {
			memoryWrite.SourceTraceID = &toolCtx.ResponseTraceID
		}
		storedMemory, err := tx.UpdateMemory(ctx, memoryInput.id, memoryWrite)
		if err != nil {
			return "", err
		}
		memoryInput.mutated = true
		return marshalContent(map[string]any{"updated": storedMemory})

	case "remove_memory":
		storedMemory, err := tx.RemoveMemory(ctx, memoryInput.id, executor.now())
		if err != nil {
			return "", err
		}
		memoryInput.mutated = true
		return marshalContent(map[string]any{"removed": storedMemory})

	case "search_memory":
		entries, err := tx.SearchMemories(ctx, executor.indexID, executor.dimensions, memoryInput.search.Embedding, memoryInput.search.FTSQuery, executor.minScore, memoryInput.search.Limit)
		if err != nil {
			return "", err
		}
		if memoryInput.kind != nil {
			filtered := entries[:0]
			for _, entry := range entries {
				if entry.Kind == *memoryInput.kind {
					filtered = append(filtered, entry)
				}
			}
			entries = filtered
		}
		return marshalContent(map[string]any{"memories": entries})

	default:
		return "", fmt.Errorf("unknown tool %q", call.Function.Name)
	}
}

type taskDescriptionArguments struct {
	Description string `json:"description"`
}

type taskListArguments struct {
	Status *state.TaskStatus `json:"status"`
}

type taskUpdateArguments struct {
	ID          int    `json:"id"`
	Description string `json:"description"`
}

type idArguments struct {
	ID int `json:"id"`
}

func (executor *Executor) addTask(ctx context.Context, tx *state.Tx, raw string) (string, error) {
	var args taskDescriptionArguments
	if err := decodeArguments(raw, &args); err != nil {
		return "", err
	}
	description := strings.TrimSpace(args.Description)
	if description == "" {
		return "", fmt.Errorf("description must not be empty")
	}
	task, err := tx.AddTask(ctx, description, executor.now())
	if err != nil {
		return "", err
	}
	return marshalContent(map[string]any{"added": executor.taskContent(task), "display_timezone": executor.location.String()})
}

func (executor *Executor) listTasks(ctx context.Context, tx *state.Tx, raw string) (string, error) {
	var args taskListArguments
	if err := decodeArguments(raw, &args); err != nil {
		return "", err
	}
	status := state.TaskOpen
	if args.Status != nil {
		status = *args.Status
	}
	tasks, err := tx.ListTasks(ctx, status)
	if err != nil {
		return "", err
	}
	items := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		items = append(items, executor.taskContent(task))
	}
	return marshalContent(map[string]any{"tasks": items, "status": status, "display_timezone": executor.location.String()})
}

func (executor *Executor) updateTask(ctx context.Context, tx *state.Tx, raw string) (string, error) {
	var args taskUpdateArguments
	if err := decodeArguments(raw, &args); err != nil {
		return "", err
	}
	if args.ID < 1 || strings.TrimSpace(args.Description) == "" {
		return "", fmt.Errorf("id must be positive and description must not be empty")
	}
	task, err := tx.UpdateTask(ctx, args.ID, args.Description)
	if err != nil {
		return "", err
	}
	return marshalContent(map[string]any{"updated": executor.taskContent(task), "display_timezone": executor.location.String()})
}

func (executor *Executor) completeTask(ctx context.Context, tx *state.Tx, raw string) (string, error) {
	var args idArguments
	if err := decodeArguments(raw, &args); err != nil {
		return "", err
	}
	if args.ID < 1 {
		return "", fmt.Errorf("id must be positive")
	}
	task, err := tx.CompleteTask(ctx, args.ID, executor.now())
	if err != nil {
		return "", err
	}
	return marshalContent(map[string]any{"completed": executor.taskContent(task), "display_timezone": executor.location.String()})
}

func (executor *Executor) removeTask(ctx context.Context, tx *state.Tx, raw string) (string, error) {
	var args idArguments
	if err := decodeArguments(raw, &args); err != nil {
		return "", err
	}
	if args.ID < 1 {
		return "", fmt.Errorf("id must be positive")
	}
	task, err := tx.DeleteTask(ctx, args.ID)
	if err != nil {
		return "", err
	}
	return marshalContent(map[string]any{"removed": executor.taskContent(task), "display_timezone": executor.location.String()})
}

func (executor *Executor) taskContent(task state.Task) map[string]any {
	var completedAt any
	if task.CompletedAt != nil {
		completedAt = task.CompletedAt.In(executor.location).Format(time.RFC3339)
	}
	return map[string]any{
		"id": task.ID, "description": task.Description, "status": task.Status(),
		"started_at":   task.StartedAt.In(executor.location).Format(time.RFC3339),
		"completed_at": completedAt,
	}
}

type scheduleArguments struct {
	Kind     state.ScheduleKind `json:"kind"`
	At       string             `json:"at"`
	EveryMS  int64              `json:"every_ms"`
	AnchorAt string             `json:"anchor_at"`
	Expr     string             `json:"expr"`
	Timezone string             `json:"timezone"`
}

type addReminderArguments struct {
	Message  string            `json:"message"`
	Schedule scheduleArguments `json:"schedule"`
}

type updateReminderArguments struct {
	ID       int                `json:"id"`
	Message  *string            `json:"message"`
	Enabled  *bool              `json:"enabled"`
	Schedule *scheduleArguments `json:"schedule"`
}

func (executor *Executor) addReminder(ctx context.Context, tx *state.Tx, toolCtx Context, raw string) (string, error) {
	var args addReminderArguments
	if err := decodeArguments(raw, &args); err != nil {
		return "", err
	}
	message := strings.TrimSpace(args.Message)
	if message == "" {
		return "", fmt.Errorf("message must not be empty")
	}
	schedule, fireAt, err := executor.resolveSchedule(args.Schedule)
	if err != nil {
		return "", fmt.Errorf("schedule: %w", err)
	}
	id, err := tx.AddReminder(ctx, toolCtx.ChannelID, toolCtx.SenderID, message, schedule, fireAt)
	if err != nil {
		return "", err
	}
	return marshalContent(map[string]any{"added": executor.reminderContent(int(id), message, schedule, fireAt, true)})
}

func (executor *Executor) listReminders(ctx context.Context, tx *state.Tx, toolCtx Context, raw string) (string, error) {
	var args struct{}
	if err := decodeArguments(raw, &args); err != nil {
		return "", err
	}
	reminders, err := tx.ListReminders(ctx, toolCtx.ChannelID, toolCtx.SenderID)
	if err != nil {
		return "", err
	}
	items := make([]map[string]any, 0, len(reminders))
	for _, reminder := range reminders {
		items = append(items, executor.reminderContent(reminder.ID, reminder.Message, reminder.Schedule, reminder.FireAt, reminder.Enabled))
	}
	return marshalContent(map[string]any{"reminders": items})
}

func (executor *Executor) updateReminder(ctx context.Context, tx *state.Tx, toolCtx Context, raw string) (string, error) {
	var args updateReminderArguments
	if err := decodeArguments(raw, &args); err != nil {
		return "", err
	}
	if args.ID < 1 {
		return "", fmt.Errorf("id must be positive")
	}
	if args.Message == nil && args.Enabled == nil && args.Schedule == nil {
		return "", fmt.Errorf("update must change message, enabled, or schedule")
	}
	reminder, err := tx.GetReminderForUser(ctx, args.ID, toolCtx.ChannelID, toolCtx.SenderID)
	if err != nil {
		return "", err
	}
	wasEnabled := reminder.Enabled
	if args.Message != nil {
		message := strings.TrimSpace(*args.Message)
		if message == "" {
			return "", fmt.Errorf("message must not be empty")
		}
		reminder.Message = message
	}
	if args.Enabled != nil {
		reminder.Enabled = *args.Enabled
	}
	if args.Schedule != nil {
		reminder.Schedule, reminder.FireAt, err = executor.resolveSchedule(*args.Schedule)
		if err != nil {
			return "", fmt.Errorf("schedule: %w", err)
		}
	}
	if args.Enabled != nil && *args.Enabled && !wasEnabled && args.Schedule == nil {
		if reminder.Schedule.Kind == state.ScheduleCron && strings.TrimSpace(reminder.Schedule.Timezone) == "" {
			reminder.Schedule.Timezone = executor.location.String()
		}
		reminder.FireAt, err = state.NextReminderRun(reminder.Schedule, executor.now())
		if err != nil {
			return "", fmt.Errorf("re-enable reminder: %w; provide a new schedule", err)
		}
	}
	if err := tx.UpdateReminderForUser(ctx, reminder); err != nil {
		return "", err
	}
	return marshalContent(map[string]any{"updated": executor.reminderContent(reminder.ID, reminder.Message, reminder.Schedule, reminder.FireAt, reminder.Enabled)})
}

func (executor *Executor) removeReminder(ctx context.Context, tx *state.Tx, toolCtx Context, raw string) (string, error) {
	var args idArguments
	if err := decodeArguments(raw, &args); err != nil {
		return "", err
	}
	if args.ID < 1 {
		return "", fmt.Errorf("id must be positive")
	}
	if err := tx.DeleteReminderForUser(ctx, args.ID, toolCtx.ChannelID, toolCtx.SenderID); err != nil {
		return "", err
	}
	return marshalContent(map[string]any{"removed_id": args.ID})
}

func (executor *Executor) resolveSchedule(args scheduleArguments) (state.ReminderSchedule, time.Time, error) {
	now := executor.now()
	schedule := state.ReminderSchedule{Kind: args.Kind}
	switch args.Kind {
	case state.ScheduleAt:
		if args.EveryMS != 0 || args.AnchorAt != "" || args.Expr != "" || args.Timezone != "" {
			return schedule, time.Time{}, fmt.Errorf("at schedules only accept at")
		}
		at, err := parseRFC3339(args.At, "at", true)
		if err != nil {
			return schedule, time.Time{}, err
		}
		schedule.At = at
	case state.ScheduleEvery:
		if args.At != "" || args.Expr != "" || args.Timezone != "" {
			return schedule, time.Time{}, fmt.Errorf("every schedules only accept every_ms and optional anchor_at")
		}
		schedule.EveryMS = args.EveryMS
		if strings.TrimSpace(args.AnchorAt) != "" {
			anchor, err := parseRFC3339(args.AnchorAt, "anchor_at", false)
			if err != nil {
				return schedule, time.Time{}, err
			}
			schedule.AnchorAt = anchor
		} else {
			schedule.AnchorAt = now
		}
	case state.ScheduleCron:
		if args.At != "" || args.EveryMS != 0 || args.AnchorAt != "" {
			return schedule, time.Time{}, fmt.Errorf("cron schedules only accept expr and timezone")
		}
		schedule.CronExpr = strings.TrimSpace(args.Expr)
		schedule.Timezone = strings.TrimSpace(args.Timezone)
		if schedule.Timezone == "" {
			schedule.Timezone = executor.location.String()
		}
	default:
		return schedule, time.Time{}, fmt.Errorf("kind must be at, every, or cron")
	}
	fireAt, err := state.NextReminderRun(schedule, now)
	return schedule, fireAt, err
}

func parseRFC3339(raw, field string, requireFuture bool) (time.Time, error) {
	if !hasExplicitOffset(raw) {
		return time.Time{}, fmt.Errorf("%s must be RFC3339 with an explicit UTC offset", field)
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be valid RFC3339: %w", field, err)
	}
	if requireFuture && parsed.IsZero() {
		return time.Time{}, fmt.Errorf("%s must not be zero", field)
	}
	return parsed, nil
}

func (executor *Executor) reminderContent(id int, message string, schedule state.ReminderSchedule, fireAt time.Time, enabled bool) map[string]any {
	return map[string]any{
		"id": id, "message": message, "schedule": state.ScheduleDescription(schedule),
		"next_fire_at":     fireAt.In(executor.location).Format(time.RFC3339),
		"display_timezone": executor.location.String(), "enabled": enabled,
	}
}

func hasExplicitOffset(value string) bool {
	value = strings.TrimSpace(value)
	if strings.HasSuffix(value, "Z") {
		return true
	}
	t := strings.LastIndexByte(value, 'T')
	if t < 0 {
		return false
	}
	return strings.ContainsAny(value[t+1:], "+-")
}

func decodeArguments(raw string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("invalid arguments: trailing JSON")
	}
	return nil
}

func marshalContent(value any) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", fmt.Errorf("encode tool result: %w", err)
	}
	return strings.TrimSpace(buffer.String()), nil
}

func errorJSON(message string) string {
	content, _ := marshalContent(map[string]string{"error": message})
	return content
}

func saveResult(ctx context.Context, tx *state.Tx, toolCtx Context, result Result) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode tool transcript result: %w", err)
	}
	audience := toolCtx.Audience
	if audience == "" {
		audience = state.AudienceConversation
	}
	return tx.SaveConversationMessageAudience(ctx, toolCtx.ChannelID, toolCtx.SenderID, "tool", state.ContentToolResult, audience, string(payload))
}

func (executor *Executor) recordResult(ctx context.Context, toolCtx Context, result Result) error {
	return executor.store.WithTx(ctx, func(tx *state.Tx) error {
		if err := saveResult(ctx, tx, toolCtx, result); err != nil {
			return err
		}
		payload, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return tx.FinishToolExecution(ctx, toolCtx.TraceEventID, string(payload), result.IsError, false)
	})
}

func toolMutation(name string) bool {
	switch name {
	case "add_reminder", "update_reminder", "remove_reminder", "add_task", "update_task", "complete_task", "remove_task", "store_memory", "update_memory", "remove_memory":
		return true
	default:
		return false
	}
}

func isMemoryMutationTool(name string) bool {
	return name == "store_memory" || name == "update_memory" || name == "remove_memory"
}
