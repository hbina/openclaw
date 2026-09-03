use std::{collections::HashMap, sync::Arc, time::Duration};

use chrono::{DateTime, Utc};
use serde::Deserialize;
use serde_json::json;
use thiserror::Error;

use crate::{
    providers::{
        Embedder, FunctionDefinition, GenerateRequest, Message, MessageRole, Provider,
        ProviderError, ToolCall, ToolDefinition,
    },
    state::{
        MemoryKind, MemoryOrigin, MemorySearchResult, MemorySource, MemoryWrite, StateError, Store,
    },
    vector,
};

pub const DEFAULT_SEARCH_LIMIT: usize = 5;
pub const MAX_SEARCH_LIMIT: usize = 20;
const SEARCH_TIMEOUT: Duration = Duration::from_secs(15);
const WRITE_TIMEOUT: Duration = Duration::from_secs(2 * 60);
const MODEL_TIMEOUT: Duration = Duration::from_secs(5 * 60);

#[derive(Debug, Error)]
pub enum MemoryError {
    #[error("{0}")]
    Validation(String),
    #[error("memory provider failed: {0}")]
    Provider(#[from] ProviderError),
    #[error("memory state failed: {0}")]
    State(#[from] StateError),
    #[error("memory operation timed out during {0}")]
    Timeout(&'static str),
    #[error("decode model memory contract: {0}")]
    Contract(#[from] serde_json::Error),
}

#[derive(Debug, Clone)]
pub struct Provenance {
    pub origin: MemoryOrigin,
    pub source: MemorySource,
    pub source_history_id: Option<i64>,
    pub source_trace_id: Option<i64>,
}

#[derive(Debug, Clone)]
pub struct PreparedSearch {
    pub query: String,
    pub fts_query: String,
    pub embedding: Vec<f32>,
    pub limit: usize,
}

pub struct Service {
    store: Arc<Store>,
    embedder: Arc<dyn Embedder>,
    index_id: String,
    dimensions: usize,
    min_score: f64,
    now: Arc<dyn Fn() -> DateTime<Utc> + Send + Sync>,
}

impl Service {
    pub fn new(
        store: Arc<Store>,
        embedder: Arc<dyn Embedder>,
        index_id: impl Into<String>,
        dimensions: usize,
        min_score: f64,
    ) -> Self {
        Self::with_clock(store, embedder, index_id, dimensions, min_score, Utc::now)
    }

    pub fn with_clock(
        store: Arc<Store>,
        embedder: Arc<dyn Embedder>,
        index_id: impl Into<String>,
        dimensions: usize,
        min_score: f64,
        now: impl Fn() -> DateTime<Utc> + Send + Sync + 'static,
    ) -> Self {
        Self {
            store,
            embedder,
            index_id: index_id.into(),
            dimensions,
            min_score,
            now: Arc::new(now),
        }
    }

    pub async fn prepare_write(
        &self,
        kind: MemoryKind,
        content: &str,
        provenance: Provenance,
    ) -> Result<MemoryWrite, MemoryError> {
        let observed_at = (self.now)();
        self.prepare_write_observed_at(kind, content, provenance, observed_at)
            .await
    }

    pub async fn prepare_write_observed_at(
        &self,
        kind: MemoryKind,
        content: &str,
        provenance: Provenance,
        observed_at: DateTime<Utc>,
    ) -> Result<MemoryWrite, MemoryError> {
        let content = content.trim();
        if content.is_empty() {
            return Err(MemoryError::Validation("content must not be empty".into()));
        }
        let input = vec![format!("title: memory | text: {content}")];
        let vectors = tokio::time::timeout(WRITE_TIMEOUT, self.embedder.embed(&input))
            .await
            .map_err(|_| MemoryError::Timeout("memory content embedding"))??;
        let [embedding] = vectors.as_slice() else {
            return Err(MemoryError::Validation(format!(
                "embedding server returned {} vectors, want 1",
                vectors.len()
            )));
        };
        if embedding.len() != self.dimensions {
            return Err(MemoryError::Validation(format!(
                "embedding server returned {} dimensions, want {}",
                embedding.len(),
                self.dimensions
            )));
        }
        Ok(MemoryWrite {
            kind,
            content: content.into(),
            origin_class: provenance.origin,
            source_kind: provenance.source,
            source_history_id: provenance.source_history_id,
            source_trace_id: provenance.source_trace_id,
            embedding_model: self.index_id.clone(),
            dimensions: self.dimensions,
            embedding: vector::pack(embedding),
            observed_at: Some(observed_at),
            now: (self.now)(),
        })
    }

    pub async fn prepare_search(
        &self,
        query: &str,
        keywords: &[String],
        limit: usize,
    ) -> Result<PreparedSearch, MemoryError> {
        let query = query.trim();
        if query.is_empty() {
            return Err(MemoryError::Validation("query must not be empty".into()));
        }
        let limit = if limit == 0 {
            DEFAULT_SEARCH_LIMIT
        } else if limit > MAX_SEARCH_LIMIT {
            return Err(MemoryError::Validation(format!(
                "max_results must be at most {MAX_SEARCH_LIMIT}"
            )));
        } else {
            limit
        };
        let input = vec![format!("task: search result | query: {query}")];
        let vectors = tokio::time::timeout(SEARCH_TIMEOUT, self.embedder.embed(&input))
            .await
            .map_err(|_| MemoryError::Timeout("memory query embedding"))??;
        let [embedding] = vectors.as_slice() else {
            return Err(MemoryError::Validation(format!(
                "embedding server returned {} vectors, want 1",
                vectors.len()
            )));
        };
        let fts_query = if keywords.is_empty() {
            safe_fts_query(&[query.to_owned()])
        } else {
            safe_fts_query(keywords)
        };
        Ok(PreparedSearch {
            query: query.into(),
            fts_query,
            embedding: embedding.clone(),
            limit,
        })
    }

    pub async fn search(
        &self,
        query: &str,
        keywords: &[String],
        limit: usize,
    ) -> Result<Vec<MemorySearchResult>, MemoryError> {
        let prepared = self.prepare_search(query, keywords, limit).await?;
        Ok(self.store.search_memories(
            &self.index_id,
            self.dimensions,
            &prepared.embedding,
            &prepared.fts_query,
            self.min_score,
            prepared.limit,
        )?)
    }

    pub async fn search_with_model(
        &self,
        model: &dyn Provider,
        query: &str,
        limit: usize,
    ) -> Result<Vec<MemorySearchResult>, MemoryError> {
        let mut plan_request = GenerateRequest {
            model: "default".into(),
            messages: vec![
                Message::text(
                    MessageRole::System,
                    "Formulate a semantic query and literal keywords for searching the owner's local memory. Always call plan_memory_search.",
                ),
                Message::text(MessageRole::User, query),
            ],
            tools: vec![tool(
                "plan_memory_search",
                "Plan one local memory search.",
                json!({
                    "type": "object",
                    "additionalProperties": false,
                    "properties": {
                        "semantic_query": {"type": "string"},
                        "keywords": {"type": "array", "maxItems": 12, "items": {"type": "string"}}
                    },
                    "required": ["semantic_query", "keywords"]
                }),
            )],
            tool_choice: "required".into(),
            max_tokens: 256,
            ..Default::default()
        };
        let response = tokio::time::timeout(MODEL_TIMEOUT, model.generate(&mut plan_request))
            .await
            .map_err(|_| MemoryError::Timeout("memory search planning"))??;
        let call = one_tool(&response.message, "plan_memory_search")?;
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Plan {
            semantic_query: String,
            keywords: Vec<String>,
        }
        let plan: Plan = serde_json::from_str(&call.function.arguments)?;
        let candidates = self
            .search(&plan.semantic_query, &plan.keywords, MAX_SEARCH_LIMIT)
            .await?;

        let effective_limit = if limit == 0 {
            DEFAULT_SEARCH_LIMIT
        } else if limit > MAX_SEARCH_LIMIT {
            return Err(MemoryError::Validation(format!(
                "max_results must be at most {MAX_SEARCH_LIMIT}"
            )));
        } else {
            limit
        };
        let payload = serde_json::to_string(&json!({
            "query": query,
            "candidates": &candidates,
            "max_results": effective_limit
        }))?;
        let mut select_request = GenerateRequest {
            model: "default".into(),
            messages: vec![
                Message::text(
                    MessageRole::System,
                    "Select only memories that materially answer the query. Always call select_memories, including with an empty list.",
                ),
                Message::text(MessageRole::User, payload),
            ],
            tools: vec![tool(
                "select_memories",
                "Select relevant Memory IDs in relevance order.",
                json!({
                    "type": "object",
                    "additionalProperties": false,
                    "properties": {
                        "memory_ids": {"type": "array", "maxItems": 20, "items": {"type": "integer", "minimum": 1}}
                    },
                    "required": ["memory_ids"]
                }),
            )],
            tool_choice: "required".into(),
            max_tokens: 256,
            ..Default::default()
        };
        let response = tokio::time::timeout(MODEL_TIMEOUT, model.generate(&mut select_request))
            .await
            .map_err(|_| MemoryError::Timeout("memory search reranking"))??;
        let call = one_tool(&response.message, "select_memories")?;
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Selection {
            memory_ids: Vec<i64>,
        }
        let selected: Selection = serde_json::from_str(&call.function.arguments)?;
        let mut by_id: HashMap<_, _> = candidates
            .into_iter()
            .map(|candidate| (candidate.memory.id, candidate))
            .collect();
        let mut result = Vec::new();
        for id in selected.memory_ids {
            if result.len() == effective_limit {
                break;
            }
            let Some(item) = by_id.remove(&id) else {
                return Err(MemoryError::Validation(format!(
                    "model selected duplicate or unknown Memory ID {id}"
                )));
            };
            result.push(item);
        }
        Ok(result)
    }

    pub async fn prepare_reindex(
        &self,
    ) -> Result<Vec<crate::state::MemoryReindexEntry>, MemoryError> {
        let active = self.store.list_all_active_memories()?;
        let mut entries = Vec::with_capacity(active.len());
        for batch in active.chunks(16) {
            let inputs: Vec<_> = batch
                .iter()
                .map(|item| format!("title: memory | text: {}", item.content))
                .collect();
            let vectors = tokio::time::timeout(WRITE_TIMEOUT, self.embedder.embed(&inputs))
                .await
                .map_err(|_| MemoryError::Timeout("memory reindex batch"))??;
            if vectors.len() != inputs.len() {
                return Err(MemoryError::Validation(
                    "memory reindex embedding count mismatch".into(),
                ));
            }
            for (item, embedding) in batch.iter().zip(vectors) {
                if embedding.len() != self.dimensions {
                    return Err(MemoryError::Validation(format!(
                        "memory reindex embedding dimensions {} do not match configured {}",
                        embedding.len(),
                        self.dimensions
                    )));
                }
                entries.push(crate::state::MemoryReindexEntry {
                    memory_id: item.id,
                    revision_id: item.revision_id,
                    content: item.content.clone(),
                    embedding_model: self.index_id.clone(),
                    dimensions: self.dimensions,
                    embedding: vector::pack(&embedding),
                });
            }
        }
        Ok(entries)
    }

    pub fn store(&self) -> &Arc<Store> {
        &self.store
    }

    pub fn index_id(&self) -> &str {
        &self.index_id
    }

    pub fn dimensions(&self) -> usize {
        self.dimensions
    }

    pub fn min_score(&self) -> f64 {
        self.min_score
    }
}

fn tool(name: &str, description: &str, parameters: serde_json::Value) -> ToolDefinition {
    ToolDefinition {
        kind: "function".into(),
        function: FunctionDefinition {
            name: name.into(),
            description: description.into(),
            parameters,
        },
    }
}

fn one_tool<'a>(message: &'a Message, name: &str) -> Result<&'a ToolCall, MemoryError> {
    let [call] = message.tool_calls.as_slice() else {
        return Err(MemoryError::Validation(format!(
            "model did not return exactly one {name} call"
        )));
    };
    if !message.content.trim().is_empty()
        || call.kind != "function"
        || call.function.name != name
        || call.id.is_empty()
    {
        return Err(MemoryError::Validation(format!(
            "model returned invalid {name} call"
        )));
    }
    Ok(call)
}

pub fn safe_fts_query(values: &[String]) -> String {
    let mut seen = std::collections::HashSet::new();
    let mut terms = Vec::new();
    for value in values {
        let mut term = String::new();
        for character in value.to_lowercase().chars().chain(std::iter::once(' ')) {
            if character.is_alphanumeric() || character == '_' {
                term.push(character);
                continue;
            }
            if term.chars().count() >= 2 && seen.insert(term.clone()) {
                terms.push(format!(r#""{}""#, term.replace('"', "\"\"")));
                if terms.len() == 16 {
                    return terms.join(" OR ");
                }
            }
            term.clear();
        }
    }
    terms.join(" OR ")
}

#[cfg(test)]
mod tests {
    use super::*;

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

    #[test]
    fn fts_query_is_safe_deduplicated_and_bounded() {
        assert_eq!(
            safe_fts_query(&["Espresso OR tea; espresso x 東京".into()]),
            r#""espresso" OR "or" OR "tea" OR "東京""#
        );
        let many: Vec<_> = (0..30).map(|index| format!("term{index}")).collect();
        assert_eq!(safe_fts_query(&many).split(" OR ").count(), 16);
    }

    #[tokio::test]
    async fn prepares_write_and_search_with_canonical_embedding_prefixes() {
        let directory = tempfile::tempdir().unwrap();
        let store = Arc::new(Store::new(directory.path().join("state.sqlite")).unwrap());
        let now = Utc::now();
        let service = Service::with_clock(
            store,
            Arc::new(FixedEmbedder),
            "test-model",
            2,
            0.35,
            move || now,
        );
        let write = service
            .prepare_write(
                MemoryKind::Durable,
                "  likes espresso ",
                Provenance {
                    origin: MemoryOrigin::Owner,
                    source: MemorySource::Chat,
                    source_history_id: Some(4),
                    source_trace_id: Some(2),
                },
            )
            .await
            .unwrap();
        assert_eq!(write.content, "likes espresso");
        assert_eq!(write.embedding, vector::pack(&[1.0, 0.0]));
        let search = service
            .prepare_search(" espresso habits ", &[], 0)
            .await
            .unwrap();
        assert_eq!(search.query, "espresso habits");
        assert_eq!(search.limit, DEFAULT_SEARCH_LIMIT);
        assert_eq!(search.fts_query, r#""espresso" OR "habits""#);
    }
}
