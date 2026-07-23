# Go Migration Plan and Status

This is the single source of truth for the slim fork's product goals, retained
scope, implementation status, migration sequence, verification, and cutover
gates. Future work starts with this document and updates it whenever scope,
status, proof, priorities, or known gaps change.

The target is a small Go reminder assistant backed by a local `llama-server`,
not parity with the full upstream OpenClaw product. The fork should be easier to
install, audit, move between hosts, and operate than upstream. It should not
carry unused apps, providers, plugins, release paths, QA infrastructure, or
runtime dependencies.

The TypeScript/Node runtime remains the production reference. The Go runtime is
an active prototype: local text generation, structured reminder/memory tools,
persisted at/every/cron schedules, reminder CRUD, and standalone-container
restart persistence are proven. Required operator-owned Soul and Identity
configuration is loaded once at startup and injected into the system prompt.
Channel parity, scheduled agent jobs, Gateway reliability, state migration, and
production cutover remain incomplete.

## Target

The Go runtime retains:

- One primary assistant profile.
- One trusted human owner per deployment; each additional person runs a separate
  bot instance.
- One local OpenAI-compatible `llama-server` endpoint.
- One Telegram text channel.
- Batch reminder creation, listing, editing, cancellation, recurring schedules,
  and delivery.
- Global durable memory storage and search.
- Persistent structured conversation history, with retrieval-augmented context
  selection planned as a future replacement for full-history replay.
- A small unauthenticated HTTP Gateway for a trusted local network.
- Canonical non-secret config plus a separate optional credential file.
- SQLite state outside the container image.
- A standalone Go image with no Node or cloud-model runtime dependency.

Streaming, media input, embeddings, reasoning fields, cloud-provider fallback,
full upstream Gateway parity, and public-internet or untrusted-network Gateway
deployment are not first-cut requirements.

## Product and Migration Principles

- Docker is the canonical deployment path. Non-secret behavior lives in one
  mounted `openclaw.json`; credentials live in a separate mounted secret file;
  SQLite state lives outside the image.
- The Go runtime uses one local OpenAI-compatible `llama-server`. Do not add
  hosted OpenAI, Anthropic, Claude API/CLI, ChatGPT, MCP subprocess, or cloud
  fallback paths. `models.providers.openai` is only the compatibility wire key.
- Keep one primary agent and only the Telegram text channel. Discord and
  WhatsApp are not part of the Go runtime; their Node implementations remain
  reference-only until Node cutover.
- Keep one trusted owner per deployment. The instance owns one global body of
  memory, persona, history, and operational state across the owner's channels.
  Multi-user accounts, tenant boundaries, and per-user data isolation are
  explicit non-goals; another person receives a separate deployment.
- The HTTP Gateway is intentionally unauthenticated and reachable on all host
  interfaces for trusted local-network clients. LAN clients and caller-selected
  `sender_id` values are trusted; channel and sender identifiers are routing and
  conversation keys for the owner, not security principals or data-ownership
  boundaries. Public-internet and untrusted-network deployment are unsupported,
  and the operator owns the router/firewall boundary.
- Channel pairing or allowlists, if retained, enforce the single-owner admission
  boundary at ingress. They do not create tenants or partition state inside the
  Go runtime.
- Gateway request limits, timeouts, and stable errors are reliability contracts,
  not authentication or hostile-network hardening.
- SQLite is canonical for reminders, history, memory, and other
  OpenClaw-owned runtime state. Persona is required non-secret configuration in
  `openclaw.json`; do not add state sidecars, persona tables, or fallback readers.
- Prefer deletion and one canonical implementation over compatibility shims for
  fork surfaces that have never shipped.
- Keep secrets out of git, build contexts, image layers, logs, and reports.
- The retained Node runtime is a behavioral reference until Go cutover, not a
  product-breadth target. Capture fixtures before deleting Node behavior that Go
  must retain.
- Remove a dependency only after its last retained runtime, build, test, and
  documentation caller is gone. Preserve or rewrite tests for retained behavior.
- Validate each slice with focused tests, a standalone Docker build, a real
  local-model request, SQLite/transcript inspection, and restart proof when
  persistence changes.

## Retained Surface

| Surface                     | Decision     | Required contract                                                                                        |
| --------------------------- | ------------ | -------------------------------------------------------------------------------------------------------- |
| Go Gateway and agent        | Keep         | One owner and primary assistant per deployment; unauthenticated trusted-LAN HTTP chat surface; health    |
| Local model                 | Keep         | OpenAI-compatible `llama-server`, required local base URL, optional LAN bearer token, model id `default` |
| Reminders                   | Keep         | Atomic batch CRUD, `at`/`every`/timezone-aware `cron`, durable delivery                                  |
| Memory and persona          | Keep         | SQLite recall/search and history; required startup-loaded `agents.defaults.soul`/`identity`              |
| Telegram                    | Keep         | Pairing/allowlist, inbound/outbound DM text, reminder delivery                                           |
| WhatsApp                    | Remove       | No Go adapter, startup/config surface, session database, or runtime dependency                           |
| Discord                     | Remove       | No Go adapter, startup/config surface, token, intents, or runtime dependency                             |
| Docker operations           | Keep         | Mounted config/secrets/state, health, restart, backup/restore, secret-free image                         |
| Node runtime                | Temporary    | Reference and fixture source until all Go cutover gates pass                                             |
| Hosted/cloud providers      | Remove       | No hosted OpenAI, Anthropic, Claude, ChatGPT, or other provider runtime                                  |
| Other apps/plugins/channels | Remove/defer | Reintroduce only for a concrete approved reminder-assistant requirement                                  |

