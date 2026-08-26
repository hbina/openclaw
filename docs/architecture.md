---
title: Architecture
summary: Retained runtime components and data flow
---

The production image contains one Go binary:

```mermaid
flowchart TD
    INPUT[Owner message or due reminder] --> PLAN[LLM recall planner]
    PLAN --> RETRIEVE[SQLite FTS5 and vector retrieval]
    PLAN -. invalid contract: raw current query .-> RETRIEVE
    RETRIEVE --> RERANK[LLM evidence selector]
    RETRIEVE -. no candidates .-> AGENT
    RERANK --> AGENT[Main LLM agent]
    AGENT --> INDEX[Synchronous conversation embedding]
    INDEX --> DELIVER[HTTP or Telegram delivery]
    DELIVER --> COMMIT[Atomic transcript, chunks, and delivery commit]
    AGENT --> MEMORY[Memory tools]
    MEMORY --> EMBED[Embedding llama-server]
    EMBED --> SQLITE[(SQLite ledger, revisions, FTS5, vectors, traces)]
```

`cmd/openclaw` loads strict configuration, opens SQLite, creates both local
model clients, probes both required local services, validates every derived
index, registers Telegram when enabled, and starts the Gateway.

The agent stores every inbound message, assistant message, tool call, and tool
result as structured transcript rows. Task, reminder, and memory mutations and
their corresponding tool-result rows commit in the same SQLite transaction.

Tasks are one owner-global ledger with no routing or scheduling columns. Their
start and completion timestamps record actual lifecycle events. Reminders are
a separate routed delivery ledger; neither state type references or updates the
other.

Recent context is the latest two complete exchanges for the current routing
key. A bounded profile/durable core is always present. The local chat model
normally rewrites the recall query. If that HTTP-successful planner response
violates its tool contract, chats and reminders use their trimmed current text
with no explicit keywords and continue through the same required SQLite and
embedding retrieval. Empty retrieval is successful and bypasses the evidence
selector; nonempty hybrid memory or conversation candidates require a valid
local-model selection. Recalled conversations are historical evidence, not
current instructions. During chats, the main assistant may use its validated
memory tools when persistence is material to the response. There is no second
post-response curator pass. Reminder rendering exposes no tools and therefore
does not mutate memory.

When a reminder is due, the agent applies the same fixed neutral behavior,
recent exchanges, and semantic conversation recall used for an inbound turn.
It asks the local chat model for a concise, neutral notification body without
exposing public tools, adds the fixed reminder heading, synchronously embeds the
completed exchange, and sends the result. Provider, embedding, SQLite,
retrieval, required selection, generation, indexing, or delivery failure
prevents completion and leaves the reminder due. Successful deliveries commit
transcript and vectors together.

Deleted Node source in Git history may be consulted only as behavioral
evidence. Historical Node application state is deliberately discarded at Go
cutover: the retained runtime does not import, translate, or read it, and no
backward-compatibility path is supported.

The memory-ledger cutover also requires a fresh Go database. Existing Go
tasks, reminders, memories, transcripts, and traces are not migrated.
