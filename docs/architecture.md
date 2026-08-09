---
title: Architecture
summary: Retained runtime components and data flow
---

The production image contains one Go binary:

```text
Telegram text ─┐
               ├─> Gateway ─> Agent/tool loop ─> chat llama-server
POST /chat ────┘          │          │
                          │          ├─> embedding llama-server
                          │          └─> SQLite
                          └─> reminder delivery loop ─> contextual agent render
                                                       ├─> chat llama-server
                                                       ├─> embedding llama-server
                                                       └─> SQLite ─> Telegram
```

`cmd/openclaw` loads strict configuration, opens SQLite, creates both local
model clients, registers Telegram when enabled, starts conversation indexing,
and starts the Gateway.

The agent stores every inbound message, assistant message, tool call, and tool
result as structured transcript rows. Task, reminder, and memory mutations and
their corresponding tool-result rows commit in the same SQLite transaction.

Tasks are one owner-global ledger with no routing or scheduling columns. Their
start and completion timestamps record actual lifecycle events. Reminders are
a separate routed delivery ledger; neither state type references or updates the
other.

Recent context is the latest two complete exchanges for the current routing
key. Older complete exchanges from any of the owner's channels are embedded
into a derived index and recalled by semantic similarity when they fit the
chat model's context window. Recalled text is marked as historical context,
not as current instructions.

When a reminder is due, the agent loads the same persona, recent exchanges,
and semantic conversation recall used for an inbound turn. It asks the local
chat model for a concise notification body without exposing tools, adds the
fixed reminder heading, and sends the result. If context retrieval or model
generation fails, delivery uses the stored reminder text. Successful
deliveries are recorded as complete scheduled-reminder exchanges and become
available to later context and recall.

The repository still contains Node source as a behavioral reference during
migration. It is not copied into the Go image and is not a supported
deployment path.
