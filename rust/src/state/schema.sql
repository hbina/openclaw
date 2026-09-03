    CREATE TABLE IF NOT EXISTS memories (
        id                  INTEGER PRIMARY KEY AUTOINCREMENT,
        kind                TEXT NOT NULL CHECK (kind IN ('profile', 'durable', 'daily')),
        status              TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'deleted')),
        current_revision_id INTEGER,
        current_content_hash TEXT,
        observed_at         DATETIME NOT NULL,
        created_at          DATETIME NOT NULL,
        updated_at          DATETIME NOT NULL,
        deleted_at          DATETIME,
        FOREIGN KEY (current_revision_id) REFERENCES memory_revisions(id)
    );

    CREATE TABLE IF NOT EXISTS memory_revisions (
        id                INTEGER PRIMARY KEY AUTOINCREMENT,
        memory_id         INTEGER NOT NULL,
        revision_number   INTEGER NOT NULL,
        content           TEXT NOT NULL,
        content_hash      TEXT NOT NULL,
        origin_class      TEXT NOT NULL CHECK (origin_class IN ('owner', 'agent', 'system', 'untrusted')),
        source_kind       TEXT NOT NULL CHECK (source_kind IN ('chat', 'operator', 'maintenance')),
        source_history_id INTEGER,
        source_trace_id   INTEGER,
        created_at        DATETIME NOT NULL,
        FOREIGN KEY (memory_id) REFERENCES memories(id),
        FOREIGN KEY (source_history_id) REFERENCES conversation_history(id),
        FOREIGN KEY (source_trace_id) REFERENCES response_traces(id),
        UNIQUE (memory_id, revision_number)
    );

    CREATE TABLE IF NOT EXISTS memory_embeddings (
        memory_id      INTEGER NOT NULL,
        revision_id    INTEGER NOT NULL,
        embedding_model TEXT NOT NULL,
        dimensions     INTEGER NOT NULL,
        embedding      BLOB NOT NULL,
        created_at     DATETIME NOT NULL,
        PRIMARY KEY (memory_id, revision_id, embedding_model),
        FOREIGN KEY (memory_id) REFERENCES memories(id),
        FOREIGN KEY (revision_id) REFERENCES memory_revisions(id)
    );

    CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
        content,
        memory_id UNINDEXED,
        revision_id UNINDEXED,
        tokenize = 'unicode61'
    );

    CREATE TABLE IF NOT EXISTS reminders (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        channel_id TEXT NOT NULL,
        sender_id TEXT NOT NULL,
        message TEXT NOT NULL,
        fire_at DATETIME NOT NULL,
        schedule_kind TEXT NOT NULL DEFAULT 'at',
        every_ms INTEGER NOT NULL DEFAULT 0,
        anchor_at DATETIME,
        cron_expr TEXT NOT NULL DEFAULT '',
        timezone TEXT NOT NULL DEFAULT '',
        enabled INTEGER NOT NULL DEFAULT 1
    );

    CREATE TABLE IF NOT EXISTS tasks (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        description TEXT NOT NULL,
        started_at DATETIME NOT NULL,
        completed_at DATETIME
    );

    CREATE TABLE IF NOT EXISTS conversation_history (
        id          INTEGER PRIMARY KEY AUTOINCREMENT,
        channel_id  TEXT    NOT NULL,
        sender_id   TEXT    NOT NULL,
        role        TEXT    NOT NULL,
        content_type TEXT   NOT NULL DEFAULT 'text',
        audience    TEXT    NOT NULL DEFAULT 'conversation' CHECK (audience IN ('conversation', 'internal')),
        content     TEXT    NOT NULL,
        created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
    );

    CREATE INDEX IF NOT EXISTS idx_conversation_history_lookup
        ON conversation_history(channel_id, sender_id, id);

    CREATE TABLE IF NOT EXISTS memory_maintenance_state (
        singleton_id          INTEGER PRIMARY KEY CHECK (singleton_id = 1),
        checkpoint_history_id INTEGER NOT NULL DEFAULT 0,
        lease_owner           TEXT NOT NULL DEFAULT '',
        lease_expires_at      DATETIME,
        next_run_at           DATETIME,
        last_success_at       DATETIME,
        updated_at            DATETIME NOT NULL
    );

    INSERT OR IGNORE INTO memory_maintenance_state
        (singleton_id, checkpoint_history_id, lease_owner, updated_at)
        VALUES (1, 0, '', CURRENT_TIMESTAMP);

    CREATE TABLE IF NOT EXISTS memory_maintenance_runs (
        id                    INTEGER PRIMARY KEY AUTOINCREMENT,
        mode                  TEXT NOT NULL CHECK (mode IN ('preview', 'apply', 'scheduled')),
        status                TEXT NOT NULL CHECK (status IN ('active', 'completed', 'failed', 'cancelled')),
        stage                 TEXT NOT NULL,
        owner_sender_id       TEXT NOT NULL,
        checkpoint_history_id INTEGER NOT NULL,
        highwater_history_id  INTEGER NOT NULL,
        processed_history_id  INTEGER NOT NULL DEFAULT 0,
        candidate_count       INTEGER NOT NULL DEFAULT 0,
        promoted_count        INTEGER NOT NULL DEFAULT 0,
        rejected_count        INTEGER NOT NULL DEFAULT 0,
        embedding_model       TEXT NOT NULL,
        dimensions            INTEGER NOT NULL,
        index_version         INTEGER NOT NULL,
        error                 TEXT NOT NULL DEFAULT '',
        started_at            DATETIME NOT NULL,
        completed_at          DATETIME
    );

    CREATE TABLE IF NOT EXISTS memory_candidates (
        id                       INTEGER PRIMARY KEY AUTOINCREMENT,
        run_id                   INTEGER NOT NULL,
        kind                     TEXT NOT NULL CHECK (kind IN ('profile', 'durable', 'daily')),
        content                  TEXT NOT NULL,
        content_hash             TEXT NOT NULL,
        origin_class             TEXT NOT NULL CHECK (origin_class IN ('owner', 'agent')),
        evidence_history_ids     TEXT NOT NULL,
        observed_at              DATETIME NOT NULL,
        recurrence_count         INTEGER NOT NULL,
        distinct_day_count       INTEGER NOT NULL,
        trust_score              REAL NOT NULL,
        recency_score            REAL NOT NULL,
        novelty_score            REAL NOT NULL DEFAULT 0,
        contradiction_score      REAL NOT NULL DEFAULT 0,
        proposed_action          TEXT NOT NULL CHECK (proposed_action IN ('pending', 'noop', 'add', 'update', 'review', 'reject')),
        target_memory_id         INTEGER,
        status                   TEXT NOT NULL CHECK (status IN ('proposed', 'accepted', 'rejected', 'failed')),
        decision_reason          TEXT NOT NULL DEFAULT '',
        created_at               DATETIME NOT NULL,
        resolved_at              DATETIME,
        FOREIGN KEY (run_id) REFERENCES memory_maintenance_runs(id),
        FOREIGN KEY (target_memory_id) REFERENCES memories(id),
        UNIQUE (run_id, content_hash, evidence_history_ids)
    );

    CREATE TABLE IF NOT EXISTS conversation_chunks (
        id                  INTEGER PRIMARY KEY AUTOINCREMENT,
        start_history_id    INTEGER NOT NULL,
        end_history_id      INTEGER NOT NULL,
        part_index          INTEGER NOT NULL,
        content_hash        TEXT NOT NULL,
        embedding_model     TEXT NOT NULL,
        dimensions          INTEGER NOT NULL,
        index_version       INTEGER NOT NULL,
        embedding           BLOB NOT NULL,
        UNIQUE (embedding_model, index_version, start_history_id, end_history_id, part_index)
    );

    CREATE TABLE IF NOT EXISTS response_traces (
        id                   INTEGER PRIMARY KEY AUTOINCREMENT,
        trigger_type         TEXT NOT NULL,
        channel_id           TEXT NOT NULL,
        sender_id            TEXT NOT NULL,
        external_message_id  TEXT NOT NULL DEFAULT '',
        reminder_id          INTEGER,
        input_json           TEXT NOT NULL,
        inbound_history_id   INTEGER,
        status               TEXT NOT NULL DEFAULT 'active',
        failure_stage        TEXT NOT NULL DEFAULT '',
        error                TEXT NOT NULL DEFAULT '',
        started_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
        completed_at         DATETIME,
        FOREIGN KEY (inbound_history_id) REFERENCES conversation_history(id)
    );

    CREATE TABLE IF NOT EXISTS trace_events (
        id           INTEGER PRIMARY KEY AUTOINCREMENT,
        trace_id     INTEGER NOT NULL,
        sequence_no  INTEGER NOT NULL,
        kind         TEXT NOT NULL,
        status       TEXT NOT NULL DEFAULT 'pending',
        error        TEXT NOT NULL DEFAULT '',
        started_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
        completed_at DATETIME,
        FOREIGN KEY (trace_id) REFERENCES response_traces(id),
        UNIQUE (trace_id, sequence_no)
    );

    CREATE TABLE IF NOT EXISTS rag_retrievals (
        event_id             INTEGER PRIMARY KEY,
        outcome              TEXT NOT NULL,
        embedding_query      TEXT NOT NULL,
        embedding_model      TEXT NOT NULL,
        dimensions           INTEGER NOT NULL,
        index_version        INTEGER NOT NULL,
        minimum_score        REAL NOT NULL,
        history_highwater_id INTEGER NOT NULL,
        candidate_count      INTEGER NOT NULL,
        excluded_count       INTEGER NOT NULL,
        qualified_count      INTEGER NOT NULL,
        selected_count       INTEGER NOT NULL,
        rendered_archive     TEXT NOT NULL,
        FOREIGN KEY (event_id) REFERENCES trace_events(id)
    );

    CREATE TABLE IF NOT EXISTS rag_matches (
        id                 INTEGER PRIMARY KEY AUTOINCREMENT,
        retrieval_event_id INTEGER NOT NULL,
        rank               INTEGER NOT NULL,
        start_history_id   INTEGER NOT NULL,
        end_history_id     INTEGER NOT NULL,
        similarity_score   REAL NOT NULL,
        content_hash       TEXT NOT NULL,
        messages_json      TEXT NOT NULL,
        FOREIGN KEY (retrieval_event_id) REFERENCES rag_retrievals(event_id),
        FOREIGN KEY (start_history_id) REFERENCES conversation_history(id),
        FOREIGN KEY (end_history_id) REFERENCES conversation_history(id),
        UNIQUE (retrieval_event_id, rank)
    );

    CREATE TABLE IF NOT EXISTS memory_rag_matches (
        id                 INTEGER PRIMARY KEY AUTOINCREMENT,
        retrieval_event_id INTEGER NOT NULL,
        rank               INTEGER NOT NULL,
        memory_id          INTEGER NOT NULL,
        revision_id        INTEGER NOT NULL,
        vector_score       REAL NOT NULL,
        keyword_score      REAL NOT NULL,
        combined_score     REAL NOT NULL,
        content_hash       TEXT NOT NULL,
        FOREIGN KEY (retrieval_event_id) REFERENCES rag_retrievals(event_id),
        FOREIGN KEY (memory_id) REFERENCES memories(id),
        FOREIGN KEY (revision_id) REFERENCES memory_revisions(id),
        UNIQUE (retrieval_event_id, rank)
    );

    CREATE TABLE IF NOT EXISTS llm_calls (
        event_id        INTEGER PRIMARY KEY,
        round_number    INTEGER NOT NULL,
        purpose         TEXT NOT NULL,
        request_json    TEXT NOT NULL,
        response_json   TEXT NOT NULL DEFAULT '',
        http_status     INTEGER NOT NULL DEFAULT 0,
        finish_reason   TEXT NOT NULL DEFAULT '',
        FOREIGN KEY (event_id) REFERENCES trace_events(id)
    );

    CREATE TABLE IF NOT EXISTS tool_executions (
        event_id          INTEGER PRIMARY KEY,
        llm_event_id      INTEGER NOT NULL,
        tool_call_id      TEXT NOT NULL,
        name              TEXT NOT NULL,
        arguments_json    TEXT NOT NULL,
        result_json       TEXT NOT NULL DEFAULT '',
        is_error          INTEGER NOT NULL DEFAULT 0,
        mutation_committed INTEGER NOT NULL DEFAULT 0,
        FOREIGN KEY (event_id) REFERENCES trace_events(id),
        FOREIGN KEY (llm_event_id) REFERENCES llm_calls(event_id)
    );

    CREATE TABLE IF NOT EXISTS response_outputs (
        event_id           INTEGER PRIMARY KEY,
        source_type        TEXT NOT NULL,
        source_llm_event_id INTEGER,
        source_content     TEXT NOT NULL,
        transformations_json TEXT NOT NULL,
        final_content      TEXT NOT NULL,
        FOREIGN KEY (event_id) REFERENCES trace_events(id),
        FOREIGN KEY (source_llm_event_id) REFERENCES llm_calls(event_id)
    );

    CREATE TABLE IF NOT EXISTS delivery_attempts (
        event_id               INTEGER PRIMARY KEY,
        output_event_id         INTEGER NOT NULL,
        channel_id              TEXT NOT NULL,
        recipient_id            TEXT NOT NULL,
        content                 TEXT NOT NULL,
        provider_message_id     TEXT NOT NULL DEFAULT '',
        conversation_history_id INTEGER,
        attempted_at            DATETIME,
        accepted_at             DATETIME,
        FOREIGN KEY (event_id) REFERENCES trace_events(id),
        FOREIGN KEY (output_event_id) REFERENCES response_outputs(event_id),
        FOREIGN KEY (conversation_history_id) REFERENCES conversation_history(id)
    );

    CREATE INDEX IF NOT EXISTS idx_conversation_chunks_active
        ON conversation_chunks(embedding_model, index_version, dimensions);

    CREATE INDEX IF NOT EXISTS idx_reminders_due
        ON reminders(enabled, fire_at);

    CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_open_description
        ON tasks(lower(trim(description)))
        WHERE completed_at IS NULL;

    CREATE UNIQUE INDEX IF NOT EXISTS idx_memories_active_content
        ON memories(current_content_hash) WHERE status = 'active';

    CREATE INDEX IF NOT EXISTS idx_memories_active_kind
        ON memories(status, kind, updated_at DESC, id ASC);

    CREATE INDEX IF NOT EXISTS idx_memory_embeddings_model
        ON memory_embeddings(embedding_model, dimensions);

    CREATE INDEX IF NOT EXISTS idx_memory_maintenance_runs_time
        ON memory_maintenance_runs(started_at DESC, id DESC);

    CREATE INDEX IF NOT EXISTS idx_memory_candidates_run
        ON memory_candidates(run_id, id);

    CREATE INDEX IF NOT EXISTS idx_response_traces_time
        ON response_traces(started_at DESC, id DESC);

    CREATE INDEX IF NOT EXISTS idx_response_traces_route
        ON response_traces(channel_id, sender_id, started_at DESC);

    CREATE INDEX IF NOT EXISTS idx_response_traces_external_message
        ON response_traces(external_message_id) WHERE external_message_id <> '';

    CREATE INDEX IF NOT EXISTS idx_trace_events_trace
        ON trace_events(trace_id, sequence_no);

    CREATE INDEX IF NOT EXISTS idx_delivery_provider_message
        ON delivery_attempts(provider_message_id) WHERE provider_message_id <> '';
