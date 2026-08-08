package state

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"

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

	store := &Store{db: db}
	if err := store.initializeSchema(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
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

	CREATE INDEX IF NOT EXISTS idx_conversation_chunks_active
		ON conversation_chunks(embedding_model, index_version, dimensions);

	CREATE INDEX IF NOT EXISTS idx_reminders_due
		ON reminders(enabled, fire_at);

	CREATE INDEX IF NOT EXISTS idx_memory_entries_embedding_model
		ON memory_entries(embedding_model, dimensions);
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
		"conversation_history": {
			"id", "channel_id", "sender_id", "role", "content_type", "content", "created_at",
		},
		"conversation_chunks": {
			"id", "start_history_id", "end_history_id", "part_index", "content_hash",
			"embedding_model", "dimensions", "index_version", "embedding", "attempts", "retry_at",
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
	return nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