Dashboard depth, browser/canvas tools, file transfer, streaming, media, local
embeddings, automatic recall, scheduled agent jobs, and exact Node Gateway
protocol compatibility remain explicit decisions rather than assumed scope.

## Repository Migration Status

- Safety and secret hygiene are established in policy and ignore rules, but a
  clean-clone image-layer audit is still required before publishing.
- The retained plugin/channel set has been narrowed, while the Node workspace,
  CLI, docs, dependency graph, Control UI, and operational surface still include
  broader upstream behavior.
- The standalone Go image builds without Node, npm, Claude, or a cloud-model
  runtime. The root Docker/Compose production path has not cut over.
- A basic Telegram adapter exists, but pairing, access control, richer channel
  semantics, and credential-backed live proof are incomplete.
- Local text, structured tools, reminders, memory, configured persona, transcripts,
  SQLite persistence, and restart behavior are implemented and tested as
  detailed below.
- Go release/distribution, Node-state migration, rollback, and production
  acceptance have not been completed.

## Package Map

- `golang/cmd/openclaw`: Startup and dependency wiring.
- `golang/internal/config`: Slim config and secret decoding.
- `golang/internal/providers`: OpenAI-compatible structured chat contract and local
  HTTP client.
- `golang/internal/tools`: Trusted in-process reminder and memory tool execution.
- `golang/internal/state`: SQLite reminders, history, and memory state.
- `golang/internal/channels`: the Telegram adapter and generic channel registry.
- `golang/internal/gateway`: Agent loop, HTTP server, and reminder delivery.

## Current Implementation

### Startup and local model

Implemented:

- `OPENCLAW_DATA_DIR` and `OPENCLAW_CONFIG_DIR` select mounted state and config.
- `openclaw.json` is required. Startup fails if it cannot be read or parsed, or
  if `agents.defaults.soul` or `agents.defaults.identity` is missing, empty, or
  whitespace-only. Both persona strings are trimmed and loaded once; edits take
  effect only after process restart.
- `models.providers.openai.baseUrl` is retained as the compatibility key for a
  local OpenAI-compatible endpoint.
- The base URL is mandatory; there is no public OpenAI endpoint default.
- The optional `openai.apiKey` secret becomes a bearer token only when present.
- Startup rejects provider prefixes other than `openai` instead of falling back
  to a cloud provider or subprocess.
- Generation intentionally sends model id `default`, matching the verified
  llama-server deployment.
- Agent calls send a 4,096-token output cap. Every provider call has a
  five-minute deadline, with an earlier caller deadline taking precedence.
- Startup resolves Go's server-local timezone once from the host/container
  environment and passes that same location to the prompt and reminder tools.
  No application timezone is hardcoded or added to config.
- Anthropic, Claude CLI, MCP-server mode, Node.js, and npm have been removed
  from the Go runtime and image.

Limitations:

- Config decoding validates JSON syntax but not all semantic constraints.
- Plugin config is parsed but does not activate a Go plugin system.
- Streaming, usage accounting, retries, capability negotiation, media, and
  structured output beyond tool calls are not implemented.

### Structured agent and tools

Implemented:

- The provider contract carries system, user, assistant, and tool messages;
  assistant tool calls; exact call ids; JSON arguments; tool definitions; and
  finish reasons.
- Normal Chat Completions requests send three function tools with sequential
  execution and `tool_choice: auto`. The model decides from full conversation
  context whether the user is actually asking for a reminder operation;
  quotations, mentions, and questions about reminder wording do not force the
  reminder tool.
- Successful `manage_reminders` mutations are tracked from non-error tool
  results. If the assistant claims it added, scheduled, updated, removed, or
  cancelled a reminder without a committed mutation, the delivered and stored
  response states that no reminder change was committed.
- Turns are serialized by trusted channel and sender identity with a
  context-aware in-memory lock. Different conversations can still generate
  concurrently, and cancellation releases the lock.
- The agent supports up to four tool rounds and returns a deterministic safety
  response if the model does not terminate the workflow.
- Tool calls and results persist as structured SQLite transcript rows and are
  reconstructed as native assistant/tool messages on later turns.
- New user turns persist as structured `inbound_message` JSON. Telegram reply
  turns retain the referenced message id, author classification, text/caption,
  selected quote, and content-availability marker; legacy plain-text user rows
  remain replayable without a schema migration.
