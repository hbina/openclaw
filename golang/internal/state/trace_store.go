package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const maxTraceErrorBytes = 64 << 10

type TraceInput struct {
	TriggerType       string
	ChannelID         string
	SenderID          string
	ExternalMessageID string
	ReminderID        *int
	InputJSON         string
}

type RAGTrace struct {
	Outcome            string                `json:"outcome"`
	EmbeddingQuery     string                `json:"embedding_query"`
	EmbeddingModel     string                `json:"embedding_model"`
	Dimensions         int                   `json:"dimensions"`
	IndexVersion       int                   `json:"index_version"`
	MinimumScore       float64               `json:"minimum_score"`
	HistoryHighwaterID int                   `json:"history_highwater_id"`
	CandidateCount     int                   `json:"candidate_count"`
	ExcludedCount      int                   `json:"excluded_count"`
	QualifiedCount     int                   `json:"qualified_count"`
	RenderedArchive    string                `json:"rendered_archive"`
	Matches            []RAGTraceMatch       `json:"matches"`
	MemoryMatches      []MemoryRAGTraceMatch `json:"memory_matches"`
}

type MemoryRAGTraceMatch struct {
	Rank          int     `json:"rank"`
	MemoryID      int64   `json:"memory_id"`
	RevisionID    int64   `json:"revision_id"`
	VectorScore   float64 `json:"vector_score"`
	KeywordScore  float64 `json:"keyword_score"`
	CombinedScore float64 `json:"combined_score"`
	ContentHash   string  `json:"content_hash"`
}

type RAGTraceMatch struct {
	Rank            int     `json:"rank"`
	StartHistoryID  int     `json:"start_history_id"`
	EndHistoryID    int     `json:"end_history_id"`
	SimilarityScore float64 `json:"similarity_score"`
	ContentHash     string  `json:"content_hash"`
	MessagesJSON    string  `json:"messages_json"`
}

type TraceSummary struct {
	ID                int64      `json:"id"`
	TriggerType       string     `json:"trigger_type"`
	ChannelID         string     `json:"channel_id"`
	SenderID          string     `json:"sender_id"`
	ExternalMessageID string     `json:"external_message_id"`
	Status            string     `json:"status"`
	StartedAt         time.Time  `json:"started_at"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	FinalContent      string     `json:"final_content"`
}

type TraceFilter struct {
	Limit             int
	ChannelID         string
	SenderID          string
	Status            string
	ExternalMessageID string
	Since             *time.Time
}

type TraceReport struct {
	Trace  map[string]any   `json:"trace"`
	Events []map[string]any `json:"events"`
}

func traceError(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if len(value) > maxTraceErrorBytes {
		value = value[:maxTraceErrorBytes]
	}
	return value
}

func (s *Store) StartResponseTrace(ctx context.Context, input TraceInput) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO response_traces
			(trigger_type, channel_id, sender_id, external_message_id, reminder_id, input_json)
		VALUES (?, ?, ?, ?, ?, ?)`,
		input.TriggerType, input.ChannelID, input.SenderID, input.ExternalMessageID, input.ReminderID, input.InputJSON,
	)
	if err != nil {
		return 0, fmt.Errorf("start response trace: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read response trace id: %w", err)
	}
	return id, nil
}

func (s *Store) LinkTraceInbound(ctx context.Context, traceID, historyID int64) error {
	return s.updateOne(ctx, `UPDATE response_traces SET inbound_history_id = ? WHERE id = ?`, "link trace inbound", historyID, traceID)
}

func (s *Store) FinishTrace(ctx context.Context, traceID int64, status, stage string, cause error) error {
	return s.updateOne(ctx, `UPDATE response_traces SET status = ?, failure_stage = ?, error = ?, completed_at = CURRENT_TIMESTAMP WHERE id = ?`,
		"finish response trace", status, stage, traceError(cause), traceID)
}

