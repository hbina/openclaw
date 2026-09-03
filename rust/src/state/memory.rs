use std::{cmp::Ordering, collections::HashMap};

use chrono::{DateTime, Utc};
use rusqlite::{OptionalExtension, Row, params};
use serde::Serialize;
use sha2::{Digest, Sha256};

use crate::vector;

use super::{ConversationReindexEntry, StateError, StateTx, Store, decode_time, encode_time};

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum MemoryKind {
    Profile,
    Durable,
    Daily,
}

impl MemoryKind {
    pub(crate) fn as_str(self) -> &'static str {
        match self {
            Self::Profile => "profile",
            Self::Durable => "durable",
            Self::Daily => "daily",
        }
    }

    pub(crate) fn parse(value: &str) -> Result<Self, StateError> {
        match value {
            "profile" => Ok(Self::Profile),
            "durable" => Ok(Self::Durable),
            "daily" => Ok(Self::Daily),
            _ => Err(StateError::Validation(format!(
                "invalid memory kind {value:?}"
            ))),
        }
    }
}

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum MemoryStatus {
    #[default]
    Active,
    Deleted,
    All,
}

impl MemoryStatus {
    pub(crate) fn as_str(self) -> &'static str {
        match self {
            Self::Active => "active",
            Self::Deleted => "deleted",
            Self::All => "all",
        }
    }

    pub(crate) fn parse(value: &str) -> Result<Self, StateError> {
        match value {
            "active" => Ok(Self::Active),
            "deleted" => Ok(Self::Deleted),
            _ => Err(StateError::Validation(format!(
                "invalid stored memory status {value:?}"
            ))),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum MemoryOrigin {
    Owner,
    Agent,
    System,
    Untrusted,
}

impl MemoryOrigin {
    pub(crate) fn as_str(self) -> &'static str {
        match self {
            Self::Owner => "owner",
            Self::Agent => "agent",
            Self::System => "system",
            Self::Untrusted => "untrusted",
        }
    }

    pub(crate) fn parse(value: &str) -> Result<Self, StateError> {
        match value {
            "owner" => Ok(Self::Owner),
            "agent" => Ok(Self::Agent),
            "system" => Ok(Self::System),
            "untrusted" => Ok(Self::Untrusted),
            _ => Err(StateError::Validation(format!(
                "invalid memory origin {value:?}"
            ))),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum MemorySource {
    Chat,
    Operator,
    Maintenance,
}

impl MemorySource {
    fn as_str(self) -> &'static str {
        match self {
            Self::Chat => "chat",
            Self::Operator => "operator",
            Self::Maintenance => "maintenance",
        }
    }

    fn parse(value: &str) -> Result<Self, StateError> {
        match value {
            "chat" => Ok(Self::Chat),
            "operator" => Ok(Self::Operator),
            "maintenance" => Ok(Self::Maintenance),
            _ => Err(StateError::Validation(format!(
                "invalid memory source {value:?}"
            ))),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Memory {
    pub id: i64,
    pub kind: MemoryKind,
    pub status: MemoryStatus,
    pub revision_id: i64,
    pub revision_number: i64,
    pub content: String,
    pub content_hash: String,
    pub origin_class: MemoryOrigin,
    pub source_kind: MemorySource,
    pub source_history_id: Option<i64>,
    pub source_trace_id: Option<i64>,
    pub observed_at: DateTime<Utc>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub deleted_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Clone)]
pub struct MemoryWrite {
    pub kind: MemoryKind,
    pub content: String,
    pub origin_class: MemoryOrigin,
    pub source_kind: MemorySource,
    pub source_history_id: Option<i64>,
    pub source_trace_id: Option<i64>,
    pub embedding_model: String,
    pub dimensions: usize,
    pub embedding: Vec<u8>,
    pub observed_at: Option<DateTime<Utc>>,
    pub now: DateTime<Utc>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MemoryReindexEntry {
    pub memory_id: i64,
    pub revision_id: i64,
    pub content: String,
    pub embedding_model: String,
    pub dimensions: usize,
    pub embedding: Vec<u8>,
}

#[derive(Debug, Clone, Copy)]
pub struct MemoryFilter {
    pub kind: Option<MemoryKind>,
    pub status: MemoryStatus,
    pub limit: usize,
}

impl Default for MemoryFilter {
    fn default() -> Self {
        Self {
            kind: None,
            status: MemoryStatus::Active,
            limit: 20,
        }
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct MemorySearchResult {
    #[serde(flatten)]
    pub memory: Memory,
    pub vector_score: f64,
    pub keyword_score: f64,
    pub combined_score: f64,
    pub snippet: String,
}

pub fn memory_content_hash(content: &str) -> String {
    let digest = Sha256::digest(content.trim().as_bytes());
    digest.iter().map(|byte| format!("{byte:02x}")).collect()
}

impl StateTx<'_> {
    pub fn store_memory(&self, input: &MemoryWrite) -> Result<(Memory, bool), StateError> {
        validate_write(input)?;
        let hash = memory_content_hash(&input.content);
        if let Some(memory) = get_memory_by_hash(&self.transaction, &hash)? {
            return Ok((memory, false));
        }
        let observed_at = input.observed_at.unwrap_or(input.now);
        self.transaction.execute(
            "INSERT INTO memories (kind, status, current_content_hash, observed_at, created_at, updated_at) VALUES (?1, 'active', ?2, ?3, ?4, ?4)",
            params![
                input.kind.as_str(),
                hash,
                encode_time(observed_at),
                encode_time(input.now),
            ],
        )?;
        let memory_id = self.transaction.last_insert_rowid();
        let revision_id = self.insert_revision(memory_id, 1, &hash, input)?;
        self.replace_indexes(memory_id, revision_id, input)?;
        self.transaction.execute(
            "UPDATE memories SET current_revision_id=?1 WHERE id=?2",
            params![revision_id, memory_id],
        )?;
        Ok((get_memory(&self.transaction, memory_id)?, true))
    }

    pub fn update_memory(&self, id: i64, input: &MemoryWrite) -> Result<Memory, StateError> {
        validate_write(input)?;
        let current = get_memory(&self.transaction, id)?;
        if current.status != MemoryStatus::Active {
            return Err(StateError::Validation(format!("memory {id} is deleted")));
        }
        let hash = memory_content_hash(&input.content);
        if let Some(duplicate) = get_memory_by_hash(&self.transaction, &hash)?
            && duplicate.id != id
        {
            return Err(StateError::Validation(format!(
                "content is already stored as Memory ID {}",
                duplicate.id
            )));
        }
        let revision_id = self.insert_revision(id, current.revision_number + 1, &hash, input)?;
        self.replace_indexes(id, revision_id, input)?;
        let observed_at = input.observed_at.unwrap_or(input.now);
        self.transaction.execute(
            "UPDATE memories SET kind=?1, current_revision_id=?2, current_content_hash=?3, observed_at=?4, updated_at=?5 WHERE id=?6 AND status='active'",
            params![
                input.kind.as_str(),
                revision_id,
                hash,
                encode_time(observed_at),
                encode_time(input.now),
                id,
            ],
        )?;
        get_memory(&self.transaction, id)
    }

    pub fn remove_memory(&self, id: i64, now: DateTime<Utc>) -> Result<Memory, StateError> {
        let memory = get_memory(&self.transaction, id)?;
        if memory.status != MemoryStatus::Active {
            return Err(StateError::Validation(format!(
                "memory {id} is already deleted"
            )));
        }
        self.transaction.execute(
            "UPDATE memories SET status='deleted', current_content_hash=NULL, updated_at=?1, deleted_at=?1 WHERE id=?2",
            params![encode_time(now), id],
        )?;
        self.transaction
            .execute("DELETE FROM memory_fts WHERE memory_id=?1", [id])?;
        self.transaction
            .execute("DELETE FROM memory_embeddings WHERE memory_id=?1", [id])?;
        get_memory(&self.transaction, id)
    }

    pub fn get_memory(&self, id: i64) -> Result<Memory, StateError> {
        get_memory(&self.transaction, id)
    }

    pub fn list_memories(&self, filter: MemoryFilter) -> Result<Vec<Memory>, StateError> {
        list_memories(&self.transaction, filter)
    }

    pub fn search_memories(
        &self,
        embedding_model: &str,
        dimensions: usize,
        query_vector: &[f32],
        fts_query: &str,
        min_vector: f64,
        limit: usize,
    ) -> Result<Vec<MemorySearchResult>, StateError> {
        search_memories(
            &self.transaction,
            embedding_model,
            dimensions,
            query_vector,
            fts_query,
            min_vector,
            limit,
        )
    }

    fn insert_revision(
        &self,
        memory_id: i64,
        revision: i64,
        hash: &str,
        input: &MemoryWrite,
    ) -> Result<i64, StateError> {
        self.transaction.execute(
            "INSERT INTO memory_revisions (memory_id, revision_number, content, content_hash, origin_class, source_kind, source_history_id, source_trace_id, created_at) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)",
            params![
                memory_id,
                revision,
                input.content.trim(),
                hash,
                input.origin_class.as_str(),
                input.source_kind.as_str(),
                input.source_history_id,
                input.source_trace_id,
                encode_time(input.now),
            ],
        )?;
        Ok(self.transaction.last_insert_rowid())
    }

    fn replace_indexes(
        &self,
        memory_id: i64,
        revision_id: i64,
        input: &MemoryWrite,
    ) -> Result<(), StateError> {
        self.transaction
            .execute("DELETE FROM memory_fts WHERE memory_id=?1", [memory_id])?;
        self.transaction.execute(
            "INSERT INTO memory_fts(content, memory_id, revision_id) VALUES (?1, ?2, ?3)",
            params![input.content.trim(), memory_id, revision_id],
        )?;
        self.transaction.execute(
            "DELETE FROM memory_embeddings WHERE memory_id=?1",
            [memory_id],
        )?;
        self.transaction.execute(
            "INSERT INTO memory_embeddings(memory_id, revision_id, embedding_model, dimensions, embedding, created_at) VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
            params![
                memory_id,
                revision_id,
                input.embedding_model,
                input.dimensions as i64,
                input.embedding,
                encode_time(input.now),
            ],
        )?;
        Ok(())
    }
}

impl Store {
    pub fn get_memory(&self, id: i64) -> Result<Memory, StateError> {
        get_memory(&*self.lock()?, id)
    }

    pub fn list_memories(&self, filter: MemoryFilter) -> Result<Vec<Memory>, StateError> {
        list_memories(&*self.lock()?, filter)
    }

    pub fn list_all_active_memories(&self) -> Result<Vec<Memory>, StateError> {
        let connection = self.lock()?;
        let mut statement = connection.prepare(&format!(
            "{MEMORY_SELECT} WHERE m.status='active' ORDER BY m.updated_at DESC, m.id ASC"
        ))?;
        let raw = statement
            .query_map([], raw_memory)?
            .collect::<Result<Vec<_>, _>>()?;
        raw.into_iter().map(Memory::try_from).collect()
    }

    pub fn memory_counts(&self) -> Result<(usize, usize), StateError> {
        let connection = self.lock()?;
        let (active, deleted): (i64, i64) = connection.query_row(
            "SELECT COALESCE(sum(CASE WHEN status='active' THEN 1 ELSE 0 END),0), COALESCE(sum(CASE WHEN status='deleted' THEN 1 ELSE 0 END),0) FROM memories",
            [],
            |row| Ok((row.get(0)?, row.get(1)?)),
        )?;
        Ok((active as usize, deleted as usize))
    }

    pub fn search_memories(
        &self,
        embedding_model: &str,
        dimensions: usize,
        query_vector: &[f32],
        fts_query: &str,
        min_vector: f64,
        limit: usize,
    ) -> Result<Vec<MemorySearchResult>, StateError> {
        search_memories(
            &*self.lock()?,
            embedding_model,
            dimensions,
            query_vector,
            fts_query,
            min_vector,
            limit,
        )
    }

    pub fn validate_derived_memory_state(
        &self,
        embedding_model: &str,
        dimensions: usize,
    ) -> Result<(), StateError> {
        let connection = self.lock()?;
        let missing: i64 = connection.query_row(
            "SELECT count(*) FROM memories m WHERE m.status='active' AND (m.current_revision_id IS NULL OR (SELECT count(*) FROM memory_embeddings e WHERE e.memory_id=m.id AND e.revision_id=m.current_revision_id AND e.embedding_model=?1 AND e.dimensions=?2) <> 1 OR (SELECT count(*) FROM memory_fts f WHERE f.memory_id=m.id AND f.revision_id=m.current_revision_id) <> 1)",
            params![embedding_model, dimensions as i64],
            |row| row.get(0),
        )?;
        if missing != 0 {
            return Err(StateError::Validation(format!(
                "{missing} active memories have incomplete derived indexes; run memory reindex"
            )));
        }
        let inactive: i64 = connection.query_row(
            "SELECT (SELECT count(*) FROM memory_embeddings e JOIN memories m ON m.id=e.memory_id WHERE m.status<>'active') + (SELECT count(*) FROM memory_fts f JOIN memories m ON m.id=f.memory_id WHERE m.status<>'active')",
            [],
            |row| row.get(0),
        )?;
        if inactive != 0 {
            return Err(StateError::Validation(
                "deleted memories remain indexed; run memory reindex".into(),
            ));
        }
        Ok(())
    }

    pub fn replace_derived_indexes(
        &self,
        memories: &[MemoryReindexEntry],
        conversations: &[ConversationReindexEntry],
        now: DateTime<Utc>,
    ) -> Result<(), StateError> {
        self.with_tx(|tx| {
            for statement in [
                "DELETE FROM memory_fts",
                "DELETE FROM memory_embeddings",
                "DELETE FROM conversation_chunks",
            ] {
                tx.transaction.execute(statement, [])?;
            }
            for item in memories {
                tx.transaction.execute(
                    "INSERT INTO memory_fts(content, memory_id, revision_id) VALUES (?1, ?2, ?3)",
                    params![item.content, item.memory_id, item.revision_id],
                )?;
                tx.transaction.execute(
                    "INSERT INTO memory_embeddings(memory_id, revision_id, embedding_model, dimensions, embedding, created_at) VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
                    params![
                        item.memory_id,
                        item.revision_id,
                        item.embedding_model,
                        item.dimensions as i64,
                        item.embedding,
                        encode_time(now),
                    ],
                )?;
            }
            for item in conversations {
                tx.save_conversation_chunks(
                    item.start_history_id,
                    item.end_history_id,
                    &item.chunks,
                )?;
            }
            Ok(())
        })
    }
}

fn validate_write(input: &MemoryWrite) -> Result<(), StateError> {
    if input.content.trim().is_empty() {
        return Err(StateError::Validation(
            "memory content must not be empty".into(),
        ));
    }
    if input.embedding_model.trim().is_empty() || input.dimensions == 0 {
        return Err(StateError::Validation(
            "memory embedding contract is required".into(),
        ));
    }
    vector::unpack(&input.embedding, input.dimensions)
        .map_err(|error| StateError::Validation(error.to_string()))?;
    Ok(())
}

const MEMORY_SELECT: &str = "SELECT m.id, m.kind, m.status, r.id, r.revision_number, r.content, r.content_hash, r.origin_class, r.source_kind, r.source_history_id, r.source_trace_id, m.observed_at, m.created_at, m.updated_at, m.deleted_at FROM memories m JOIN memory_revisions r ON r.id=m.current_revision_id";

fn get_memory(connection: &rusqlite::Connection, id: i64) -> Result<Memory, StateError> {
    connection
        .query_row(&format!("{MEMORY_SELECT} WHERE m.id=?1"), [id], raw_memory)
        .optional()?
        .ok_or_else(|| StateError::Validation(format!("memory {id} not found")))?
        .try_into()
}

fn get_memory_by_hash(
    connection: &rusqlite::Connection,
    hash: &str,
) -> Result<Option<Memory>, StateError> {
    connection
        .query_row(
            &format!("{MEMORY_SELECT} WHERE m.status='active' AND m.current_content_hash=?1"),
            [hash],
            raw_memory,
        )
        .optional()?
        .map(Memory::try_from)
        .transpose()
}

fn list_memories(
    connection: &rusqlite::Connection,
    filter: MemoryFilter,
) -> Result<Vec<Memory>, StateError> {
    let limit = if filter.limit == 0 {
        20
    } else {
        filter.limit.min(100)
    };
    let kind = filter.kind.map(MemoryKind::as_str);
    let mut statement = connection.prepare(&format!(
        "{MEMORY_SELECT} WHERE (?1='all' OR m.status=?1) AND (?2 IS NULL OR m.kind=?2) ORDER BY m.updated_at DESC, m.id ASC LIMIT ?3"
    ))?;
    let raw = statement
        .query_map(
            params![filter.status.as_str(), kind, limit as i64],
            raw_memory,
        )?
        .collect::<Result<Vec<_>, _>>()?;
    raw.into_iter().map(Memory::try_from).collect()
}

fn search_memories(
    connection: &rusqlite::Connection,
    embedding_model: &str,
    dimensions: usize,
    query_vector: &[f32],
    fts_query: &str,
    min_vector: f64,
    limit: usize,
) -> Result<Vec<MemorySearchResult>, StateError> {
    let limit = if limit == 0 || limit > 20 { 5 } else { limit };
    let mut candidates = HashMap::new();
    let mut statement = connection.prepare(
        "SELECT m.id, m.kind, m.status, r.id, r.revision_number, r.content, r.content_hash, r.origin_class, r.source_kind, r.source_history_id, r.source_trace_id, m.observed_at, m.created_at, m.updated_at, m.deleted_at, e.embedding FROM memories m JOIN memory_revisions r ON r.id=m.current_revision_id JOIN memory_embeddings e ON e.memory_id=m.id AND e.revision_id=r.id WHERE m.status='active' AND e.embedding_model=?1 AND e.dimensions=?2",
    )?;
    let rows = statement.query_map(params![embedding_model, dimensions as i64], |row| {
        Ok((raw_memory(row)?, row.get::<_, Vec<u8>>(15)?))
    })?;
    for row in rows {
        let (raw, packed) = row?;
        let memory: Memory = raw.try_into()?;
        let stored = vector::unpack(&packed, dimensions)
            .map_err(|error| StateError::Validation(error.to_string()))?;
        let score = vector::dot(query_vector, &stored);
        if score >= min_vector {
            candidates.insert(
                memory.id,
                MemorySearchResult {
                    snippet: memory.content.clone(),
                    memory,
                    vector_score: score,
                    keyword_score: 0.0,
                    combined_score: 0.0,
                },
            );
        }
    }
    drop(statement);

    if !fts_query.trim().is_empty() {
        let mut statement = connection.prepare("SELECT memory_id, revision_id, snippet(memory_fts, 0, '[', ']', ' … ', 24), bm25(memory_fts) FROM memory_fts WHERE memory_fts MATCH ?1 ORDER BY bm25(memory_fts) LIMIT 24")?;
        let rows = statement.query_map([fts_query], |row| {
            Ok((
                row.get::<_, i64>(0)?,
                row.get::<_, i64>(1)?,
                row.get::<_, String>(2)?,
            ))
        })?;
        for (ordinal, row) in rows.enumerate() {
            let (memory_id, revision_id, snippet) = row?;
            if let std::collections::hash_map::Entry::Vacant(entry) = candidates.entry(memory_id) {
                let memory = get_memory(connection, memory_id)?;
                if memory.status != MemoryStatus::Active || memory.revision_id != revision_id {
                    continue;
                }
                entry.insert(MemorySearchResult {
                    memory,
                    vector_score: 0.0,
                    keyword_score: 0.0,
                    combined_score: 0.0,
                    snippet: String::new(),
                });
            }
            let candidate = candidates
                .get_mut(&memory_id)
                .expect("candidate was inserted or already present");
            candidate.keyword_score = 1.0 / (ordinal + 1) as f64;
            candidate.snippet = snippet;
        }
    }

    let mut results: Vec<_> = candidates
        .into_values()
        .map(|mut candidate| {
            candidate.combined_score =
                0.65 * candidate.vector_score + 0.35 * candidate.keyword_score;
            candidate
        })
        .collect();
    results.sort_by(|left, right| {
        right
            .combined_score
            .partial_cmp(&left.combined_score)
            .unwrap_or(Ordering::Equal)
            .then_with(|| right.memory.updated_at.cmp(&left.memory.updated_at))
            .then_with(|| left.memory.id.cmp(&right.memory.id))
    });
    results.truncate(limit);
    Ok(results)
}

type RawMemory = (
    i64,
    String,
    String,
    i64,
    i64,
    String,
    String,
    String,
    String,
    Option<i64>,
    Option<i64>,
    String,
    String,
    String,
    Option<String>,
);

fn raw_memory(row: &Row<'_>) -> rusqlite::Result<RawMemory> {
    Ok((
        row.get(0)?,
        row.get(1)?,
        row.get(2)?,
        row.get(3)?,
        row.get(4)?,
        row.get(5)?,
        row.get(6)?,
        row.get(7)?,
        row.get(8)?,
        row.get(9)?,
        row.get(10)?,
        row.get(11)?,
        row.get(12)?,
        row.get(13)?,
        row.get(14)?,
    ))
}

impl TryFrom<RawMemory> for Memory {
    type Error = StateError;

    fn try_from(raw: RawMemory) -> Result<Self, Self::Error> {
        Ok(Self {
            id: raw.0,
            kind: MemoryKind::parse(&raw.1)?,
            status: MemoryStatus::parse(&raw.2)?,
            revision_id: raw.3,
            revision_number: raw.4,
            content: raw.5,
            content_hash: raw.6,
            origin_class: MemoryOrigin::parse(&raw.7)?,
            source_kind: MemorySource::parse(&raw.8)?,
            source_history_id: raw.9,
            source_trace_id: raw.10,
            observed_at: decode_time(raw.11)?,
            created_at: decode_time(raw.12)?,
            updated_at: decode_time(raw.13)?,
            deleted_at: raw.14.map(decode_time).transpose()?,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn write(content: &str, vector: &[f32], now: DateTime<Utc>) -> MemoryWrite {
        MemoryWrite {
            kind: MemoryKind::Durable,
            content: content.into(),
            origin_class: MemoryOrigin::Owner,
            source_kind: MemorySource::Operator,
            source_history_id: None,
            source_trace_id: None,
            embedding_model: "test-model".into(),
            dimensions: vector.len(),
            embedding: vector::pack(vector),
            observed_at: None,
            now,
        }
    }

    #[test]
    fn memory_ledger_revisions_indexes_and_deletion_stay_aligned() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("state.sqlite");
        let store = Store::new(&path).unwrap();
        let now = Utc::now();
        let (first, created) = store
            .with_tx(|tx| tx.store_memory(&write(" likes espresso ", &[1.0, 0.0], now)))
            .unwrap();
        assert!(created);
        assert_eq!(first.content, "likes espresso");
        let (duplicate, created) = store
            .with_tx(|tx| tx.store_memory(&write("likes espresso", &[1.0, 0.0], now)))
            .unwrap();
        assert!(!created);
        assert_eq!(duplicate.id, first.id);

        let updated = store
            .with_tx(|tx| {
                tx.update_memory(
                    first.id,
                    &write(
                        "likes espresso after lunch",
                        &[1.0, 0.0],
                        now + chrono::Duration::seconds(1),
                    ),
                )
            })
            .unwrap();
        assert_eq!(updated.revision_number, 2);
        store
            .validate_derived_memory_state("test-model", 2)
            .unwrap();
        let removed = store
            .with_tx(|tx| tx.remove_memory(first.id, now + chrono::Duration::seconds(2)))
            .unwrap();
        assert_eq!(removed.status, MemoryStatus::Deleted);
        assert_eq!(store.memory_counts().unwrap(), (0, 1));
        store
            .validate_derived_memory_state("test-model", 2)
            .unwrap();
        drop(store);
        assert_eq!(
            Store::new(path).unwrap().get_memory(first.id).unwrap(),
            removed
        );
    }

    #[test]
    fn hybrid_search_combines_vector_and_keyword_candidates() {
        let directory = tempfile::tempdir().unwrap();
        let store = Store::new(directory.path().join("state.sqlite")).unwrap();
        let now = Utc::now();
        for (content, embedding) in [
            ("likes espresso", [1.0, 0.0]),
            ("prefers tea", [0.0, 1.0]),
            ("espresso after lunch", [1.0, 0.0]),
        ] {
            store
                .with_tx(|tx| tx.store_memory(&write(content, &embedding, now)))
                .unwrap();
        }
        let results = store
            .search_memories("test-model", 2, &[1.0, 0.0], "espresso", 0.5, 5)
            .unwrap();
        assert_eq!(results.len(), 2);
        assert!(results.iter().all(|result| result.snippet.contains('[')));
        assert!(results[0].combined_score >= results[1].combined_score);
    }
}
