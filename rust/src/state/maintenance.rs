use std::time::Duration;

use chrono::{DateTime, Utc};
use rusqlite::{OptionalExtension, params};

use super::{MemoryKind, MemoryOrigin, StateError, StateTx, Store, decode_time, encode_time};

pub const MAINTENANCE_INDEX_VERSION: i64 = 1;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum MaintenanceMode {
    Preview,
    Apply,
    Scheduled,
}

impl MaintenanceMode {
    fn as_str(self) -> &'static str {
        match self {
            Self::Preview => "preview",
            Self::Apply => "apply",
            Self::Scheduled => "scheduled",
        }
    }

    fn parse(value: &str) -> Result<Self, StateError> {
        match value {
            "preview" => Ok(Self::Preview),
            "apply" => Ok(Self::Apply),
            "scheduled" => Ok(Self::Scheduled),
            _ => Err(StateError::Validation(format!(
                "invalid maintenance mode {value:?}"
            ))),
        }
    }
}

#[derive(Debug, Clone)]
pub struct MaintenanceState {
    pub checkpoint_history_id: i64,
    pub lease_owner: String,
    pub lease_expires_at: Option<DateTime<Utc>>,
    pub next_run_at: Option<DateTime<Utc>>,
    pub last_success_at: Option<DateTime<Utc>>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone)]
pub struct MaintenanceRun {
    pub id: i64,
    pub mode: MaintenanceMode,
    pub status: String,
    pub stage: String,
    pub owner_sender_id: String,
    pub checkpoint_history_id: i64,
    pub highwater_history_id: i64,
    pub processed_history_id: i64,
    pub candidate_count: usize,
    pub promoted_count: usize,
    pub rejected_count: usize,
    pub embedding_model: String,
    pub dimensions: usize,
    pub index_version: i64,
    pub error: String,
    pub started_at: DateTime<Utc>,
    pub completed_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Clone)]
pub struct MaintenanceExchange {
    pub start_history_id: i64,
    pub end_history_id: i64,
    pub content_type: String,
    pub content: String,
    pub observed_at: DateTime<Utc>,
}

#[derive(Debug, Clone)]
pub struct MaintenanceCandidate {
    pub id: i64,
    pub run_id: i64,
    pub kind: MemoryKind,
    pub content: String,
    pub content_hash: String,
    pub origin_class: MemoryOrigin,
    pub evidence_history_ids: Vec<i64>,
    pub observed_at: DateTime<Utc>,
    pub recurrence_count: usize,
    pub distinct_day_count: usize,
    pub trust_score: f64,
    pub recency_score: f64,
    pub novelty_score: f64,
    pub contradiction_score: f64,
    pub proposed_action: String,
    pub target_memory_id: Option<i64>,
    pub status: String,
    pub decision_reason: String,
    pub created_at: DateTime<Utc>,
    pub resolved_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Clone)]
pub struct MaintenanceRunStart {
    pub mode: MaintenanceMode,
    pub owner_sender_id: String,
    pub lease_owner: String,
    pub lease_duration: Duration,
    pub embedding_model: String,
    pub dimensions: usize,
    pub now: DateTime<Utc>,
}

#[derive(Debug, Clone)]
pub struct MaintenanceRunCompletion {
    pub run_id: i64,
    pub lease_owner: String,
    pub processed_history_id: i64,
    pub candidate_count: usize,
    pub promoted_count: usize,
    pub rejected_count: usize,
    pub advance_checkpoint: bool,
    pub next_run_at: Option<DateTime<Utc>>,
    pub now: DateTime<Utc>,
}

