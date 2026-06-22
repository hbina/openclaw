package state

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

// Store represents the SQLite connection for agent and memory state.
type Store struct {
	db *sql.DB
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
		return nil, fmt.Errorf("failed to ping sqlite database: %w", err)
	}

	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return store, nil
}

// migrate sets up the necessary schema for the agent state and memory.
func (s *Store) migrate() error {
	query := `
	CREATE TABLE IF NOT EXISTS memory_entries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		content TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	
	CREATE TABLE IF NOT EXISTS agent_state (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS personality_documents (
		name TEXT PRIMARY KEY,
		content TEXT NOT NULL,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS reminders (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		channel_id TEXT NOT NULL,
		sender_id TEXT NOT NULL,
		message TEXT NOT NULL,
		fire_at DATETIME NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
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
		ON conversation_history(channel_id, sender_id, created_at);

	CREATE TABLE IF NOT EXISTS conversation_compactions (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		channel_id    TEXT    NOT NULL,
		sender_id     TEXT    NOT NULL,
		summary       TEXT    NOT NULL,
		tokens_before INTEGER NOT NULL,
		first_kept_id INTEGER NOT NULL,
		created_at    DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	`

	if _, err := s.db.Exec(query); err != nil {
		return err
	}

	// Add content_type to existing databases that predate this column.
	// SQLite returns "duplicate column name" when the column already exists; ignore it.
	if _, err := s.db.Exec(`ALTER TABLE conversation_history ADD COLUMN content_type TEXT NOT NULL DEFAULT 'text'`); err != nil {
		if !isDuplicateColumnErr(err) {
			return fmt.Errorf("failed to add content_type column: %w", err)
		}
	}
	return nil
}

func isDuplicateColumnErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column")
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
