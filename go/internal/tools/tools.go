package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/openclaw/openclaw/go/internal/providers"
	"github.com/openclaw/openclaw/go/internal/state"
)

const memorySearchLimit = 5

type Context struct {
	ChannelID string
	SenderID  string
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
	store    *state.Store
	now      func() time.Time
	location *time.Location
}

func NewExecutor(store *state.Store, now func() time.Time, location *time.Location) *Executor {
	if now == nil {
		now = time.Now
	}
	if location == nil {
		location = time.Local
	}
	return &Executor{store: store, now: now, location: location}
}

func Definitions(location *time.Location) []providers.ToolDefinition {
	return []providers.ToolDefinition{
		ReminderDefinition(location),
		definition("manage_personality", "View or update the agent's SQLite-backed SOUL.md or IDENTITY.md. Only update when the user explicitly asks to change the agent's identity, name, voice, values, or personality.", `{
			"type":"object","additionalProperties":false,
			"properties":{
				"action":{"type":"string","enum":["view","update"]},
				"document":{"type":"string","enum":["SOUL.md","IDENTITY.md"]},
				"content":{"type":"string","description":"Complete replacement Markdown for update."}
			},"required":["action"]
		}`),
		definition("store_memory", "Store a stable preference or durable fact in the agent's global memory.", `{
			"type":"object","additionalProperties":false,
			"properties":{"content":{"type":"string","description":"One concise durable fact."}},"required":["content"]
		}`),
		definition("search_memory", "Search the agent's global memory for relevant durable facts.", `{
			"type":"object","additionalProperties":false,
			"properties":{"query":{"type":"string"}},"required":["query"]
		}`),
	}
}