impl Store {
    pub fn start_maintenance_run(
        &self,
        input: &MaintenanceRunStart,
    ) -> Result<MaintenanceRun, StateError> {
        if input.owner_sender_id.is_empty()
            || input.lease_owner.is_empty()
            || input.lease_duration.is_zero()
            || input.embedding_model.is_empty()
            || input.dimensions == 0
        {
            return Err(StateError::Validation(
                "complete maintenance run identity and index contract are required".into(),
            ));
        }
        self.with_tx(|tx| {
            let (checkpoint, lease_owner, lease_expires): (i64, String, Option<String>) = tx
                .transaction
                .query_row(
                    "SELECT checkpoint_history_id, lease_owner, lease_expires_at FROM memory_maintenance_state WHERE singleton_id=1",
                    [],
                    |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)),
                )?;
            if !lease_owner.is_empty()
                && lease_expires
                    .as_deref()
                    .map(|value| decode_time(value.to_owned()))
                    .transpose()?
                    .is_some_and(|expires| expires > input.now)
            {
                return Err(StateError::MaintenanceBusy);
            }
            if !lease_owner.is_empty() {
                tx.transaction.execute(
                    "UPDATE memory_maintenance_runs SET status='failed', stage='lease', error='maintenance lease expired before completion', completed_at=?1 WHERE status='active'",
                    [encode_time(input.now)],
                )?;
            }
            let highwater: i64 = tx.transaction.query_row(
                "SELECT COALESCE(MAX(u.id), 0) FROM conversation_history u
                 WHERE u.channel_id='telegram' AND u.sender_id=?1 AND u.audience='conversation'
                   AND u.role='user' AND u.content_type IN ('text','inbound_message')
                   AND EXISTS (SELECT 1 FROM conversation_history a
                     WHERE a.channel_id=u.channel_id AND a.sender_id=u.sender_id
                       AND a.audience='conversation' AND a.role='assistant' AND a.content_type='text'
                       AND a.id>u.id AND NOT EXISTS (SELECT 1 FROM conversation_history n
                         WHERE n.channel_id=u.channel_id AND n.sender_id=u.sender_id
                           AND n.audience='conversation' AND n.role='user' AND n.id>u.id AND n.id<a.id))",
                [&input.owner_sender_id],
                |row| row.get(0),
            )?;
            let lease_millis = i64::try_from(input.lease_duration.as_millis()).map_err(|_| {
                StateError::Validation("maintenance lease duration is too large".into())
            })?;
            let expires = input.now + chrono::Duration::milliseconds(lease_millis);
            let changed = tx.transaction.execute(
                "UPDATE memory_maintenance_state SET lease_owner=?1, lease_expires_at=?2, updated_at=?3 WHERE singleton_id=1 AND (lease_owner='' OR lease_expires_at IS NULL OR julianday(lease_expires_at)<=julianday(?3))",
                params![input.lease_owner, encode_time(expires), encode_time(input.now)],
            )?;
            if changed != 1 {
                return Err(StateError::MaintenanceBusy);
            }
            tx.transaction.execute(
                "INSERT INTO memory_maintenance_runs (mode,status,stage,owner_sender_id,checkpoint_history_id,highwater_history_id,processed_history_id,embedding_model,dimensions,index_version,started_at) VALUES (?1,'active','select',?2,?3,?4,?3,?5,?6,?7,?8)",
                params![input.mode.as_str(), input.owner_sender_id, checkpoint, highwater, input.embedding_model, input.dimensions as i64, MAINTENANCE_INDEX_VERSION, encode_time(input.now)],
            )?;
            Ok(MaintenanceRun {
                id: tx.transaction.last_insert_rowid(),
                mode: input.mode,
                status: "active".into(),
                stage: "select".into(),
                owner_sender_id: input.owner_sender_id.clone(),
                checkpoint_history_id: checkpoint,
                highwater_history_id: highwater,
                processed_history_id: checkpoint,
                candidate_count: 0,
                promoted_count: 0,
                rejected_count: 0,
                embedding_model: input.embedding_model.clone(),
                dimensions: input.dimensions,
                index_version: MAINTENANCE_INDEX_VERSION,
                error: String::new(),
                started_at: input.now,
                completed_at: None,
            })
        })
    }

    pub fn refresh_maintenance_lease(
        &self,
        lease_owner: &str,
        lease_duration: Duration,
        stage: &str,
        run_id: i64,
        now: DateTime<Utc>,
    ) -> Result<(), StateError> {
        if lease_owner.is_empty() || lease_duration.is_zero() {
            return Err(StateError::Validation(
                "maintenance lease identity is required".into(),
            ));
        }
        let millis = i64::try_from(lease_duration.as_millis())
            .map_err(|_| StateError::Validation("maintenance lease is too large".into()))?;
        self.with_tx(|tx| {
            let changed = tx.transaction.execute(
                "UPDATE memory_maintenance_state SET lease_expires_at=?1, updated_at=?2 WHERE singleton_id=1 AND lease_owner=?3",
                params![encode_time(now + chrono::Duration::milliseconds(millis)), encode_time(now), lease_owner],
            )?;
            if changed != 1 {
                return Err(StateError::Validation(
                    "memory maintenance lease was lost".into(),
                ));
            }
            tx.transaction.execute(
                "UPDATE memory_maintenance_runs SET stage=?1 WHERE id=?2 AND status='active'",
                params![stage, run_id],
            )?;
            Ok(())
        })
    }

    pub fn load_maintenance_exchanges(
        &self,
        owner_sender_id: &str,
        after_id: i64,
        highwater_id: i64,
        limit: usize,
    ) -> Result<Vec<MaintenanceExchange>, StateError> {
        if !(1..=100).contains(&limit) {
            return Err(StateError::Validation(
                "maintenance batch limit must be between 1 and 100".into(),
            ));
        }
        let connection = self.lock()?;
        let mut statement = connection.prepare(
            "SELECT u.id,
               (SELECT MAX(a.id) FROM conversation_history a
                WHERE a.channel_id=u.channel_id AND a.sender_id=u.sender_id
                  AND a.audience='conversation' AND a.role='assistant' AND a.content_type='text'
                  AND a.id>u.id AND NOT EXISTS (SELECT 1 FROM conversation_history n
                    WHERE n.channel_id=u.channel_id AND n.sender_id=u.sender_id
                      AND n.audience='conversation' AND n.role='user' AND n.id>u.id AND n.id<a.id)),
               u.content_type,u.content,u.created_at
             FROM conversation_history u
             WHERE u.channel_id='telegram' AND u.sender_id=?1 AND u.audience='conversation'
               AND u.role='user' AND u.content_type IN ('text','inbound_message')
               AND u.id>?2 AND u.id<=?3
               AND EXISTS (SELECT 1 FROM conversation_history a
                 WHERE a.channel_id=u.channel_id AND a.sender_id=u.sender_id
                   AND a.audience='conversation' AND a.role='assistant' AND a.content_type='text'
                   AND a.id>u.id AND NOT EXISTS (SELECT 1 FROM conversation_history n
                     WHERE n.channel_id=u.channel_id AND n.sender_id=u.sender_id
                       AND n.audience='conversation' AND n.role='user' AND n.id>u.id AND n.id<a.id))
             ORDER BY u.id ASC LIMIT ?4",
        )?;
        let raw = statement
            .query_map(
                params![owner_sender_id, after_id, highwater_id, limit as i64],
                |row| {
                    Ok((
                        row.get::<_, i64>(0)?,
                        row.get::<_, i64>(1)?,
                        row.get::<_, String>(2)?,
                        row.get::<_, String>(3)?,
                        row.get::<_, String>(4)?,
                    ))
                },
            )?
            .collect::<Result<Vec<_>, _>>()?;
        raw.into_iter()
            .map(|item| {
                Ok(MaintenanceExchange {
                    start_history_id: item.0,
                    end_history_id: item.1,
                    content_type: item.2,
                    content: item.3,
                    observed_at: decode_time(item.4)?,
                })
            })
            .collect()
    }

    pub fn insert_maintenance_candidate(
        &self,
        candidate: &MaintenanceCandidate,
    ) -> Result<MaintenanceCandidate, StateError> {
        let evidence = serde_json::to_string(&candidate.evidence_history_ids)
            .map_err(|error| StateError::Validation(error.to_string()))?;
        let connection = self.lock()?;
        connection.execute(
            "INSERT INTO memory_candidates (run_id,kind,content,content_hash,origin_class,evidence_history_ids,observed_at,recurrence_count,distinct_day_count,trust_score,recency_score,novelty_score,contradiction_score,proposed_action,status,created_at) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,'pending','proposed',?14)",
            params![candidate.run_id, candidate.kind.as_str(), candidate.content, candidate.content_hash, candidate.origin_class.as_str(), evidence, encode_time(candidate.observed_at), candidate.recurrence_count as i64, candidate.distinct_day_count as i64, candidate.trust_score, candidate.recency_score, candidate.novelty_score, candidate.contradiction_score, encode_time(candidate.created_at)],
        )?;
        let mut result = candidate.clone();
        result.id = connection.last_insert_rowid();
        result.proposed_action = "pending".into();
        result.status = "proposed".into();
        Ok(result)
    }

    pub fn score_maintenance_candidate(
        &self,
        id: i64,
        novelty: f64,
        contradiction: f64,
    ) -> Result<(), StateError> {
        if !(0.0..=1.0).contains(&novelty) || !(0.0..=1.0).contains(&contradiction) {
            return Err(StateError::Validation(
                "maintenance candidate scores must be between 0 and 1".into(),
            ));
        }
        self.update_one(
            "UPDATE memory_candidates SET novelty_score=?1, contradiction_score=?2 WHERE id=?3 AND status='proposed'",
            params![novelty, contradiction, id],
            "score maintenance candidate",
        )
    }

    pub fn complete_maintenance_run(
        &self,
        completion: &MaintenanceRunCompletion,
    ) -> Result<(), StateError> {
        self.with_tx(|tx| {
            let unresolved: i64 = tx.transaction.query_row(
                "SELECT count(*) FROM memory_candidates WHERE run_id=?1 AND status NOT IN ('accepted','rejected')",
                [completion.run_id],
                |row| row.get(0),
            )?;
            if unresolved != 0 {
                return Err(StateError::Validation(format!(
                    "maintenance run {} has {unresolved} non-terminal candidates",
                    completion.run_id
                )));
            }
            let changed = tx.transaction.execute(
                "UPDATE memory_maintenance_runs SET status='completed',stage='complete',processed_history_id=?1,candidate_count=?2,promoted_count=?3,rejected_count=?4,completed_at=?5 WHERE id=?6 AND status='active'",
                params![completion.processed_history_id, completion.candidate_count as i64, completion.promoted_count as i64, completion.rejected_count as i64, encode_time(completion.now), completion.run_id],
            )?;
            if changed != 1 {
                return Err(StateError::Validation(format!(
                    "maintenance run {} is not active",
                    completion.run_id
                )));
            }
            let sql = if completion.advance_checkpoint {
                "UPDATE memory_maintenance_state SET checkpoint_history_id=MAX(checkpoint_history_id,?1),lease_owner='',lease_expires_at=NULL,next_run_at=COALESCE(?2,next_run_at),last_success_at=?3,updated_at=?3 WHERE singleton_id=1 AND lease_owner=?4"
            } else {
                "UPDATE memory_maintenance_state SET checkpoint_history_id=checkpoint_history_id,lease_owner='',lease_expires_at=NULL,next_run_at=COALESCE(?2,next_run_at),updated_at=?3 WHERE singleton_id=1 AND lease_owner=?4 AND ?1=?1"
            };
            let changed = tx.transaction.execute(
                sql,
                params![completion.processed_history_id, completion.next_run_at.map(encode_time), encode_time(completion.now), completion.lease_owner],
            )?;
            if changed != 1 {
                return Err(StateError::Validation(
                    "memory maintenance lease was lost before completion".into(),
                ));
            }
            Ok(())
        })
    }

    #[allow(clippy::too_many_arguments)]
    pub fn fail_maintenance_run(
        &self,
        run_id: i64,
        lease_owner: &str,
        stage: &str,
        message: &str,
        cancelled: bool,
        next_run_at: Option<DateTime<Utc>>,
        now: DateTime<Utc>,
    ) -> Result<(), StateError> {
        let status = if cancelled { "cancelled" } else { "failed" };
        let message: String = message.chars().take(1000).collect();
        self.with_tx(|tx| {
            tx.transaction.execute(
                "UPDATE memory_maintenance_runs SET status=?1,stage=?2,error=?3,completed_at=?4,candidate_count=(SELECT count(*) FROM memory_candidates WHERE run_id=?5),promoted_count=CASE WHEN mode='preview' THEN 0 ELSE (SELECT count(*) FROM memory_candidates WHERE run_id=?5 AND status='accepted' AND proposed_action IN ('add','update')) END,rejected_count=(SELECT count(*) FROM memory_candidates WHERE run_id=?5 AND status='rejected') WHERE id=?5 AND status='active'",
                params![status, stage, message, encode_time(now), run_id],
            )?;
            tx.transaction.execute(
                "UPDATE memory_maintenance_state SET lease_owner='',lease_expires_at=NULL,next_run_at=COALESCE(?1,next_run_at),updated_at=?2 WHERE singleton_id=1 AND lease_owner=?3",
                params![next_run_at.map(encode_time), encode_time(now), lease_owner],
            )?;
            Ok(())
        })
    }

    pub fn maintenance_status(
        &self,
    ) -> Result<(MaintenanceState, Option<MaintenanceRun>), StateError> {
        let connection = self.lock()?;
        let state_raw: (i64, String, Option<String>, Option<String>, Option<String>, String) =
            connection.query_row(
                "SELECT checkpoint_history_id,lease_owner,lease_expires_at,next_run_at,last_success_at,updated_at FROM memory_maintenance_state WHERE singleton_id=1",
                [],
                |row| Ok((row.get(0)?,row.get(1)?,row.get(2)?,row.get(3)?,row.get(4)?,row.get(5)?)),
            )?;
        let state = MaintenanceState {
            checkpoint_history_id: state_raw.0,
            lease_owner: state_raw.1,
            lease_expires_at: state_raw.2.map(decode_time).transpose()?,
            next_run_at: state_raw.3.map(decode_time).transpose()?,
            last_success_at: state_raw.4.map(decode_time).transpose()?,
            updated_at: decode_time(state_raw.5)?,
        };
        let run_raw = connection
            .query_row(
                "SELECT id,mode,status,stage,owner_sender_id,checkpoint_history_id,highwater_history_id,processed_history_id,candidate_count,promoted_count,rejected_count,embedding_model,dimensions,index_version,error,started_at,completed_at FROM memory_maintenance_runs ORDER BY id DESC LIMIT 1",
                [],
                |row| Ok((row.get::<_,i64>(0)?,row.get::<_,String>(1)?,row.get::<_,String>(2)?,row.get::<_,String>(3)?,row.get::<_,String>(4)?,row.get::<_,i64>(5)?,row.get::<_,i64>(6)?,row.get::<_,i64>(7)?,row.get::<_,i64>(8)?,row.get::<_,i64>(9)?,row.get::<_,i64>(10)?,row.get::<_,String>(11)?,row.get::<_,i64>(12)?,row.get::<_,i64>(13)?,row.get::<_,String>(14)?,row.get::<_,String>(15)?,row.get::<_,Option<String>>(16)?)),
            )
            .optional()?;
        let run = run_raw
            .map(|raw| {
                Ok::<MaintenanceRun, StateError>(MaintenanceRun {
                    id: raw.0,
                    mode: MaintenanceMode::parse(&raw.1)?,
                    status: raw.2,
                    stage: raw.3,
                    owner_sender_id: raw.4,
                    checkpoint_history_id: raw.5,
                    highwater_history_id: raw.6,
                    processed_history_id: raw.7,
                    candidate_count: raw.8 as usize,
                    promoted_count: raw.9 as usize,
                    rejected_count: raw.10 as usize,
                    embedding_model: raw.11,
                    dimensions: raw.12 as usize,
                    index_version: raw.13,
                    error: raw.14,
                    started_at: decode_time(raw.15)?,
                    completed_at: raw.16.map(decode_time).transpose()?,
                })
            })
            .transpose()?;
        Ok((state, run))
    }

    pub fn set_maintenance_next_run(
        &self,
        next: DateTime<Utc>,
        now: DateTime<Utc>,
    ) -> Result<(), StateError> {
        self.update_one(
            "UPDATE memory_maintenance_state SET next_run_at=?1,updated_at=?2 WHERE singleton_id=1",
            params![encode_time(next), encode_time(now)],
            "set next maintenance run",
        )
    }

    pub fn list_maintenance_candidates(
        &self,
        run_id: i64,
    ) -> Result<Vec<MaintenanceCandidate>, StateError> {
        let connection = self.lock()?;
        let mut statement = connection.prepare(
            "SELECT id,run_id,kind,content,content_hash,origin_class,evidence_history_ids,observed_at,recurrence_count,distinct_day_count,trust_score,recency_score,novelty_score,contradiction_score,proposed_action,target_memory_id,status,decision_reason,created_at,resolved_at FROM memory_candidates WHERE run_id=?1 ORDER BY id ASC",
        )?;
        let raw = statement
            .query_map([run_id], |row| {
                Ok((
                    row.get::<_, i64>(0)?,
                    row.get::<_, i64>(1)?,
                    row.get::<_, String>(2)?,
                    row.get::<_, String>(3)?,
                    row.get::<_, String>(4)?,
                    row.get::<_, String>(5)?,
                    row.get::<_, String>(6)?,
                    row.get::<_, String>(7)?,
                    row.get::<_, i64>(8)?,
                    row.get::<_, i64>(9)?,
                    row.get::<_, f64>(10)?,
                    row.get::<_, f64>(11)?,
                    row.get::<_, f64>(12)?,
                    row.get::<_, f64>(13)?,
                    row.get::<_, String>(14)?,
                    row.get::<_, Option<i64>>(15)?,
                    row.get::<_, String>(16)?,
                    row.get::<_, String>(17)?,
                    row.get::<_, String>(18)?,
                    row.get::<_, Option<String>>(19)?,
                ))
            })?
            .collect::<Result<Vec<_>, _>>()?;
        raw.into_iter()
            .map(|item| {
                Ok(MaintenanceCandidate {
                    id: item.0,
                    run_id: item.1,
                    kind: MemoryKind::parse(&item.2)?,
                    content: item.3,
                    content_hash: item.4,
                    origin_class: MemoryOrigin::parse(&item.5)?,
                    evidence_history_ids: serde_json::from_str(&item.6)
                        .map_err(|error| StateError::Validation(error.to_string()))?,
                    observed_at: decode_time(item.7)?,
                    recurrence_count: item.8 as usize,
                    distinct_day_count: item.9 as usize,
                    trust_score: item.10,
                    recency_score: item.11,
                    novelty_score: item.12,
                    contradiction_score: item.13,
                    proposed_action: item.14,
                    target_memory_id: item.15,
                    status: item.16,
                    decision_reason: item.17,
                    created_at: decode_time(item.18)?,
                    resolved_at: item.19.map(decode_time).transpose()?,
                })
            })
            .collect()
    }
}

