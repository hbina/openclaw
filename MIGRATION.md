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
restart persistence are proven. SQLite-backed SOUL/IDENTITY defaults, updates,
and per-turn prompt injection are implemented. Channel parity, scheduled agent
jobs, security, state migration, and production cutover remain incomplete.

## Target

The Go runtime retains:

- One primary assistant profile.
- One local OpenAI-compatible `llama-server` endpoint.
- Telegram, WhatsApp, and Discord text channels.
- Batch reminder creation, listing, editing, cancellation, recurring schedules,
  and delivery.
- Global durable memory storage and search.
- Persistent structured conversation history and compaction.
- A small authenticated Gateway surface.
- Canonical non-secret config plus a separate optional credential file.
- SQLite state outside the container image.
- A standalone Go image with no Node or cloud-model runtime dependency.

Streaming, media input, embeddings, reasoning fields, cloud-provider fallback,
and full upstream Gateway parity are not first-cut requirements.

## Product and Migration Principles

- Docker is the canonical deployment path. Non-secret behavior lives in one
  mounted `openclaw.json`; credentials live in a separate mounted secret file;
  SQLite state lives outside the image.
- The Go runtime uses one local OpenAI-compatible `llama-server`. Do not add
  hosted OpenAI, Anthropic, Claude API/CLI, ChatGPT, MCP subprocess, or cloud
  fallback paths. `models.providers.openai` is only the compatibility wire key.
- Keep one primary agent and only Telegram, WhatsApp, and Discord text channels.
- SQLite is canonical for reminders, history, compaction, memory, personality,
  and other OpenClaw-owned runtime state. Do not add state sidecars or runtime
  fallback readers.
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
| Go Gateway and agent        | Keep         | One primary assistant, authenticated operator/chat surface, health endpoint                              |
| Local model                 | Keep         | OpenAI-compatible `llama-server`, required local base URL, optional LAN bearer token, model id `default` |
| Reminders                   | Keep         | Atomic batch CRUD, `at`/`every`/timezone-aware `cron`, durable delivery                                  |
| Memory and personality      | Keep         | SQLite recall/search, history/compaction, canonical `SOUL.md` and `IDENTITY.md` rows                     |
| Telegram                    | Keep         | Pairing/allowlist, inbound/outbound DM text, reminder delivery                                           |
| WhatsApp                    | Keep         | QR/device setup, pairing/allowlist, inbound/outbound text, reminder delivery                             |
| Discord                     | Keep         | Required intents, pairing/allowlist, conservative DM text, reminder delivery                             |
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
- Basic Go channel adapters exist, but pairing, access control, WhatsApp setup,
  richer channel semantics, and credential-backed live proof are incomplete.
- Local text, structured tools, reminders, memory, personality, transcripts,
  SQLite persistence, and restart behavior are implemented and tested as
  detailed below.
- Go release/distribution, Node-state migration, rollback, and production
  acceptance have not been completed.

## Package Map

- `go/cmd/openclaw`: Startup and dependency wiring.
- `go/internal/config`: Slim config and secret decoding.
- `go/internal/providers`: OpenAI-compatible structured chat contract and local
  HTTP client.
- `go/internal/tools`: Trusted in-process reminder and memory tool execution.
- `go/internal/state`: SQLite reminders, history, compaction, personality, and
  memory state.
- `go/internal/channels`: Telegram, Discord, and WhatsApp adapters.
- `go/internal/gateway`: Agent loop, HTTP server, reminder delivery, and
  compaction.

## Current Implementation

### Startup and local model

Implemented:

- `OPENCLAW_DATA_DIR` and `OPENCLAW_CONFIG_DIR` select mounted state and config.
- `models.providers.openai.baseUrl` is retained as the compatibility key for a
  local OpenAI-compatible endpoint.
- The base URL is mandatory; there is no public OpenAI endpoint default.
- The optional `openai.apiKey` secret becomes a bearer token only when present.
- Startup rejects provider prefixes other than `openai` instead of falling back
  to a cloud provider or subprocess.
- Generation intentionally sends model id `default`, matching the verified
  llama-server deployment.
- Startup resolves Go's server-local timezone once from the host/container
  environment and passes that same location to the prompt and reminder tools.
  No application timezone is hardcoded or added to config.
- Anthropic, Claude CLI, MCP-server mode, Node.js, and npm have been removed
  from the Go runtime and image.

Limitations:

