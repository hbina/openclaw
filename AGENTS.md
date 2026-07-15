# Repository Guidelines

This branch is a slim, Docker-first OpenClaw fork migrating the retained reminder-assistant product from TypeScript/Node to Go. `MIGRATION.md` is the single source of truth for product goals, scope, implementation status, priorities, proof, and cutover gates. Read it before changing runtime behavior and update it whenever any of those facts change.

## Start Here

- Run `git status -sb` first. The migration is intentionally staged and may be dirty; never reset, restore, stash, delete, or overwrite unrelated work.
- Read the complete nearest scoped `AGENTS.md` before subtree work.
- Use repo-root references in reports, for example `go/internal/gateway/agent.go:220`; do not report absolute paths.
- For docs/user-visible work, run `pnpm docs:list` and read only the relevant docs.
- Diagnose from source, callers, tests, current behavior, and dependency contracts. Do not guess API behavior or declare parity from a diff alone.
- Never print, commit, copy into images, or expose credentials. `config_test/secrets.json` is local-only.

## Product Direction

The production target is one local personal assistant with:

- a standalone Go Gateway and agent loop;
- Telegram, WhatsApp, and Discord text channels;
- recurring and one-shot reminders;
- SQLite-backed memory, conversation history, compaction, `SOUL.md`, and `IDENTITY.md`;
- one OpenAI-compatible **local `llama-server`** provider;
- mounted non-secret config, separate secrets, and persistent SQLite state.

Do not add hosted OpenAI, Anthropic, ChatGPT, Claude API, Claude CLI, MCP subprocess, or cloud fallback paths to the Go runtime. The config name `models.providers.openai` is retained only as the OpenAI-compatible wire-protocol key for `llama-server`. The current deployment intentionally sends model id `default`.

Node remains the behavioral reference while migration is incomplete. Inspect its retained implementation and tests before parity changes, but do not expand the Go target to upstream OpenClaw’s full feature set.

## Current Go Status

Implemented under `go/`:

- local Chat Completions text and structured tool calls;
- atomic tool execution plus structured SQLite transcripts;
- `manage_reminders`: batch add, list, update, remove; `at`, anchored `every`, and timezone-aware five/six-field `cron` schedules;
- server-derived local-time prompt context, omitted-cron timezone defaults, and local next-fire display without an application-specific hardcoded timezone;
- recurring advancement after successful delivery and one-shot deletion;
- global memory store/search;
- SQLite-canonical `SOUL.md` and `IDENTITY.md`, first-run defaults, per-turn reload, and `manage_personality` updates;
- history limits, compaction, `/healthz`, `/chat`, and basic channel adapters;
- standalone Alpine image without Node, npm, Claude, OpenAI-hosted, or Anthropic runtime dependencies.

Important gaps:

- Gateway HTTP authentication, request limits, safe bind policy, and stable errors;
- channel pairing/allowlists, correct DM/group identity, media/threads/reactions, multi-account support, and live credential-backed delivery proof;
- WhatsApp QR/device setup;
- durable reminder claim/lease and delivery idempotency;
- Node-style scheduled agent jobs. Static reminders cannot silently watch a site, suppress unchanged results, or contact another person;
- semantic memory/automatic recall, Node-to-Go state migration, backup/restore, Compose/root-image cutover, and side-by-side parity fixtures.

Continue from `MIGRATION.md` “Immediate Next Actions” unless the user sets another priority. Prefer closing one gap completely with source, tests, Docker proof, and documentation over starting several partial paths.

## Project Structure

- `go/cmd/openclaw`: Go startup and dependency wiring.
- `go/internal/{config,providers,tools,state,channels,gateway}`: retained Go runtime.
- `src/`, `packages/`, `extensions/`: Node reference runtime, protocol, and plugins.
- `docs/`: upstream/source documentation.
- `MIGRATION.md`: fork goals, retained scope, current Go behavior, proof, roadmap, cutover gates, and next actions.
- `config_test/`: ignored local test config, secrets, and persisted Go SQLite state.

OpenClaw-owned runtime state belongs in SQLite, not new JSON/JSONL/TXT sidecars. The live Go database is `config_test/agent_data_go/openclaw-agent.sqlite`. Personality documents are rows in `personality_documents`; do not reintroduce mounted `SOUL.md` or `IDENTITY.md` files.

## Local Model Contract

The configured provider must be the local llama.cpp server, currently reachable from Docker at:

```text
http://172.17.0.1:8080/v1
```

Before provider/tool work, verify both the host server and container path. Use the live endpoint for user-visible behavior proof; mocked HTTP tests alone are insufficient. Keep tool schemas simple and strict because the deployed local Gemma model must call them reliably. For reminder requests, preserve the required-tool guard so the model cannot claim success without a committed tool result.

## Running Test Container

As of 2026-07-16, the persistent live test deployment is:

```text
name:  openclaw-go-test-ubuntu
image: openclaw-go-ubuntu-test:server-timezone
port:  0.0.0.0:18792 -> 18789/tcp
```

