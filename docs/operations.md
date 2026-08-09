---
title: Operations
summary: Health, backup, deployment, and recovery
---

Health checks:

```bash
curl -fsS http://127.0.0.1:18789/healthz
curl -fsS http://127.0.0.1:8080/health
curl -fsS http://127.0.0.1:8081/health
```

Before database maintenance, stop the container and make a unique SQLite
backup:

```bash
sqlite3 /path/to/openclaw-agent.sqlite \
  ".backup '/path/to/openclaw-agent.sqlite.backup'"
sqlite3 /path/to/openclaw-agent.sqlite.backup "PRAGMA integrity_check;"
```

Keep the backup outside the image. Validate row counts for reminders, memory,
tasks, and history before and after a rebuild. `conversation_chunks` is derived:
recreate it empty and let the background indexer repopulate it.

## Task-ledger schema transition

The task ledger changes the canonical table set. The runtime does not migrate
it at startup, and a pre-task binary rejects the transitioned database. Use an
offline, backup-first transition and retain the verified backup.

First stop the running container. Create a uniquely named backup, run
`PRAGMA integrity_check`, copy that backup to a rehearsal database, and apply
this exact transaction to the rehearsal copy:

```sql
BEGIN IMMEDIATE;
CREATE TABLE tasks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    description TEXT NOT NULL,
    started_at DATETIME NOT NULL,
    completed_at DATETIME
);
CREATE UNIQUE INDEX idx_tasks_open_description
    ON tasks(lower(trim(description)))
    WHERE completed_at IS NULL;
COMMIT;
PRAGMA integrity_check;
```

The table deliberately starts empty. Do not infer rows from conversation text
or seed examples. Prove the candidate image accepts the rehearsal copy before
changing the live database:

```bash
docker run --rm -v /path/to/rehearsal-dir:/data \
  openclaw-go:candidate \
  ./openclaw --check-state /data/openclaw-agent.rehearsal.sqlite
```

After the candidate check passes, apply the same transaction to the stopped
live database, integrity-check it, start the candidate with the preserved
mounts and settings, and compare row counts. Keep both the old image/container
and the pre-transition backup until health, task/reminder behavior, and restart
persistence pass.

If candidate deployment fails, stop and remove the candidate, restore the
pre-transition backup while no OpenClaw process is running, integrity-check the
restored database, and only then restart the old image. For example:

```bash
sqlite3 /path/to/openclaw-agent.sqlite \
  ".restore '/path/to/openclaw-agent.sqlite.before-UNIQUE-ID'"
sqlite3 /path/to/openclaw-agent.sqlite "PRAGMA integrity_check;"
```

Restarting the old image before restoration is not a valid rollback: it rejects
the added canonical table and remains unavailable.

For a deployment:

1. run Go test, race, vet, and explicit-output build gates;
2. verify both local model endpoints;
3. inspect and preserve mounts, environment, published ports, and restart
   policy;
4. build an immutable candidate tag;
5. stop the container, then back up and integrity-check SQLite;
6. rehearse any canonical schema transition on a backup and prove the candidate
   opens it;
7. transition the live database and recreate the container;
8. prove health, a real `/chat` turn, tasks, reminders, memory, conversation
   recall, and restart persistence;
9. retain one known-good database backup.

The repository test deployment helper is `scripts/deploy-go-test.sh`. It is
specific to the local test container described in the repo-root `AGENTS.md`.