- A Telegram reply is rendered to the local model as explicit reply context
  followed by the current user message. The model sees the source author,
  source body, and selected text, but not the Telegram message id. Ordinary
  non-reply input remains unchanged at the provider boundary.
- Each model request loads all SQLite transcript rows for its conversation.
  There is no fixed row limit, channel/DM history-limit config, summarization,
  trimming, or current context-bounding mechanism.
- Mutating tool state and the matching result row commit in one transaction.
- Invalid arguments, unknown tools, and execution failures return structured
  error results so the model can correct its request.
- Channel and sender identifiers come from trusted agent context and are absent
  from model-visible schemas.
- The startup-loaded persona is appended to every system prompt in stable plain
  text sections named `Soul:` and `Identity:`. Chat callers cannot view or mutate
  it through a model tool.

Available tools:

- `manage_reminders(action, ...)` is the user-scoped reminder surface:
  - `add` accepts 1-50 items in one transaction.
  - `list` returns persisted ids, schedules, enabled state, and next-fire times.
  - `update` edits message, enabled state, and/or the schedule by id.
  - `remove` atomically removes one or more ids.
- Reminder schedules match the retained Node shapes: one-shot `at`, fixed
  `every` with an optional anchor, and five/six-field `cron` expressions with
  an optional IANA timezone. An omitted cron timezone is persisted as the
  resolved server timezone, and next-fire values are rendered in that timezone.
  IANA data is embedded for the minimal Alpine image.
- `store_memory(content)` stores a global durable fact and deduplicates exact
  repeats.
- `search_memory(query)` returns up to five global substring matches.

Reminder mutations and their matching tool-result transcript commit together.
Batch validation is atomic, so one invalid item rolls back the whole add. The
prompt permits model-initiated storage of stable preferences and durable facts,
excludes credentials and transient details, and explicitly distinguishes static
reminder delivery from unsupported scheduled agent work.

Limitations:

- Memory and inspectable bot state are intentionally instance-wide for the one
  trusted owner; channel/sender conversation keys do not create data partitions.
- Memory search is SQL substring matching, not semantic/vector retrieval.
- There is no deterministic pre-generation memory injection; the model must
  choose `search_memory`.
- Semantic reminder selection depends on the local model. An unsupported
  mutation claim receives a deterministic corrective note rather than an
  automatic forced-tool retry.
- Reminder polling has no durable claim/lease or delivery-idempotency protocol.
- Static Go reminders cannot yet execute Node-style isolated agent turns. A
  watcher that stays silent unless data changes and a task that contacts another
  recipient are not implemented by storing those words as a reminder message.
- Existing reminders with an explicit timezone are not silently rewritten when
  the server timezone changes. Vague phrases such as "tonight" still require an
  exact time decision or clarification; this change fixes the timezone basis,
  not the missing-hour ambiguity.
- Persona is global to the single agent and operator-controlled. Configuration
  changes require a restart; there is no runtime reload endpoint.
- Full-history replay can exceed the local model context window as transcripts
  grow. Retrieval-augmented context selection is planned but is not implemented;
  the runtime currently performs no input token estimation or context bounding.

### Gateway and channels

Implemented:

- `GET /healthz` and unauthenticated `POST /chat` over HTTP.
- The Gateway listens on all interfaces for its intended trusted-LAN deployment.
  `/chat` accepts a caller-selected `sender_id` as a conversation key; it is not
  an authenticated identity.
- A one-minute reminder loop delivers due reminders through the matching channel.
  It deletes a successfully delivered one-shot and advances a successfully
  delivered recurring reminder to its next anchored/cron occurrence. Failed
  sends remain due for retry.
- Telegram inbound/outbound text adapter with one-level reply extraction for
  `reply_to_message`, including text, caption fallback, selected quote, source
  author classification, and an explicit unavailable-content marker.
- Bot responses remain ordinary Telegram messages rather than native Telegram
  replies.

Limitations:

- The Node WebSocket Gateway protocol is not implemented.
- HTTP has no request-size policy, explicit server timeout/concurrency policy,
  or stable HTTP error schema. These are local reliability gaps; inbound
  authentication and hostile-network hardening are explicit non-goals.
- Pairing, allowlists, group policy, mentions, media, threads, reactions,
  commands, streaming updates, and multi-account routing are absent.
- Replied-to media is not downloaded or interpreted, and external or recursive
  reply chains are not reconstructed.
- No credential-backed inbound Telegram reply or reminder-delivery proof is
  recorded.

### State and compatibility

Implemented:

- SQLite stores reminders, including canonical schedule kind/definition,
  timezone, enabled state, and next-fire timestamp, plus global memory,
  conversation rows, and generic agent state. Existing Go one-shot rows migrate
  to `schedule_kind = at`.
- Fresh databases do not create `personality_documents`. Opening an older Go
  database drops that table without importing or preserving its contents and
  without changing reminders, memory, or history.
- Fresh databases do not create `conversation_compactions`. Older databases may
  retain that table as unused legacy data; startup does not read, write, trim,
  migrate, or delete it.
