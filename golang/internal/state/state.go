package state

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

// Store represents the SQLite connection for agent and memory state.
type Store struct {
	db *sql.DB
}

// Tx exposes state operations that must commit atomically with their matching
// conversation rows.
type Tx struct {
	tx *sql.Tx
}

func (s *Store) WithTx(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin state transaction: %w", err)
	}
	wrapped := &Tx{tx: tx}
	if err := fn(wrapped); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit state transaction: %w", err)
	}
	return nil
}

// NewStore initializes the SQLite database at the specified path.
func NewStore(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to ping sqlite database: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable SQLite foreign keys: %w", err)
	}

	store := &Store{db: db}
	if err := store.initializeSchema(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	return store, nil
}

// OpenReadOnlyStore opens an existing canonical database for local diagnostic
// inspection without creating files or applying schema statements.
func OpenReadOnlyStore(dbPath string) (*Store, error) {
	info, err := os.Stat(dbPath)
	if err != nil {
		return nil, fmt.Errorf("inspect state database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("state database is not a regular file")
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve state database: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro&_busy_timeout=10000"}).String()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read-only state database: %w", err)
	}
	store := &Store{db: db}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping read-only state database: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.validateCanonicalSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// initializeSchema creates the one canonical schema supported by this runtime.
// Existing databases must already match it; legacy schemas are rebuilt by the
// operator rather than migrated at startup.
func (s *Store) initializeSchema() error {
	var existingTables int
	if err := s.db.QueryRow(`
		SELECT count(*)
		FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
	`).Scan(&existingTables); err != nil {
		return fmt.Errorf("count existing schema tables: %w", err)
	}
	if existingTables > 0 {
		if err := s.validateCanonicalSchema(); err != nil {
			return err
		}
	}

	query := `
	CREATE TABLE IF NOT EXISTS memory_entries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		content TEXT NOT NULL,
		embedding_model TEXT NOT NULL DEFAULT '',
		dimensions INTEGER NOT NULL DEFAULT 0,
		embedding BLOB
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
		content     TEXT    NOT NULL,
		created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_conversation_history_lookup
		ON conversation_history(channel_id, sender_id, id);

	CREATE TABLE IF NOT EXISTS conversation_chunks (
		id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		start_history_id    INTEGER NOT NULL,
		end_history_id      INTEGER NOT NULL,
		part_index          INTEGER NOT NULL,
		content_hash        TEXT NOT NULL,
		embedding_model     TEXT NOT NULL,
		dimensions          INTEGER NOT NULL,
		index_version       INTEGER NOT NULL,
		embedding           BLOB,
		attempts            INTEGER NOT NULL DEFAULT 0,
		retry_at            DATETIME,
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

	CREATE INDEX IF NOT EXISTS idx_memory_entries_embedding_model
		ON memory_entries(embedding_model, dimensions);

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
	`

	if _, err := s.db.Exec(query); err != nil {
		return err
	}
	return s.validateCanonicalSchema()
}

func (s *Store) validateCanonicalSchema() error {
	expected := map[string][]string{
		"memory_entries": {
			"id", "content", "embedding_model", "dimensions", "embedding",
		},
		"reminders": {
			"id", "channel_id", "sender_id", "message", "fire_at", "schedule_kind",
			"every_ms", "anchor_at", "cron_expr", "timezone", "enabled",
		},
		"tasks": {
			"id", "description", "started_at", "completed_at",
		},
		"conversation_history": {
			"id", "channel_id", "sender_id", "role", "content_type", "content", "created_at",
		},
		"conversation_chunks": {
			"id", "start_history_id", "end_history_id", "part_index", "content_hash",
			"embedding_model", "dimensions", "index_version", "embedding", "attempts", "retry_at",
		},
		"response_traces": {
			"id", "trigger_type", "channel_id", "sender_id", "external_message_id",
			"reminder_id", "input_json", "inbound_history_id", "status", "failure_stage",
			"error", "started_at", "completed_at",
		},
		"trace_events": {
			"id", "trace_id", "sequence_no", "kind", "status", "error", "started_at", "completed_at",
		},
		"rag_retrievals": {
			"event_id", "outcome", "embedding_query", "embedding_model", "dimensions", "index_version",
			"minimum_score", "history_highwater_id", "candidate_count", "excluded_count",
			"qualified_count", "selected_count", "rendered_archive",
		},
		"rag_matches": {
			"id", "retrieval_event_id", "rank", "start_history_id", "end_history_id",
			"similarity_score", "content_hash", "messages_json",
		},
		"llm_calls": {
			"event_id", "round_number", "purpose", "request_json", "response_json", "http_status", "finish_reason",
		},
		"tool_executions": {
			"event_id", "llm_event_id", "tool_call_id", "name", "arguments_json", "result_json", "is_error", "mutation_committed",
		},
		"response_outputs": {
			"event_id", "source_type", "source_llm_event_id", "source_content", "transformations_json", "final_content",
		},
		"delivery_attempts": {
			"event_id", "output_event_id", "channel_id", "recipient_id", "content", "provider_message_id",
			"conversation_history_id", "attempted_at", "accepted_at",
		},
	}

	rows, err := s.db.Query(`
		SELECT name
		FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name
	`)
	if err != nil {
		return fmt.Errorf("list schema tables: %w", err)
	}
	var actualTables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan schema table: %w", err)
		}
		actualTables = append(actualTables, name)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close schema table rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate schema tables: %w", err)
	}
	expectedTables := make([]string, 0, len(expected))
	for name := range expected {
		expectedTables = append(expectedTables, name)
	}
	slices.Sort(expectedTables)
	if !slices.Equal(actualTables, expectedTables) {
		return fmt.Errorf("database tables %v do not match canonical tables %v; rebuild the database", actualTables, expectedTables)
	}

	for _, table := range expectedTables {
		columnRows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info(%q)`, table))
		if err != nil {
			return fmt.Errorf("inspect %s columns: %w", table, err)
		}
		var actualColumns []string
		for columnRows.Next() {
			var (
				position  int
				name      string
				columnTyp string
				notNull   int
				defaultV  sql.NullString
				primary   int
			)
			if err := columnRows.Scan(&position, &name, &columnTyp, &notNull, &defaultV, &primary); err != nil {
				_ = columnRows.Close()
				return fmt.Errorf("scan %s column: %w", table, err)
			}
			actualColumns = append(actualColumns, name)
		}
		if err := columnRows.Close(); err != nil {
			return fmt.Errorf("close %s column rows: %w", table, err)
		}
		if err := columnRows.Err(); err != nil {
			return fmt.Errorf("iterate %s columns: %w", table, err)
		}
		if !slices.Equal(actualColumns, expected[table]) {
			return fmt.Errorf("%s columns %v do not match canonical columns %v; rebuild the database", table, actualColumns, expected[table])
		}
	}
	var taskIndexSQL string
	if err := s.db.QueryRow(`
		SELECT sql FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_tasks_open_description'
	`).Scan(&taskIndexSQL); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("canonical task duplicate-prevention index is missing; rebuild the database")
		}
		return fmt.Errorf("inspect canonical task index: %w", err)
	}
	normalizedIndexSQL := strings.ToLower(strings.Join(strings.Fields(taskIndexSQL), " "))
	normalizedIndexSQL = strings.Replace(normalizedIndexSQL, " if not exists", "", 1)
	const expectedTaskIndexSQL = "create unique index idx_tasks_open_description on tasks(lower(trim(description))) where completed_at is null"
	if normalizedIndexSQL != expectedTaskIndexSQL {
		return fmt.Errorf("task duplicate-prevention index does not match the canonical definition; rebuild the database")
	}
	for _, name := range []string{"idx_response_traces_time", "idx_response_traces_route", "idx_response_traces_external_message", "idx_trace_events_trace", "idx_delivery_provider_message"} {
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&count); err != nil {
			return fmt.Errorf("inspect canonical provenance index %s: %w", name, err)
		}
		if count != 1 {
			return fmt.Errorf("canonical provenance index %s is missing; rebuild the database", name)
		}
	}
	return nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
