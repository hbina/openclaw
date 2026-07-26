---
title: Reminders and memory
summary: Agent tools retained by the Go runtime
---

The model receives exactly three tools:

- `manage_reminders`;
- `store_memory`;
- `search_memory`.

`manage_reminders` supports atomic batch add, list, update, and remove actions.
Schedules are:

- `at`: one future RFC 3339 timestamp with an explicit offset;
- `every`: a fixed millisecond interval with an optional anchor;
- `cron`: a timezone-aware five- or six-field cron expression.

An omitted cron timezone uses the server timezone. Reminder ids are scoped to
the trusted channel/sender routing key used for the tool call. The runtime
corrects assistant text that claims a reminder mutation unless a successful,
committed tool result backs the claim.

Durable memory is global to the one owner. A memory write stores one concise
fact with its embedding. Search embeds the query and scores compatible stored
vectors. The search is semantic, not a substring lookup.

Conversation recall is separate from durable memory. Complete transcript
exchanges are indexed asynchronously and can be recalled across the owner's
Telegram and HTTP routing keys. The index is derived and can be rebuilt from
`conversation_history`.

Static reminders cannot browse a site, suppress unchanged results, run an
agent job, or contact another person.
