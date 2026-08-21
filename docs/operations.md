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

## Fresh-state cutover

The revisioned memory ledger changes the canonical schema. Use a new empty
data directory for this release. Existing Node and Go databases are not
migrated, imported, translated, or read by the new runtime. Keep the prior
directory untouched if it is needed as an operator archive; there is no
application-level compatibility or export command.

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
```

`memory search` uses the local chat model to plan and rerank hybrid FTS5/vector
results. `memory reindex` embeds every active memory and completed conversation
before replacing all derived rows in one transaction. A failure leaves the
previous derived index intact.

There are deliberately no memory import, export, JSON-output, background
repair, or filesystem-synchronization commands.

## Backup and inspection

Stop the container before database maintenance and make a unique SQLite
backup:

```bash
sqlite3 /path/to/openclaw-agent.sqlite \
  ".backup '/path/to/openclaw-agent.sqlite.backup'"
sqlite3 /path/to/openclaw-agent.sqlite.backup "PRAGMA integrity_check;"
```

Keep the backup outside the image. Memories, revisions, tasks, reminders,
transcripts, and traces are authoritative. FTS5 tables, memory vectors, and
conversation chunks are derived and rebuilt only with the explicit offline
`memory reindex` command.

Inspect recent traces locally:

```bash
./openclaw trace list --database /data/openclaw-agent.sqlite --limit 20
./openclaw trace show --database /data/openclaw-agent.sqlite --id 42
./openclaw trace show --database /data/openclaw-agent.sqlite --id 42 --json
```

## Deployment gate

1. Run test, race, vet, and build with the `sqlite_fts5` tag.
2. Verify both local model endpoints.
3. Build an immutable candidate image.
4. Point it at a fresh persistent data directory and preserved configuration.
5. Prove startup readiness, a real HTTP turn, a Telegram turn, proactive
   memory capture, hybrid recall, tasks, reminders, and restart persistence.
6. Verify that model or embedding failure prevents delivery and leaves a due
   reminder retryable.
7. Retain one known-good SQLite backup and image rollback target.

The repository deployment helper is `scripts/deploy-go-test.sh`.