func (s *Store) beginTraceEvent(ctx context.Context, traceID int64, kind string) (int64, error) {
	var eventID int64
	err := s.WithTx(ctx, func(tx *Tx) error {
		var sequence int
		if err := tx.tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence_no), 0) + 1 FROM trace_events WHERE trace_id = ?`, traceID).Scan(&sequence); err != nil {
			return fmt.Errorf("allocate trace event sequence: %w", err)
		}
		result, err := tx.tx.ExecContext(ctx, `INSERT INTO trace_events (trace_id, sequence_no, kind) VALUES (?, ?, ?)`, traceID, sequence, kind)
		if err != nil {
			return fmt.Errorf("start trace event: %w", err)
		}
		eventID, err = result.LastInsertId()
		return err
	})
	return eventID, err
}

func (s *Store) finishTraceEvent(ctx context.Context, eventID int64, status string, cause error) error {
	return s.updateOne(ctx, `UPDATE trace_events SET status = ?, error = ?, completed_at = CURRENT_TIMESTAMP WHERE id = ?`,
		"finish trace event", status, traceError(cause), eventID)
}

func (s *Store) RecordRAGTrace(ctx context.Context, traceID int64, detail RAGTrace, cause error) error {
	eventID, err := s.beginTraceEvent(ctx, traceID, "rag")
	if err != nil {
		return err
	}
	status := "succeeded"
	if cause != nil {
		status = "failed"
	}
	err = s.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.tx.ExecContext(ctx, `INSERT INTO rag_retrievals
			(event_id, outcome, embedding_query, embedding_model, dimensions, index_version, minimum_score,
			 history_highwater_id, candidate_count, excluded_count, qualified_count, selected_count, rendered_archive)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, eventID, detail.Outcome, detail.EmbeddingQuery,
			detail.EmbeddingModel, detail.Dimensions, detail.IndexVersion, detail.MinimumScore,
			detail.HistoryHighwaterID, detail.CandidateCount, detail.ExcludedCount, detail.QualifiedCount,
			len(detail.Matches)+len(detail.MemoryMatches), detail.RenderedArchive)
		if err != nil {
			return fmt.Errorf("record RAG retrieval: %w", err)
		}
		for _, match := range detail.Matches {
			if _, err := tx.tx.ExecContext(ctx, `INSERT INTO rag_matches
				(retrieval_event_id, rank, start_history_id, end_history_id, similarity_score, content_hash, messages_json)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, eventID, match.Rank, match.StartHistoryID, match.EndHistoryID,
				match.SimilarityScore, match.ContentHash, match.MessagesJSON); err != nil {
				return fmt.Errorf("record RAG match: %w", err)
			}
		}
		for _, match := range detail.MemoryMatches {
			if _, err := tx.tx.ExecContext(ctx, `INSERT INTO memory_rag_matches
				(retrieval_event_id,rank,memory_id,revision_id,vector_score,keyword_score,combined_score,content_hash)
				VALUES (?,?,?,?,?,?,?,?)`, eventID, match.Rank, match.MemoryID, match.RevisionID, match.VectorScore, match.KeywordScore, match.CombinedScore, match.ContentHash); err != nil {
				return fmt.Errorf("record memory RAG match: %w", err)
			}
		}
		_, err = tx.tx.ExecContext(ctx, `UPDATE trace_events SET status = ?, error = ?, completed_at = CURRENT_TIMESTAMP WHERE id = ?`, status, traceError(cause), eventID)
		return err
	})
	return err
}

func (s *Store) StartLLMCall(ctx context.Context, traceID int64, round int, purpose, requestJSON string) (int64, error) {
	eventID, err := s.beginTraceEvent(ctx, traceID, "llm")
	if err != nil {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO llm_calls (event_id, round_number, purpose, request_json) VALUES (?, ?, ?, ?)`, eventID, round, purpose, requestJSON); err != nil {
		return 0, fmt.Errorf("record LLM request: %w", err)
	}
	return eventID, nil
}

func (s *Store) FinishLLMCall(ctx context.Context, eventID int64, responseJSON string, httpStatus int, finishReason string, cause error) error {
	status := "succeeded"
	if cause != nil {
		status = "failed"
	}
	return s.WithTx(ctx, func(tx *Tx) error {
		if _, err := tx.tx.ExecContext(ctx, `UPDATE llm_calls SET response_json = ?, http_status = ?, finish_reason = ? WHERE event_id = ?`, responseJSON, httpStatus, finishReason, eventID); err != nil {
			return err
		}
		result, err := tx.tx.ExecContext(ctx, `UPDATE trace_events SET status = ?, error = ?, completed_at = CURRENT_TIMESTAMP WHERE id = ?`, status, traceError(cause), eventID)
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return fmt.Errorf("LLM trace event %d not found", eventID)
		}
		return nil
	})
}

