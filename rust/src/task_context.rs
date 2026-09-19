use std::{sync::Arc, time::Duration};

use thiserror::Error;

use crate::{
    memory::safe_fts_query,
    providers::{Embedder, ProviderError},
    state::{StateError, Store, TaskIndex, TaskReindexEntry, TaskSearchHit},
    vector,
};

const EMBEDDING_TIMEOUT: Duration = Duration::from_secs(20);

#[derive(Debug, Error)]
pub enum TaskContextError {
    #[error("task embedding failed: {0}")]
    Provider(#[from] ProviderError),
    #[error("task state failed: {0}")]
    State(#[from] StateError),
    #[error("task embedding timed out")]
    Timeout,
    #[error("task embedding dimensions do not match configuration")]
    Dimensions,
}

pub struct TaskContextService {
    store: Arc<Store>,
    embedder: Arc<dyn Embedder>,
    model: String,
    dimensions: usize,
    min_score: f64,
}

impl TaskContextService {
    pub fn new(
        store: Arc<Store>,
        embedder: Arc<dyn Embedder>,
        model: String,
        dimensions: usize,
        min_score: f64,
    ) -> Self {
        Self {
            store,
            embedder,
            model,
            dimensions,
            min_score,
        }
    }

    pub async fn prepare_index(&self, content: &str) -> Result<TaskIndex, TaskContextError> {
        let input = [format!("title: task | text: {}", content.trim())];
        let vectors = tokio::time::timeout(EMBEDDING_TIMEOUT, self.embedder.embed(&input))
            .await
            .map_err(|_| TaskContextError::Timeout)??;
        let [embedding] = vectors.as_slice() else {
            return Err(TaskContextError::Dimensions);
        };
        if embedding.len() != self.dimensions {
            return Err(TaskContextError::Dimensions);
        }
        Ok(TaskIndex {
            model: self.model.clone(),
            dimensions: self.dimensions,
            embedding: vector::pack(embedding),
        })
    }

    pub async fn prepare_reindex(&self) -> Result<Vec<TaskReindexEntry>, TaskContextError> {
        let documents = self.store.task_reindex_documents()?;
        let mut entries = Vec::with_capacity(documents.len());
        for (task_id, context_id, content) in documents {
            let index = self.prepare_index(&content).await?;
            entries.push(TaskReindexEntry {
                task_id,
                context_id,
                content,
                index,
            });
        }
        Ok(entries)
    }

    pub async fn search(
        &self,
        query: &str,
        keywords: &[String],
        include_completed: bool,
        limit: usize,
    ) -> Result<Vec<TaskSearchHit>, TaskContextError> {
        if query.trim().is_empty() {
            return Ok(Vec::new());
        }
        if !self.store.has_tasks()? {
            return Ok(Vec::new());
        }
        let input = [format!("task: search result | query: {}", query.trim())];
        let vectors = tokio::time::timeout(EMBEDDING_TIMEOUT, self.embedder.embed(&input))
            .await
            .map_err(|_| TaskContextError::Timeout)??;
        let [embedding] = vectors.as_slice() else {
            return Err(TaskContextError::Dimensions);
        };
        if embedding.len() != self.dimensions {
            return Err(TaskContextError::Dimensions);
        }
        let fts = if keywords.is_empty() {
            safe_fts_query(&[query.to_owned()])
        } else {
            safe_fts_query(keywords)
        };
        let mut hits =
            self.store
                .search_tasks(embedding, &fts, &self.model, include_completed, limit)?;
        hits.retain(|hit| hit.score >= self.min_score);
        Ok(hits)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::providers::ProviderError;

    struct FixedEmbedder;

    #[async_trait::async_trait]
    impl Embedder for FixedEmbedder {
        async fn embed(&self, inputs: &[String]) -> Result<Vec<Vec<f32>>, ProviderError> {
            Ok(inputs.iter().map(|_| vec![1.0, 0.0]).collect())
        }
        async fn tokenize(&self, _content: &str) -> Result<Vec<i32>, ProviderError> {
            unreachable!()
        }
        async fn detokenize(&self, _tokens: &[i32]) -> Result<String, ProviderError> {
            unreachable!()
        }
    }

    #[tokio::test]
    async fn offline_reindex_restores_missing_task_vectors() {
        let store = Arc::new(Store::new(":memory:").unwrap());
        let now = chrono::Utc::now();
        store.with_tx(|tx| tx.add_task("Study PCIe", now)).unwrap();
        assert!(store.validate_task_indexes("test", 2).is_err());
        let service = TaskContextService::new(
            Arc::clone(&store),
            Arc::new(FixedEmbedder),
            "test".into(),
            2,
            0.35,
        );
        let tasks = service.prepare_reindex().await.unwrap();
        store
            .replace_derived_indexes(&[], &[], &tasks, now)
            .unwrap();
        store.validate_task_indexes("test", 2).unwrap();
    }
}
