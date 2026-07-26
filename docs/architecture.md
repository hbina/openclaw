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
                          └─> reminder delivery loop ─> Telegram
```

`cmd/openclaw` loads strict configuration, opens SQLite, creates both local
model clients, registers Telegram when enabled, starts conversation indexing,
and starts the Gateway.

The agent stores every inbound message, assistant message, tool call, and tool
result as structured transcript rows. Tool mutations and their corresponding
tool-result rows commit in the same SQLite transaction.

Recent context is the latest two complete exchanges for the current routing
key. Older complete exchanges from any of the owner's channels are embedded
into a derived index and recalled by semantic similarity when they fit the
chat model's context window. Recalled text is marked as historical context,
not as current instructions.

The repository still contains Node source as a behavioral reference during
migration. It is not copied into the Go image and is not a supported
deployment path.
