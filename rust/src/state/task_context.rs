use std::{
    cmp::Ordering,
    collections::{HashMap, HashSet},
};

use chrono::{DateTime, Utc};
use rusqlite::{OptionalExtension, params};
use serde::Serialize;

use crate::vector;

use super::{StateError, StateTx, Store, Task, decode_time, encode_time};

#[derive(Debug, Clone)]
pub struct TaskIndex {
    pub model: String,
    pub dimensions: usize,
    pub embedding: Vec<u8>,
}

#[derive(Debug, Clone, Serialize)]
pub struct TaskContext {
    pub id: i64,
    pub task_id: i64,
    pub kind: String,
    pub content: String,
    pub recorded_at: DateTime<Utc>,
    pub source_history_id: Option<i64>,
    pub source_trace_event_id: Option<i64>,
    pub supersedes_id: Option<i64>,
    pub superseded: bool,
}

#[derive(Debug, Clone)]
pub struct TaskSearchHit {
    pub task: Task,
    pub score: f64,
    pub evidence: String,
}

#[derive(Debug, Clone)]
pub struct TaskReindexEntry {
    pub task_id: i64,
    pub context_id: i64,
    pub content: String,
    pub index: TaskIndex,
}

impl StateTx<'_> {
    pub(crate) fn replace_task_indexes(
        &self,
        entries: &[TaskReindexEntry],
    ) -> Result<(), StateError> {
        self.transaction.execute("DELETE FROM task_fts", [])?;
        self.transaction
            .execute("DELETE FROM task_embeddings", [])?;
        for entry in entries {
            self.transaction.execute(
                "INSERT INTO task_fts(content,task_id,context_id) VALUES (?1,?2,?3)",
                params![entry.content, entry.task_id, entry.context_id],
            )?;
            self.put_task_embedding(entry.task_id, entry.context_id, &entry.index)?;
        }
        Ok(())
    }
    pub fn get_task_context_note(&self, id: i64) -> Result<TaskContext, StateError> {
        let row = self.transaction.query_row(
            "SELECT c.id,c.task_id,c.kind,c.content,c.recorded_at,c.source_history_id,c.source_trace_event_id,c.supersedes_id,EXISTS(SELECT 1 FROM task_context n WHERE n.supersedes_id=c.id) FROM task_context c WHERE c.id=?1",
            [id],
            |row| Ok((row.get::<_, i64>(0)?, row.get::<_, i64>(1)?, row.get::<_, String>(2)?, row.get::<_, String>(3)?, row.get::<_, String>(4)?, row.get::<_, Option<i64>>(5)?, row.get::<_, Option<i64>>(6)?, row.get::<_, Option<i64>>(7)?, row.get::<_, bool>(8)?))
        ).optional()?.ok_or_else(|| StateError::Validation(format!("task context note {id} not found")))?;
        Ok(TaskContext {
            id: row.0,
            task_id: row.1,
            kind: row.2,
            content: row.3,
            recorded_at: decode_time(row.4)?,
            source_history_id: row.5,
            source_trace_event_id: row.6,
            supersedes_id: row.7,
            superseded: row.8,
        })
    }
    pub fn index_task_title(
        &self,
        task_id: i64,
        description: &str,
        index: &TaskIndex,
    ) -> Result<(), StateError> {
        self.transaction.execute(
            "DELETE FROM task_fts WHERE task_id=?1 AND context_id=0",
            [task_id],
        )?;
        self.transaction.execute(
            "INSERT INTO task_fts(content,task_id,context_id) VALUES (?1,?2,0)",
            params![description, task_id],
        )?;
        self.put_task_embedding(task_id, 0, index)
    }

    #[allow(clippy::too_many_arguments)]
    pub fn append_task_context(
        &self,
        task_id: i64,
        kind: &str,
        content: &str,
        source_history_id: Option<i64>,
        source_trace_event_id: Option<i64>,
        supersedes_id: Option<i64>,
        now: DateTime<Utc>,
        index: &TaskIndex,
    ) -> Result<TaskContext, StateError> {
        let task = self.get_task(task_id)?;
        if task.completed_at.is_some() {
            return Err(StateError::Validation(format!(
                "task {task_id} is completed"
            )));
        }
        let content = content.trim();
        if content.is_empty() || content.len() > 4000 {
            return Err(StateError::Validation(
                "task context must be 1 to 4000 bytes".into(),
            ));
        }
        if !matches!(
            kind,
            "context" | "progress" | "decision" | "blocker" | "next_step"
        ) {
            return Err(StateError::Validation(format!(
                "invalid task context kind {kind:?}"
            )));
        }
        if let Some(old_id) = supersedes_id {
            let old_task_id: Option<i64> = self
                .transaction
                .query_row(
                    "SELECT task_id FROM task_context WHERE id=?1",
                    [old_id],
                    |row| row.get(0),
                )
                .optional()?;
            if old_task_id != Some(task_id) {
                return Err(StateError::Validation(
                    "corrected note does not belong to task".into(),
                ));
            }
            let replaced: i64 = self.transaction.query_row(
                "SELECT count(*) FROM task_context WHERE supersedes_id=?1",
                [old_id],
                |row| row.get(0),
            )?;
            if replaced != 0 {
                return Err(StateError::Validation(
                    "task context note was already corrected".into(),
                ));
            }
        }
        self.transaction.execute(
            "INSERT INTO task_context(task_id,kind,content,recorded_at,source_history_id,source_trace_event_id,supersedes_id) VALUES (?1,?2,?3,?4,?5,?6,?7)",
            params![task_id, kind, content, encode_time(now), source_history_id, source_trace_event_id, supersedes_id],
        )?;
        let id = self.transaction.last_insert_rowid();
        if let Some(old_id) = supersedes_id {
            self.transaction.execute(
                "DELETE FROM task_fts WHERE task_id=?1 AND context_id=?2",
                params![task_id, old_id],
            )?;
            self.transaction.execute(
                "DELETE FROM task_embeddings WHERE task_id=?1 AND context_id=?2",
                params![task_id, old_id],
            )?;
        }
        self.transaction.execute(
            "INSERT INTO task_fts(content,task_id,context_id) VALUES (?1,?2,?3)",
            params![content, task_id, id],
        )?;
        self.put_task_embedding(task_id, id, index)?;
        Ok(TaskContext {
            id,
            task_id,
            kind: kind.into(),
            content: content.into(),
            recorded_at: now,
            source_history_id,
            source_trace_event_id,
            supersedes_id,
            superseded: false,
        })
    }

    pub fn task_context(&self, task_id: i64) -> Result<Vec<TaskContext>, StateError> {
        self.get_task(task_id)?;
        let mut statement = self.transaction.prepare(
            "SELECT c.id,c.task_id,c.kind,c.content,c.recorded_at,c.source_history_id,c.source_trace_event_id,c.supersedes_id,EXISTS(SELECT 1 FROM task_context n WHERE n.supersedes_id=c.id) FROM task_context c WHERE c.task_id=?1 ORDER BY c.recorded_at,c.id"
        )?;
        let rows = statement.query_map([task_id], |row| {
            Ok((
                row.get::<_, i64>(0)?,
                row.get::<_, i64>(1)?,
                row.get::<_, String>(2)?,
                row.get::<_, String>(3)?,
                row.get::<_, String>(4)?,
                row.get::<_, Option<i64>>(5)?,
                row.get::<_, Option<i64>>(6)?,
                row.get::<_, Option<i64>>(7)?,
                row.get::<_, bool>(8)?,
            ))
        })?;
        rows.map(|row| {
            let (
                id,
                task_id,
                kind,
                content,
                time,
                source_history_id,
                source_trace_event_id,
                supersedes_id,
                superseded,
            ) = row?;
            Ok(TaskContext {
                id,
                task_id,
                kind,
                content,
                recorded_at: decode_time(time)?,
                source_history_id,
                source_trace_event_id,
                supersedes_id,
                superseded,
            })
        })
        .collect()
    }

    fn put_task_embedding(
        &self,
        task_id: i64,
        context_id: i64,
        index: &TaskIndex,
    ) -> Result<(), StateError> {
        if index.dimensions == 0 || index.embedding.len() != index.dimensions * 4 {
            return Err(StateError::Validation(
                "invalid task embedding dimensions".into(),
            ));
        }
        self.transaction.execute(
            "DELETE FROM task_embeddings WHERE task_id=?1 AND context_id=?2",
            params![task_id, context_id],
        )?;
        self.transaction.execute(
            "INSERT INTO task_embeddings(task_id,context_id,embedding_model,dimensions,embedding) VALUES (?1,?2,?3,?4,?5)",
            params![task_id, context_id, index.model, index.dimensions as i64, index.embedding],
        )?;
        Ok(())
    }
}

