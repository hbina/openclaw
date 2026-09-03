mod conversation;
mod maintenance;
mod memory;
mod rag;
mod reminders;
mod tasks;
mod trace;

use std::{
    collections::BTreeMap,
    fs,
    path::Path,
    sync::{Mutex, MutexGuard},
};

use chrono::{DateTime, NaiveDateTime, Utc};
use rusqlite::{Connection, OpenFlags, Transaction};
use thiserror::Error;

pub use conversation::{
    AUDIENCE_CONVERSATION, AUDIENCE_INTERNAL, CONTENT_INBOUND_MESSAGE, CONTENT_SCHEDULED_REMINDER,
    CONTENT_TEXT, CONTENT_TOOL_CALL, CONTENT_TOOL_RESULT, ConversationTurn,
};
pub use maintenance::{
    MaintenanceCandidate, MaintenanceExchange, MaintenanceMode, MaintenanceRun,
    MaintenanceRunCompletion, MaintenanceRunStart, MaintenanceState,
};
pub use memory::{
    Memory, MemoryFilter, MemoryKind, MemoryOrigin, MemoryReindexEntry, MemorySearchResult,
    MemorySource, MemoryStatus, MemoryWrite, memory_content_hash,
};
pub use rag::{ConversationChunk, ConversationEmbedding, ConversationReindexEntry};
pub(crate) use reminders::validate_standard_cron;
pub use reminders::{Reminder, ReminderSchedule, ScheduleKind, next_reminder_run};
pub use tasks::{Task, TaskStatus};
pub use trace::{
    MemoryRagTraceMatch, RagTrace, RagTraceMatch, RecallPlanContractReason, TraceFilter,
    TraceInput, TraceReport, TraceSummary,
};

const SCHEMA: &str = include_str!("schema.sql");

#[derive(Debug, Error)]
pub enum StateError {
    #[error("SQLite state error: {0}")]
    Sqlite(#[from] rusqlite::Error),
    #[error("state I/O error: {0}")]
    Io(#[from] std::io::Error),
    #[error("state lock was poisoned")]
    Poisoned,
    #[error("{0}")]
    Validation(String),
    #[error("memory maintenance is already running")]
    MaintenanceBusy,
    #[error("invalid stored timestamp {0:?}")]
    InvalidTimestamp(String),
}

pub struct Store {
    connection: Mutex<Connection>,
}

pub struct StateTx<'connection> {
    transaction: Transaction<'connection>,
}

impl Store {
    pub fn new(path: impl AsRef<Path>) -> Result<Self, StateError> {
        let path = path.as_ref();
        if let Some(parent) = path.parent()
            && !parent.as_os_str().is_empty()
        {
            fs::create_dir_all(parent)?;
        }
        let connection = Connection::open(path)?;
        connection.pragma_update(None, "foreign_keys", "ON")?;
        initialize_schema(&connection)?;
        Ok(Self {
            connection: Mutex::new(connection),
        })
    }

    pub fn open_read_only(path: impl AsRef<Path>) -> Result<Self, StateError> {
        let path = path.as_ref();
        if !path.metadata()?.is_file() {
            return Err(StateError::Validation(
                "state database is not a regular file".into(),
            ));
        }
        let connection = Connection::open_with_flags(
            path,
            OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_URI,
        )?;
        connection.pragma_update(None, "foreign_keys", "ON")?;
        validate_canonical_schema(&connection)?;
        Ok(Self {
            connection: Mutex::new(connection),
        })
    }