- Config decoding validates JSON syntax but not all semantic constraints.
- Missing config files still initially decode through the prototype's empty
  defaults before local-provider construction fails with an actionable error.
- Plugin config is parsed but does not activate a Go plugin system.
- Streaming, usage accounting, retries, capability negotiation, media, and
  structured output beyond tool calls are not implemented.

### Structured agent and tools

Implemented:

- The provider contract carries system, user, assistant, and tool messages;
  assistant tool calls; exact call ids; JSON arguments; tool definitions; and
  finish reasons.
- Normal Chat Completions requests send four function tools with sequential
  execution. Reminder-intent turns expose only `manage_reminders` and send
  `tool_choice: required` until a reminder operation succeeds.
- The agent supports up to four tool rounds and returns a deterministic safety
  response if the model does not terminate the workflow.
- Tool calls and results persist as structured SQLite transcript rows and are
  reconstructed as native assistant/tool messages on later turns.
- Mutating tool state and the matching result row commit in one transaction.
- Invalid arguments, unknown tools, and execution failures return structured
  error results so the model can correct its request.
- Channel and sender identifiers come from trusted agent context and are absent
  from model-visible schemas.
- `SOUL.md` and `IDENTITY.md` are canonical SQLite documents. A new database
  receives useful defaults with the name OpenClaw; startup uses insert-if-missing
  semantics so user edits are never reset. Both documents reload on every turn.

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
- `manage_personality(action, ...)` views or explicitly replaces `SOUL.md` or
  `IDENTITY.md`. Updates and their tool-result transcript commit atomically.
- `store_memory(content)` stores a global durable fact and deduplicates exact
  repeats.
- `search_memory(query)` returns up to five global substring matches.

Reminder mutations and their matching tool-result transcript commit together.
Batch validation is atomic, so one invalid item rolls back the whole add. The
prompt permits model-initiated storage of stable preferences and durable facts,
excludes credentials and transient details, and explicitly distinguishes static
reminder delivery from unsupported scheduled agent work.

Limitations:

- Global memory is intentionally shared by every user reaching this agent.
- Memory search is SQL substring matching, not semantic/vector retrieval.
- There is no deterministic pre-generation memory injection; the model must
  choose `search_memory`.
- Reminder polling has no durable claim/lease or delivery-idempotency protocol.
- Static Go reminders cannot yet execute Node-style isolated agent turns. A
  watcher that stays silent unless data changes and a task that contacts another
  recipient are not implemented by storing those words as a reminder message.
- Existing reminders with an explicit timezone are not silently rewritten when
  the server timezone changes. Vague phrases such as "tonight" still require an
  exact time decision or clarification; this change fixes the timezone basis,
  not the missing-hour ambiguity.
- Personality is global to the single agent. Until Gateway/channel access
  control is implemented, any caller that can reach chat can request a global
  `manage_personality` update; deployments must restrict access accordingly.
- The fixed history-row limit can retain fewer natural-language turns when a
  conversation contains many tool rows.
- Context size still uses a fixed 100,000-token assumption and a four-character
  estimate rather than tokenizer/model metadata.

### Gateway and channels

Implemented:

- `GET /healthz` and unauthenticated `POST /chat` over HTTP.
- A 30-second reminder loop delivers due reminders through the matching channel.
  It deletes a successfully delivered one-shot and advances a successfully
  delivered recurring reminder to its next anchored/cron occurrence. Failed
  sends remain due for retry.
- Basic Telegram, Discord, and WhatsApp inbound/outbound text adapters.

Limitations:

- The Node WebSocket Gateway protocol is not implemented.
- HTTP has no authentication, request-size policy, stable public error schema,
  or safe bind policy and currently listens on all interfaces.
- Pairing, allowlists, group policy, mentions, media, threads, reactions,
  commands, streaming updates, and multi-account routing are absent.
- WhatsApp requires an existing device session; QR/device setup is absent.
- Adapter channel ids still do not match the `-dm` convention used by the Go
  DM-history resolver.
- No current credential-backed channel proof is recorded.

### State and compatibility

Implemented:

- SQLite stores reminders, including canonical schedule kind/definition,
  timezone, enabled state, and next-fire timestamp, plus global memory,
  personality documents, conversation rows, compaction records, and generic
  agent state. Existing Go one-shot rows migrate to `schedule_kind = at`.
- Personality documents live only in `personality_documents`; the Go runtime
  does not depend on mounted workspace Markdown files. Default insertion is
  idempotent and custom identity content survives store/container restart.