- The Go image is now a Go binary plus Alpine CA certificates and SQLite runtime
  libraries; it no longer installs Node/npm/Claude.

Limitations:

- Go state does not match Node session, transcript, checkpoint, branching,
  locking, usage, or metadata contracts.
- No Node-to-Go migration, rollback, production first-run acceptance,
  backup/restore, or secret-layer audit is implemented.
- The root `Dockerfile` still builds Node; production has not cut over to
  `golang/Dockerfile`.

## Verification

Automated Go coverage includes:

- Local HTTP request/response wire shape, optional authorization, automatic tool
  choice, bounded `max_tokens`, cancellation, tool calls, call ids, malformed
  responses, empty choices, and HTTP failures.
- Tool schemas, strict arguments, trusted identity, scoped mutation, atomic
  batch rollback, add/list/update/remove, at/every/cron validation and next-run
  calculation, server-timezone defaulting and display, recurring post-delivery
  advancement, global memory, exact deduplication, and structured result
  persistence.
- Required persona validation/trimming, exact Soul-before-Identity prompt
  sections, startup snapshot behavior, removed personality tool handling, and
  legacy personality-table removal without runtime-state loss.
- Multi-round agent execution, validation-error recovery, tool-call/result
  replay, semantic reminder routing, uncommitted-claim correction, per-conversation
  serialization and cancellation, complete-history loading, the four-round
  limit, prompt/tool identity separation, history, the one-minute reminder
  polling interval, Gateway health, and SQLite state.
- Telegram adapter extraction for plain text, assistant/user/other reply
  authors, caption fallback, selected quotes, unavailable non-text sources, and
  malformed updates. Agent coverage proves exact reply rendering, transport
  metadata exclusion, structured persistence/reopen/replay, ordinary-input
  preservation, and invalid reply rejection before persistence.

Live standalone-container proof includes:

- Building `openclaw-go-local-tools:test` from `golang/Dockerfile`.
- Health and text replies through the current local Gemma llama-server.
- Global memory storage and recall from a different sender.
- Relative reminder add/list/delete and absolute RFC3339 scheduling.
- Container restart followed by successful memory recall and reminder listing.
- SQLite inspection showing paired structured tool-call/result rows, one durable
  memory, and zero pending reminders after cleanup.
- Runtime inspection showing no `node`, `npm`, or `claude` executable.

Live recurring-reminder proof on 2026-07-15 includes:

- Rebuilding and restarting `openclaw-go-test-ubuntu` from `golang/Dockerfile` while
  preserving its config, secret, state mounts, local llama-server URL, restart
  policy, and host port 18792.
- Sending one natural-language request containing five recurring schedules and
  two one-shots through `POST /chat`.
- One structured `manage_reminders` batch call creating all seven rows.
- SQLite inspection proving daily, weekday, Mon/Wed/Fri, Saturday, and Sunday
  cron expressions in `Asia/Kuala_Lumpur`, correct next-fire timestamps, and the
  two explicit GMT+8 one-shots.
- A second model/tool turn listing the persisted reminders and a third removing
  all seven test rows. Final SQLite count for the test sender was zero; the
  structured call/result audit rows remain.

Server-timezone proof on 2026-07-16 includes:

- Building `openclaw-go-ubuntu-test:server-timezone` from `golang/Dockerfile` and
  starting an isolated container with `TZ=Asia/Kuala_Lumpur`, separate config,
  and a fresh SQLite database.
- Startup logging `Using server timezone Asia/Kuala_Lumpur`; `/healthz` passed.
- A real natural-language daily-08:00 request through the local llama-server.
  The model omitted the cron timezone, the tool persisted
  `Asia/Kuala_Lumpur`, and SQLite stored the next fire as
  `2026-07-16 08:00:00+08:00`.
- The structured tool result reported `display_timezone` as
  `Asia/Kuala_Lumpur` and the same local next-fire timestamp. The isolated proof
  container was removed; the persistent test container and its state were not
  changed.

Operator-owned persona proof on 2026-07-16 includes:

- Running `go test ./...`, `go test -race ./...`, `go vet ./...`, and
  `go build -o /tmp/openclaw-go ./cmd/openclaw` successfully with a writable Go
  cache. Focused coverage proves trimming and required-field failures, exact
  Soul/Identity prompt sections and startup snapshots, a three-tool catalog with
  no persona mutation tool, and legacy-table removal without reminder, memory,
  or history loss.
- Building `openclaw-go-ubuntu-test:persona-config` from `golang/Dockerfile` with
  image id `sha256:6e81012ca4737ba90334daea28c8446d3100e1668470ea9a64b939157b3bc8fe`.
- Recreating `openclaw-go-test-ubuntu` from that image while preserving both
  named volumes, read-only config/secret binds, the existing `/data` bind, port
  18792, `OPENCLAW_CONFIG_DIR`, `OPENCLAW_DATA_DIR`, `TZ=Asia/Kuala_Lumpur`, and
  restart policy `unless-stopped`.