impl Store {
    pub fn validate_task_indexes(&self, model: &str, dimensions: usize) -> Result<(), StateError> {
        let connection = self.lock()?;
        let expected: i64 = connection.query_row(
            "SELECT (SELECT count(*) FROM tasks) + (SELECT count(*) FROM task_context c WHERE NOT EXISTS(SELECT 1 FROM task_context n WHERE n.supersedes_id=c.id))", [], |row| row.get(0)
        )?;
        let embeddings: i64 = connection.query_row(
            "SELECT count(*) FROM task_embeddings WHERE embedding_model=?1 AND dimensions=?2",
            params![model, dimensions as i64],
            |row| row.get(0),
        )?;
        let fts: i64 =
            connection.query_row("SELECT count(*) FROM task_fts", [], |row| row.get(0))?;
        let missing: i64 = connection.query_row(
            "SELECT count(*) FROM (SELECT id AS task_id, 0 AS context_id FROM tasks UNION ALL SELECT c.task_id,c.id FROM task_context c WHERE NOT EXISTS(SELECT 1 FROM task_context n WHERE n.supersedes_id=c.id)) d WHERE NOT EXISTS(SELECT 1 FROM task_embeddings e WHERE e.task_id=d.task_id AND e.context_id=d.context_id AND e.embedding_model=?1 AND e.dimensions=?2) OR NOT EXISTS(SELECT 1 FROM task_fts f WHERE f.task_id=d.task_id AND f.context_id=d.context_id)",
            params![model, dimensions as i64], |row| row.get(0)
        )?;
        if expected != embeddings || expected != fts || missing != 0 {
            return Err(StateError::Validation(format!(
                "task derived indexes incomplete (documents {expected}, embeddings {embeddings}, FTS {fts}, missing {missing}); run memory reindex"
            )));
        }
        Ok(())
    }
    pub fn task_reindex_documents(&self) -> Result<Vec<(i64, i64, String)>, StateError> {
        self.with_tx(|tx| {
            let tasks = tx.list_tasks(super::TaskStatus::All)?;
            let mut result = Vec::new();
            for task in tasks {
                result.push((task.id, 0, task.description));
                for note in tx.task_context(task.id)? {
                    if !note.superseded {
                        result.push((task.id, note.id, note.content));
                    }
                }
            }
            Ok(result)
        })
    }

