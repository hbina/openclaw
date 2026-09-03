use chrono::{DateTime, Utc};
use rusqlite::{OptionalExtension, Row, params};

use super::{StateError, StateTx, decode_time, encode_time};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TaskStatus {
    Open,
    Completed,
    All,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Task {
    pub id: i64,
    pub description: String,
    pub started_at: DateTime<Utc>,
    pub completed_at: Option<DateTime<Utc>>,
}

impl Task {
    pub fn status(&self) -> TaskStatus {
        if self.completed_at.is_some() {
            TaskStatus::Completed
        } else {
            TaskStatus::Open
        }
    }
}

impl StateTx<'_> {
    pub fn add_task(
        &self,
        description: &str,
        started_at: DateTime<Utc>,
    ) -> Result<Task, StateError> {
        let description = description.trim();
        if description.is_empty() {
            return Err(StateError::Validation(
                "task description must not be empty".into(),
            ));
        }
        let existing: Option<i64> = self
            .transaction
            .query_row(
                "SELECT id FROM tasks WHERE completed_at IS NULL AND lower(trim(description)) = lower(trim(?1))",
                [description],
                |row| row.get(0),
            )
            .optional()?;
        if let Some(id) = existing {
            return Err(StateError::Validation(format!(
                "open task {id} already has description {description:?}"
            )));
        }
        self.transaction.execute(
            "INSERT INTO tasks (description, started_at, completed_at) VALUES (?1, ?2, NULL)",
            params![description, encode_time(started_at)],
        )?;
        Ok(Task {
            id: self.transaction.last_insert_rowid(),
            description: description.into(),
            started_at,
            completed_at: None,
        })
    }

    pub fn list_tasks(&self, status: TaskStatus) -> Result<Vec<Task>, StateError> {
        let where_clause = match status {
            TaskStatus::Open => " WHERE completed_at IS NULL",
            TaskStatus::Completed => " WHERE completed_at IS NOT NULL",
            TaskStatus::All => "",
        };
        let sql = format!(
            "SELECT id, description, started_at, completed_at FROM tasks{where_clause} ORDER BY started_at ASC, id ASC"
        );
        let mut statement = self.transaction.prepare(&sql)?;
        let raw = statement
            .query_map([], raw_task)?
            .collect::<Result<Vec<_>, _>>()?;
        raw.into_iter().map(Task::try_from).collect()
    }

    pub fn get_task(&self, id: i64) -> Result<Task, StateError> {
        let raw = self
            .transaction
            .query_row(
                "SELECT id, description, started_at, completed_at FROM tasks WHERE id=?1",
                [id],
                raw_task,
            )
            .optional()?
            .ok_or_else(|| StateError::Validation(format!("task {id} not found")))?;
        raw.try_into()
    }

    pub fn update_task(&self, id: i64, description: &str) -> Result<Task, StateError> {
        let description = description.trim();
        if description.is_empty() {
            return Err(StateError::Validation(
                "task description must not be empty".into(),
            ));
        }
        let mut task = self.get_task(id)?;
        if task.completed_at.is_some() {
            return Err(StateError::Validation(format!(
                "task {id} is completed and cannot be updated"
            )));
        }
        let duplicate: Option<i64> = self
            .transaction
            .query_row(
                "SELECT id FROM tasks WHERE id <> ?1 AND completed_at IS NULL AND lower(trim(description)) = lower(trim(?2))",
                params![id, description],
                |row| row.get(0),
            )
            .optional()?;
        if let Some(duplicate) = duplicate {
            return Err(StateError::Validation(format!(
                "open task {duplicate} already has description {description:?}"
            )));
        }
        let changed = self.transaction.execute(
            "UPDATE tasks SET description=?1 WHERE id=?2 AND completed_at IS NULL",
            params![description, id],
        )?;
        if changed != 1 {
            return Err(StateError::Validation(format!("task {id} is not open")));
        }
        task.description = description.into();
        Ok(task)
    }

    pub fn complete_task(&self, id: i64, completed_at: DateTime<Utc>) -> Result<Task, StateError> {
        let mut task = self.get_task(id)?;
        if task.completed_at.is_some() {
            return Err(StateError::Validation(format!(
                "task {id} is already completed"
            )));
        }
        let changed = self.transaction.execute(
            "UPDATE tasks SET completed_at=?1 WHERE id=?2 AND completed_at IS NULL",
            params![encode_time(completed_at), id],
        )?;
        if changed != 1 {
            return Err(StateError::Validation(format!("task {id} is not open")));
        }
        task.completed_at = Some(completed_at);
        Ok(task)
    }

    pub fn delete_task(&self, id: i64) -> Result<Task, StateError> {
        let task = self.get_task(id)?;
        if self
            .transaction
            .execute("DELETE FROM tasks WHERE id=?1", [id])?
            != 1
        {
            return Err(StateError::Validation(format!("task {id} not found")));
        }
        Ok(task)
    }
}

type RawTask = (i64, String, String, Option<String>);

fn raw_task(row: &Row<'_>) -> rusqlite::Result<RawTask> {
    Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?))
}

impl TryFrom<RawTask> for Task {
    type Error = StateError;

    fn try_from(raw: RawTask) -> Result<Self, Self::Error> {
        Ok(Self {
            id: raw.0,
            description: raw.1,
            started_at: decode_time(raw.2)?,
            completed_at: raw.3.map(decode_time).transpose()?,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::Store;

    #[test]
    fn task_lifecycle_is_global_deduplicated_and_persistent() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("state.sqlite");
        let started = Utc::now();
        let store = Store::new(&path).unwrap();
        let task = store
            .with_tx(|tx| tx.add_task("  Write Rust port  ", started))
            .unwrap();
        assert_eq!(task.description, "Write Rust port");
        assert!(
            store
                .with_tx(|tx| tx.add_task("write rust PORT", started))
                .is_err()
        );
        let completed = store
            .with_tx(|tx| {
                let updated = tx.update_task(task.id, "Ship Rust port")?;
                assert_eq!(updated.started_at, started);
                tx.complete_task(task.id, started + chrono::Duration::minutes(5))
            })
            .unwrap();
        assert_eq!(completed.status(), TaskStatus::Completed);
        assert!(
            store
                .with_tx(|tx| tx.update_task(task.id, "change it"))
                .is_err()
        );
        drop(store);

        let reopened = Store::new(path).unwrap();
        let tasks = reopened
            .with_tx(|tx| tx.list_tasks(TaskStatus::All))
            .unwrap();
        assert_eq!(tasks, vec![completed]);
    }

    #[test]
    fn transaction_rolls_back_all_mutations_on_error() {
        let directory = tempfile::tempdir().unwrap();
        let store = Store::new(directory.path().join("state.sqlite")).unwrap();
        let now = Utc::now();
        assert!(
            store
                .with_tx(|tx| {
                    tx.add_task("one", now)?;
                    tx.add_task("ONE", now)?;
                    Ok::<(), StateError>(())
                })
                .is_err()
        );
        assert!(
            store
                .with_tx(|tx| tx.list_tasks(TaskStatus::All))
                .unwrap()
                .is_empty()
        );
    }
}