- Verifying the local llama-server health endpoint from both the host bridge and
  inside the container, followed by Gateway health and startup in
  `Asia/Kuala_Lumpur`.
- Opening the existing SQLite database dropped `personality_documents` while
  retaining its eight existing reminders and 33 pre-proof conversation rows.
  A unique live `/chat` turn added paired user/assistant text rows.
- The real local Gemma response was: “I'm Jet 🦊, your local AI familiar and
  personal assistant, and I'm warm, direct, curious, resourceful, and
  occasionally mischievous.” A container restart then passed health with the
  table still absent, both proof transcript rows retained, and all eight
  reminders retained.

Context-aware reminder-routing proof on 2026-07-17 includes:

- Running `go test ./...`, `go vet ./...`, `go test -race ./...`, and
  `go build -o /tmp/openclaw-go ./cmd/openclaw` successfully with
  `GOCACHE=/tmp/openclaw-go-cache`.
- Building `openclaw-go-ubuntu-test:node-reminder-routing` from
  `golang/Dockerfile`, image id
  `sha256:f8c82202523ac57838bdf05956fc6413c20d34a274a6a6c2e852ba5cce724864`,
  and recreating `openclaw-go-test-ubuntu` while preserving all mounts,
  environment, port 18792, and restart policy.
- Sending a fresh-sender request that quoted “I will keep the reminder active”
  and asked for an opinion. The real local Gemma returned a normal wording
  critique in 12 seconds; SQLite contained only the user and assistant text
  rows and zero reminder or tool rows.
- Sending genuine natural-language add, list, and remove requests under the same
  sender. Automatic tool selection created reminder id 15 for
  `2026-07-18T09:25:00+08:00`, listed that persisted id, and removed it. Final
  SQLite inspection showed 22 proof transcript rows, five paired structured
  call/results, zero proof reminders, and the seven pre-existing reminders
  unchanged.
- Gateway and llama-server health remained HTTP 200 with no OpenClaw generation
  errors or context-size errors during proof. Verbose server metadata confirmed
  the 4,096-token cap on all 11 live generation calls. Restarting the container
  preserved all 22 proof transcript rows, zero proof reminders, and all seven
  pre-existing reminders; the host llama-server remained on pid 7675 without a
  restart.

Renamed-directory redeployment proof on 2026-07-21 includes:

- Running `go test ./...`, `go test -race ./...`, `go vet ./...`, and
  `go build -o /tmp/openclaw-go ./cmd/openclaw` successfully from `golang/` for
  commit `8a572e723817`.
- Building `openclaw-go-ubuntu-test:golang-dir-20260721` from `golang/`. The
  resulting image id remained
  `sha256:f8c82202523ac57838bdf05956fc6413c20d34a274a6a6c2e852ba5cce724864`
  because the commit renamed the directory without changing runtime source
  bytes.
- Recreating `openclaw-go-test-ubuntu` from the new tag while preserving both
  named volumes, all three binds, the three explicit environment overrides,
  bridge networking, host port 18792, and restart policy `unless-stopped`.
- Verifying host and container access to the configured local llama-server,
  Gateway health, `Asia/Kuala_Lumpur` startup, and SQLite integrity. The
  pre-recreation baseline was four enabled cron reminders, 177 transcript rows,
  zero memory rows, and no legacy personality table.
- Sending a unique harmless live `/chat` request through the local model. It
  produced one user and one assistant text row; a final container restart
  retained all four reminders, both proof rows, and all 179 transcript rows.

One-minute reminder polling proof on 2026-07-22 includes:

- Replacing the hardcoded 30-second ticker with a named one-minute interval and
  adding a focused Gateway regression test for the exact duration.
- Running `go test ./...`, `go test -race ./...`, `go vet ./...`, and
  `go build -o /tmp/openclaw-go ./cmd/openclaw` successfully with
  `GOCACHE=/tmp/openclaw-go-cache`.
- Building `openclaw-go-ubuntu-test:minute-poll-20260722` from `golang/`, image
  id `sha256:05fb757d771812a7bb97058f6f01c0251e2b5804df2b876632503d022b4c492c`,
  and recreating `openclaw-go-test-ubuntu` while preserving both volumes, all
  three binds, the three explicit environment overrides, bridge networking,
  host port 18792, and restart policy `unless-stopped`.
- Verifying Gateway health, host and container access to the local llama-server,
  and a harmless live `/chat` turn through the real local model. SQLite held the
  paired user/assistant proof rows, five reminders, 223 transcript rows, one
  memory row, and passed `PRAGMA integrity_check`.
- Restarting the final container and verifying host and in-container health;
  both proof rows and all existing SQLite state remained intact.

Unlimited conversation-history proof on 2026-07-23 includes:

- Removing the fixed 20-row query cap and the Go-only channel/DM history-limit
  config fields. Focused coverage seeds 24 prior rows and proves that the next
  provider request receives all 24 in order, plus the system and current-user
  messages.
