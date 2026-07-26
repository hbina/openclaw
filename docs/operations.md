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

Before database maintenance, stop the container and make an online SQLite
backup:

```bash
sqlite3 /path/to/openclaw-agent.sqlite \
  ".backup '/path/to/openclaw-agent.sqlite.backup'"
sqlite3 /path/to/openclaw-agent.sqlite.backup "PRAGMA integrity_check;"
```

Keep the backup outside the image. Validate row counts for reminders, memory,
and history before and after a rebuild. `conversation_chunks` is derived:
recreate it empty and let the background indexer repopulate it.

For a deployment:

1. run Go test, race, vet, and explicit-output build gates;
2. verify both local model endpoints;
3. inspect and preserve mounts, environment, published ports, and restart
   policy;
4. back up and integrity-check SQLite;
5. build an immutable candidate tag and recreate the container;
6. prove health, a real `/chat` turn, reminders, memory, conversation recall,
   and restart persistence;
7. retain one known-good database backup.

The repository test deployment helper is `scripts/deploy-go-test.sh`. It is
specific to the local test container described in the repo-root `AGENTS.md`.
