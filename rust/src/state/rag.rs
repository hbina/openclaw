use rusqlite::params;

use super::{StateError, StateTx, Store};

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ConversationChunk {
    pub part_index: i64,
    pub content_hash: String,
    pub embedding_model: String,
    pub dimensions: usize,
    pub index_version: i64,
    pub embedding: Vec<u8>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ConversationEmbedding {
    pub id: i64,
    pub start_history_id: i64,
    pub end_history_id: i64,
    pub embedding: Vec<u8>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ConversationReindexEntry {
    pub start_history_id: i64,
    pub end_history_id: i64,
    pub chunks: Vec<ConversationChunk>,
}

impl StateTx<'_> {
    pub fn save_conversation_chunks(
        &self,
        start_history_id: i64,
        end_history_id: i64,
        chunks: &[ConversationChunk],
    ) -> Result<(), StateError> {
        if start_history_id < 1 || end_history_id < start_history_id || chunks.is_empty() {
            return Err(StateError::Validation(
                "complete conversation chunk range is required".into(),
            ));
        }
        for chunk in chunks {
            if chunk.embedding.is_empty() {
                return Err(StateError::Validation(format!(
                    "conversation chunk {} has no embedding",
                    chunk.part_index
                )));
            }
            self.transaction.execute(
                "INSERT INTO conversation_chunks (start_history_id, end_history_id, part_index, content_hash, embedding_model, dimensions, index_version, embedding) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)",
                params![
                    start_history_id,
                    end_history_id,
                    chunk.part_index,
                    chunk.content_hash,
                    chunk.embedding_model,
                    chunk.dimensions as i64,
                    chunk.index_version,
                    chunk.embedding,
                ],
            )?;
        }
        Ok(())
    }
}

impl Store {
    pub fn load_conversation_embeddings(
        &self,
        model: &str,
        version: i64,
        dimensions: usize,
    ) -> Result<Vec<ConversationEmbedding>, StateError> {
        let connection = self.lock()?;
        let mut statement = connection.prepare(
            "SELECT id, start_history_id, end_history_id, embedding FROM conversation_chunks WHERE embedding_model=?1 AND index_version=?2 AND dimensions=?3 ORDER BY id ASC",
        )?;
        Ok(statement
            .query_map(params![model, version, dimensions as i64], |row| {
                Ok(ConversationEmbedding {
                    id: row.get(0)?,
                    start_history_id: row.get(1)?,
                    end_history_id: row.get(2)?,
                    embedding: row.get(3)?,
                })
            })?
            .collect::<Result<_, _>>()?)
    }

    pub fn count_conversation_index_gaps(
        &self,
        model: &str,
        version: i64,
        dimensions: usize,
    ) -> Result<usize, StateError> {
        let count: i64 = self.lock()?.query_row(
            "WITH completed AS (
                SELECT u.id AS start_id,
                    (SELECT MAX(a.id) FROM conversation_history a
                     WHERE a.channel_id=u.channel_id AND a.sender_id=u.sender_id
                       AND a.audience='conversation' AND a.id>u.id
                       AND a.role='assistant' AND a.content_type='text'
                       AND NOT EXISTS (
                           SELECT 1 FROM conversation_history next_u
                           WHERE next_u.channel_id=u.channel_id AND next_u.sender_id=u.sender_id
                             AND next_u.audience='conversation' AND next_u.role='user'
                             AND next_u.id>u.id AND next_u.id<a.id
                       )) AS end_id
                FROM conversation_history u
                WHERE u.audience='conversation' AND u.role='user'
            )
            SELECT count(*) FROM completed c
            WHERE c.end_id IS NOT NULL AND NOT EXISTS (
                SELECT 1 FROM conversation_chunks x
                WHERE x.start_history_id=c.start_id AND x.end_history_id=c.end_id
                  AND x.embedding_model=?1 AND x.index_version=?2 AND x.dimensions=?3
            )",
            params![model, version, dimensions as i64],
            |row| row.get(0),
        )?;
        Ok(count as usize)
    }

    pub fn replace_conversation_indexes(
        &self,
        entries: &[ConversationReindexEntry],
    ) -> Result<(), StateError> {
        self.with_tx(|tx| {
            tx.transaction
                .execute("DELETE FROM conversation_chunks", [])?;
            for entry in entries {
                tx.save_conversation_chunks(
                    entry.start_history_id,
                    entry.end_history_id,
                    &entry.chunks,
                )?;
            }
            Ok(())
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::{CONTENT_TEXT, Store};

    #[test]
    fn indexes_complete_exchanges_and_detects_gaps() {
        let directory = tempfile::tempdir().unwrap();
        let store = Store::new(directory.path().join("state.sqlite")).unwrap();
        let start = store
            .save_conversation_message("telegram", "owner", "user", CONTENT_TEXT, "question")
            .unwrap();
        let end = store
            .save_conversation_message("telegram", "owner", "assistant", CONTENT_TEXT, "answer")
            .unwrap();
        assert_eq!(
            store
                .count_conversation_index_gaps("embed-v1", 1, 2)
                .unwrap(),
            1
        );
        let chunk = ConversationChunk {
            part_index: 0,
            content_hash: "hash".into(),
            embedding_model: "embed-v1".into(),
            dimensions: 2,
            index_version: 1,
            embedding: vec![0; 8],
        };
        store
            .with_tx(|tx| tx.save_conversation_chunks(start, end, &[chunk]))
            .unwrap();
        assert_eq!(
            store
                .count_conversation_index_gaps("embed-v1", 1, 2)
                .unwrap(),
            0
        );
        let loaded = store
            .load_conversation_embeddings("embed-v1", 1, 2)
            .unwrap();
        assert_eq!(loaded.len(), 1);
        assert_eq!(loaded[0].start_history_id, start);
        assert_eq!(loaded[0].end_history_id, end);
    }
}
