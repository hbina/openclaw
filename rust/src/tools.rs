use std::sync::Arc;

use chrono::{DateTime, Local, SecondsFormat, Utc};
use chrono_tz::Tz;
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use serde_json::{Value, json};
use thiserror::Error;

use crate::{
    memory::{MemoryError, PreparedSearch, Provenance, Service as MemoryService},
    providers::{Embedder, FunctionDefinition, Message, MessageRole, ToolCall, ToolDefinition},
    state::{
        AUDIENCE_CONVERSATION, CONTENT_TOOL_RESULT, MemoryFilter, MemoryKind, MemoryOrigin,
        MemorySource, MemoryStatus, MemoryWrite, ReminderSchedule, ScheduleKind, StateError,
        StateTx, Store, Task, TaskIndex, TaskStatus, next_reminder_run,
    },
    task_context::{TaskContextError, TaskContextService},
};

#[derive(Debug, Clone, Default)]
pub struct ToolContext {
    pub channel_id: String,
    pub sender_id: String,
    pub conversation_id: String,
    pub trace_event_id: i64,
    pub response_trace_id: i64,
    pub source_history_id: i64,
    pub audience: String,
    pub inbound_event_id: Option<i64>,
    pub inbound_lease_owner: String,
    pub inbound_lease_generation: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ToolResult {
    pub tool_call_id: String,
    pub name: String,
    pub content: String,
    pub is_error: bool,
}

impl ToolResult {
    pub fn message(&self) -> Message {
        Message {
            role: MessageRole::Tool,
            content: self.content.clone(),
            tool_calls: Vec::new(),
            tool_call_id: self.tool_call_id.clone(),
        }
    }
}

#[derive(Debug, Error)]
pub enum ToolError {
    #[error("{0}")]
    Validation(String),
    #[error("memory operation failed: {0}")]
    Memory(#[from] MemoryError),
    #[error("task context operation failed: {0}")]
    TaskContext(#[from] TaskContextError),
    #[error("state operation failed: {0}")]
    State(#[from] StateError),
    #[error("JSON operation failed: {0}")]
    Json(#[from] serde_json::Error),
}

pub struct Executor {
    store: Arc<Store>,
    timezone: RuntimeTimezone,
    index_id: String,
    dimensions: usize,
    min_score: f64,
    memory: MemoryService,
    task_context: TaskContextService,
    now: Arc<dyn Fn() -> DateTime<Utc> + Send + Sync>,
}

#[derive(Debug, Clone)]
enum RuntimeTimezone {
    Local,
    Named(Tz),
}

impl RuntimeTimezone {
    fn parse(value: &str) -> Result<Self, ToolError> {
        if value.trim().is_empty() || value == "Local" {
            Ok(Self::Local)
        } else {
            value
                .parse::<Tz>()
                .map(Self::Named)
                .map_err(|_| ToolError::Validation(format!("invalid IANA timezone {value:?}")))
        }
    }

    fn name(&self) -> String {
        match self {
            Self::Local => "Local".into(),
            Self::Named(timezone) => timezone.name().into(),
        }
    }

    fn format(&self, value: DateTime<Utc>) -> String {
        match self {
            Self::Local => value
                .with_timezone(&Local)
                .to_rfc3339_opts(SecondsFormat::Secs, true),
            Self::Named(timezone) => value
                .with_timezone(timezone)
                .to_rfc3339_opts(SecondsFormat::Secs, true),
        }
    }
}

#[derive(Default)]
struct MemoryToolInput {
    write: Option<MemoryWrite>,
    search: Option<PreparedSearch>,
    id: i64,
    kind: Option<MemoryKind>,
    status: MemoryStatus,
    limit: usize,
    mutated: bool,
}

#[derive(Default)]
struct TaskToolInput {
    title_index: Option<TaskIndex>,
    context_index: Option<TaskIndex>,
}

impl Executor {
    pub fn new(
        store: Arc<Store>,
        timezone: &str,
        embedder: Arc<dyn Embedder>,
        index_id: impl Into<String>,
        dimensions: usize,
        min_score: f64,
    ) -> Result<Self, ToolError> {
        Self::with_clock(
            store,
            timezone,
            embedder,
            index_id,
            dimensions,
            min_score,
            Utc::now,
        )
    }

    #[allow(clippy::too_many_arguments)]
    pub fn with_clock(
        store: Arc<Store>,
        timezone: &str,
        embedder: Arc<dyn Embedder>,
        index_id: impl Into<String>,
        dimensions: usize,
        min_score: f64,
        now: impl Fn() -> DateTime<Utc> + Send + Sync + 'static,
    ) -> Result<Self, ToolError> {
        let index_id = index_id.into();
        let now: Arc<dyn Fn() -> DateTime<Utc> + Send + Sync> = Arc::new(now);
        let memory_now = Arc::clone(&now);
        let memory = MemoryService::with_clock(
            Arc::clone(&store),
            Arc::clone(&embedder),
            index_id.clone(),
            dimensions,
            min_score,
            move || memory_now(),
        );
        let task_context = TaskContextService::new(
            Arc::clone(&store),
            embedder,
            index_id.clone(),
            dimensions,
            min_score,
        );
        Ok(Self {
            store,
            timezone: RuntimeTimezone::parse(timezone)?,
            index_id,
            dimensions,
            min_score,
            memory,
            task_context,
            now,
        })
    }

    pub async fn execute_and_record(
        &self,
        context: &ToolContext,
        call: &ToolCall,
    ) -> Result<ToolResult, ToolError> {
        let mut result = ToolResult {
            tool_call_id: call.id.clone(),
            name: call.function.name.clone(),
            content: String::new(),
            is_error: false,
        };

        let mut validation_error = if context.channel_id.trim().is_empty()
            || context.sender_id.trim().is_empty()
            || context.conversation_id.trim().is_empty()
        {
            Some("trusted channel, sender, and conversation identity are required".to_owned())
        } else if call.id.is_empty() {
            Some("tool call id is required".to_owned())
        } else if call.kind != "function" {
            Some(format!("unsupported tool call type {:?}", call.kind))
        } else {
            None
        };

        let mut memory_input = None;
        let mut task_input = None;
        if validation_error.is_none() {
            match self.prepare_memory_input(call).await {
                Ok(input) => memory_input = input,
                Err(error) => validation_error = Some(error.to_string()),
            }
        }
        if validation_error.is_none() {
            match self.prepare_task_input(call).await {
                Ok(input) => task_input = input,
                Err(error) => validation_error = Some(error.to_string()),
            }
        }
        if let Some(error) = validation_error {
            result.content = error_json(&error);
            result.is_error = true;
            self.record_result(context, &result)?;
            return Ok(result);
        }

        let operation: Result<(), ToolError> = self.store.with_tx(|tx| {
            if let Some(event_id) = context.inbound_event_id {
                tx.assert_inbound_claim(
                    event_id,
                    &context.inbound_lease_owner,
                    context.inbound_lease_generation,
                    (self.now)(),
                )?;
            }
            result.content = self.execute(
                tx,
                context,
                call,
                memory_input.as_mut(),
                task_input.as_ref(),
            )?;
            save_result(tx, context, &result)?;
            let mut committed = tool_mutation(&call.function.name);
            if is_memory_mutation_tool(&call.function.name) {
                committed = memory_input.as_ref().is_some_and(|input| input.mutated);
            }
            tx.finish_tool_execution(
                context.trace_event_id,
                &serde_json::to_string(&result)?,
                result.is_error,
                committed,
            )?;
            Ok::<(), ToolError>(())
        });
        if let Err(error) = operation {
            result.content = error_json(&error.to_string());
            result.is_error = true;
            self.record_result(context, &result)?;
        }
        Ok(result)
    }

    async fn prepare_memory_input(
        &self,
        call: &ToolCall,
    ) -> Result<Option<MemoryToolInput>, ToolError> {
        match call.function.name.as_str() {
            "store_memory" => {
                let args: StoreMemoryArguments = decode_arguments(&call.function.arguments)?;
                let kind = parse_memory_kind(&args.kind)?;
                let write = self
                    .memory
                    .prepare_write(
                        kind,
                        &args.content,
                        Provenance {
                            origin: MemoryOrigin::Agent,
                            source: MemorySource::Chat,
                            source_history_id: None,
                            source_trace_id: None,
                        },
                    )
                    .await?;
                Ok(Some(MemoryToolInput {
                    write: Some(write),
                    ..Default::default()
                }))
            }
            "update_memory" => {
                let args: UpdateMemoryArguments = decode_arguments(&call.function.arguments)?;
                if args.id < 1 {
                    return Err(ToolError::Validation("id must be positive".into()));
                }
                let requested_kind = args.kind.as_deref().map(parse_memory_kind).transpose()?;
                let write = self
                    .memory
                    .prepare_write(
                        requested_kind.unwrap_or(MemoryKind::Durable),
                        &args.content,
                        Provenance {
                            origin: MemoryOrigin::Agent,
                            source: MemorySource::Chat,
                            source_history_id: None,
                            source_trace_id: None,
                        },
                    )
                    .await?;
                Ok(Some(MemoryToolInput {
                    write: Some(write),
                    id: args.id,
                    kind: requested_kind,
                    ..Default::default()
                }))
            }
            "get_memory" | "remove_memory" => {
                let args: IdArguments = decode_arguments(&call.function.arguments)?;
                positive_id(args.id)?;
                Ok(Some(MemoryToolInput {
                    id: args.id,
                    ..Default::default()
                }))
            }
            "list_memories" => {
                let args: ListMemoryArguments = decode_arguments(&call.function.arguments)?;
                let kind = args.kind.as_deref().map(parse_memory_kind).transpose()?;
                let status = parse_memory_status(args.status.as_deref().unwrap_or("active"))?;
                if args.limit > 100 {
                    return Err(ToolError::Validation(
                        "limit must be between 1 and 100".into(),
                    ));
                }
                Ok(Some(MemoryToolInput {
                    kind,
                    status,
                    limit: args.limit,
                    ..Default::default()
                }))
            }
            "search_memory" => {
                let args: SearchMemoryArguments = decode_arguments(&call.function.arguments)?;
                let kind = args.kind.as_deref().map(parse_memory_kind).transpose()?;
                let search = self
                    .memory
                    .prepare_search(&args.query, &[], args.max_results)
                    .await?;
                Ok(Some(MemoryToolInput {
                    kind,
                    search: Some(search),
                    ..Default::default()
                }))
            }
            _ => Ok(None),
        }
    }

    async fn prepare_task_input(
        &self,
        call: &ToolCall,
    ) -> Result<Option<TaskToolInput>, ToolError> {
        let mut input = TaskToolInput::default();
        match call.function.name.as_str() {
            "add_task" => {
                let args: TaskDescriptionArguments = decode_arguments(&call.function.arguments)?;
                if !args.description.trim().is_empty() {
                    input.title_index =
                        Some(self.task_context.prepare_index(&args.description).await?);
                }
                if let Some(context) = args.context.filter(|item| !item.trim().is_empty()) {
                    input.context_index = Some(self.task_context.prepare_index(&context).await?);
                }
            }
            "update_task" => {
                let args: TaskUpdateArguments = decode_arguments(&call.function.arguments)?;
                if !args.description.trim().is_empty() {
                    input.title_index =
                        Some(self.task_context.prepare_index(&args.description).await?);
                }
            }
            "add_task_context" => {
                let args: TaskContextArguments = decode_arguments(&call.function.arguments)?;
                if !args.content.trim().is_empty() {
                    input.context_index =
                        Some(self.task_context.prepare_index(&args.content).await?);
                }
            }
            "correct_task_context" => {
                let args: CorrectTaskContextArguments = decode_arguments(&call.function.arguments)?;
                if !args.content.trim().is_empty() {
                    input.context_index =
                        Some(self.task_context.prepare_index(&args.content).await?);
                }
            }
            "complete_task" => {
                let args: TaskCompletionArguments = decode_arguments(&call.function.arguments)?;
                if let Some(content) = args.context.filter(|item| !item.trim().is_empty()) {
                    input.context_index = Some(self.task_context.prepare_index(&content).await?);
                }
            }
            _ => return Ok(None),
        }
        Ok(Some(input))
    }

    fn execute(
        &self,
        tx: &StateTx<'_>,
        context: &ToolContext,
        call: &ToolCall,
        memory_input: Option<&mut MemoryToolInput>,
        task_input: Option<&TaskToolInput>,
    ) -> Result<String, ToolError> {
        match call.function.name.as_str() {
            "add_reminder" => self.add_reminder(tx, context, &call.function.arguments),
            "list_reminders" => self.list_reminders(tx, context, &call.function.arguments),
            "update_reminder" => self.update_reminder(tx, context, &call.function.arguments),
            "remove_reminder" => self.remove_reminder(tx, context, &call.function.arguments),
            "add_task" => self.add_task(tx, context, &call.function.arguments, task_input),
            "list_tasks" => self.list_tasks(tx, &call.function.arguments),
            "get_task" => self.get_task(tx, &call.function.arguments),
            "update_task" => self.update_task(tx, &call.function.arguments, task_input),
            "add_task_context" => {
                self.add_task_context(tx, context, &call.function.arguments, task_input)
            }
            "correct_task_context" => {
                self.correct_task_context(tx, context, &call.function.arguments, task_input)
            }
            "complete_task" => {
                self.complete_task(tx, context, &call.function.arguments, task_input)
            }
            "remove_task" => self.remove_task(tx, &call.function.arguments),
            "store_memory" => {
                let input = require_memory_input(memory_input)?;
                let mut write = input.write.take().expect("prepared memory write");
                attach_provenance(&mut write, context);
                let (memory, stored) = tx.store_memory(&write)?;
                input.mutated = stored;
                Ok(serde_json::to_string(
                    &json!({"memory": memory, "stored": stored}),
                )?)
            }
            "get_memory" => {
                let input = require_memory_input(memory_input)?;
                Ok(serde_json::to_string(
                    &json!({"memory": tx.get_memory(input.id)?}),
                )?)
            }
            "list_memories" => {
                let input = require_memory_input(memory_input)?;
                let memories = tx.list_memories(MemoryFilter {
                    kind: input.kind,
                    status: input.status,
                    limit: input.limit,
                })?;
                Ok(serde_json::to_string(&json!({"memories": memories}))?)
            }
            "update_memory" => {
                let input = require_memory_input(memory_input)?;
                let mut write = input.write.take().expect("prepared memory write");
                if input.kind.is_none() {
                    write.kind = tx.get_memory(input.id)?.kind;
                }
                attach_provenance(&mut write, context);
                let memory = tx.update_memory(input.id, &write)?;
                input.mutated = true;
                Ok(serde_json::to_string(&json!({"updated": memory}))?)
            }
            "remove_memory" => {
                let input = require_memory_input(memory_input)?;
                let memory = tx.remove_memory(input.id, (self.now)())?;
                input.mutated = true;
                Ok(serde_json::to_string(&json!({"removed": memory}))?)
            }
            "search_memory" => {
                let input = require_memory_input(memory_input)?;
                let search = input.search.as_ref().expect("prepared memory search");
                let mut memories = tx.search_memories(
                    &self.index_id,
                    self.dimensions,
                    &search.embedding,
                    &search.fts_query,
                    self.min_score,
                    search.limit,
                )?;
                if let Some(kind) = input.kind {
                    memories.retain(|entry| entry.memory.kind == kind);
                }
                Ok(serde_json::to_string(&json!({"memories": memories}))?)
            }
            name => Err(ToolError::Validation(format!("unknown tool {name:?}"))),
        }
    }

    fn add_task(
        &self,
        tx: &StateTx<'_>,
        context: &ToolContext,
        raw: &str,
        prepared: Option<&TaskToolInput>,
    ) -> Result<String, ToolError> {
        let args: TaskDescriptionArguments = decode_arguments(raw)?;
        if args.description.trim().is_empty() {
            return Err(ToolError::Validation(
                "description must not be empty".into(),
            ));
        }
        let task = tx.add_task(&args.description, (self.now)())?;
        let prepared =
            prepared.ok_or_else(|| ToolError::Validation("task index missing".into()))?;
        tx.index_task_title(
            task.id,
            &task.description,
            prepared
                .title_index
                .as_ref()
                .ok_or_else(|| ToolError::Validation("task title index missing".into()))?,
        )?;
        let initial_context =
            if let Some(content) = args.context.filter(|item| !item.trim().is_empty()) {
                Some(tx.append_task_context(
                    task.id,
                    "context",
                    &content,
                    valid_history_id(context),
                    valid_trace_event_id(context),
                    None,
                    (self.now)(),
                    prepared.context_index.as_ref().ok_or_else(|| {
                        ToolError::Validation("task context index missing".into())
                    })?,
                )?)
            } else {
                None
            };
        Ok(serde_json::to_string(&json!({
            "added": self.task_content(&task),
            "initial_context": initial_context,
            "display_timezone": self.timezone.name(),
        }))?)
    }

    fn list_tasks(&self, tx: &StateTx<'_>, raw: &str) -> Result<String, ToolError> {
        let args: TaskListArguments = decode_arguments(raw)?;
        let status = match args.status.as_deref().unwrap_or("open") {
            "open" => TaskStatus::Open,
            "completed" => TaskStatus::Completed,
            "all" => TaskStatus::All,
            _ => {
                return Err(ToolError::Validation(
                    "task status must be open, completed, or all".into(),
                ));
            }
        };
        let tasks: Vec<_> = tx
            .list_tasks(status)?
            .iter()
            .map(|task| self.task_content(task))
            .collect();
        Ok(serde_json::to_string(&json!({
            "tasks": tasks,
            "status": args.status.unwrap_or_else(|| "open".into()),
            "display_timezone": self.timezone.name(),
        }))?)
    }

    fn get_task(&self, tx: &StateTx<'_>, raw: &str) -> Result<String, ToolError> {
        let args: IdArguments = decode_arguments(raw)?;
        positive_id(args.id)?;
        Ok(serde_json::to_string(&json!({
            "task": self.task_content(&tx.get_task(args.id)?),
            "context": tx.task_context(args.id)?,
        }))?)
    }

    fn add_task_context(
        &self,
        tx: &StateTx<'_>,
        context: &ToolContext,
        raw: &str,
        prepared: Option<&TaskToolInput>,
    ) -> Result<String, ToolError> {
        let args: TaskContextArguments = decode_arguments(raw)?;
        positive_id(args.task_id)?;
        let index = prepared
            .and_then(|item| item.context_index.as_ref())
            .ok_or_else(|| ToolError::Validation("task context index missing".into()))?;
        let note = tx.append_task_context(
            args.task_id,
            &args.kind,
            &args.content,
            valid_history_id(context),
            valid_trace_event_id(context),
            None,
            (self.now)(),
            index,
        )?;
        Ok(serde_json::to_string(&json!({"added": note}))?)
    }

    fn correct_task_context(
        &self,
        tx: &StateTx<'_>,
        context: &ToolContext,
        raw: &str,
        prepared: Option<&TaskToolInput>,
    ) -> Result<String, ToolError> {
        let args: CorrectTaskContextArguments = decode_arguments(raw)?;
        positive_id(args.note_id)?;
        let old = tx.get_task_context_note(args.note_id)?;
        let index = prepared
            .and_then(|item| item.context_index.as_ref())
            .ok_or_else(|| ToolError::Validation("task context index missing".into()))?;
        let note = tx.append_task_context(
            old.task_id,
            &old.kind,
            &args.content,
            valid_history_id(context),
            valid_trace_event_id(context),
            Some(old.id),
            (self.now)(),
            index,
        )?;
        Ok(serde_json::to_string(&json!({"corrected": note}))?)
    }

    fn update_task(
        &self,
        tx: &StateTx<'_>,
        raw: &str,
        prepared: Option<&TaskToolInput>,
    ) -> Result<String, ToolError> {
        let args: TaskUpdateArguments = decode_arguments(raw)?;
        if args.id < 1 || args.description.trim().is_empty() {
            return Err(ToolError::Validation(
                "id must be positive and description must not be empty".into(),
            ));
        }
        let task = tx.update_task(args.id, &args.description)?;
        tx.index_task_title(
            task.id,
            &task.description,
            prepared
                .and_then(|item| item.title_index.as_ref())
                .ok_or_else(|| ToolError::Validation("task title index missing".into()))?,
        )?;
        Ok(serde_json::to_string(&json!({
            "updated": self.task_content(&task),
            "display_timezone": self.timezone.name(),
        }))?)
    }

    fn complete_task(
        &self,
        tx: &StateTx<'_>,
        context: &ToolContext,
        raw: &str,
        prepared: Option<&TaskToolInput>,
    ) -> Result<String, ToolError> {
        let args: TaskCompletionArguments = decode_arguments(raw)?;
        positive_id(args.id)?;
        let final_context =
            if let Some(content) = args.context.filter(|item| !item.trim().is_empty()) {
                Some(
                    tx.append_task_context(
                        args.id,
                        "progress",
                        &content,
                        valid_history_id(context),
                        valid_trace_event_id(context),
                        None,
                        (self.now)(),
                        prepared
                            .and_then(|item| item.context_index.as_ref())
                            .ok_or_else(|| {
                                ToolError::Validation("task context index missing".into())
                            })?,
                    )?,
                )
            } else {
                None
            };
        let task = tx.complete_task(args.id, (self.now)())?;
        Ok(serde_json::to_string(&json!({
            "completed": self.task_content(&task),
            "final_context": final_context,
            "display_timezone": self.timezone.name(),
        }))?)
    }

    fn remove_task(&self, tx: &StateTx<'_>, raw: &str) -> Result<String, ToolError> {
        let args: IdArguments = decode_arguments(raw)?;
        positive_id(args.id)?;
        let task = tx.delete_task(args.id)?;
        Ok(serde_json::to_string(&json!({
            "removed": self.task_content(&task),
            "display_timezone": self.timezone.name(),
        }))?)
    }

    fn task_content(&self, task: &Task) -> Value {
        json!({
            "id": task.id,
            "description": task.description,
            "status": match task.status() { TaskStatus::Open => "open", _ => "completed" },
            "started_at": self.timezone.format(task.started_at),
            "completed_at": task.completed_at.map(|time| self.timezone.format(time)),
        })
    }

    fn add_reminder(
        &self,
        tx: &StateTx<'_>,
        context: &ToolContext,
        raw: &str,
    ) -> Result<String, ToolError> {
        let args: AddReminderArguments = decode_arguments(raw)?;
        let message = args.message.trim();
        if message.is_empty() {
            return Err(ToolError::Validation("message must not be empty".into()));
        }
        let (schedule, fire_at) = self.resolve_schedule(&args.schedule)?;
        let id = tx.add_reminder(
            &context.channel_id,
            &context.sender_id,
            &context.conversation_id,
            message,
            &schedule,
            fire_at,
        )?;
        Ok(serde_json::to_string(&json!({
            "added": self.reminder_content(id, message, &schedule, fire_at, true)
        }))?)
    }

    fn list_reminders(
        &self,
        tx: &StateTx<'_>,
        context: &ToolContext,
        raw: &str,
    ) -> Result<String, ToolError> {
        let _: EmptyArguments = decode_arguments(raw)?;
        let reminders: Vec<_> = tx
            .list_reminders(&context.channel_id, &context.sender_id)?
            .iter()
            .map(|reminder| {
                self.reminder_content(
                    reminder.id,
                    &reminder.message,
                    &reminder.schedule,
                    reminder.fire_at,
                    reminder.enabled,
                )
            })
            .collect();
        Ok(serde_json::to_string(&json!({"reminders": reminders}))?)
    }

    fn update_reminder(
        &self,
        tx: &StateTx<'_>,
        context: &ToolContext,
        raw: &str,
    ) -> Result<String, ToolError> {
        let args: UpdateReminderArguments = decode_arguments(raw)?;
        positive_id(args.id)?;
        if args.message.is_none() && args.enabled.is_none() && args.schedule.is_none() {
            return Err(ToolError::Validation(
                "update must change message, enabled, or schedule".into(),
            ));
        }
        let mut reminder =
            tx.get_reminder_for_user(args.id, &context.channel_id, &context.sender_id)?;
        let was_enabled = reminder.enabled;
        if let Some(message) = args.message {
            if message.trim().is_empty() {
                return Err(ToolError::Validation("message must not be empty".into()));
            }
            reminder.message = message.trim().into();
        }
        if let Some(enabled) = args.enabled {
            reminder.enabled = enabled;
        }
        if let Some(schedule) = args.schedule.as_ref() {
            (reminder.schedule, reminder.fire_at) = self.resolve_schedule(schedule)?;
        }
        if args.enabled == Some(true) && !was_enabled && args.schedule.is_none() {
            if reminder.schedule.kind == ScheduleKind::Cron
                && reminder.schedule.timezone.trim().is_empty()
            {
                reminder.schedule.timezone = self.timezone.name();
            }
            reminder.fire_at =
                next_reminder_run(&reminder.schedule, (self.now)()).map_err(|error| {
                    ToolError::Validation(format!(
                        "re-enable reminder: {error}; provide a new schedule"
                    ))
                })?;
        }
        tx.update_reminder_for_user(&reminder)?;
        Ok(serde_json::to_string(&json!({
            "updated": self.reminder_content(
                reminder.id,
                &reminder.message,
                &reminder.schedule,
                reminder.fire_at,
                reminder.enabled,
            )
        }))?)
    }

    fn remove_reminder(
        &self,
        tx: &StateTx<'_>,
        context: &ToolContext,
        raw: &str,
    ) -> Result<String, ToolError> {
        let args: IdArguments = decode_arguments(raw)?;
        positive_id(args.id)?;
        tx.delete_reminder_for_user(args.id, &context.channel_id, &context.sender_id)?;
        Ok(serde_json::to_string(&json!({"removed_id": args.id}))?)
    }

    fn resolve_schedule(
        &self,
        args: &ScheduleArguments,
    ) -> Result<(ReminderSchedule, DateTime<Utc>), ToolError> {
        let now = (self.now)();
        let schedule = match args.kind.as_str() {
            "at" => {
                if args.every_ms != 0
                    || !args.anchor_at.is_empty()
                    || !args.expr.is_empty()
                    || !args.timezone.is_empty()
                {
                    return Err(ToolError::Validation(
                        "schedule: at schedules only accept at".into(),
                    ));
                }
                ReminderSchedule::at(parse_explicit_rfc3339(&args.at, "at")?)
            }
            "every" => {
                if !args.at.is_empty() || !args.expr.is_empty() || !args.timezone.is_empty() {
                    return Err(ToolError::Validation(
                        "schedule: every schedules only accept every_ms and optional anchor_at"
                            .into(),
                    ));
                }
                ReminderSchedule {
                    kind: ScheduleKind::Every,
                    at: None,
                    every_ms: args.every_ms,
                    anchor_at: if args.anchor_at.trim().is_empty() {
                        Some(now)
                    } else {
                        Some(parse_explicit_rfc3339(&args.anchor_at, "anchor_at")?)
                    },
                    cron_expr: String::new(),
                    timezone: String::new(),
                }
            }
            "cron" => {
                if !args.at.is_empty() || args.every_ms != 0 || !args.anchor_at.is_empty() {
                    return Err(ToolError::Validation(
                        "schedule: cron schedules only accept expr and timezone".into(),
                    ));
                }
                ReminderSchedule {
                    kind: ScheduleKind::Cron,
                    at: None,
                    every_ms: 0,
                    anchor_at: None,
                    cron_expr: args.expr.trim().into(),
                    timezone: if args.timezone.trim().is_empty() {
                        self.timezone.name()
                    } else {
                        args.timezone.trim().into()
                    },
                }
            }
            _ => {
                return Err(ToolError::Validation(
                    "schedule: kind must be at, every, or cron".into(),
                ));
            }
        };
        let fire_at = next_reminder_run(&schedule, now)
            .map_err(|error| ToolError::Validation(format!("schedule: {error}")))?;
        Ok((schedule, fire_at))
    }

    fn reminder_content(
        &self,
        id: i64,
        message: &str,
        schedule: &ReminderSchedule,
        fire_at: DateTime<Utc>,
        enabled: bool,
    ) -> Value {
        json!({
            "id": id,
            "message": message,
            "schedule": schedule_description(schedule),
            "next_fire_at": self.timezone.format(fire_at),
            "display_timezone": self.timezone.name(),
            "enabled": enabled,
        })
    }

    fn record_result(&self, context: &ToolContext, result: &ToolResult) -> Result<(), ToolError> {
        self.store.with_tx(|tx| {
            save_result(tx, context, result)?;
            tx.finish_tool_execution(
                context.trace_event_id,
                &serde_json::to_string(result)?,
                result.is_error,
                false,
            )?;
            Ok::<(), ToolError>(())
        })?;
        Ok(())
    }
}

pub fn definitions(timezone: &str) -> Vec<ToolDefinition> {
    let schedule = json!({
        "type": "object",
        "additionalProperties": false,
        "properties": {
            "kind": {"type": "string", "enum": ["at", "every", "cron"]},
            "at": {"type": "string", "description": "Future RFC3339 timestamp with explicit UTC offset for kind=at."},
            "every_ms": {"type": "integer", "minimum": 1, "description": "Fixed interval milliseconds for kind=every."},
            "anchor_at": {"type": "string", "description": "Optional RFC3339 interval anchor for kind=every."},
            "expr": {"type": "string", "description": "Five- or six-field cron expression in timezone wall-clock time for kind=cron."},
            "timezone": {"type": "string", "description": format!("IANA timezone for kind=cron; omit to use server timezone {timezone}.")}
        },
        "required": ["kind"]
    });
    vec![
        definition(
            "add_reminder",
            &format!(
                "Add one reminder for the current user. The server timezone is {timezone}; omit cron timezone to use it."
            ),
            json!({"type":"object","additionalProperties":false,"properties":{"message":{"type":"string"},"schedule":schedule},"required":["message","schedule"]}),
        ),
        definition(
            "list_reminders",
            "List reminders for the current user.",
            json!({"type":"object","additionalProperties":false,"properties":{}}),
        ),
        definition(
            "update_reminder",
            "Update one reminder for the current user. Supply at least one of message, enabled, or schedule.",
            json!({"type":"object","additionalProperties":false,"minProperties":2,"properties":{"id":{"type":"integer","minimum":1},"message":{"type":"string"},"enabled":{"type":"boolean"},"schedule":schedule},"required":["id"]}),
        ),
        definition(
            "remove_reminder",
            "Remove one reminder for the current user.",
            id_schema(),
        ),
        definition(
            "add_task",
            "Add one owner-global task. Put material context from the current owner request in context. Tasks have no schedule.",
            json!({"type":"object","additionalProperties":false,"properties":{"description":{"type":"string"},"context":{"type":"string"}},"required":["description"]}),
        ),
        definition(
            "list_tasks",
            "List owner-global tasks. The status defaults to open.",
            json!({"type":"object","additionalProperties":false,"properties":{"status":{"type":"string","enum":["open","completed","all"]}}}),
        ),
        definition(
            "get_task",
            "Get one task and its dated context notes.",
            id_schema(),
        ),
        definition(
            "update_task",
            "Replace the description of one open owner-global task.",
            json!({"type":"object","additionalProperties":false,"properties":{"id":{"type":"integer","minimum":1},"description":{"type":"string"}},"required":["id","description"]}),
        ),
        definition(
            "add_task_context",
            "Append one owner-stated progress, decision, blocker, next step, or other context to an open task. Never record your own suggestion as owner progress.",
            json!({"type":"object","additionalProperties":false,"properties":{"task_id":{"type":"integer","minimum":1},"kind":{"type":"string","enum":["context","progress","decision","blocker","next_step"]},"content":{"type":"string"}},"required":["task_id","kind","content"]}),
        ),
        definition(
            "correct_task_context",
            "Replace an incorrect active task context note while retaining its history.",
            json!({"type":"object","additionalProperties":false,"properties":{"note_id":{"type":"integer","minimum":1},"content":{"type":"string"}},"required":["note_id","content"]}),
        ),
        definition(
            "complete_task",
            "Complete one open owner-global task. Include an optional final outcome from the current owner message as context. Completion is final.",
            json!({"type":"object","additionalProperties":false,"properties":{"id":{"type":"integer","minimum":1},"context":{"type":"string"}},"required":["id"]}),
        ),
        definition("remove_task", "Remove one owner-global task.", id_schema()),
        definition(
            "store_memory",
            "Store one profile, durable, or daily memory.",
            json!({"type":"object","additionalProperties":false,"properties":{"content":{"type":"string","description":"One concise standalone fact."},"kind":{"type":"string","enum":["profile","durable","daily"]}},"required":["content","kind"]}),
        ),
        definition(
            "get_memory",
            "Get one memory by its stable Memory ID.",
            id_schema(),
        ),
        definition(
            "list_memories",
            "List memories. Status defaults to active and limit defaults to 20.",
            json!({"type":"object","additionalProperties":false,"properties":{"kind":{"type":"string","enum":["profile","durable","daily"]},"status":{"type":"string","enum":["active","deleted","all"]},"limit":{"type":"integer","minimum":1,"maximum":100}}}),
        ),
        definition(
            "update_memory",
            "Create a new revision of one active memory, retaining its stable Memory ID.",
            json!({"type":"object","additionalProperties":false,"properties":{"id":{"type":"integer","minimum":1},"content":{"type":"string"},"kind":{"type":"string","enum":["profile","durable","daily"]}},"required":["id","content"]}),
        ),
        definition(
            "remove_memory",
            "Stop one memory from being recalled. Audit revisions are retained.",
            id_schema(),
        ),
        definition(
            "search_memory",
            "Hybrid keyword and semantic search over active memories.",
            json!({"type":"object","additionalProperties":false,"properties":{"query":{"type":"string"},"kind":{"type":"string","enum":["profile","durable","daily"]},"max_results":{"type":"integer","minimum":1,"maximum":20}},"required":["query"]}),
        ),
    ]
}

fn definition(name: &str, description: &str, parameters: Value) -> ToolDefinition {
    ToolDefinition {
        kind: "function".into(),
        function: FunctionDefinition {
            name: name.into(),
            description: description.into(),
            parameters,
        },
    }
}

fn id_schema() -> Value {
    json!({"type":"object","additionalProperties":false,"properties":{"id":{"type":"integer","minimum":1}},"required":["id"]})
}

fn save_result(
    tx: &StateTx<'_>,
    context: &ToolContext,
    result: &ToolResult,
) -> Result<(), ToolError> {
    let audience = if context.audience.is_empty() {
        AUDIENCE_CONVERSATION
    } else {
        &context.audience
    };
    if context.trace_event_id > 0 {
        tx.save_conversation_message_for_event(
            context.trace_event_id,
            &context.channel_id,
            &context.sender_id,
            &context.conversation_id,
            "tool",
            CONTENT_TOOL_RESULT,
            audience,
            &serde_json::to_string(result)?,
        )?;
    } else {
        tx.save_conversation_message(
            &context.channel_id,
            &context.sender_id,
            &context.conversation_id,
            "tool",
            CONTENT_TOOL_RESULT,
            audience,
            &serde_json::to_string(result)?,
        )?;
    }
    Ok(())
}

fn attach_provenance(write: &mut MemoryWrite, context: &ToolContext) {
    if context.source_history_id > 0 {
        write.source_history_id = Some(context.source_history_id);
    }
    if context.response_trace_id > 0 {
        write.source_trace_id = Some(context.response_trace_id);
    }
}

fn require_memory_input(
    input: Option<&mut MemoryToolInput>,
) -> Result<&mut MemoryToolInput, ToolError> {
    input.ok_or_else(|| ToolError::Validation("memory input was not prepared".into()))
}

fn parse_memory_kind(value: &str) -> Result<MemoryKind, ToolError> {
    match value {
        "profile" => Ok(MemoryKind::Profile),
        "durable" => Ok(MemoryKind::Durable),
        "daily" => Ok(MemoryKind::Daily),
        _ => Err(ToolError::Validation("invalid memory kind".into())),
    }
}

fn parse_memory_status(value: &str) -> Result<MemoryStatus, ToolError> {
    match value {
        "active" => Ok(MemoryStatus::Active),
        "deleted" => Ok(MemoryStatus::Deleted),
        "all" => Ok(MemoryStatus::All),
        _ => Err(ToolError::Validation("invalid memory status".into())),
    }
}

fn positive_id(id: i64) -> Result<(), ToolError> {
    if id < 1 {
        Err(ToolError::Validation("id must be positive".into()))
    } else {
        Ok(())
    }
}

fn parse_explicit_rfc3339(value: &str, field: &str) -> Result<DateTime<Utc>, ToolError> {
    let value = value.trim();
    let Some(time_index) = value.find('T') else {
        return Err(ToolError::Validation(format!(
            "schedule: {field} must be RFC3339 with an explicit UTC offset"
        )));
    };
    if !value.ends_with('Z') && !value[time_index + 1..].contains(['+', '-']) {
        return Err(ToolError::Validation(format!(
            "schedule: {field} must be RFC3339 with an explicit UTC offset"
        )));
    }
    DateTime::parse_from_rfc3339(value)
        .map(|value| value.with_timezone(&Utc))
        .map_err(|error| {
            ToolError::Validation(format!("schedule: {field} must be valid RFC3339: {error}"))
        })
}

fn schedule_description(schedule: &ReminderSchedule) -> Value {
    match schedule.kind {
        ScheduleKind::At => json!({
            "kind": "at",
            "at": schedule.at.map(|at| at.to_rfc3339_opts(SecondsFormat::Secs, true)),
        }),
        ScheduleKind::Every => json!({
            "kind": "every",
            "every_ms": schedule.every_ms,
            "anchor_at": schedule.anchor_at.map(|at| at.to_rfc3339_opts(SecondsFormat::Secs, true)),
        }),
        ScheduleKind::Cron => json!({
            "kind": "cron",
            "expr": schedule.cron_expr,
            "timezone": schedule.timezone,
        }),
    }
}

fn decode_arguments<T: DeserializeOwned>(raw: &str) -> Result<T, ToolError> {
    let mut deserializer = serde_json::Deserializer::from_str(raw);
    let value = T::deserialize(&mut deserializer)
        .map_err(|error| ToolError::Validation(format!("invalid arguments: {error}")))?;
    deserializer
        .end()
        .map_err(|_| ToolError::Validation("invalid arguments: trailing JSON".into()))?;
    Ok(value)
}

fn error_json(message: &str) -> String {
    serde_json::to_string(&json!({"error": message})).expect("error JSON is infallible")
}

fn tool_mutation(name: &str) -> bool {
    matches!(
        name,
        "add_reminder"
            | "update_reminder"
            | "remove_reminder"
            | "add_task"
            | "update_task"
            | "complete_task"
            | "remove_task"
            | "add_task_context"
            | "correct_task_context"
            | "store_memory"
            | "update_memory"
            | "remove_memory"
    )
}

fn is_memory_mutation_tool(name: &str) -> bool {
    matches!(name, "store_memory" | "update_memory" | "remove_memory")
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct EmptyArguments {}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct IdArguments {
    id: i64,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct TaskDescriptionArguments {
    description: String,
    context: Option<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct TaskListArguments {
    status: Option<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct TaskUpdateArguments {
    id: i64,
    description: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct TaskCompletionArguments {
    id: i64,
    context: Option<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct TaskContextArguments {
    task_id: i64,
    kind: String,
    content: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct CorrectTaskContextArguments {
    note_id: i64,
    content: String,
}

fn valid_history_id(context: &ToolContext) -> Option<i64> {
    (context.source_history_id > 0).then_some(context.source_history_id)
}

fn valid_trace_event_id(context: &ToolContext) -> Option<i64> {
    (context.trace_event_id > 0).then_some(context.trace_event_id)
}

#[derive(Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
struct ScheduleArguments {
    kind: String,
    at: String,
    every_ms: i64,
    anchor_at: String,
    expr: String,
    timezone: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct AddReminderArguments {
    message: String,
    schedule: ScheduleArguments,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct UpdateReminderArguments {
    id: i64,
    message: Option<String>,
    enabled: Option<bool>,
    schedule: Option<ScheduleArguments>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct StoreMemoryArguments {
    content: String,
    kind: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct UpdateMemoryArguments {
    id: i64,
    content: String,
    kind: Option<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ListMemoryArguments {
    kind: Option<String>,
    status: Option<String>,
    #[serde(default)]
    limit: usize,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct SearchMemoryArguments {
    query: String,
    kind: Option<String>,
    #[serde(default)]
    max_results: usize,
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::providers::ProviderError;
    use chrono::TimeZone;

    struct FixedEmbedder;

    #[async_trait::async_trait]
    impl Embedder for FixedEmbedder {
        async fn embed(&self, inputs: &[String]) -> Result<Vec<Vec<f32>>, ProviderError> {
            Ok(inputs.iter().map(|_| vec![1.0, 0.0]).collect())
        }

        async fn tokenize(&self, _content: &str) -> Result<Vec<i32>, ProviderError> {
            unreachable!()
        }

        async fn detokenize(&self, _tokens: &[i32]) -> Result<String, ProviderError> {
            unreachable!()
        }
    }

    fn call(id: &str, name: &str, arguments: &str) -> ToolCall {
        ToolCall {
            id: id.into(),
            kind: "function".into(),
            function: crate::providers::FunctionCall {
                name: name.into(),
                arguments: arguments.into(),
            },
        }
    }

    fn setup() -> (Arc<Store>, Executor, ToolContext, DateTime<Utc>) {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.keep().join("state.sqlite");
        let store = Arc::new(Store::new(path).unwrap());
        let now = Utc.with_ymd_and_hms(2026, 7, 15, 0, 0, 0).unwrap();
        let executor = Executor::with_clock(
            Arc::clone(&store),
            "Asia/Kuala_Lumpur",
            Arc::new(FixedEmbedder),
            "test-model",
            2,
            0.35,
            move || now,
        )
        .unwrap();
        let context = ToolContext {
            channel_id: "telegram".into(),
            sender_id: "owner".into(),
            conversation_id: "owner-chat".into(),
            ..Default::default()
        };
        (store, executor, context, now)
    }

    #[tokio::test]
    async fn tasks_are_owner_global_and_results_are_structured_history() {
        let (store, executor, context, _) = setup();
        let added = executor
            .execute_and_record(
                &context,
                &call("call-1", "add_task", r#"{"description":"Write port"}"#),
            )
            .await
            .unwrap();
        assert!(!added.is_error);
        let value: Value = serde_json::from_str(&added.content).unwrap();
        assert_eq!(value["added"]["status"], "open");
        let listed = executor
            .execute_and_record(&context, &call("call-2", "list_tasks", r#"{}"#))
            .await
            .unwrap();
        assert!(listed.content.contains("Write port"));
        let history = store
            .get_conversation_history("telegram", "owner-chat")
            .unwrap();
        assert_eq!(history.len(), 2);
        assert!(
            history
                .iter()
                .all(|turn| turn.content_type == CONTENT_TOOL_RESULT)
        );
    }

    #[tokio::test]
    async fn task_context_tools_preserve_notes_and_reject_completed_updates() {
        let (store, executor, context, _) = setup();
        let created = executor
            .execute_and_record(
                &context,
                &call(
                    "create",
                    "add_task",
                    r#"{"description":"Build strategy","context":"Start with daily bars"}"#,
                ),
            )
            .await
            .unwrap();
        assert!(!created.is_error);
        let created_value: Value = serde_json::from_str(&created.content).unwrap();
        let task_id = created_value["added"]["id"].as_i64().unwrap();
        let initial_id = created_value["initial_context"]["id"].as_i64().unwrap();
        let appended = executor
            .execute_and_record(
                &context,
                &call(
                    "note",
                    "add_task_context",
                    &format!(
                        r#"{{"task_id":{task_id},"kind":"blocker","content":"Need clean data"}}"#
                    ),
                ),
            )
            .await
            .unwrap();
        assert!(!appended.is_error);
        let corrected = executor
            .execute_and_record(
                &context,
                &call(
                    "correct",
                    "correct_task_context",
                    &format!(
                        r#"{{"note_id":{initial_id},"content":"Start with daily OHLC and volume"}}"#
                    ),
                ),
            )
            .await
            .unwrap();
        assert!(!corrected.is_error);
        let fetched = executor
            .execute_and_record(
                &context,
                &call("get", "get_task", &format!(r#"{{"id":{task_id}}}"#)),
            )
            .await
            .unwrap();
        let value: Value = serde_json::from_str(&fetched.content).unwrap();
        assert_eq!(value["context"].as_array().unwrap().len(), 3);
        assert_eq!(value["context"][0]["superseded"], true);
        let completed = executor
            .execute_and_record(
                &context,
                &call(
                    "complete",
                    "complete_task",
                    &format!(r#"{{"id":{task_id},"context":"Finished a working notebook"}}"#),
                ),
            )
            .await
            .unwrap();
        assert!(!completed.is_error);
        assert!(completed.content.contains("Finished a working notebook"));
        let rejected = executor
            .execute_and_record(
                &context,
                &call(
                    "late",
                    "add_task_context",
                    &format!(
                        r#"{{"task_id":{task_id},"kind":"progress","content":"Later update"}}"#
                    ),
                ),
            )
            .await
            .unwrap();
        assert!(rejected.is_error);
        assert_eq!(store.get_task_with_context(task_id).unwrap().1.len(), 4);
    }

    #[tokio::test]
    async fn reminders_use_trusted_identity_and_scope_removal() {
        let (store, executor, context, now) = setup();
        let added = executor
            .execute_and_record(
                &context,
                &call(
                    "call-1",
                    "add_reminder",
                    r#"{"message":"briefing","schedule":{"kind":"at","at":"2026-07-15T01:00:00Z"}}"#,
                ),
            )
            .await
            .unwrap();
        assert!(!added.is_error);
        let reminders = store
            .with_tx(|tx| tx.list_reminders("telegram", "owner"))
            .unwrap();
        assert_eq!(reminders.len(), 1);
        assert_eq!(reminders[0].sender_id, "owner");
        assert_eq!(reminders[0].conversation_id, "owner-chat");
        assert_eq!(reminders[0].fire_at, now + chrono::Duration::hours(1));
        let other = ToolContext {
            sender_id: "intruder".into(),
            ..context.clone()
        };
        let removed = executor
            .execute_and_record(&other, &call("call-2", "remove_reminder", r#"{"id":1}"#))
            .await
            .unwrap();
        assert!(removed.is_error);
        assert_eq!(
            store
                .with_tx(|tx| tx.list_reminders("telegram", "owner"))
                .unwrap()
                .len(),
            1
        );
    }

    #[tokio::test]
    async fn memory_mutation_is_idempotent_and_provenance_is_attached() {
        let (store, executor, mut context, _) = setup();
        context.source_history_id = store
            .save_conversation_message(
                "telegram",
                "owner",
                "owner-chat",
                "user",
                "text",
                "remember",
            )
            .unwrap();
        let trace = store
            .start_response_trace(&crate::state::TraceInput {
                trigger_type: "chat".into(),
                channel_id: "telegram".into(),
                sender_id: "owner".into(),
                conversation_id: "owner-chat".into(),
                external_message_id: String::new(),
                reminder_id: None,
                input_json: "{}".into(),
                inbound_event_id: None,
            })
            .unwrap();
        context.response_trace_id = trace;
        let first = executor
            .execute_and_record(
                &context,
                &call(
                    "call-1",
                    "store_memory",
                    r#"{"content":"likes espresso","kind":"durable"}"#,
                ),
            )
            .await
            .unwrap();
        let second = executor
            .execute_and_record(
                &context,
                &call(
                    "call-2",
                    "store_memory",
                    r#"{"content":"likes espresso","kind":"durable"}"#,
                ),
            )
            .await
            .unwrap();
        assert_eq!(
            serde_json::from_str::<Value>(&first.content).unwrap()["stored"],
            true
        );
        assert_eq!(
            serde_json::from_str::<Value>(&second.content).unwrap()["stored"],
            false
        );
        let memory = store.get_memory(1).unwrap();
        assert_eq!(memory.source_history_id, Some(context.source_history_id));
        assert_eq!(memory.source_trace_id, Some(trace));
    }

    #[tokio::test]
    async fn validation_errors_commit_no_state_but_are_recorded() {
        let (store, executor, context, _) = setup();
        let result = executor
            .execute_and_record(
                &context,
                &call(
                    "call-1",
                    "add_task",
                    r#"{"description":"x","due":"tomorrow"}"#,
                ),
            )
            .await
            .unwrap();
        assert!(result.is_error);
        assert!(
            store
                .with_tx(|tx| tx.list_tasks(TaskStatus::All))
                .unwrap()
                .is_empty()
        );
        assert_eq!(
            store
                .get_conversation_history("telegram", "owner-chat")
                .unwrap()
                .len(),
            1
        );
    }

    #[tokio::test]
    async fn mutation_transcript_and_trace_commit_together() {
        let (store, executor, mut context, _) = setup();
        let trace_id = store
            .start_response_trace(&crate::state::TraceInput {
                trigger_type: "chat".into(),
                channel_id: "telegram".into(),
                sender_id: "owner".into(),
                conversation_id: "owner-chat".into(),
                external_message_id: "in-1".into(),
                reminder_id: None,
                input_json: "{}".into(),
                inbound_event_id: None,
            })
            .unwrap();
        let llm_id = store.start_llm_call(trace_id, 1, "chat", "{}").unwrap();
        store
            .finish_llm_call(llm_id, "{}", 200, "tool_calls", None)
            .unwrap();
        context.trace_event_id = store
            .start_tool_execution(
                trace_id,
                llm_id,
                "call-1",
                "add_task",
                r#"{"description":"atomic"}"#,
            )
            .unwrap();
        executor
            .execute_and_record(
                &context,
                &call("call-1", "add_task", r#"{"description":"atomic"}"#),
            )
            .await
            .unwrap();

        let report = store.get_trace_report(trace_id).unwrap();
        let tool = report
            .events
            .iter()
            .find(|event| event["kind"] == "tool")
            .unwrap();
        assert_eq!(tool["status"], "succeeded");
        assert_eq!(tool["detail"]["mutation_committed"], 1);
        assert_eq!(
            store
                .with_tx(|tx| tx.list_tasks(TaskStatus::All))
                .unwrap()
                .len(),
            1
        );
        assert_eq!(
            store
                .get_conversation_history("telegram", "owner-chat")
                .unwrap()
                .len(),
            1
        );
    }

    #[test]
    fn catalog_has_only_single_purpose_tools_without_identity_arguments() {
        let definitions = definitions("UTC");
        assert_eq!(definitions.len(), 18);
        for definition in definitions {
            let properties = &definition.function.parameters["properties"];
            assert!(properties.get("channel_id").is_none());
            assert!(properties.get("sender_id").is_none());
        }
    }
}