- Running `go test ./...`, `go test -race ./...`, `go vet ./...`, and
  `go build -o /tmp/openclaw-go ./cmd/openclaw` successfully with
  `GOCACHE=/tmp/openclaw-go-cache`.
- Building `openclaw-go-ubuntu-test:unlimited-history-20260723` from `golang/`,
  image id `sha256:14e68751bb34024e17468dec288c87f9b97636177415ed3f96412f7d677af446`.
- Starting an isolated candidate container with a tmpfs database, confirming
  Gateway and in-container llama-server health, and receiving the exact requested
  reply `unlimited history smoke passed` through the real local model. SQLite
  contained the paired user/assistant text rows, zero proof reminders, and passed
  `PRAGMA integrity_check`.
- Leaving the persistent `openclaw-go-test-ubuntu` deployment on
  `minute-poll-20260722`; its health remained HTTP 200.

Compaction-removal proof on 2026-07-23 includes:

- Removing the Go summarization prompt and model call, token-threshold decision,
  summary injection, history trimming, compaction store API, and fresh-database
  `conversation_compactions` schema. Retrieval-augmented context selection was
  explicitly deferred; complete-history replay remains the interim behavior.
- Focused coverage proves a transcript larger than the former threshold causes
  only one normal generation and retains every row. State coverage proves fresh
  databases omit the compaction table while older unused compaction data is not
  deleted during startup.
- Running `go test ./...`, `go test -race ./...`, `go vet ./...`, and
  `go build -o /tmp/openclaw-go ./cmd/openclaw` successfully with
  `GOCACHE=/tmp/openclaw-go-cache`.
- Building `openclaw-go-ubuntu-test:no-compaction-20260723` from `golang/`,
  image id `sha256:cc92c5f9f763db379ccf454b92c05304b6f8f9bf9e50981bf6756dd48f1eaa18`.
- Starting an isolated candidate with a tmpfs database and disabled Telegram
  credentials, confirming Gateway health, and receiving the exact replies
  `compaction removal smoke passed` and `compaction removal sqlite proof`
  through the real local model. SQLite contained the paired user/assistant
  rows, no `conversation_compactions` table, and passed `PRAGMA integrity_check`.
- Removing the isolated candidate, then recreating the persistent
  `openclaw-go-test-ubuntu` deployment on
  `openclaw-go-ubuntu-test:no-compaction-20260723`, container id
  `d947777408ad`, while preserving both named volumes, all three bind mounts,
  the three explicit environment overrides, bridge networking, host port
  `18792`, and restart policy `unless-stopped`.
- Recording five reminders, 237 transcript rows, one memory, and an empty
  legacy compaction table before recreation. After a real-model `/chat` request,
  SQLite held the expected paired proof rows, all five reminders, the memory,
  zero legacy compaction records, and passed `PRAGMA integrity_check`.
- Receiving the exact reply `updated deployment healthy`, restarting the
  recreated container, and verifying HTTP 200 health plus persistence of the
  proof transcript and all pre-existing state.

Telegram-only Go runtime proof on 2026-07-23 includes:

- Deleting the Discord and WhatsApp Go adapters, startup/config fields, and
  direct dependencies. `go mod tidy` also removed their unused transitive
  dependency graph. The built binary reports only SQLite, cron, and Telebot
  dependencies. Legacy Discord/WhatsApp JSON keys remain harmlessly ignored.
- Preserving the generic channel/state contracts and all existing SQLite data.
  The persistent database had five Telegram reminders and no Discord or
  WhatsApp reminders before deployment; 38 historical Discord transcript rows
  were not deleted or rewritten.
- Running `go test ./...`, `go test -race ./...`, `go vet ./...`, and
  `go build -o /tmp/openclaw-go ./cmd/openclaw` successfully with writable
  caches under `/tmp`.
- Building `openclaw-go-ubuntu-test:telegram-only-20260723` from `golang/`,
  image id `sha256:017cd4c84cfc0fdca79b8c50bd8f00bbe3ee1da9e298e71e97f533396b0da0b0`.
- Starting an isolated no-secrets candidate with tmpfs state, confirming Gateway
  and in-container llama-server health, and receiving the exact reply
  `telegram only runtime smoke passed` through the real local model. The copied
  SQLite snapshot contained the paired text rows, zero reminders, and passed
  `PRAGMA integrity_check`.
- Recreating `openclaw-go-test-ubuntu` on the new image while preserving its two
  volumes, three binds, environment, bridge network, host port 18792, and
  `unless-stopped` restart policy. A persistent live-model request returned
  `persistent telegram only smoke passed`; after restart, both proof rows, all
  five Telegram reminders, and SQLite integrity remained intact.

Telegram reply-context proof on 2026-07-24 includes:

- Adding one canonical structured inbound-turn path for Telegram and HTTP,
  one-level Telegram reply extraction, deterministic reply rendering for the
  model, and SQLite replay across restart. Telegram message ids remain stored
  transport metadata and are excluded from model content; outbound messages
  remain unthreaded.
