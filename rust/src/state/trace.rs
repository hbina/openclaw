use chrono::{DateTime, Utc};
use rusqlite::{OptionalExtension, params};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use super::{
    CONTENT_TEXT, ConversationChunk, StateError, StateTx, Store, decode_time, encode_time,
};

const MAX_TRACE_ERROR_BYTES: usize = 64 << 10;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RecallPlanContractReason {
    ExpectedSingleCall,
    InvalidCall,
    MalformedArguments,
    EmptyQuery,
    TooManyKeywords,
}

impl RecallPlanContractReason {
    fn as_str(self) -> &'static str {
        match self {
            Self::ExpectedSingleCall => "expected exactly one structured plan_recall call",
            Self::InvalidCall => "invalid plan_recall tool call",
            Self::MalformedArguments => "malformed plan_recall arguments",
            Self::EmptyQuery => "empty semantic query",
            Self::TooManyKeywords => "too many keywords",
        }
    }
}

#[derive(Debug, Clone)]
pub struct TraceInput {
    pub trigger_type: String,
    pub channel_id: String,
    pub sender_id: String,
    pub external_message_id: String,
    pub reminder_id: Option<i64>,
    pub input_json: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RagTrace {
    pub outcome: String,
    pub embedding_query: String,
    pub embedding_model: String,
    pub dimensions: usize,
    pub index_version: i64,
    pub minimum_score: f64,
    pub history_highwater_id: i64,
    pub candidate_count: usize,
    pub excluded_count: usize,
    pub qualified_count: usize,
    pub rendered_archive: String,
    pub matches: Vec<RagTraceMatch>,
    pub memory_matches: Vec<MemoryRagTraceMatch>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MemoryRagTraceMatch {
    pub rank: usize,
    pub memory_id: i64,
    pub revision_id: i64,
    pub vector_score: f64,
    pub keyword_score: f64,
    pub combined_score: f64,
    pub content_hash: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RagTraceMatch {
    pub rank: usize,
    pub start_history_id: i64,
    pub end_history_id: i64,
    pub similarity_score: f64,
    pub content_hash: String,
    pub messages_json: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct TraceSummary {
    pub id: i64,
    pub trigger_type: String,
    pub channel_id: String,
    pub sender_id: String,
    pub external_message_id: String,
    pub status: String,
    pub started_at: DateTime<Utc>,
    pub completed_at: Option<DateTime<Utc>>,
    pub final_content: String,
}

#[derive(Debug, Clone, Default)]
pub struct TraceFilter {
    pub limit: usize,
    pub channel_id: String,
    pub sender_id: String,
    pub status: String,
    pub external_message_id: String,
    pub since: Option<DateTime<Utc>>,
}

#[derive(Debug, Clone)]
pub struct TraceReport {
    pub trace: Value,
    pub events: Vec<Value>,
}

impl Store {
    pub fn start_response_trace(&self, input: &TraceInput) -> Result<i64, StateError> {
        let connection = self.lock()?;
        connection.execute(
            "INSERT INTO response_traces (trigger_type, channel_id, sender_id, external_message_id, reminder_id, input_json) VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
            params![
                input.trigger_type,
                input.channel_id,
                input.sender_id,
                input.external_message_id,
                input.reminder_id,
                input.input_json,
            ],
        )?;
        Ok(connection.last_insert_rowid())
    }

    pub fn link_trace_inbound(&self, trace_id: i64, history_id: i64) -> Result<(), StateError> {
        self.update_one(
            "UPDATE response_traces SET inbound_history_id=?1 WHERE id=?2",
            params![history_id, trace_id],
            "link trace inbound",
        )
    }

    pub fn finish_trace(
        &self,
        trace_id: i64,
        status: &str,
        stage: &str,
        cause: Option<&str>,
    ) -> Result<(), StateError> {
        self.update_one(
            "UPDATE response_traces SET status=?1, failure_stage=?2, error=?3, completed_at=CURRENT_TIMESTAMP WHERE id=?4",
            params![status, stage, trace_error(cause), trace_id],
            "finish response trace",
        )
    }

    fn begin_trace_event(&self, trace_id: i64, kind: &str) -> Result<i64, StateError> {
        self.with_tx(|tx| {
            let sequence: i64 = tx.transaction.query_row(
                "SELECT COALESCE(MAX(sequence_no), 0) + 1 FROM trace_events WHERE trace_id=?1",
                [trace_id],
                |row| row.get(0),
            )?;
            tx.transaction.execute(
                "INSERT INTO trace_events (trace_id, sequence_no, kind) VALUES (?1, ?2, ?3)",
                params![trace_id, sequence, kind],
            )?;
            Ok(tx.transaction.last_insert_rowid())
        })
    }

    fn finish_trace_event(
        &self,
        event_id: i64,
        status: &str,
        cause: Option<&str>,
    ) -> Result<(), StateError> {
        self.update_one(
            "UPDATE trace_events SET status=?1, error=?2, completed_at=CURRENT_TIMESTAMP WHERE id=?3",
            params![status, trace_error(cause), event_id],
            "finish trace event",
        )
    }

    pub fn record_rag_trace(
        &self,
        trace_id: i64,
        detail: &RagTrace,
        cause: Option<&str>,
    ) -> Result<(), StateError> {
        let event_id = self.begin_trace_event(trace_id, "rag")?;
        let status = if cause.is_some() {
            "failed"
        } else {
            "succeeded"
        };
        self.with_tx(|tx| {
            tx.transaction.execute(
                "INSERT INTO rag_retrievals (event_id, outcome, embedding_query, embedding_model, dimensions, index_version, minimum_score, history_highwater_id, candidate_count, excluded_count, qualified_count, selected_count, rendered_archive) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13)",
                params![
                    event_id,
                    detail.outcome,
                    detail.embedding_query,
                    detail.embedding_model,
                    detail.dimensions as i64,
                    detail.index_version,
                    detail.minimum_score,
                    detail.history_highwater_id,
                    detail.candidate_count as i64,
                    detail.excluded_count as i64,
                    detail.qualified_count as i64,
                    (detail.matches.len() + detail.memory_matches.len()) as i64,
                    detail.rendered_archive,
                ],
            )?;
            for item in &detail.matches {
                tx.transaction.execute(
                    "INSERT INTO rag_matches (retrieval_event_id, rank, start_history_id, end_history_id, similarity_score, content_hash, messages_json) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
                    params![event_id, item.rank as i64, item.start_history_id, item.end_history_id, item.similarity_score, item.content_hash, item.messages_json],
                )?;
            }
            for item in &detail.memory_matches {
                tx.transaction.execute(
                    "INSERT INTO memory_rag_matches (retrieval_event_id, rank, memory_id, revision_id, vector_score, keyword_score, combined_score, content_hash) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)",
                    params![event_id, item.rank as i64, item.memory_id, item.revision_id, item.vector_score, item.keyword_score, item.combined_score, item.content_hash],
                )?;
            }
            tx.transaction.execute(
                "UPDATE trace_events SET status=?1, error=?2, completed_at=CURRENT_TIMESTAMP WHERE id=?3",
                params![status, trace_error(cause), event_id],
            )?;
            Ok(())
        })
    }

    pub fn start_llm_call(
        &self,
        trace_id: i64,
        round: usize,
        purpose: &str,
        request_json: &str,
    ) -> Result<i64, StateError> {
        let event_id = self.begin_trace_event(trace_id, "llm")?;
        self.lock()?.execute(
            "INSERT INTO llm_calls (event_id, round_number, purpose, request_json) VALUES (?1, ?2, ?3, ?4)",
            params![event_id, round as i64, purpose, request_json],
        )?;
        Ok(event_id)
    }

    pub fn finish_llm_call(
        &self,
        event_id: i64,
        response_json: &str,
        http_status: u16,
        finish_reason: &str,
        cause: Option<&str>,
    ) -> Result<(), StateError> {
        let status = if cause.is_some() {
            "failed"
        } else {
            "succeeded"
        };
        self.with_tx(|tx| {
            tx.transaction.execute(
                "UPDATE llm_calls SET response_json=?1, http_status=?2, finish_reason=?3 WHERE event_id=?4",
                params![response_json, i64::from(http_status), finish_reason, event_id],
            )?;
            let changed = tx.transaction.execute(
                "UPDATE trace_events SET status=?1, error=?2, completed_at=CURRENT_TIMESTAMP WHERE id=?3",
                params![status, trace_error(cause), event_id],
            )?;
            if changed != 1 {
                return Err(StateError::Validation(format!(
                    "LLM trace event {event_id} not found"
                )));
            }
            Ok(())
        })
    }

    pub fn fail_recall_plan_call(
        &self,
        event_id: i64,
        reason: RecallPlanContractReason,
    ) -> Result<(), StateError> {
        self.update_one(
            "UPDATE trace_events SET status='failed', error=?1, completed_at=CURRENT_TIMESTAMP WHERE id=?2 AND status='succeeded' AND EXISTS (SELECT 1 FROM llm_calls WHERE event_id=trace_events.id AND purpose='recall_plan')",
            params![format!("recall planner contract: {}", reason.as_str()), event_id],
            "fail recall plan LLM event",
        )
    }

    pub fn start_tool_execution(
        &self,
        trace_id: i64,
        llm_event_id: i64,
        call_id: &str,
        name: &str,
        arguments: &str,
    ) -> Result<i64, StateError> {
        let event_id = self.begin_trace_event(trace_id, "tool")?;
        self.lock()?.execute(
            "INSERT INTO tool_executions (event_id, llm_event_id, tool_call_id, name, arguments_json) VALUES (?1, ?2, ?3, ?4, ?5)",
            params![event_id, llm_event_id, call_id, name, arguments],
        )?;
        Ok(event_id)
    }

    pub fn finish_tool_execution(
        &self,
        event_id: i64,
        result_json: &str,
        is_error: bool,
        mutation_committed: bool,
        cause: Option<&str>,
    ) -> Result<(), StateError> {
        let status = if cause.is_some() {
            "failed"
        } else {
            "succeeded"
        };
        self.with_tx(|tx| {
            tx.transaction.execute(
                "UPDATE tool_executions SET result_json=?1, is_error=?2, mutation_committed=?3 WHERE event_id=?4",
                params![result_json, is_error, mutation_committed, event_id],
            )?;
            tx.transaction.execute(
                "UPDATE trace_events SET status=?1, error=?2, completed_at=CURRENT_TIMESTAMP WHERE id=?3",
                params![status, trace_error(cause), event_id],
            )?;
            Ok(())
        })
    }

    pub fn record_response_output(
        &self,
        trace_id: i64,
        source_type: &str,
        source_llm_event_id: Option<i64>,
        source_content: &str,
        transformations_json: &str,
        final_content: &str,
    ) -> Result<i64, StateError> {
        let event_id = self.begin_trace_event(trace_id, "output")?;
        self.with_tx(|tx| {
            tx.transaction.execute(
                "INSERT INTO response_outputs (event_id, source_type, source_llm_event_id, source_content, transformations_json, final_content) VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
                params![event_id, source_type, source_llm_event_id, source_content, transformations_json, final_content],
            )?;
            tx.transaction.execute(
                "UPDATE trace_events SET status='succeeded', completed_at=CURRENT_TIMESTAMP WHERE id=?1",
                [event_id],
            )?;
            Ok(event_id)
        })
    }

    pub fn prepare_delivery(
        &self,
        trace_id: i64,
        output_event_id: i64,
        channel_id: &str,
        recipient_id: &str,
        content: &str,
    ) -> Result<i64, StateError> {
        let event_id = self.begin_trace_event(trace_id, "delivery")?;
        self.lock()?.execute(
            "INSERT INTO delivery_attempts (event_id, output_event_id, channel_id, recipient_id, content) VALUES (?1, ?2, ?3, ?4, ?5)",
            params![event_id, output_event_id, channel_id, recipient_id, content],
        )?;
        Ok(event_id)
    }

    pub fn mark_delivery_attempting(&self, event_id: i64) -> Result<(), StateError> {
        self.with_tx(|tx| {
            let changed = tx.transaction.execute(
                "UPDATE delivery_attempts SET attempted_at=CURRENT_TIMESTAMP WHERE event_id=?1",
                [event_id],
            )?;
            if changed != 1 {
                return Err(StateError::Validation(format!(
                    "delivery trace {event_id} not found"
                )));
            }
            tx.transaction.execute(
                "UPDATE trace_events SET status='attempting' WHERE id=?1",
                [event_id],
            )?;
            Ok(())
        })
    }

    pub fn fail_delivery(
        &self,
        trace_id: i64,
        event_id: i64,
        cause: &str,
    ) -> Result<(), StateError> {
        self.finish_trace_event(event_id, "failed", Some(cause))?;
        self.finish_trace(trace_id, "failed", "delivery", Some(cause))
    }

    pub fn complete_delivery(
        &self,
        trace_id: i64,
        event_id: i64,
        provider_message_id: &str,
        channel_id: &str,
        sender_id: &str,
        content: &str,
    ) -> Result<(), StateError> {
        self.complete_delivery_indexed(
            trace_id,
            event_id,
            provider_message_id,
            channel_id,
            sender_id,
            content,
            0,
            &[],
        )
    }

    #[allow(clippy::too_many_arguments)]
    pub fn complete_delivery_indexed(
        &self,
        trace_id: i64,
        event_id: i64,
        provider_message_id: &str,
        channel_id: &str,
        sender_id: &str,
        content: &str,
        start_history_id: i64,
        chunks: &[ConversationChunk],
    ) -> Result<(), StateError> {
        self.with_tx(|tx| {
            let history_id = tx.save_conversation_message(
                channel_id,
                sender_id,
                "assistant",
                CONTENT_TEXT,
                "conversation",
                content,
            )?;
            if !chunks.is_empty() {
                tx.save_conversation_chunks(start_history_id, history_id, chunks)?;
            }
            tx.transaction.execute(
                "UPDATE delivery_attempts SET provider_message_id=?1, conversation_history_id=?2, accepted_at=CURRENT_TIMESTAMP WHERE event_id=?3",
                params![provider_message_id, history_id, event_id],
            )?;
            tx.transaction.execute(
                "UPDATE trace_events SET status='succeeded', completed_at=CURRENT_TIMESTAMP WHERE id=?1",
                [event_id],
            )?;
            tx.transaction.execute(
                "UPDATE response_traces SET status='completed', completed_at=CURRENT_TIMESTAMP WHERE id=?1",
                [trace_id],
            )?;
            Ok(())
        })
    }

    pub fn list_response_traces(
        &self,
        filter: &TraceFilter,
    ) -> Result<Vec<TraceSummary>, StateError> {
        let limit = if filter.limit == 0 {
            20
        } else {
            filter.limit.min(1000)
        };
        let since = filter.since.map(encode_time);
        let connection = self.lock()?;
        let mut statement = connection.prepare(
            "SELECT DISTINCT t.id, t.trigger_type, t.channel_id, t.sender_id, t.external_message_id, t.status, t.started_at, t.completed_at, COALESCE(o.final_content, '')
             FROM response_traces t
             LEFT JOIN trace_events e ON e.trace_id=t.id AND e.kind='output'
             LEFT JOIN response_outputs o ON o.event_id=e.id
             LEFT JOIN trace_events de ON de.trace_id=t.id AND de.kind='delivery'
             LEFT JOIN delivery_attempts d ON d.event_id=de.id
             WHERE (?1='' OR t.channel_id=?1) AND (?2='' OR t.sender_id=?2)
               AND (?3='' OR t.status=?3)
               AND (?4='' OR t.external_message_id=?4 OR d.provider_message_id=?4)
               AND (?5 IS NULL OR julianday(t.started_at)>=julianday(?5))
             ORDER BY t.id DESC LIMIT ?6",
        )?;
        let raw = statement
            .query_map(
                params![
                    filter.channel_id,
                    filter.sender_id,
                    filter.status,
                    filter.external_message_id,
                    since,
                    limit as i64,
                ],
                |row| {
                    Ok((
                        row.get::<_, i64>(0)?,
                        row.get::<_, String>(1)?,
                        row.get::<_, String>(2)?,
                        row.get::<_, String>(3)?,
                        row.get::<_, String>(4)?,
                        row.get::<_, String>(5)?,
                        row.get::<_, String>(6)?,
                        row.get::<_, Option<String>>(7)?,
                        row.get::<_, String>(8)?,
                    ))
                },
            )?
            .collect::<Result<Vec<_>, _>>()?;
        raw.into_iter()
            .map(|item| {
                Ok(TraceSummary {
                    id: item.0,
                    trigger_type: item.1,
                    channel_id: item.2,
                    sender_id: item.3,
                    external_message_id: item.4,
                    status: item.5,
                    started_at: decode_time(item.6)?,
                    completed_at: item.7.map(decode_time).transpose()?,
                    final_content: item.8,
                })
            })
            .collect()
    }

    pub fn get_trace_report(&self, trace_id: i64) -> Result<TraceReport, StateError> {
        let connection = self.lock()?;
        let trace = connection
            .query_row(
                "SELECT trigger_type, channel_id, sender_id, external_message_id, reminder_id, input_json, inbound_history_id, status, failure_stage, error, started_at, completed_at FROM response_traces WHERE id=?1",
                [trace_id],
                |row| {
                    Ok((
                        row.get::<_, String>(0)?, row.get::<_, String>(1)?,
                        row.get::<_, String>(2)?, row.get::<_, String>(3)?,
                        row.get::<_, Option<i64>>(4)?, row.get::<_, String>(5)?,
                        row.get::<_, Option<i64>>(6)?, row.get::<_, String>(7)?,
                        row.get::<_, String>(8)?, row.get::<_, String>(9)?,
                        row.get::<_, String>(10)?, row.get::<_, Option<String>>(11)?,
                    ))
                },
            )
            .optional()?
            .ok_or_else(|| StateError::Validation(format!("trace {trace_id} not found")))?;
        let input = serde_json::from_str(&trace.5).unwrap_or(Value::String(trace.5));
        let mut trace_value = json!({
            "id": trace_id,
            "trigger_type": trace.0,
            "channel_id": trace.1,
            "sender_id": trace.2,
            "external_message_id": trace.3,
            "input": input,
            "status": trace.7,
            "failure_stage": trace.8,
            "error": trace.9,
            "started_at": decode_time(trace.10)?,
        });
        if let Some(value) = trace.4 {
            trace_value["reminder_id"] = json!(value);
        }
        if let Some(value) = trace.6 {
            trace_value["inbound_history_id"] = json!(value);
        }
        if let Some(value) = trace.11 {
            trace_value["completed_at"] = json!(decode_time(value)?);
        }

        let mut statement = connection.prepare(
            "SELECT id, sequence_no, kind, status, error, started_at, completed_at FROM trace_events WHERE trace_id=?1 ORDER BY sequence_no",
        )?;
        let event_rows = statement
            .query_map([trace_id], |row| {
                Ok((
                    row.get::<_, i64>(0)?,
                    row.get::<_, i64>(1)?,
                    row.get::<_, String>(2)?,
                    row.get::<_, String>(3)?,
                    row.get::<_, String>(4)?,
                    row.get::<_, String>(5)?,
                    row.get::<_, Option<String>>(6)?,
                ))
            })?
            .collect::<Result<Vec<_>, _>>()?;
        let mut events = Vec::with_capacity(event_rows.len());
        for event in event_rows {
            let mut value = json!({
                "id": event.0,
                "sequence_no": event.1,
                "kind": event.2,
                "status": event.3,
                "error": event.4,
                "started_at": decode_time(event.5)?,
                "detail": self.trace_event_detail_locked(&connection, event.0, &event.2)?,
            });
            if let Some(completed) = event.6 {
                value["completed_at"] = json!(decode_time(completed)?);
            }
            events.push(value);
        }
        Ok(TraceReport {
            trace: trace_value,
            events,
        })
    }

    fn trace_event_detail_locked(
        &self,
        connection: &rusqlite::Connection,
        event_id: i64,
        kind: &str,
    ) -> Result<Value, StateError> {
        let query = match kind {
            "rag" => {
                "SELECT json_object('outcome',outcome,'embedding_query',embedding_query,'embedding_model',embedding_model,'dimensions',dimensions,'index_version',index_version,'minimum_score',minimum_score,'history_highwater_id',history_highwater_id,'candidate_count',candidate_count,'excluded_count',excluded_count,'qualified_count',qualified_count,'selected_count',selected_count,'rendered_archive',rendered_archive) FROM rag_retrievals WHERE event_id=?1"
            }
            "llm" => {
                "SELECT json_object('round_number',round_number,'purpose',purpose,'request_json',request_json,'response_json',response_json,'http_status',http_status,'finish_reason',finish_reason) FROM llm_calls WHERE event_id=?1"
            }
            "tool" => {
                "SELECT json_object('llm_event_id',llm_event_id,'tool_call_id',tool_call_id,'name',name,'arguments_json',arguments_json,'result_json',result_json,'is_error',is_error,'mutation_committed',mutation_committed) FROM tool_executions WHERE event_id=?1"
            }
            "output" => {
                "SELECT json_object('source_type',source_type,'source_llm_event_id',source_llm_event_id,'source_content',source_content,'transformations_json',transformations_json,'final_content',final_content) FROM response_outputs WHERE event_id=?1"
            }
            "delivery" => {
                "SELECT json_object('output_event_id',output_event_id,'channel_id',channel_id,'recipient_id',recipient_id,'content',content,'provider_message_id',provider_message_id,'conversation_history_id',conversation_history_id,'attempted_at',attempted_at,'accepted_at',accepted_at) FROM delivery_attempts WHERE event_id=?1"
            }
            _ => return Ok(json!({})),
        };
        let raw: String = connection.query_row(query, [event_id], |row| row.get(0))?;
        let mut result: Value = serde_json::from_str(&raw)
            .map_err(|error| StateError::Validation(format!("invalid trace detail: {error}")))?;
        if kind == "rag" {
            let mut statement = connection.prepare("SELECT rank, start_history_id, end_history_id, similarity_score, content_hash, messages_json FROM rag_matches WHERE retrieval_event_id=?1 ORDER BY rank")?;
            let rows = statement
                .query_map([event_id], |row| {
                    Ok((
                        row.get::<_, i64>(0)?,
                        row.get::<_, i64>(1)?,
                        row.get::<_, i64>(2)?,
                        row.get::<_, f64>(3)?,
                        row.get::<_, String>(4)?,
                        row.get::<_, String>(5)?,
                    ))
                })?
                .collect::<Result<Vec<_>, _>>()?;
            let matches: Vec<_> = rows.into_iter().map(|item| json!({
                "rank": item.0, "start_history_id": item.1, "end_history_id": item.2,
                "similarity_score": item.3, "content_hash": item.4,
                "messages_json": serde_json::from_str::<Value>(&item.5).unwrap_or(Value::String(item.5)),
            })).collect();
            result["matches"] = Value::Array(matches);

            let mut statement = connection.prepare("SELECT rank, memory_id, revision_id, vector_score, keyword_score, combined_score, content_hash FROM memory_rag_matches WHERE retrieval_event_id=?1 ORDER BY rank")?;
            let rows = statement
                .query_map([event_id], |row| {
                    Ok(json!({
                        "rank": row.get::<_, i64>(0)?,
                        "memory_id": row.get::<_, i64>(1)?,
                        "revision_id": row.get::<_, i64>(2)?,
                        "vector_score": row.get::<_, f64>(3)?,
                        "keyword_score": row.get::<_, f64>(4)?,
                        "combined_score": row.get::<_, f64>(5)?,
                        "content_hash": row.get::<_, String>(6)?,
                    }))
                })?
                .collect::<Result<Vec<_>, _>>()?;
            result["memory_matches"] = Value::Array(rows);
        }
        Ok(result)
    }

    pub(crate) fn update_one<P: rusqlite::Params>(
        &self,
        query: &str,
        params: P,
        operation: &str,
    ) -> Result<(), StateError> {
        let changed = self.lock()?.execute(query, params)?;
        if changed != 1 {
            return Err(StateError::Validation(format!(
                "{operation} affected {changed} rows"
            )));
        }
        Ok(())
    }
}

impl StateTx<'_> {
    pub fn finish_tool_execution(
        &self,
        event_id: i64,
        result_json: &str,
        is_error: bool,
        mutation_committed: bool,
    ) -> Result<(), StateError> {
        if event_id == 0 {
            return Ok(());
        }
        self.transaction.execute(
            "UPDATE tool_executions SET result_json=?1, is_error=?2, mutation_committed=?3 WHERE event_id=?4",
            params![result_json, is_error, mutation_committed, event_id],
        )?;
        let changed = self.transaction.execute(
            "UPDATE trace_events SET status='succeeded', completed_at=CURRENT_TIMESTAMP WHERE id=?1",
            [event_id],
        )?;
        if changed != 1 {
            return Err(StateError::Validation(format!(
                "tool trace event {event_id} not found"
            )));
        }
        Ok(())
    }
}

fn trace_error(cause: Option<&str>) -> String {
    let Some(cause) = cause else {
        return String::new();
    };
    if cause.len() <= MAX_TRACE_ERROR_BYTES {
        return cause.into();
    }
    let mut end = MAX_TRACE_ERROR_BYTES;
    while !cause.is_char_boundary(end) {
        end -= 1;
    }
    cause[..end].into()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::{CONTENT_INBOUND_MESSAGE, Store};

    fn input() -> TraceInput {
        TraceInput {
            trigger_type: "chat".into(),
            channel_id: "telegram".into(),
            sender_id: "owner".into(),
            external_message_id: "in-7".into(),
            reminder_id: None,
            input_json: r#"{"content":"why?"}"#.into(),
        }
    }

    #[test]
    fn response_trace_persists_complete_diagnostic_chain() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("trace.sqlite");
        let store = Store::new(&path).unwrap();
        let trace_id = store.start_response_trace(&input()).unwrap();
        let history_id = store
            .save_conversation_message(
                "telegram",
                "owner",
                "user",
                CONTENT_INBOUND_MESSAGE,
                r#"{"content":"why?"}"#,
            )
            .unwrap();
        store.link_trace_inbound(trace_id, history_id).unwrap();
        store
            .record_rag_trace(
                trace_id,
                &RagTrace {
                    outcome: "selected".into(),
                    embedding_query: "query".into(),
                    embedding_model: "embed-v1".into(),
                    dimensions: 3,
                    index_version: 1,
                    minimum_score: 0.35,
                    history_highwater_id: history_id,
                    candidate_count: 2,
                    excluded_count: 0,
                    qualified_count: 1,
                    rendered_archive: "archive".into(),
                    matches: vec![RagTraceMatch {
                        rank: 1,
                        start_history_id: history_id,
                        end_history_id: history_id,
                        similarity_score: 0.8,
                        content_hash: "hash".into(),
                        messages_json: r#"[{"role":"user","content":"why?"}]"#.into(),
                    }],
                    memory_matches: vec![],
                },
                None,
            )
            .unwrap();
        let llm_id = store
            .start_llm_call(trace_id, 1, "chat", r#"{"messages":[]}"#)
            .unwrap();
        store
            .finish_llm_call(llm_id, r#"{"choices":[]}"#, 200, "stop", None)
            .unwrap();
        let output_id = store
            .record_response_output(trace_id, "llm", Some(llm_id), "raw", "[]", "final")
            .unwrap();
        let delivery_id = store
            .prepare_delivery(trace_id, output_id, "telegram", "owner", "final")
            .unwrap();
        store.mark_delivery_attempting(delivery_id).unwrap();
        store
            .complete_delivery(trace_id, delivery_id, "out-9", "telegram", "owner", "final")
            .unwrap();
        drop(store);

        let read_only = Store::open_read_only(path).unwrap();
        let traces = read_only
            .list_response_traces(&TraceFilter {
                external_message_id: "out-9".into(),
                ..Default::default()
            })
            .unwrap();
        assert_eq!(traces.len(), 1);
        assert_eq!(traces[0].status, "completed");
        assert_eq!(traces[0].final_content, "final");
        let report = read_only.get_trace_report(trace_id).unwrap();
        assert_eq!(report.events.len(), 4);
        assert!(read_only.start_response_trace(&input()).is_err());
    }

    #[test]
    fn failed_llm_trace_retains_malformed_provider_body() {
        let directory = tempfile::tempdir().unwrap();
        let store = Store::new(directory.path().join("state.sqlite")).unwrap();
        let trace_id = store.start_response_trace(&input()).unwrap();
        let event_id = store
            .start_llm_call(trace_id, 1, "chat", r#"{"messages":[]}"#)
            .unwrap();
        store
            .finish_llm_call(event_id, "{", 200, "", Some("decode response"))
            .unwrap();
        let report = store.get_trace_report(trace_id).unwrap();
        assert_eq!(report.events[0]["status"], "failed");
        assert_eq!(report.events[0]["detail"]["response_json"], "{");
    }

    #[test]
    fn recall_contract_failure_preserves_request_and_response() {
        let directory = tempfile::tempdir().unwrap();
        let store = Store::new(directory.path().join("state.sqlite")).unwrap();
        let trace_id = store.start_response_trace(&input()).unwrap();
        let event_id = store
            .start_llm_call(trace_id, 1, "recall_plan", r#"{"request":"exact"}"#)
            .unwrap();
        store
            .finish_llm_call(event_id, r#"{"response":"exact"}"#, 200, "stop", None)
            .unwrap();
        store
            .fail_recall_plan_call(event_id, RecallPlanContractReason::ExpectedSingleCall)
            .unwrap();
        let report = store.get_trace_report(trace_id).unwrap();
        assert_eq!(report.events[0]["status"], "failed");
        assert_eq!(
            report.events[0]["error"],
            "recall planner contract: expected exactly one structured plan_recall call"
        );
        assert_eq!(
            report.events[0]["detail"]["request_json"],
            r#"{"request":"exact"}"#
        );
    }
}