    pub fn has_open_tasks(&self) -> Result<bool, StateError> {
        let connection = self.lock()?;
        let count: i64 = connection.query_row(
            "SELECT count(*) FROM tasks WHERE completed_at IS NULL",
            [],
            |row| row.get(0),
        )?;
        Ok(count > 0)
    }
    pub fn has_tasks(&self) -> Result<bool, StateError> {
        let connection = self.lock()?;
        let count: i64 =
            connection.query_row("SELECT count(*) FROM tasks", [], |row| row.get(0))?;
        Ok(count > 0)
    }
    pub fn get_task_with_context(
        &self,
        task_id: i64,
    ) -> Result<(Task, Vec<TaskContext>), StateError> {
        self.with_tx(|tx| Ok((tx.get_task(task_id)?, tx.task_context(task_id)?)))
    }

    pub fn search_tasks(
        &self,
        query_vector: &[f32],
        fts_query: &str,
        model: &str,
        include_completed: bool,
        limit: usize,
    ) -> Result<Vec<TaskSearchHit>, StateError> {
        let connection = self.lock()?;
        let mut scores: HashMap<i64, f64> = HashMap::new();
        let mut evidence: HashMap<i64, String> = HashMap::new();
        let mut statement = connection.prepare(
            "SELECT e.task_id,e.context_id,e.embedding,t.description,c.content FROM task_embeddings e JOIN tasks t ON t.id=e.task_id LEFT JOIN task_context c ON c.id=e.context_id WHERE e.embedding_model=?1 AND e.dimensions=?2 AND (?3 OR t.completed_at IS NULL)"
        )?;
        let rows = statement.query_map(
            params![model, query_vector.len() as i64, include_completed],
            |row| {
                Ok((
                    row.get::<_, i64>(0)?,
                    row.get::<_, i64>(1)?,
                    row.get::<_, Vec<u8>>(2)?,
                    row.get::<_, String>(3)?,
                    row.get::<_, Option<String>>(4)?,
                ))
            },
        )?;
        for row in rows {
            let (task_id, _context_id, packed, description, note) = row?;
            let stored = vector::unpack(&packed, query_vector.len())
                .map_err(|error| StateError::Validation(error.to_string()))?;
            let score = vector::dot(query_vector, &stored);
            if scores.get(&task_id).is_none_or(|current| score > *current) {
                scores.insert(task_id, score);
                evidence.insert(task_id, note.unwrap_or(description));
            }
        }
        drop(statement);
        if !fts_query.is_empty() {
            let mut keyword_evidence = HashSet::new();
            let mut statement = connection.prepare(
                "SELECT f.task_id,f.content FROM task_fts f JOIN tasks t ON t.id=f.task_id WHERE task_fts MATCH ?1 AND (?2 OR t.completed_at IS NULL) ORDER BY bm25(task_fts) LIMIT 32"
            )?;
            let rows = statement.query_map(params![fts_query, include_completed], |row| {
                Ok((row.get::<_, i64>(0)?, row.get::<_, String>(1)?))
            })?;
            for (rank, row) in rows.enumerate() {
                let (task_id, content) = row?;
                let bonus = 1.0 / (rank + 1) as f64;
                scores
                    .entry(task_id)
                    .and_modify(|score| *score += bonus)
                    .or_insert(bonus);
                if keyword_evidence.insert(task_id) {
                    evidence.insert(task_id, content);
                }
            }
        }
        let mut hits = Vec::new();
        for (task_id, score) in scores {
            let task = connection
                .query_row(
                    "SELECT id,description,started_at,completed_at FROM tasks WHERE id=?1",
                    [task_id],
                    super::tasks::raw_task,
                )?
                .try_into()?;
            hits.push(TaskSearchHit {
                task,
                score,
                evidence: evidence.remove(&task_id).unwrap_or_default(),
            });
        }
        hits.sort_by(|a, b| {
            b.score
                .partial_cmp(&a.score)
                .unwrap_or(Ordering::Equal)
                .then_with(|| a.task.id.cmp(&b.task.id))
        });
        hits.truncate(limit);
        Ok(hits)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone;

    fn index(text: &str) -> TaskIndex {
        let vector = if text.contains("strategy") || text.contains("OHLC") {
            [1.0, 0.0]
        } else {
            [0.0, 1.0]
        };
        TaskIndex {
            model: "test".into(),
            dimensions: 2,
            embedding: vector::pack(&vector),
        }
    }

    #[test]
    fn task_notes_are_durable_searchable_correctable_and_removed_with_task() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("state.sqlite");
        let store = Store::new(&path).unwrap();
        let now = Utc.with_ymd_and_hms(2026, 9, 17, 0, 0, 0).unwrap();
        let (task_id, old_id, new_id) = store
            .with_tx(|tx| {
                let task = tx.add_task("Build strategy", now)?;
                tx.index_task_title(task.id, &task.description, &index("strategy"))?;
                let old = tx.append_task_context(
                    task.id,
                    "decision",
                    "Use intraday data",
                    None,
                    None,
                    None,
                    now,
                    &index("strategy"),
                )?;
                let new = tx.append_task_context(
                    task.id,
                    "decision",
                    "Use daily OHLC data",
                    None,
                    None,
                    Some(old.id),
                    now,
                    &index("OHLC"),
                )?;
                Ok::<_, StateError>((task.id, old.id, new.id))
            })
            .unwrap();
        drop(store);
        let store = Store::new(&path).unwrap();
        store.validate_task_indexes("test", 2).unwrap();
        let (task, notes) = store.get_task_with_context(task_id).unwrap();
        assert_eq!(task.description, "Build strategy");
        assert_eq!(notes.len(), 2);
        assert!(notes[0].superseded);
        assert_eq!(notes[1].supersedes_id, Some(old_id));
        assert_eq!(notes[1].id, new_id);
        let hits = store
            .search_tasks(&[1.0, 0.0], "\"OHLC\"", "test", false, 5)
            .unwrap();
        assert_eq!(hits[0].task.id, task_id);
        assert!(hits[0].evidence.contains("OHLC"));
        assert!(
            store
                .search_tasks(&[0.0, 0.0], "\"intraday\"", "test", false, 5)
                .unwrap()
                .iter()
                .all(|hit| hit.score < 0.1)
        );
        store.with_tx(|tx| tx.delete_task(task_id)).unwrap();
        store.validate_task_indexes("test", 2).unwrap();
        assert!(!store.has_open_tasks().unwrap());
        let connection = store.lock().unwrap();
        let count: i64 = connection
            .query_row("SELECT count(*) FROM task_context", [], |row| row.get(0))
            .unwrap();
        assert_eq!(count, 0);
    }

    #[test]
    fn failed_note_index_rolls_back_the_note() {
        let store = Store::new(":memory:").unwrap();
        let now = Utc::now();
        let task = store.with_tx(|tx| tx.add_task("Study PCIe", now)).unwrap();
        let bad = TaskIndex {
            model: "test".into(),
            dimensions: 2,
            embedding: vec![0],
        };
        assert!(
            store
                .with_tx(|tx| tx.append_task_context(
                    task.id,
                    "progress",
                    "Measured DMA",
                    None,
                    None,
                    None,
                    now,
                    &bad
                ))
                .is_err()
        );
        assert!(store.get_task_with_context(task.id).unwrap().1.is_empty());
    }
}