- Running `go test ./...`, `go test -race ./...`, `go vet ./...`, and
  `go build -o /tmp/openclaw-go ./cmd/openclaw` successfully with
  `GOCACHE=/tmp/openclaw-go-cache`.
- Building
  `openclaw-go-ubuntu-test:telegram-reply-context-20260723`, image id
  `sha256:8a558d5f27a39a436fee449bc1aa607df77316d1aa5e671c14a63a9683894ad`.
- Starting an isolated no-secrets candidate with bind-mounted temporary state,
  confirming Gateway and in-container llama-server health, and exercising the
  exact reply-context format against the real local Gemma model. After an
  earlier assistant message said `3 PM` and a later conflicting message said
  `6 PM`, the reply-context turn targeting the earlier message returned exactly
  `3 PM`. The six proof rows survived restart and SQLite passed
  `PRAGMA integrity_check`; the candidate container was then removed.
- Recreating `openclaw-go-test-ubuntu` on the new image while preserving both
  named volumes, all three binds, the three explicit environment overrides,
  bridge networking, port 18792, and restart policy `unless-stopped`. The
  recreated container id was `9c5cf836b094`.
- Recording a pre-recreation baseline of five reminders, 239 transcript rows,
  one memory, and clean SQLite integrity. A persistent real-model request
  returned exactly `deployed reply context healthy` and stored the user turn as
  `inbound_message`; the resulting 241 rows, all reminders, the memory, and
  SQLite integrity survived container restart.
- Credential-backed Telegram startup used the mounted secret, but no inbound
  owner reply was generated during automated proof. Actual Telegram reply
  ingress remains a recorded proof gap rather than being inferred from unit or
  `/chat` behavior.

Canonical local commands:

```text
cd golang
go test ./...
go test -race ./...
go vet ./...
go build -o /tmp/openclaw-go ./cmd/openclaw
```

Still required before cutover:

- Production-image/Compose first-run, backup/restore, image-layer secret audit,
  and failure/recovery acceptance beyond the disposable smoke.
- Credential-backed Telegram inbound/reply tests plus pairing and routing
  coverage.
- Credential-backed recurring delivery proof, including persistence and
  rescheduling after an actual successful send.
- Node-style scheduled agent execution for conditional watchers or explicit
  acceptance that the Go product supports static reminders only.
- Gateway request limits, timeouts, stable errors, and malformed/oversized-input
  reliability tests.
- Go/Node fixture parity for the retained channel, config, and state contracts.
- Node-to-Go migration and rollback proof.

## Deployment Baseline

As of 2026-07-24, the persistent live test deployment is:

```text
name:  openclaw-go-test-ubuntu
image: openclaw-go-ubuntu-test:telegram-reply-context-20260723
port:  0.0.0.0:18792 -> 18789/tcp
model: http://172.17.0.1:8080/v1
```

It mounts config at `/config`, state at `/data`, sets
`OPENCLAW_CONFIG_DIR=/config`, `OPENCLAW_DATA_DIR=/data`, and
`TZ=Asia/Kuala_Lumpur`, and uses restart policy `unless-stopped`. Container ids
are ephemeral. Before recreating it, inspect and preserve every mount,
environment value, published port, and restart policy. A restart alone does not
load a rebuilt image.

The container was recreated from the `telegram-reply-context-20260723` tag on
2026-07-24 using the existing `openclaw-agent.sqlite`. Startup resolved
`Asia/Kuala_Lumpur`; Gateway and local-model health passed before and after the
final restart; SQLite integrity passed; all five existing reminders and the
existing memory remained unchanged; and a unique live-model proof turn
persisted as structured inbound JSON plus assistant text. The final database
contained 241 transcript rows and one memory row. Credential-backed Telegram
reply ingress still requires an owner-generated reply. The database predating
the earlier server-timezone deployment remains retained locally as
`config_test/agent_data_go/openclaw-agent.sqlite.before-server-timezone-20260716-063656`.

## Migration Roadmap

### 1. Freeze the retained contract

- Retain a smaller unauthenticated HTTP v1 for trusted-LAN use; exact Node
  WebSocket compatibility and public/untrusted-network exposure are non-goals.
- Capture focused Node fixtures for config, provider calls, channel envelopes,
  pairing, memory, state, startup, health, and retained error behavior.
- Audit inherited Node docs/UI/setup and stop advertising unsupported features.

Exit: every retained behavior has an owner, fixture or explicit test, config
shape, and documented non-goals.

### 2. Stabilize the Gateway and scheduler

- Add request-size limits, explicit server timeouts/concurrency policy, stable
  errors, and malformed/oversized-input tests for local reliability. Preserve
  the intentional unauthenticated all-interface trusted-LAN contract.
- Add durable reminder claim/lease and delivery idempotency so concurrent or
  restarted workers cannot duplicate successful delivery.
- Decide whether first release includes scheduled agent jobs for conditional
  watchers/contacting another recipient or explicitly supports static reminders
  only.

Exit: Gateway behavior matches the documented trusted-LAN contract and has
reliability proof; reminder delivery has restart/concurrency proof; scheduled
behavior is accurately documented.

