# Go Runtime (`golang/`)

This is the retained Go implementation of the OpenClaw personal-assistant
Gateway. Read the repo-root `AGENTS.md` first — it is the single source of
truth for product direction, current status, priorities, and known gaps. This
file only covers what's specific to working inside `golang/`.

## Build and Test

Run all commands from `golang/`, using a writable `GOCACHE` since the default
may be read-only in this environment:

```bash
GOCACHE=/tmp/openclaw-go-cache go test ./...
GOCACHE=/tmp/openclaw-go-cache go test ./internal/gateway/...       # one package
GOCACHE=/tmp/openclaw-go-cache go test ./internal/gateway/ -run TestAgentHandlesMessage  # one test
GOCACHE=/tmp/openclaw-go-cache go vet ./...
GOCACHE=/tmp/openclaw-go-cache go test -race ./...
GOCACHE=/tmp/openclaw-go-cache go build -o /tmp/openclaw-go ./cmd/openclaw
```

Never run `go build` without `-o`: build outputs do not belong in the source
tree. Run `gofmt -w` on touched files and `git diff --check` before handoff.
CGO is required
(`go-sqlite3`); the Dockerfile installs `gcc`/`musl-dev` for the build stage
and only `ca-certificates`/`sqlite-libs` in the final Alpine image.

Tests are colocated `*_test.go` per package (`internal/{channels,config,gateway,providers,state,tools}`)
using Go's `testing` package plus `testify` for assertions. Cover success,
validation, ownership boundaries, persistence/reopen, and failure behavior.
Provider/RAG/reminder-delivery changes need live proof against the local
llama-server endpoints described in the root `AGENTS.md` — mocked HTTP tests
alone don't establish parity.

## Runtime Shape

`cmd/openclaw/main.go` wires everything and has no logic of its own: load
`openclaw.json`/`secrets.json` → open the SQLite `state.Store` → construct the
chat provider (`providers.OpenAIClient`) and embedding provider
(`providers.EmbeddingClient`) → register enabled channels → build `RAGService`,
`Agent`, `Gateway` → start the RAG indexer and the Gateway → block on
SIGINT/SIGTERM → graceful shutdown. `OPENCLAW_DATA_DIR` and
`OPENCLAW_CONFIG_DIR` are required env vars; there is no default.

Package responsibilities:

- **`internal/config`** — strict JSON decode of `openclaw.json` (public) and
  `secrets.json` (credentials, loaded separately, tolerated as missing at
  startup). `LoadConfig` validates required fields at load time (non-empty
  `agents.defaults.soul`/`identity`, a well-formed `models.embeddings.baseUrl`,
  etc.) so invalid config fails fast instead of surfacing as a runtime nil.
- **`internal/providers`** — the model boundary. `Provider` (chat/tool
  generation), `Embedder` (embed/tokenize/detokenize), and `PromptSizer`
  (context size + prompt token counting) are narrow interfaces so
  `gateway`/`tools` tests can inject fakes instead of hitting a live
  llama-server. `OpenAIClient` implements `Provider` + `PromptSizer` against
  the chat llama-server; `EmbeddingClient` implements `Embedder` against the
  dedicated EmbeddingGemma llama-server. `Message.ToolCalls`/`ToolCallID` carry
  exact call IDs end-to-end because history reconstruction depends on them
  matching.
- **`internal/tools`** — the only place tool arguments are trusted. Every
  tool call goes through `Executor.ExecuteAndRecord`, which validates
  `ChannelID`/`SenderID` came from routing (never from model-controlled
  arguments), executes inside `state.Store.WithTx`, and commits the state
  mutation and its tool-result transcript row in the same transaction — so a
  reminder can never be added/updated/removed without a matching recorded
  result (see `hasUnbackedReminderCommitment` in `gateway/agent.go`, which
  corrects the assistant if it claims a mutation without one). `manage_reminders`
  is one tool with `add`/`list`/`update`/`remove` actions and `at`/`every`/`cron`
  schedule kinds — extend by adding actions/kinds here, not new tools, to keep
  the schema simple enough for the deployed local model to call reliably.
- **`internal/state`** — the SQLite layer (`mattn/go-sqlite3`).
  `Store.initializeSchema` creates a fresh database and then validates the exact
  supported table and column set. There are no runtime migrations or schema
  compatibility branches: back up and deliberately rebuild a non-canonical
  database before deploying code that changes the schema.
  `conversation_history` is the append-only structured transcript (`content_type` ∈ text/inbound
  message/tool call/tool result); `conversation_chunks` is the derived RAG
  index over it, keyed by `(embedding_model, index_version, start/end history
  id, part index)` so re-indexing after a model/version change doesn't collide
  with stale rows. `Tx` exposes the narrow subset of operations (reminders,
  memory) that must commit atomically with a tool result.
- **`internal/channels`** — `Channel` interface (`Start`/`Stop`/`SendMessage`)
  plus a `Registry`. `telegram.go` is the only implementation. `ReplyContext`
  carries one-level Telegram reply metadata into the agent as routing/context
  data, not as a tool argument.
- **`internal/gateway`** — the agent loop and HTTP surface.
  - `agent.go`: `Agent.Chat` is the canonical turn handler used by both
    `/chat` and channel delivery. It serializes per-conversation turns through
    `conversationLockManager` (`conversation_lock.go`, keyed by
    `channelID\x00senderID`), loads history, reconstructs it into provider
    messages (`reconstructHistory`, which stops replay at an incomplete
    trailing tool-call sequence rather than erroring), optionally splices in
    RAG-recalled archive context, then runs the tool-call loop up to
    `maxToolRounds` before saving the assistant turn.
  - `rag.go`: `RAGService` runs a background indexer (`run`, woken by a ticker
    or `Notify()` after each turn) that chunks completed conversation
    exchanges, embeds them via `Embedder`, and stores vectors in
    `conversation_chunks`. `Retrieve` embeds the query, scores stored vectors
    by dot product against `minScore`, then binary-searches the largest
    prefix of matches that still fits the model's context window (via
    `PromptSizer.CountPromptTokens`/`ContextSize`) before rendering an archive
    system message. Recalled context is explicitly framed
    (`recalledHistoryPreamble`) as non-authoritative so the model won't replay
    an old mutation from archived history.
  - `gateway.go`: owns `/healthz`, `/chat`, channel startup/shutdown, and the
    one-minute reminder-delivery ticker (`FetchDueReminders` →
    `SendMessage` → `CompleteReminder`).

## Conventions Specific to This Package

- Keep tool JSON schemas minimal and `additionalProperties: false` — the
  deployed local model must call them reliably with `tool_choice: auto`.
- Never let model-controlled tool arguments substitute for `ChannelID`/
  `SenderID`; those always come from the channel/HTTP layer.
- When adding config, extend the strict structs in `internal/config/config.go`
  and validate in `LoadConfig`/`LoadSecrets`; there's no loose/dynamic config
  path.
- A schema change replaces the canonical definition and validator together.
  Provide a separately reviewed, backup-first operator rebuild; do not add
  runtime `ALTER TABLE` migrations, legacy table drops, or compatibility
  fallbacks.