    pub fn with_tx<T, E>(
        &self,
        operation: impl FnOnce(&StateTx<'_>) -> Result<T, E>,
    ) -> Result<T, E>
    where
        E: From<StateError>,
    {
        let mut connection = self.lock().map_err(E::from)?;
        let transaction = connection
            .transaction()
            .map_err(StateError::from)
            .map_err(E::from)?;
        let tx = StateTx { transaction };
        let result = operation(&tx)?;
        tx.transaction
            .commit()
            .map_err(StateError::from)
            .map_err(E::from)?;
        Ok(result)
    }

    pub(crate) fn lock(&self) -> Result<MutexGuard<'_, Connection>, StateError> {
        self.connection.lock().map_err(|_| StateError::Poisoned)
    }
}

fn initialize_schema(connection: &Connection) -> Result<(), StateError> {
    let count: i64 = connection.query_row(
        "SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'",
        [],
        |row| row.get(0),
    )?;
    if count > 0 {
        validate_canonical_schema(connection)?;
    } else {
        connection.execute_batch(SCHEMA)?;
        validate_canonical_schema(connection)?;
    }
    Ok(())
}

fn validate_canonical_schema(connection: &Connection) -> Result<(), StateError> {
    let canonical = Connection::open_in_memory()?;
    canonical.pragma_update(None, "foreign_keys", "ON")?;
    canonical.execute_batch(SCHEMA)?;

    let expected = schema_signature(&canonical)?;
    let actual = schema_signature(connection)?;
    if actual.tables != expected.tables {
        return Err(StateError::Validation(format!(
            "state tables do not match the canonical schema; rebuild the database (actual {:?}, expected {:?})",
            actual.tables.keys().collect::<Vec<_>>(),
            expected.tables.keys().collect::<Vec<_>>()
        )));
    }
    for (table, expected_columns) in &expected.tables {
        let actual_columns = &actual.tables[table];
        if actual_columns != expected_columns {
            return Err(StateError::Validation(format!(
                "{table} columns {actual_columns:?} do not match canonical columns {expected_columns:?}; rebuild the database"
            )));
        }
    }
    if actual.indexes != expected.indexes {
        return Err(StateError::Validation(
            "state indexes do not match the canonical schema; rebuild the database".into(),
        ));
    }
    Ok(())
}

struct SchemaSignature {
    tables: BTreeMap<String, Vec<String>>,
    indexes: BTreeMap<String, String>,
}

fn schema_signature(connection: &Connection) -> Result<SchemaSignature, StateError> {
    let mut statement = connection.prepare(
        "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name",
    )?;
    let names: Vec<String> = statement
        .query_map([], |row| row.get(0))?
        .collect::<Result<_, _>>()?;
    let mut tables = BTreeMap::new();
    for name in names {
        if !name
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_')
        {
            return Err(StateError::Validation(format!(
                "invalid table name {name:?}"
            )));
        }
        let mut columns = connection.prepare(&format!("PRAGMA table_info({name})"))?;
        let columns = columns
            .query_map([], |row| row.get(1))?
            .collect::<Result<Vec<String>, _>>()?;
        tables.insert(name, columns);
    }

    let mut statement = connection.prepare(
        "SELECT name, sql FROM sqlite_master WHERE type='index' AND sql IS NOT NULL ORDER BY name",
    )?;
    let indexes = statement
        .query_map([], |row| {
            let name: String = row.get(0)?;
            let sql: String = row.get(1)?;
            Ok((name, normalize_sql(&sql)))
        })?
        .collect::<Result<BTreeMap<_, _>, _>>()?;
    Ok(SchemaSignature { tables, indexes })
}

fn normalize_sql(sql: &str) -> String {
    sql.split_whitespace()
        .map(str::to_lowercase)
        .collect::<Vec<_>>()
        .join(" ")
        .replace(" if not exists", "")
}

pub(crate) fn encode_time(value: DateTime<Utc>) -> String {
    value.to_rfc3339_opts(chrono::SecondsFormat::Nanos, true)
}

pub(crate) fn decode_time(value: String) -> Result<DateTime<Utc>, StateError> {
    if let Ok(value) = DateTime::parse_from_rfc3339(&value) {
        return Ok(value.with_timezone(&Utc));
    }
    for format in ["%Y-%m-%d %H:%M:%S%.f%:z", "%Y-%m-%d %H:%M:%S%.f"] {
        if let Ok(value) = DateTime::parse_from_str(&value, format) {
            return Ok(value.with_timezone(&Utc));
        }
        if let Ok(value) = NaiveDateTime::parse_from_str(&value, format) {
            return Ok(value.and_utc());
        }
    }
    Err(StateError::InvalidTimestamp(value))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fresh_database_has_the_canonical_schema() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("state.sqlite");
        let store = Store::new(&path).unwrap();
        drop(store);
        Store::open_read_only(path).unwrap();
    }

    #[test]
    fn rejects_a_noncanonical_existing_database() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("state.sqlite");
        Connection::open(&path)
            .unwrap()
            .execute("CREATE TABLE tasks (id INTEGER PRIMARY KEY)", [])
            .unwrap();
        assert!(matches!(Store::new(path), Err(StateError::Validation(_))));
    }
}