impl StateTx<'_> {
    pub fn resolve_maintenance_candidate(
        &self,
        id: i64,
        action: &str,
        status: &str,
        reason: &str,
        target_memory_id: Option<i64>,
        now: DateTime<Utc>,
    ) -> Result<(), StateError> {
        if !matches!(action, "noop" | "add" | "update" | "review" | "reject") {
            return Err(StateError::Validation(format!(
                "invalid maintenance candidate action {action:?}"
            )));
        }
        if !matches!(status, "accepted" | "rejected" | "failed") {
            return Err(StateError::Validation(format!(
                "invalid maintenance candidate status {status:?}"
            )));
        }
        let changed = self.transaction.execute(
            "UPDATE memory_candidates SET proposed_action=?1,target_memory_id=?2,status=?3,decision_reason=?4,resolved_at=?5 WHERE id=?6 AND status='proposed'",
            params![action,target_memory_id,status,reason,encode_time(now),id],
        )?;
        if changed != 1 {
            return Err(StateError::Validation(format!(
                "maintenance candidate {id} is not proposed"
            )));
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn maintenance_lease_is_exclusive_and_completion_advances_checkpoint() {
        let directory = tempfile::tempdir().unwrap();
        let store = Store::new(directory.path().join("state.sqlite")).unwrap();
        let now = Utc::now();
        let input = MaintenanceRunStart {
            mode: MaintenanceMode::Apply,
            owner_sender_id: "owner".into(),
            lease_owner: "worker-one".into(),
            lease_duration: Duration::from_secs(60),
            embedding_model: "embed-v1".into(),
            dimensions: 2,
            now,
        };
        let run = store.start_maintenance_run(&input).unwrap();
        let mut second = input.clone();
        second.lease_owner = "worker-two".into();
        assert!(matches!(
            store.start_maintenance_run(&second),
            Err(StateError::MaintenanceBusy)
        ));
        store
            .complete_maintenance_run(&MaintenanceRunCompletion {
                run_id: run.id,
                lease_owner: input.lease_owner,
                processed_history_id: 0,
                candidate_count: 0,
                promoted_count: 0,
                rejected_count: 0,
                advance_checkpoint: true,
                next_run_at: None,
                now,
            })
            .unwrap();
        let (state, latest) = store.maintenance_status().unwrap();
        assert!(state.lease_owner.is_empty());
        assert_eq!(latest.unwrap().status, "completed");
    }
}
