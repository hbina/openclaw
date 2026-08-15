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

## Response-provenance schema transition

Response tracing adds canonical tables without synthesizing historical traces.
Stop the runtime, create and integrity-check a unique backup, and rehearse the
transition on a copy before touching the live database. Apply the following as
one transaction to the rehearsal and then the stopped live database:

```sql
BEGIN IMMEDIATE;
CREATE TABLE response_traces (
  id INTEGER PRIMARY KEY AUTOINCREMENT, trigger_type TEXT NOT NULL,
  channel_id TEXT NOT NULL, sender_id TEXT NOT NULL,
  external_message_id TEXT NOT NULL DEFAULT '', reminder_id INTEGER,
  input_json TEXT NOT NULL, inbound_history_id INTEGER,
  status TEXT NOT NULL DEFAULT 'active', failure_stage TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '', started_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  completed_at DATETIME,
  FOREIGN KEY (inbound_history_id) REFERENCES conversation_history(id)
);
CREATE TABLE trace_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT, trace_id INTEGER NOT NULL,
  sequence_no INTEGER NOT NULL, kind TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending', error TEXT NOT NULL DEFAULT '',
  started_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, completed_at DATETIME,
  FOREIGN KEY (trace_id) REFERENCES response_traces(id), UNIQUE(trace_id, sequence_no)
);
CREATE TABLE rag_retrievals (
  event_id INTEGER PRIMARY KEY, outcome TEXT NOT NULL, embedding_query TEXT NOT NULL,
  embedding_model TEXT NOT NULL, dimensions INTEGER NOT NULL, index_version INTEGER NOT NULL,
  minimum_score REAL NOT NULL, history_highwater_id INTEGER NOT NULL,
  candidate_count INTEGER NOT NULL, excluded_count INTEGER NOT NULL,
  qualified_count INTEGER NOT NULL, selected_count INTEGER NOT NULL,
  rendered_archive TEXT NOT NULL,
  FOREIGN KEY (event_id) REFERENCES trace_events(id)
);
CREATE TABLE rag_matches (
  id INTEGER PRIMARY KEY AUTOINCREMENT, retrieval_event_id INTEGER NOT NULL,
  rank INTEGER NOT NULL, start_history_id INTEGER NOT NULL, end_history_id INTEGER NOT NULL,
  similarity_score REAL NOT NULL, content_hash TEXT NOT NULL, messages_json TEXT NOT NULL,
  FOREIGN KEY (retrieval_event_id) REFERENCES rag_retrievals(event_id),
  FOREIGN KEY (start_history_id) REFERENCES conversation_history(id),
  FOREIGN KEY (end_history_id) REFERENCES conversation_history(id),
  UNIQUE(retrieval_event_id, rank)
);
CREATE TABLE llm_calls (
  event_id INTEGER PRIMARY KEY, round_number INTEGER NOT NULL, purpose TEXT NOT NULL,
  request_json TEXT NOT NULL, response_json TEXT NOT NULL DEFAULT '',
  http_status INTEGER NOT NULL DEFAULT 0, finish_reason TEXT NOT NULL DEFAULT '',
  FOREIGN KEY (event_id) REFERENCES trace_events(id)
);
CREATE TABLE tool_executions (
  event_id INTEGER PRIMARY KEY, llm_event_id INTEGER NOT NULL,
  tool_call_id TEXT NOT NULL, name TEXT NOT NULL, arguments_json TEXT NOT NULL,
  result_json TEXT NOT NULL DEFAULT '', is_error INTEGER NOT NULL DEFAULT 0,
  mutation_committed INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY (event_id) REFERENCES trace_events(id),
  FOREIGN KEY (llm_event_id) REFERENCES llm_calls(event_id)
);
CREATE TABLE response_outputs (
  event_id INTEGER PRIMARY KEY, source_type TEXT NOT NULL,
  source_llm_event_id INTEGER, source_content TEXT NOT NULL,
  transformations_json TEXT NOT NULL, final_content TEXT NOT NULL,
  FOREIGN KEY (event_id) REFERENCES trace_events(id),
  FOREIGN KEY (source_llm_event_id) REFERENCES llm_calls(event_id)
);
CREATE TABLE delivery_attempts (
  event_id INTEGER PRIMARY KEY, output_event_id INTEGER NOT NULL,
  channel_id TEXT NOT NULL, recipient_id TEXT NOT NULL, content TEXT NOT NULL,
  provider_message_id TEXT NOT NULL DEFAULT '', conversation_history_id INTEGER,
  attempted_at DATETIME, accepted_at DATETIME,
  FOREIGN KEY (event_id) REFERENCES trace_events(id),
  FOREIGN KEY (output_event_id) REFERENCES response_outputs(event_id),
  FOREIGN KEY (conversation_history_id) REFERENCES conversation_history(id)
);
CREATE INDEX idx_response_traces_time ON response_traces(started_at DESC, id DESC);
CREATE INDEX idx_response_traces_route ON response_traces(channel_id, sender_id, started_at DESC);
CREATE INDEX idx_response_traces_external_message ON response_traces(external_message_id)
  WHERE external_message_id <> '';
CREATE INDEX idx_trace_events_trace ON trace_events(trace_id, sequence_no);
CREATE INDEX idx_delivery_provider_message ON delivery_attempts(provider_message_id)
  WHERE provider_message_id <> '';
COMMIT;
PRAGMA integrity_check;
```

Prove the candidate accepts the rehearsal database with `--check-state`, then
compare all pre-existing table row counts before and after the live transition.
The new provenance tables must start empty. Retain the pre-transition backup
and old image until a traced HTTP turn, traced Telegram turn, restart, and
read-only inspection all pass.

Inspect recent traces without stopping the running service:

```bash
./openclaw trace list --database /data/openclaw-agent.sqlite --limit 20
./openclaw trace show --database /data/openclaw-agent.sqlite --id 42
./openclaw trace show --database /data/openclaw-agent.sqlite --id 42 --json
```

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
