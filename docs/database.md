---
title: SQLite database
summary: Canonical tables, columns, relationships, and rebuild policy
---

The runtime supports one exact schema. Startup creates it only for a new
database and then validates the complete table and column set. It does not run
legacy `ALTER TABLE` migrations. A non-canonical database fails startup. This
cutover requires an empty new database; prior Go and Node state is deliberately
not migrated.

## Memory ledger

`memories` is the stable owner-facing record. It stores the Memory ID, kind,
active/deleted status, current revision pointer, active-content hash, and
observed/created/updated/deleted timestamps. The partial unique content-hash
index makes exact active duplicates idempotent.

`memory_revisions` is immutable audit history. Each row stores a revision
number, content and hash, trusted origin class, chat/operator source class,
optional source conversation and trace references, and creation time. Updating
a memory advances the pointer in `memories`; removing one retains all
revisions.

`memory_embeddings` contains the current active revision's packed vector,
stable embedding index id, and dimensions. `memory_fts` is an FTS5 virtual
table containing the same current active revision. Deleted memories have rows
in neither derived table. Both indexes are replaced with the ledger mutation
in the same transaction.

## `reminders`

Scheduled outbound reminder state.

| Column | Purpose |
| --- | --- |
| `id` | Autoincremented reminder identifier exposed by the reminder tool. |
| `channel_id` | Delivery adapter and routing key, currently `telegram` or test-only `cli`. |
| `sender_id` | Recipient/conversation routing key supplied by trusted ingress. |
| `message` | Reminder text. |
| `fire_at` | Next due instant stored in UTC-compatible SQLite time form. |
| `schedule_kind` | `at`, `every`, or `cron`. |
| `every_ms` | Fixed recurrence interval; zero for other schedule kinds. |
| `anchor_at` | Optional recurrence anchor for `every`. |
| `cron_expr` | Five- or six-field cron expression. |
| `timezone` | IANA timezone used to interpret cron wall-clock time. |
| `enabled` | Whether the reminder is eligible for delivery. |

The due index covers `enabled, fire_at`.

## `tasks`

Owner-global unfinished work and completed history. Tasks are independent from
reminders and contain no delivery, schedule, due-date, recurrence, timezone, or
linkage fields.

| Column | Purpose |
| --- | --- |
| `id` | Autoincremented task identifier exposed by the task tool. |
| `description` | Trimmed description of the work. |
| `started_at` | Actual server-recorded creation time. |
| `completed_at` | Actual server-recorded completion time, or null while open. |

Status is derived: a null `completed_at` means `open`; otherwise the task is
`completed`. A partial unique index on `lower(trim(description))` prevents two
open tasks with the same case-insensitive description. A completed description
may be used by a new task.

## `conversation_history`

Append-only structured transcript and source of truth for conversation recall.

| Column | Purpose |
| --- | --- |
| `id` | Global autoincremented transcript sequence. |
| `channel_id` | Source routing channel. |
| `sender_id` | Source conversation routing key. |
| `role` | Provider role such as `user`, `assistant`, or `tool`. |
| `content_type` | `text`, `inbound_message`, `scheduled_reminder`, `tool_call`, or `tool_result`. |
| `audience` | `conversation` for owner-visible replay or `internal` for recall-planning and evidence-selection audit calls. |
| `content` | Plain text or the structured JSON payload for the content type. |
| `created_at` | Timestamp used when rendering historical conversation documents. |

`channel_id, sender_id, id` is indexed for ordered route replay. Tool calls
and tool results are related through exact tool-call ids stored inside their
JSON payloads. A `scheduled_reminder` row stores the reminder id, original
message, and scheduled occurrence; its following assistant text row contains
the exact delivered notification.

## `conversation_chunks`

Rebuildable semantic index derived only from complete exchanges in
`conversation_history`.

| Column | Purpose |
| --- | --- |
| `id` | Autoincremented index-row identifier. |
| `start_history_id` | First source transcript row in the exchange. |
| `end_history_id` | Last source transcript row in the exchange. |
| `part_index` | Zero-based part when one exchange exceeds embedding input limits. |
| `content_hash` | Detects a changed rendered source document. |
| `embedding_model` | Stable embedding index id. |
| `dimensions` | Stored vector dimension count. |
| `index_version` | Rendering/chunking format version. |
| `embedding` | Required packed vector. Incomplete index rows cannot exist. |

The source relationship is
`conversation_chunks.start_history_id..end_history_id` to the inclusive range
of `conversation_history.id`. It is intentionally not a foreign key because
the index is disposable and rebuilt explicitly. Uniqueness is enforced
for model/version/source range/part.

There are no `agent_state`, `conversation_compactions`, or personality tables.
Assistant behavior is fixed in the runtime and is neither configured nor stored.

## Response provenance

Response provenance is always enabled and retained until the operator performs
an explicit future maintenance action. It records diagnostic inputs and
outcomes, not credentials or transport authorization headers.

- `response_traces` is the root record for one inbound chat or scheduled
  reminder attempt. It stores the trusted route, original structured input,
  external inbound id, optional reminder id, history link, lifecycle status,
  and failure stage.
- `trace_events` provides a stable sequence for the RAG, LLM, tool, output, and
  delivery stages. Independent conversations can interleave globally without
  losing their per-trace order.
- `rag_retrievals` records the exact embedding query, index contract, selection
  counts, outcome, and rendered archive inserted into the prompt.
  `rag_matches` records conversation candidates and `memory_rag_matches`
  records memory/revision IDs plus keyword, vector, and combined scores.
- `llm_calls` records each tool-loop or reminder-model round, including the
  sanitized request body actually sent and the response body returned by the
  local OpenAI-compatible server. A contract-invalid optional `recall_plan`
  call is marked failed while retaining that exact request and response; its
  parent response trace may still complete after raw-query retrieval.
- `tool_executions` relates exact model call ids, arguments, results, errors,
  and mutation outcomes to their LLM round.
- `response_outputs` keeps raw model content, ordered application
  transformations, and exact final channel text.
- `delivery_attempts` is persisted before external I/O. `attempting` with no
  completion is deliberately ambiguous: the process may have stopped after
  provider acceptance but before SQLite finalization. An accepted delivery
  links the final assistant history row and provider message id.

Full trace content is available only through the local `openclaw trace`
commands. The HTTP API returns the numeric trace id in
`X-OpenClaw-Trace-ID`; Telegram reply text is not modified.