func (s *Store) StartToolExecution(ctx context.Context, traceID, llmEventID int64, callID, name, arguments string) (int64, error) {
	eventID, err := s.beginTraceEvent(ctx, traceID, "tool")
	if err != nil {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tool_executions (event_id, llm_event_id, tool_call_id, name, arguments_json) VALUES (?, ?, ?, ?, ?)`, eventID, llmEventID, callID, name, arguments); err != nil {
		return 0, fmt.Errorf("record tool execution: %w", err)
	}
	return eventID, nil
}

func (s *Store) FinishToolExecution(ctx context.Context, eventID int64, resultJSON string, isError, mutationCommitted bool, cause error) error {
	status := "succeeded"
	if cause != nil {
		status = "failed"
	}
	return s.WithTx(ctx, func(tx *Tx) error {
		if _, err := tx.tx.ExecContext(ctx, `UPDATE tool_executions SET result_json = ?, is_error = ?, mutation_committed = ? WHERE event_id = ?`, resultJSON, isError, mutationCommitted, eventID); err != nil {
			return err
		}
		_, err := tx.tx.ExecContext(ctx, `UPDATE trace_events SET status = ?, error = ?, completed_at = CURRENT_TIMESTAMP WHERE id = ?`, status, traceError(cause), eventID)
		return err
	})
}

// FinishToolExecution updates provenance inside the caller's transaction so a
// tool mutation, model-visible tool result, and diagnostic outcome agree.
func (tx *Tx) FinishToolExecution(ctx context.Context, eventID int64, resultJSON string, isError, mutationCommitted bool) error {
	if eventID == 0 {
		return nil
	}
	if _, err := tx.tx.ExecContext(ctx, `UPDATE tool_executions SET result_json = ?, is_error = ?, mutation_committed = ? WHERE event_id = ?`, resultJSON, isError, mutationCommitted, eventID); err != nil {
		return err
	}
	result, err := tx.tx.ExecContext(ctx, `UPDATE trace_events SET status = 'succeeded', completed_at = CURRENT_TIMESTAMP WHERE id = ?`, eventID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("tool trace event %d not found", eventID)
	}
	return nil
}

func (s *Store) RecordResponseOutput(ctx context.Context, traceID int64, sourceType string, sourceLLMEventID *int64, sourceContent, transformationsJSON, finalContent string) (int64, error) {
	eventID, err := s.beginTraceEvent(ctx, traceID, "output")
	if err != nil {
		return 0, err
	}
	err = s.WithTx(ctx, func(tx *Tx) error {
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO response_outputs (event_id, source_type, source_llm_event_id, source_content, transformations_json, final_content) VALUES (?, ?, ?, ?, ?, ?)`, eventID, sourceType, sourceLLMEventID, sourceContent, transformationsJSON, finalContent); err != nil {
			return err
		}
		_, err := tx.tx.ExecContext(ctx, `UPDATE trace_events SET status = 'succeeded', completed_at = CURRENT_TIMESTAMP WHERE id = ?`, eventID)
		return err
	})
	return eventID, err
}

