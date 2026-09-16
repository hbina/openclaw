---
title: Operations
summary: Health, memory maintenance, backup, deployment, and recovery
---

The chat server, embedding server, and SQLite are required services. Startup
probes both model servers and verifies that every active memory and completed
conversation has a current derived index. The Gateway does not start in a
degraded mode.

```bash
curl -fsS http://127.0.0.1:18789/healthz
curl -fsS http://127.0.0.1:8080/health
curl -fsS http://127.0.0.1:8081/health
```

## Terminal chat

Use the running Gateway's canonical chat path without sending a Telegram
message:

```bash
./openclaw chat "List my open tasks"
printf 'What do you remember about my preferences?\n' | ./openclaw chat
./openclaw chat --sender-id debugging --json "List my reminders"
```

The command connects to `http://127.0.0.1:18789` by default. Set
`OPENCLAW_GATEWAY_URL` or pass `--url` before the message when the Gateway uses
a different address. `--timeout` defaults to `10m`.

Human-readable output writes the reply to stdout and the response trace ID to
stderr. Pass `--json` to emit both fields as one JSON object on stdout, then use
the trace ID with `openclaw trace show` when diagnosing a turn.

All terminal turns use the `cli` channel. The default sender ID is `cli-user`,
so separate invocations continue that CLI conversation; `--sender-id` selects a
different CLI conversation key and never impersonates Telegram history. The
command requires a running Gateway and does not open SQLite or start a second
agent. The Rust Gateway enforces loopback binding, bounded requests and
connections, and stable errors, but it has no HTTP authentication; keep it
local.

## Fresh-state cutover

Every Rust test cutover uses a new empty database. Existing Node, Go, and older
Rust databases are never migrated, imported, translated, or read. If old test
state has diagnostic value, stop the service and copy it to a uniquely named
backup before removing the active database. The backup is a forensic artifact
only; rollback restores the matching old binary and database together.

## Memory commands

Stop the Gateway before maintenance mutations or reindexing. Commands are
human-readable only:

```bash
./openclaw memory status --database /data/openclaw-agent.sqlite
./openclaw memory list --database /data/openclaw-agent.sqlite --status active
./openclaw memory get --database /data/openclaw-agent.sqlite --id 42
./openclaw memory add --database /data/openclaw-agent.sqlite --config-dir /config \
  --kind profile --content "Owner prefers concise answers."
./openclaw memory update --database /data/openclaw-agent.sqlite --config-dir /config \
  --id 42 --content "Owner prefers concise answers with examples."
./openclaw memory remove --database /data/openclaw-agent.sqlite --id 42
./openclaw memory search --database /data/openclaw-agent.sqlite --config-dir /config \
  --query "How should I format answers?" --max-results 5
./openclaw memory reindex --database /data/openclaw-agent.sqlite --config-dir /config
./openclaw memory maintenance status --database /data/openclaw-agent.sqlite
./openclaw memory maintenance preview --database /data/openclaw-agent.sqlite --config-dir /config
./openclaw memory maintenance run --database /data/openclaw-agent.sqlite --config-dir /config
./openclaw memory maintenance candidates --database /data/openclaw-agent.sqlite --run-id 7
```

`memory search` uses the local chat model to plan and rerank hybrid FTS5/vector
results. It remains a strict standalone maintenance command and does not use
the chat/reminder planner fallback. `memory reindex` embeds every active memory
and completed conversation before replacing all derived rows in one
transaction. A failure leaves the previous derived index intact.

`memory maintenance preview` runs extraction, deterministic gates, hybrid
comparison, and consolidation without changing memory or advancing the durable
checkpoint. `memory maintenance run` applies accepted adds and updates and
advances only past a fully terminal bounded batch. `status` and `candidates`
expose the durable schedule, lease, checkpoint, run failure, evidence, scores,
and decision state. The scheduled worker and manual commands share one SQLite
lease, so an overlapping invocation fails instead of duplicating work.

There are deliberately no memory import, export, JSON-output, background
index repair, or filesystem-synchronization commands. Background consolidation
is not index repair: every accepted mutation writes its revision and complete
derived index atomically.

## Backup and inspection

Stop the service before database maintenance and make a unique SQLite
backup:

```bash
sqlite3 /path/to/openclaw-agent.sqlite \
  ".backup '/path/to/openclaw-agent.sqlite.backup'"
sqlite3 /path/to/openclaw-agent.sqlite.backup "PRAGMA integrity_check;"
```

Keep the backup outside the image. Memories, revisions, maintenance runs and
checkpoints, tasks, reminders, transcripts, and traces are authoritative. FTS5
tables, memory vectors, and conversation chunks are derived and rebuilt only
with the explicit offline `memory reindex` command.

Inspect recent traces locally:

```bash
./openclaw trace list --database /data/openclaw-agent.sqlite --limit 20
./openclaw trace show --database /data/openclaw-agent.sqlite --id 42
./openclaw trace show --database /data/openclaw-agent.sqlite --id 42 --json
```

## Deployment gate

1. Run `cargo fmt --all -- --check`, strict Clippy, the full test suite, ignored
   local-Gemma tests, and a locked release build.
2. Record both local model identities and context/dimension contracts.
3. Stop the Rust service and copy the active SQLite file to a unique backup.
4. Remove only the explicitly resolved active test database and start the new
   release so it creates the canonical schema from scratch.
5. Prove loopback health and one real HTTP response, then restart and prove
   health again.
6. With exactly one long-poll consumer, send a private owner DM and verify the
   inbound row, route, trace, response delivery, synchronous index, restart
   persistence, and duplicate no-reexecution invariants. Unsupported sender and
   chat shapes are additionally verified by ingress fixtures because the Bot
   API cannot impersonate another account.
7. Run `PRAGMA integrity_check`, create a SQLite backup of the new state, and
   rehearse opening that backup with the same release.

The final standalone Rust image and Compose definition are not yet shipped. Do
not use the retained Go deployment scripts as a cutover or rollback path.