- Compaction summaries preserve readable tool activity and retained history is
  aligned to a user-turn boundary.
- The Go image is now a Go binary plus Alpine CA certificates and SQLite runtime
  libraries; it no longer installs Node/npm/Claude.

Limitations:

- Go state does not match Node session, transcript, checkpoint, branching,
  locking, usage, or metadata contracts.
- No Node-to-Go migration, rollback, production first-run acceptance,
  backup/restore, or secret-layer audit is implemented.
- The root `Dockerfile` still builds Node; production has not cut over to
  `go/Dockerfile`.

## Verification

Automated Go coverage includes:

- Local HTTP request/response wire shape, optional authorization, tool calls,
  call ids, malformed responses, empty choices, and HTTP failures.
- Tool schemas, strict arguments, trusted identity, scoped mutation, atomic
  batch rollback, add/list/update/remove, at/every/cron validation and next-run
  calculation, server-timezone defaulting and display, recurring post-delivery
  advancement, global memory, exact deduplication, and structured result
  persistence.
- Default personality seeding, stable SOUL-before-IDENTITY ordering, validated
  updates, restart persistence, and per-turn system-prompt refresh.
- Multi-round agent execution, validation-error recovery, tool-call/result
  replay, the four-round limit, prompt/tool identity separation, history,
  compaction decisions, Gateway health, and SQLite state.

Live standalone-container proof includes:

- Building `openclaw-go-local-tools:test` from `go/Dockerfile`.
- Health and text replies through the current local Gemma llama-server.
- Global memory storage and recall from a different sender.
- Relative reminder add/list/delete and absolute RFC3339 scheduling.
- Container restart followed by successful memory recall and reminder listing.
- SQLite inspection showing paired structured tool-call/result rows, one durable
  memory, and zero pending reminders after cleanup.
- Runtime inspection showing no `node`, `npm`, or `claude` executable.

Live recurring-reminder proof on 2026-07-15 includes:

- Rebuilding and restarting `openclaw-go-test-ubuntu` from `go/Dockerfile` while
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
- The personality build inserted `SOUL.md` and `IDENTITY.md` into the existing
  SQLite state. Before restart the local model identified itself as OpenClaw and
  described the seeded voice; after container restart it again answered from the
  same persisted identity. No workspace Markdown mount was present.
- A natural-language identity-change request produced a structured
  `manage_personality` update, SQLite contained the replacement Markdown, and a
  subsequent restart preserved the name, vibe, and emoji.

Server-timezone proof on 2026-07-16 includes:

- Building `openclaw-go-ubuntu-test:server-timezone` from `go/Dockerfile` and
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

Canonical local commands:

```text
cd go
go test ./...
go build ./cmd/openclaw
```

Still required before cutover:

- Production-image/Compose first-run, backup/restore, image-layer secret audit,
  and failure/recovery acceptance beyond the disposable smoke.
- Telegram, Discord, and WhatsApp adapter unit and live tests.
- Credential-backed recurring delivery proof, including persistence and
  rescheduling after an actual successful send.
- Node-style scheduled agent execution for conditional watchers or explicit
  acceptance that the Go product supports static reminders only.
- Gateway authentication and hostile-input tests.
- Go/Node fixture parity for the retained channel, config, and state contracts.
- Node-to-Go migration and rollback proof.

## Deployment Baseline

As of 2026-07-16, the persistent live test deployment is:

```text
name:  openclaw-go-test-ubuntu
image: openclaw-go-ubuntu-test:server-timezone
port:  0.0.0.0:18792 -> 18789/tcp
model: http://172.17.0.1:8080/v1
```

It mounts config at `/config`, state at `/data`, sets
`OPENCLAW_CONFIG_DIR=/config`, `OPENCLAW_DATA_DIR=/data`, and
`TZ=Asia/Kuala_Lumpur`, and uses restart policy `unless-stopped`. Container ids
are ephemeral. Before recreating it, inspect and preserve every mount,
environment value, published port, and restart policy. A restart alone does not
load a rebuilt image.

The container was recreated from the server-timezone image on 2026-07-16 with
a fresh `openclaw-agent.sqlite`. Startup resolved
`Asia/Kuala_Lumpur` from the container environment; health passed; reminders,
conversation history, and memories were empty; and first-run initialization
seeded only the two SQLite personality documents. The previous database is
retained locally as
`config_test/agent_data_go/openclaw-agent.sqlite.before-server-timezone-20260716-063656`.

