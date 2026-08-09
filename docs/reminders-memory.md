---
title: Tasks, reminders, and memory
summary: Persistent owner-state tools retained by the Go runtime
---

The model receives exactly four tools, in deterministic order:

- `manage_reminders`;
- `manage_tasks`;
- `store_memory`;
- `search_memory`.

`manage_tasks` keeps an owner-global ledger independent from reminders. Adding
one to 50 descriptions is atomic. Tasks start at the server-recorded creation
time and remain open until explicitly completed or removed. Listing defaults
to open tasks; `completed` and `all` expose history only when explicitly
requested. Open tasks may be renamed or completed, completed tasks are final,
and either open or completed records may be explicitly removed. Duplicate open
descriptions are rejected after trimming and case folding, while the same work
may be added again after completion.

Tasks have no schedule, due date, recurrence, timezone, route, or reminder
linkage. Tool results return Task IDs, derived status, and lifecycle timestamps
in the server timezone. The runtime corrects assistant text that claims a task
mutation unless a successful committed `manage_tasks` result backs it.

`manage_reminders` supports atomic batch add, list, update, and remove actions.
Schedules are:

- `at`: one future RFC 3339 timestamp with an explicit offset;
- `every`: a fixed millisecond interval with an optional anchor;
- `cron`: a timezone-aware five- or six-field cron expression.

An omitted cron timezone uses the server timezone. Reminder ids are scoped to
the trusted channel/sender routing key used for the tool call. The runtime
corrects assistant text that claims a reminder mutation unless a successful,
committed tool result backs the claim.

An explicit task or unfinished-work request creates a task without inventing a
schedule. An explicit reminder requires a schedule. If the user says “remind
me to…” without a time, the agent asks for one and creates neither kind of
state. A combined task-and-reminder listing uses both tools and labels Task IDs
and Reminder IDs in separate sections.

Durable memory is global to the one owner. A memory write stores one concise
fact with its embedding. Search embeds the query and scores compatible stored
vectors. The search is semantic, not a substring lookup.

Conversation recall is separate from durable memory. Complete transcript
exchanges are indexed asynchronously and can be recalled across the owner's
Telegram and HTTP routing keys. The index is derived and can be rebuilt from
`conversation_history`.

When a reminder fires, its stored text is used as a semantic recall query. The
agent combines relevant archived context with the recent conversation, current
local time, and configured persona, then asks the local chat model for a
concise notification body. Reminder rendering exposes no tools. The runtime
adds the fixed reminder heading and falls back to the stored text when context
retrieval or generation is unavailable.

Successful reminder notifications are stored as a structured scheduled event
and the exact assistant text sent to the channel. That exchange participates
in subsequent recent context and semantic recall. Contextual reminders still
cannot browse a site, suppress unchanged results, run an agent job, or contact
another person.