func ReminderDefinition(location *time.Location) providers.ToolDefinition {
	if location == nil {
		location = time.Local
	}
	timezone := location.String()
	description := fmt.Sprintf("Add, list, update, or remove reminders for the current user. Add accepts multiple items and commits them together. Use cron schedules for wall-clock recurrence. The server timezone is %s; omit timezone to use it.", timezone)
	schema := fmt.Sprintf(`{
		"type":"object","additionalProperties":false,
		"properties":{
			"action":{"type":"string","enum":["add","list","update","remove"]},
			"items":{"type":"array","minItems":1,"maxItems":50,"items":{"type":"object","additionalProperties":false,"properties":{
				"message":{"type":"string"},
				"schedule":{"type":"object","additionalProperties":false,"properties":{
					"kind":{"type":"string","enum":["at","every","cron"]},
					"at":{"type":"string","description":"Future RFC3339 timestamp with explicit UTC offset for kind=at."},
					"every_ms":{"type":"integer","minimum":1,"description":"Fixed interval milliseconds for kind=every."},
					"anchor_at":{"type":"string","description":"Optional RFC3339 interval anchor for kind=every."},
					"expr":{"type":"string","description":"Five- or six-field cron expression in timezone wall-clock time for kind=cron."},
					"timezone":{"type":"string","description":"IANA timezone for kind=cron; omit to use server timezone %s."}
				},"required":["kind"]}
			},"required":["message","schedule"]}},
			"id":{"type":"integer","minimum":1,"description":"Reminder id for update."},
			"ids":{"type":"array","minItems":1,"items":{"type":"integer","minimum":1},"description":"Reminder ids for remove."},
			"patch":{"type":"object","additionalProperties":false,"properties":{
				"message":{"type":"string"},"enabled":{"type":"boolean"},
				"schedule":{"type":"object","additionalProperties":false,"properties":{
					"kind":{"type":"string","enum":["at","every","cron"]},"at":{"type":"string"},
					"every_ms":{"type":"integer","minimum":1},"anchor_at":{"type":"string"},
					"expr":{"type":"string"},"timezone":{"type":"string"}
				},"required":["kind"]}
			}}
		},"required":["action"]
	}`, timezone)
	return definition("manage_reminders", description, schema)
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
	if validationError != "" {
		result.Content = errorJSON(validationError)
		result.IsError = true
		return result, executor.recordResult(ctx, toolCtx, result)
	}

	err := executor.store.WithTx(ctx, func(tx *state.Tx) error {
		content, err := executor.execute(ctx, tx, toolCtx, call)
		if err != nil {
			return err
		}
		result.Content = content
		return saveResult(ctx, tx, toolCtx, result)
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

func (executor *Executor) execute(ctx context.Context, tx *state.Tx, toolCtx Context, call providers.ToolCall) (string, error) {
	switch call.Function.Name {
	case "manage_reminders":
		return executor.manageReminders(ctx, tx, toolCtx, call.Function.Arguments)
	case "manage_personality":
		return executor.managePersonality(ctx, tx, call.Function.Arguments)

	case "store_memory":
		var args struct {
			Content string `json:"content"`
		}
		if err := decodeArguments(call.Function.Arguments, &args); err != nil {
			return "", err
		}
		args.Content = strings.TrimSpace(args.Content)
		if args.Content == "" {
			return "", fmt.Errorf("content must not be empty")
		}
		stored, err := tx.SaveMemoryUnique(ctx, args.Content)
		if err != nil {
			return "", err
		}
		return marshalContent(map[string]any{"content": args.Content, "stored": stored})

	case "search_memory":
		var args struct {
			Query string `json:"query"`
		}
		if err := decodeArguments(call.Function.Arguments, &args); err != nil {
			return "", err
		}
		args.Query = strings.TrimSpace(args.Query)
		if args.Query == "" {
			return "", fmt.Errorf("query must not be empty")
		}
		entries, err := tx.SearchMemory(ctx, args.Query, memorySearchLimit)
		if err != nil {
			return "", err
		}
		memories := make([]string, 0, len(entries))
		for _, entry := range entries {
			memories = append(memories, entry.Content)
		}
		return marshalContent(map[string]any{"memories": memories})

	default:
		return "", fmt.Errorf("unknown tool %q", call.Function.Name)
	}
}

func (executor *Executor) managePersonality(ctx context.Context, tx *state.Tx, raw string) (string, error) {
	var args struct {
		Action   string `json:"action"`
		Document string `json:"document"`
		Content  string `json:"content"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return "", err
	}
	switch args.Action {
	case "view":
		if args.Document != "" || args.Content != "" {
			return "", fmt.Errorf("view does not accept document or content")
		}
		documents, err := tx.LoadPersonality(ctx)
		if err != nil {
			return "", err
		}
		return marshalContent(map[string]any{"documents": documents})
	case "update":
		if err := tx.UpdatePersonality(ctx, args.Document, args.Content); err != nil {
			return "", err
		}
		return marshalContent(map[string]any{
			"updated": true, "document": args.Document, "content": strings.TrimSpace(args.Content),
		})
	default:
		return "", fmt.Errorf("action must be view or update")
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

type reminderItemArguments struct {
	Message  string            `json:"message"`
	Schedule scheduleArguments `json:"schedule"`
}

type reminderPatchArguments struct {
	Message  *string            `json:"message"`
	Enabled  *bool              `json:"enabled"`
	Schedule *scheduleArguments `json:"schedule"`
}

type manageReminderArguments struct {
	Action string                  `json:"action"`
	Items  []reminderItemArguments `json:"items"`
	ID     int                     `json:"id"`
	IDs    []int                   `json:"ids"`
	Patch  *reminderPatchArguments `json:"patch"`
}

func (executor *Executor) manageReminders(ctx context.Context, tx *state.Tx, toolCtx Context, raw string) (string, error) {
	var args manageReminderArguments
	if err := decodeArguments(raw, &args); err != nil {
		return "", err
	}
	switch args.Action {
	case "add":
		if len(args.Items) == 0 || len(args.Items) > 50 {
			return "", fmt.Errorf("items must contain between 1 and 50 reminders")
		}
		added := make([]map[string]any, 0, len(args.Items))
		for index, item := range args.Items {
			message := strings.TrimSpace(item.Message)
			if message == "" {
				return "", fmt.Errorf("items[%d].message must not be empty", index)
			}
			schedule, fireAt, err := executor.resolveSchedule(item.Schedule)
			if err != nil {
				return "", fmt.Errorf("items[%d].schedule: %w", index, err)
			}
			id, err := tx.AddReminder(ctx, toolCtx.ChannelID, toolCtx.SenderID, message, schedule, fireAt)
			if err != nil {
				return "", err
			}
			added = append(added, executor.reminderContent(int(id), message, schedule, fireAt, true))
		}
		return marshalContent(map[string]any{"added": added, "count": len(added)})

	case "list":
		reminders, err := tx.ListReminders(ctx, toolCtx.ChannelID, toolCtx.SenderID)
		if err != nil {
			return "", err
		}
		items := make([]map[string]any, 0, len(reminders))
		for _, reminder := range reminders {
			items = append(items, executor.reminderContent(reminder.ID, reminder.Message, reminder.Schedule, reminder.FireAt, reminder.Enabled))
		}
		return marshalContent(map[string]any{"reminders": items})

	case "update":
		if args.ID < 1 || args.Patch == nil {
			return "", fmt.Errorf("update requires a positive id and patch")
		}
		if args.Patch.Message == nil && args.Patch.Enabled == nil && args.Patch.Schedule == nil {
			return "", fmt.Errorf("patch must change message, enabled, or schedule")
		}
		reminder, err := tx.GetReminderForUser(ctx, args.ID, toolCtx.ChannelID, toolCtx.SenderID)
		if err != nil {
			return "", err
		}
		wasEnabled := reminder.Enabled
		if args.Patch.Message != nil {
			message := strings.TrimSpace(*args.Patch.Message)
			if message == "" {
				return "", fmt.Errorf("patch.message must not be empty")
			}
			reminder.Message = message
		}
		if args.Patch.Enabled != nil {
			reminder.Enabled = *args.Patch.Enabled
		}
		if args.Patch.Schedule != nil {
			reminder.Schedule, reminder.FireAt, err = executor.resolveSchedule(*args.Patch.Schedule)
			if err != nil {
				return "", fmt.Errorf("patch.schedule: %w", err)
			}
		}
		if args.Patch.Enabled != nil && *args.Patch.Enabled && !wasEnabled && args.Patch.Schedule == nil {
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

	case "remove":
		if len(args.IDs) == 0 {
			return "", fmt.Errorf("remove requires at least one id")
		}
		seen := make(map[int]struct{}, len(args.IDs))
		for _, id := range args.IDs {
			if id < 1 {
				return "", fmt.Errorf("ids must contain positive integers")
			}
			if _, duplicate := seen[id]; duplicate {
				return "", fmt.Errorf("ids must not contain duplicates")
			}
			seen[id] = struct{}{}
			if err := tx.DeleteReminderForUser(ctx, id, toolCtx.ChannelID, toolCtx.SenderID); err != nil {
				return "", err
			}
		}
		return marshalContent(map[string]any{"removed_ids": args.IDs, "count": len(args.IDs)})

	default:
		return "", fmt.Errorf("action must be add, list, update, or remove")
	}
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
	return tx.SaveConversationMessage(ctx, toolCtx.ChannelID, toolCtx.SenderID, "tool", state.ContentToolResult, string(payload))
}

func (executor *Executor) recordResult(ctx context.Context, toolCtx Context, result Result) error {
	return executor.store.WithTx(ctx, func(tx *state.Tx) error {
		return saveResult(ctx, tx, toolCtx, result)
	})
}