func (s *Store) PrepareDelivery(ctx context.Context, traceID, outputEventID int64, channelID, recipientID, content string) (int64, error) {
	eventID, err := s.beginTraceEvent(ctx, traceID, "delivery")
	if err != nil {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO delivery_attempts (event_id, output_event_id, channel_id, recipient_id, content) VALUES (?, ?, ?, ?, ?)`, eventID, outputEventID, channelID, recipientID, content); err != nil {
		return 0, fmt.Errorf("prepare delivery trace: %w", err)
	}
	return eventID, nil
}

func (s *Store) MarkDeliveryAttempting(ctx context.Context, eventID int64) error {
	return s.WithTx(ctx, func(tx *Tx) error {
		result, err := tx.tx.ExecContext(ctx, `UPDATE delivery_attempts SET attempted_at = CURRENT_TIMESTAMP WHERE event_id = ?`, eventID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("delivery trace %d not found", eventID)
		}
		_, err = tx.tx.ExecContext(ctx, `UPDATE trace_events SET status = 'attempting' WHERE id = ?`, eventID)
		return err
	})
}

func (s *Store) FailDelivery(ctx context.Context, traceID, eventID int64, cause error) error {
	if err := s.finishTraceEvent(ctx, eventID, "failed", cause); err != nil {
		return err
	}
	return s.FinishTrace(ctx, traceID, "failed", "delivery", cause)
}

func (s *Store) CompleteDelivery(ctx context.Context, traceID, eventID int64, providerMessageID string, channelID, senderID, content string) error {
	return s.CompleteDeliveryIndexed(ctx, traceID, eventID, providerMessageID, channelID, senderID, content, 0, nil)
}

func (s *Store) CompleteDeliveryIndexed(ctx context.Context, traceID, eventID int64, providerMessageID string, channelID, senderID, content string, startHistoryID int64, chunks []ConversationChunk) error {
	return s.WithTx(ctx, func(tx *Tx) error {
		result, err := tx.tx.ExecContext(ctx, `INSERT INTO conversation_history (channel_id, sender_id, role, content_type, content) VALUES (?, ?, 'assistant', ?, ?)`, channelID, senderID, ContentText, content)
		if err != nil {
			return err
		}
		historyID, err := result.LastInsertId()
		if err != nil {
			return err
		}
		if len(chunks) > 0 {
			if err := tx.SaveConversationChunks(ctx, startHistoryID, historyID, chunks); err != nil {
				return err
			}
		}
		if _, err := tx.tx.ExecContext(ctx, `UPDATE delivery_attempts SET provider_message_id = ?, conversation_history_id = ?, accepted_at = CURRENT_TIMESTAMP WHERE event_id = ?`, providerMessageID, historyID, eventID); err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx, `UPDATE trace_events SET status = 'succeeded', completed_at = CURRENT_TIMESTAMP WHERE id = ?`, eventID); err != nil {
			return err
		}
		_, err = tx.tx.ExecContext(ctx, `UPDATE response_traces SET status = 'completed', completed_at = CURRENT_TIMESTAMP WHERE id = ?`, traceID)
		return err
	})
}

func (s *Store) updateOne(ctx context.Context, query, operation string, args ...any) error {
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s row count: %w", operation, err)
	}
	if count != 1 {
		return fmt.Errorf("%s affected %d rows", operation, count)
	}
	return nil
}

func (s *Store) ListResponseTraces(ctx context.Context, filter TraceFilter) ([]TraceSummary, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 1000 {
		limit = 1000
	}
	where := []string{"1=1"}
	args := []any{}
	if filter.ChannelID != "" {
		where = append(where, "t.channel_id = ?")
		args = append(args, filter.ChannelID)
	}
	if filter.SenderID != "" {
		where = append(where, "t.sender_id = ?")
		args = append(args, filter.SenderID)
	}
	if filter.Status != "" {
		where = append(where, "t.status = ?")
		args = append(args, filter.Status)
	}
	if filter.ExternalMessageID != "" {
		where = append(where, "(t.external_message_id = ? OR d.provider_message_id = ?)")
		args = append(args, filter.ExternalMessageID, filter.ExternalMessageID)
	}
	if filter.Since != nil {
		where = append(where, "t.started_at >= ?")
		args = append(args, *filter.Since)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT t.id, t.trigger_type, t.channel_id, t.sender_id, t.external_message_id, t.status, t.started_at, t.completed_at, COALESCE(o.final_content, '') FROM response_traces t LEFT JOIN trace_events e ON e.trace_id=t.id AND e.kind='output' LEFT JOIN response_outputs o ON o.event_id=e.id LEFT JOIN trace_events de ON de.trace_id=t.id AND de.kind='delivery' LEFT JOIN delivery_attempts d ON d.event_id=de.id WHERE `+strings.Join(where, " AND ")+` ORDER BY t.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list response traces: %w", err)
	}
	defer rows.Close()
	result := make([]TraceSummary, 0)
	for rows.Next() {
		var item TraceSummary
		var completed sql.NullTime
		if err := rows.Scan(&item.ID, &item.TriggerType, &item.ChannelID, &item.SenderID, &item.ExternalMessageID, &item.Status, &item.StartedAt, &completed, &item.FinalContent); err != nil {
			return nil, err
		}
		if completed.Valid {
			item.CompletedAt = &completed.Time
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) GetTraceReport(ctx context.Context, traceID int64) (TraceReport, error) {
	var trigger, channel, sender, external, input, status, stage, errText string
	var reminder sql.NullInt64
	var inbound sql.NullInt64
	var started time.Time
	var completed sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT trigger_type, channel_id, sender_id, external_message_id, reminder_id, input_json, inbound_history_id, status, failure_stage, error, started_at, completed_at FROM response_traces WHERE id=?`, traceID).Scan(&trigger, &channel, &sender, &external, &reminder, &input, &inbound, &status, &stage, &errText, &started, &completed); err != nil {
		return TraceReport{}, fmt.Errorf("load trace %d: %w", traceID, err)
	}
	var decodedInput any
	if err := json.Unmarshal([]byte(input), &decodedInput); err != nil {
		decodedInput = input
	}
	report := TraceReport{Trace: map[string]any{"id": traceID, "trigger_type": trigger, "channel_id": channel, "sender_id": sender, "external_message_id": external, "input": decodedInput, "status": status, "failure_stage": stage, "error": errText, "started_at": started}}
	if reminder.Valid {
		report.Trace["reminder_id"] = reminder.Int64
	}
	if inbound.Valid {
		report.Trace["inbound_history_id"] = inbound.Int64
	}
	if completed.Valid {
		report.Trace["completed_at"] = completed.Time
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, sequence_no, kind, status, error, started_at, completed_at FROM trace_events WHERE trace_id=? ORDER BY sequence_no`, traceID)
	if err != nil {
		return TraceReport{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var seq int
		var kind, eventStatus, eventErr string
		var began time.Time
		var ended sql.NullTime
		if err := rows.Scan(&id, &seq, &kind, &eventStatus, &eventErr, &began, &ended); err != nil {
			return TraceReport{}, err
		}
		item := map[string]any{"id": id, "sequence_no": seq, "kind": kind, "status": eventStatus, "error": eventErr, "started_at": began}
		if ended.Valid {
			item["completed_at"] = ended.Time
		}
		detail, err := s.traceEventDetail(ctx, id, kind)
		if err != nil {
			return TraceReport{}, err
		}
		item["detail"] = detail
		report.Events = append(report.Events, item)
	}
	return report, rows.Err()
}

func (s *Store) traceEventDetail(ctx context.Context, eventID int64, kind string) (map[string]any, error) {
	var query string
	switch kind {
	case "rag":
		query = `SELECT json_object('outcome',outcome,'embedding_query',embedding_query,'embedding_model',embedding_model,'dimensions',dimensions,'index_version',index_version,'minimum_score',minimum_score,'history_highwater_id',history_highwater_id,'candidate_count',candidate_count,'excluded_count',excluded_count,'qualified_count',qualified_count,'selected_count',selected_count,'rendered_archive',rendered_archive) FROM rag_retrievals WHERE event_id=?`
	case "llm":
		query = `SELECT json_object('round_number',round_number,'purpose',purpose,'request_json',request_json,'response_json',response_json,'http_status',http_status,'finish_reason',finish_reason) FROM llm_calls WHERE event_id=?`
	case "tool":
		query = `SELECT json_object('llm_event_id',llm_event_id,'tool_call_id',tool_call_id,'name',name,'arguments_json',arguments_json,'result_json',result_json,'is_error',is_error,'mutation_committed',mutation_committed) FROM tool_executions WHERE event_id=?`
	case "output":
		query = `SELECT json_object('source_type',source_type,'source_llm_event_id',source_llm_event_id,'source_content',source_content,'transformations_json',transformations_json,'final_content',final_content) FROM response_outputs WHERE event_id=?`
	case "delivery":
		query = `SELECT json_object('output_event_id',output_event_id,'channel_id',channel_id,'recipient_id',recipient_id,'content',content,'provider_message_id',provider_message_id,'conversation_history_id',conversation_history_id,'attempted_at',attempted_at,'accepted_at',accepted_at) FROM delivery_attempts WHERE event_id=?`
	default:
		return map[string]any{}, nil
	}
	var raw string
	if err := s.db.QueryRowContext(ctx, query, eventID).Scan(&raw); err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, err
	}
	if kind == "rag" {
		rows, err := s.db.QueryContext(ctx, `SELECT rank,start_history_id,end_history_id,similarity_score,content_hash,messages_json FROM rag_matches WHERE retrieval_event_id=? ORDER BY rank`, eventID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var matches []map[string]any
		for rows.Next() {
			var rank, start, end int
			var score float64
			var hash, messages string
			if err := rows.Scan(&rank, &start, &end, &score, &hash, &messages); err != nil {
				return nil, err
			}
			matches = append(matches, map[string]any{"rank": rank, "start_history_id": start, "end_history_id": end, "similarity_score": score, "content_hash": hash, "messages_json": json.RawMessage(messages)})
		}
		result["matches"] = matches
		memoryRows, err := s.db.QueryContext(ctx, `SELECT rank,memory_id,revision_id,vector_score,keyword_score,combined_score,content_hash FROM memory_rag_matches WHERE retrieval_event_id=? ORDER BY rank`, eventID)
		if err != nil {
			return nil, err
		}
		defer memoryRows.Close()
		var memoryMatches []map[string]any
		for memoryRows.Next() {
			var rank int
			var memoryID, revisionID int64
			var vectorScore, keywordScore, combinedScore float64
			var hash string
			if err := memoryRows.Scan(&rank, &memoryID, &revisionID, &vectorScore, &keywordScore, &combinedScore, &hash); err != nil {
				return nil, err
			}
			memoryMatches = append(memoryMatches, map[string]any{"rank": rank, "memory_id": memoryID, "revision_id": revisionID, "vector_score": vectorScore, "keyword_score": keywordScore, "combined_score": combinedScore, "content_hash": hash})
		}
		result["memory_matches"] = memoryMatches
	}
	return result, nil
}