The observed container id is `261c47d0ee5f`, but ids and uptime are ephemeral; re-check with:

```bash
docker ps --filter name=openclaw-go-test-ubuntu
docker inspect openclaw-go-test-ubuntu
docker logs --tail 80 openclaw-go-test-ubuntu
curl -fsS http://127.0.0.1:18792/healthz
```

It mounts local config at `/config`, persistent state at `/data`, and uses `OPENCLAW_CONFIG_DIR=/config`, `OPENCLAW_DATA_DIR=/data`, `TZ=Asia/Kuala_Lumpur`, plus restart policy `unless-stopped`. Before recreating it, inspect and preserve every bind/volume, environment value, published port, and restart policy. `docker restart` does not load a rebuilt image.

Build a candidate with:

```bash
docker build -t openclaw-go-ubuntu-test:<tag> go
```

After rebuilding, recreate the named container with its existing mounts, then prove health, a real `/chat` request through the local model, the expected structured transcript, and SQLite state. Use a unique test sender and clean up test reminders so the 30-second delivery loop does not retain undeliverable `cli` jobs.

Useful inspection:

```bash
sqlite3 -header -column config_test/agent_data_go/openclaw-agent.sqlite \
  "SELECT name, length(content) FROM personality_documents ORDER BY name;"
```

Never display secret-file contents or bearer/channel tokens in logs or reports.

## Build and Test Commands

Run Go commands from `go/`. Use a writable cache when the default cache is read-only:

```bash
GOCACHE=/tmp/openclaw-go-cache go test ./...
GOCACHE=/tmp/openclaw-go-cache go vet ./...
GOCACHE=/tmp/openclaw-go-cache go test -race ./...
GOCACHE=/tmp/openclaw-go-cache go build -o /tmp/openclaw-go ./cmd/openclaw
```

Do not build without `-o`; the repository contains a tracked `go/openclaw` binary that must not be changed as a side effect. Run `gofmt -w` on touched Go files and `git diff --check` before handoff.

For Node reference changes, use repository commands only: `pnpm test <path>`, `pnpm check:changed --staged`, `pnpm build`, and oxfmt wrappers. Never run bare Vitest watch mode or introduce `tsc --noEmit`.

Tests use Go’s `testing` package and Node Vitest. Name Go tests `TestBehavior`; keep `*_test.go` colocated. Cover success, validation, ownership boundaries, persistence/reopen, and failure behavior. User-visible provider, reminder, personality, state, Docker, or channel changes require proportional live proof.

## Coding and Architecture Rules

- Go: `gofmt`, small packages, explicit errors with context, `context.Context` at I/O boundaries, strict JSON decoding, and no hidden global fallbacks.
- TypeScript: strict ESM, no `any`/`@ts-nocheck`, schemas at external boundaries, and oxfmt formatting.
- Prefer one canonical path. Delete stale cloud/provider branches rather than adding compatibility shims.
- Keep trusted channel/sender identity outside model-controlled tool arguments.
- Commit state mutation and its matching tool-result transcript atomically.
- Preserve deterministic prompt/tool ordering and exact tool-call ids.
- Inspect direct dependency source/docs/types before changing dependency-backed behavior. Pin new Go dependencies and run `go mod tidy`.
- Config/env additions require strong justification. Keep non-secrets in `openclaw.json`, credentials in `secrets.json`, and state outside the image.
- Personality is global to the single agent. Until access control lands, any chat caller may request `manage_personality`; document and test changes affecting this risk.

## Migration Workflow

For each migration slice:

1. Read `MIGRATION.md`, relevant Node owner/callers/tests/docs, and current Go siblings.
2. State the retained contract and known non-goals before coding.
3. Implement the canonical Go path; avoid a second fallback path.
4. Add focused unit/integration coverage and run Go test, vet, race, and build gates.
5. Build/recreate the Docker candidate and exercise the real local llama-server behavior.
6. Inspect SQLite/transcripts for proof; verify restart persistence when state changes.
7. Update `MIGRATION.md` with exact status, commands, container/image details, proof, priorities, and remaining gaps.
8. Review `git diff --numstat`, trim unnecessary production LOC, run `git diff --check`, and report all proof gaps.

Do not mark broad Node parity complete merely because the reminder use case works. Cutover requires every gate recorded in `MIGRATION.md`.

## Commits and Pull Requests

Use concise conventional-style subjects such as `feat(go): add reminder recurrence` or `fix(go): reload SQLite identity`. Commit only when asked, stage intended files only, and use `scripts/committer "<message>" <files...>`. Never reset or absorb unrelated staged work.

PRs should explain retained behavior, Node-reference differences, config/state impact, migration risk, and verification. Link issues when applicable. Include exact tests and Docker/live proof; attach screenshots only for UI changes. Do not edit `CHANGELOG.md` for ordinary work.

`CLAUDE.md` must remain a symlink to this file. Edit `AGENTS.md`, never the symlink.