## Migration Roadmap

### 1. Freeze the retained contract

- Decide whether the public Gateway is a compatible Node WebSocket subset or a
  smaller authenticated HTTP v1.
- Capture focused Node fixtures for config, provider calls, channel envelopes,
  pairing, memory, state, startup, health, and retained error behavior.
- Audit inherited Node docs/UI/setup and stop advertising unsupported features.

Exit: every retained behavior has an owner, fixture or explicit test, config
shape, and documented non-goals.

### 2. Harden the Gateway and scheduler

- Add authentication, request-size limits, safe bind defaults, stable errors,
  hostile-input tests, and an explicit trusted-transport policy.
- Add durable reminder claim/lease and delivery idempotency so concurrent or
  restarted workers cannot duplicate successful delivery.
- Decide whether first release includes scheduled agent jobs for conditional
  watchers/contacting another recipient or explicitly supports static reminders
  only.

Exit: exposed endpoints are reviewed and authenticated; reminder delivery has
restart/concurrency proof; scheduled behavior is accurately documented.

### 3. Complete retained channels

- Define canonical sender, DM/group, account, pairing, and allowlist contracts.
- Fix adapter channel identities and implement pairing for Telegram, WhatsApp,
  and Discord; add WhatsApp QR/device setup and required Discord intents.
- Add focused adapter tests and credential-backed live inbound, reply, recurring
  delivery, persistence, retry, and rescheduling proof for all three channels.
- Keep group/server behavior disabled or tightly allowlisted until explicitly
  retained. Defer media, reactions, threads, streaming, and multi-account
  routing unless a concrete requirement promotes them.

Exit: each retained channel pairs safely and completes a model-backed DM reply
and recurring reminder delivery after container restart.

### 4. Finalize memory and state contracts

- Decide whether substring search is sufficient or add local embeddings; decide
  whether deterministic automatic recall and dreaming belong in the first
  release. No cloud dependency is allowed.
- Define one-way migration for retained Node config, personality, memory,
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

1. Define the authenticated Gateway protocol and bind policy.
2. Add request limits, stable errors, and hostile-input tests.
3. Define channel identity/access control, then implement pairing and correct
   DM/group session keys.
4. Implement WhatsApp QR/device setup and record live pairing/reply/reminder
   delivery proof for all three channels.
5. Add durable reminder lease/idempotency behavior.
6. Decide the first-release scheduled-job and local-memory contracts.
7. Capture Node fixtures and implement migration, rollback, and backup/restore.
8. Integrate the Go image into Compose and run clean-volume/secret-layer proof.

## Open Decisions

- Smaller authenticated HTTP v1 or retained Node Gateway protocol subset?
- Static reminders only, or scheduled agent turns for watchers and delegated
  delivery?
- Substring memory only, or local embeddings, automatic recall, and dreaming?
- CLI/API-only operation, a trimmed dashboard, or the current dashboard?
- Existing WhatsApp QR/session runtime or another local integration path?
- Which Discord intents and group/server behavior are enabled by default?
- Which browser, canvas, file-transfer, streaming, media, and reasoning features
  are genuinely required?
- Loopback-only Docker ports by default or opt-in LAN exposure?
- Docker-only distribution, and should fork image/package names change?
- Keep SSH in the production image or provide it only in a debug/operator image?

## Cutover Gate

Go replaces Node only when:

- The retained contract is explicit and fixture-backed.
- A clean clone builds the Go binary and production Docker image.
- Local llama-server text and structured reminder/memory workflows pass in the
  standalone container after restart.
- Telegram, WhatsApp, and Discord pass access-control, pairing, inbound, and
  outbound live scenarios, including recurring reminder delivery.
- Existing retained config/state have a documented tested migration or are
  explicitly declared unsupported before the first release.
- Gateway exposure is authenticated, safely bound, request-limited, and
  reviewed; durable reminder delivery is lease/idempotency safe.
- A clean image passes build, startup, restart, health, persistence, backup,
  and secret-audit acceptance without Node or cloud-provider dependencies.
- Operator documentation and visible UI advertise only retained functionality.
- The focused Go test lane, race detector, vet/build gates, fixture parity lane,
  and Docker smoke all pass.
- The TypeScript runtime is no longer required in the production image, and a
  rollback procedure has been exercised.

Before publishing, record the exact image tag, commit SHA, verification commands,
fresh-volume result, backup/restore result, and image-layer secret audit.
