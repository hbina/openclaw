---
title: SQLite database
summary: Canonical tables, columns, relationships, and rebuild policy
---

The runtime supports one exact schema. Startup creates it only for a new
database and then validates the complete table and column set. It does not run
legacy `ALTER TABLE` migrations. A non-canonical database fails startup and
must be rebuilt deliberately from a verified backup.

## `memory_entries`

Global durable facts for the one owner.

| Column | Purpose |
| --- | --- |
| `id` | Autoincremented memory identifier. |
| `content` | The durable fact stored by the agent. Exact duplicates are not inserted. |
| `embedding_model` | Stable embedding index id used for this vector. |
| `dimensions` | Vector dimension count used to validate and decode `embedding`. |
| `embedding` | Packed float vector used by semantic memory search. |

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
| `embedding` | Packed vector; null while work is pending. |
| `attempts` | Consecutive failed embedding attempts. |
| `retry_at` | Earliest retry time after an embedding failure. |

The source relationship is
`conversation_chunks.start_history_id..end_history_id` to the inclusive range
of `conversation_history.id`. It is intentionally not a foreign key because
the index is disposable and rebuilt asynchronously. Uniqueness is enforced
for model/version/source range/part.

There are no `agent_state`, `conversation_compactions`, or persona tables.
Persona remains operator-owned configuration.