### 3. Complete retained channels

- Define canonical sender, DM/group, and account routing keys plus any
  owner-admission pairing/allowlist contract. Do not add internal tenant or
  per-user data isolation.
- Fix Telegram DM/group routing identities and implement the retained
  owner-admission pairing/allowlist contract.
- Add credential-backed live inbound, reply, recurring delivery, persistence,
  retry, and rescheduling proof for Telegram; extend focused adapter coverage
  alongside pairing and routing changes.
- Keep group/server behavior disabled or tightly allowlisted until explicitly
  retained. Defer media, reactions, threads, streaming, and multi-account
  routing unless a concrete requirement promotes them.

Exit: Telegram pairs safely and completes a model-backed DM reply and recurring
reminder delivery after container restart.

### 4. Finalize memory and state contracts

- Design and implement the retrieval-augmented replacement for full-history
  replay, including the recent-turn budget, retrieval corpus, ranking contract,
  structured tool-sequence handling, and failure behavior.
- Decide whether retrieval and memory use deterministic lexical search or local
  embeddings, and whether automatic recall and dreaming belong in the first
  release. No cloud dependency is allowed.
- Define one-way migration for retained Node config, memory,
  reminders, and conversation state. Define unsupported data explicitly.
- Add migration verification, rollback, backup/restore, corruption, and
  interrupted-migration tests. Runtime reads only the canonical Go SQLite shape.

Exit: retained user data has a documented tested migration and rollback path;
memory behavior matches the declared first-release contract across restart.

### 5. Productionize Docker and distribution

- Integrate the standalone Go image into the production Compose/root path with
  mounted non-secret config, separate secrets, persistent SQLite state, health,
  and minimal published ports.
- Decide whether SSH remains in the production image and whether npm/package
  distribution is retired in favor of Docker only.
- Prove clean-clone build, fresh-volume first run, restart, failure recovery,
  backup/restore, and image-layer secret absence. Define image name/tag policy
  and a minimal release checklist.

Exit: a new operator can build and run the documented image without Node or a
cloud provider, and no private instance state appears in the image.

### 6. Prove parity and cut over

- Run Go and Node side by side against the retained fixture corpus.
- Switch the root image only after every cutover gate below passes.
- Remove Node production runtime code, unused workspaces, dependencies, scripts,
  provider/channel registries, setup paths, tests for removed products, and
  generated metadata. Keep only reference fixtures and tooling still required.
- Regenerate the lockfile and retained metadata, then audit docs/UI so the fork
  exposes only its shipped surface.

Exit: production no longer requires the Node runtime, removed providers/plugins
cannot load, focused Go and Docker lanes pass, and operator documentation matches
the shipped product.

## Immediate Next Actions

1. Add Gateway request limits, timeouts, stable errors, and
   malformed/oversized-input reliability tests.
2. Define channel routing identity and the owner-admission boundary, then
   implement any retained pairing and correct DM/group session keys without
   adding internal tenant isolation.
3. Record live Telegram pairing/reply/reminder delivery proof.
4. Add durable reminder lease/idempotency behavior.
5. Define the first-release scheduled-job, RAG, and local-memory contracts.
6. Capture Node fixtures and implement migration, rollback, and backup/restore.
7. Integrate the Go image into Compose and run clean-volume/secret-layer proof.

## Open Decisions

- Static reminders only, or scheduled agent turns for watchers and delegated
  delivery?
- Which local RAG retrieval and context-budget contract replaces full-history
  replay: lexical search or local embeddings, and how much recent history?
- Substring memory only, or automatic recall and dreaming?
- CLI/API-only operation, a trimmed dashboard, or the current dashboard?
- Which browser, canvas, file-transfer, streaming, media, and reasoning features
  are genuinely required?
- Docker-only distribution, and should fork image/package names change?
- Keep SSH in the production image or provide it only in a debug/operator image?

## Cutover Gate

Go replaces Node only when:

- The retained contract is explicit and fixture-backed.
- A clean clone builds the Go binary and production Docker image.
- Local llama-server text and structured reminder/memory workflows pass in the
  standalone container after restart.
- Telegram passes access-control, pairing, inbound, and outbound live scenarios,
  including recurring reminder delivery.
- Existing retained config/state have a documented tested migration or are
  explicitly declared unsupported before the first release.
- Gateway exposure matches the documented unauthenticated trusted-LAN contract,
  is request-limited and reliability-tested, and remains unsupported on public
  or untrusted networks; durable reminder delivery is lease/idempotency safe.
- A clean image passes build, startup, restart, health, persistence, backup,
  and secret-audit acceptance without Node or cloud-provider dependencies.
- Operator documentation and visible UI advertise only retained functionality.
- The focused Go test lane, race detector, vet/build gates, fixture parity lane,
  and Docker smoke all pass.
- The TypeScript runtime is no longer required in the production image, and a
  rollback procedure has been exercised.

Before publishing, record the exact image tag, commit SHA, verification commands,
fresh-volume result, backup/restore result, and image-layer secret audit.
