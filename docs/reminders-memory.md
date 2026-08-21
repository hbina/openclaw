---
title: Tasks, reminders, and memory
summary: Persistent owner-state tools retained by the Go runtime
---

The model receives exactly fifteen tools, in deterministic order:

- `add_reminder`, `list_reminders`, `update_reminder`, and `remove_reminder`;
- `add_task`, `list_tasks`, `update_task`, `complete_task`, and `remove_task`;
- `store_memory`, `get_memory`, `list_memories`, `update_memory`,
  `remove_memory`, and `search_memory`.

The task tools keep an owner-global ledger independent from reminders. Each
mutation accepts one task. When a turn requests several changes, the model
emits several tool calls and each call commits or fails independently; earlier
successes are not rolled back and later calls still run after a failure. Tasks
start at the server-recorded creation time and remain open until explicitly
completed or removed. Listing defaults to open tasks; `completed` and `all`
expose history only when explicitly requested. Open tasks may be renamed or
completed, completed tasks are final, and either open or completed records may
be explicitly removed. Duplicate open descriptions are rejected after
trimming and case folding, while the same work may be added again after
completion.

Tasks have no schedule, due date, recurrence, timezone, route, or reminder
linkage. Tool results return Task IDs, derived status, and lifecycle timestamps
in the server timezone. The runtime corrects assistant text that claims a task
mutation without a successful committed result and adds a fixed warning when a
turn contains both successful and failed task mutations.

The reminder tools likewise accept one reminder per mutation and commit
multiple same-turn calls independently. Schedules are:

- `at`: one future RFC 3339 timestamp with an explicit offset;
- `every`: a fixed millisecond interval with an optional anchor;
- `cron`: a timezone-aware five- or six-field cron expression.

An omitted cron timezone uses the server timezone. Reminder ids are scoped to
the trusted channel/sender routing key used for the tool call. The runtime
corrects assistant text that claims an uncommitted reminder mutation and adds
a fixed warning when reminder mutations have mixed results.

An explicit task or unfinished-work request creates a task without inventing a
schedule. An explicit reminder requires a schedule. If the user says “remind
me to…” without a time, the agent asks for one and creates neither kind of
state. A combined task-and-reminder listing uses both tools and labels Task IDs
and Reminder IDs in separate sections.

Memory is global to the one owner. Every record has a stable Memory ID, a kind
(`profile`, `durable`, or `daily`), timestamps, provenance, and immutable
revisions. Updating a memory preserves its ID and prior revisions. Removing it
stops recall and removes its FTS/vector rows while retaining local audit
history. Daily memories are retrieved on demand; a bounded set of active
profile and durable memories is included on every model turn.

Every chat and reminder uses a quality-first local-model pipeline. A dedicated
chat-model pass plans the query, SQLite FTS5 and EmbeddingGemma retrieve memory
and conversation candidates, and another chat-model pass selects the evidence.
The main assistant then responds. A final bounded curator pass may proactively
store or revise useful memories, but cannot delete them. SQLite validates every
proposal and remains the state authority.

Complete transcript exchanges are embedded synchronously before delivery and
committed with the exact delivered transcript. There is no background indexer,
pending index state, retry worker, or retrieval fallback.

When a reminder fires, its stored text is used as a semantic recall query. The
agent combines relevant archived context with the recent conversation, current
local time, and configured persona, then asks the local chat model for a
concise notification body. Reminder rendering exposes no public tools. The
runtime adds the fixed reminder heading. Recall, generation, curation, or
indexing failure prevents delivery and leaves the reminder due.

Successful reminder notifications are stored as a structured scheduled event
and the exact assistant text sent to the channel. That exchange participates
in subsequent recent context and semantic recall. Contextual reminders still
cannot browse a site, suppress unchanged results, run an agent job, or contact
another person.
