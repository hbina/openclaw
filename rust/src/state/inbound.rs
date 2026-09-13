use std::time::Duration;

use chrono::{DateTime, Utc};
use rusqlite::{OptionalExtension, params};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::channels::InboundMessage;

use super::{StateError, StateTx, Store, decode_time, encode_time};

pub const MAX_INBOUND_ATTEMPTS: i64 = 5;
const MAX_INBOUND_ERROR_BYTES: usize = 64 << 10;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum InboundStatus {
    Received,
    Processing,
    Abandoned,
    Completed,
    Failed,
}

impl InboundStatus {
    fn parse(value: &str) -> Result<Self, StateError> {
        match value {
            "received" => Ok(Self::Received),
            "processing" => Ok(Self::Processing),
            "abandoned" => Ok(Self::Abandoned),
            "completed" => Ok(Self::Completed),
            "failed" => Ok(Self::Failed),
            other => Err(StateError::Validation(format!(
                "invalid inbound event status {other:?}"
            ))),
        }
    }
}

#[derive(Debug, Clone)]
pub struct InboundEvent {
    pub id: i64,
    pub message: InboundMessage,
    pub status: InboundStatus,
    pub attempt_count: i64,
    pub max_attempts: i64,
    pub next_attempt_at: DateTime<Utc>,
    pub lease_owner: String,
    pub lease_generation: i64,
    pub lease_expires_at: Option<DateTime<Utc>>,
    pub last_error: String,
}

#[derive(Debug, Clone)]
pub struct InboundClaim {
    pub event: InboundEvent,
    pub lease_owner: String,
    pub lease_generation: i64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum InboundDuplicate {
    Inserted,
    Completed,
    Active,
    Retryable,
    PermanentlyFailed,
}

impl Store {
    pub fn channel_next_update_id(&self, channel_id: &str) -> Result<i64, StateError> {
        Ok(self
            .lock()?
            .query_row(
                "SELECT next_update_id FROM channel_polling_checkpoints WHERE channel_id=?1",
                [channel_id],
                |row| row.get(0),
            )
            .optional()?
            .unwrap_or(0))
    }

    pub fn advance_channel_checkpoint(
        &self,
        channel_id: &str,
        update_id: i64,
        now: DateTime<Utc>,
    ) -> Result<i64, StateError> {
        self.with_tx(|tx| {
            advance_checkpoint(&tx.transaction, channel_id, update_id, now)?;
            Ok(update_id.saturating_add(1))
        })
    }

    pub fn record_inbound_message(
        &self,
        message: &InboundMessage,
        now: DateTime<Utc>,
    ) -> Result<(i64, InboundDuplicate, i64), StateError> {
        let payload_json = serde_json::to_string(message)
            .map_err(|error| StateError::Validation(format!("serialize inbound event: {error}")))?;
        let payload_hash = format!("{:x}", Sha256::digest(payload_json.as_bytes()));
        self.with_tx(|tx| {
            let existing = tx
                .transaction
                .query_row(
                    "SELECT id,payload_hash,status,attempt_count,max_attempts,lease_expires_at FROM inbound_events WHERE (channel_id=?1 AND update_id=?2) OR (channel_id=?1 AND chat_id=?3 AND message_id=?4) ORDER BY id LIMIT 1",
                    params![message.channel_id, message.update_id, message.chat_id, message.message_id],
                    |row| Ok((row.get::<_, i64>(0)?, row.get::<_, String>(1)?, row.get::<_, String>(2)?, row.get::<_, i64>(3)?, row.get::<_, i64>(4)?, row.get::<_, Option<String>>(5)?)),
                )
                .optional()?;
            let (id, duplicate) = if let Some((id, stored_hash, status, attempts, maximum, lease)) = existing {
                if stored_hash != payload_hash {
                    return Err(StateError::Validation(format!(
                        "inbound identity conflict for channel {:?}, update {}, chat {}, message {}",
                        message.channel_id, message.update_id, message.chat_id, message.message_id
                    )));
                }
                let status = InboundStatus::parse(&status)?;
                let lease_active = lease
                    .map(decode_time)
                    .transpose()?
                    .is_some_and(|expires| expires > now);
                let duplicate = match status {
                    InboundStatus::Completed => InboundDuplicate::Completed,
                    InboundStatus::Processing if lease_active => InboundDuplicate::Active,
                    _ if attempts >= maximum && !lease_active => {
                        InboundDuplicate::PermanentlyFailed
                    }
                    _ => InboundDuplicate::Retryable,
                };
                (id, duplicate)
            } else {
                tx.transaction.execute(
                    "INSERT INTO inbound_events (channel_id,update_id,chat_id,message_id,sender_id,occurred_at,payload_json,payload_hash,status,attempt_count,max_attempts,next_attempt_at,received_at,updated_at) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,'received',0,?9,?10,?10,?10)",
                    params![message.channel_id, message.update_id, message.chat_id, message.message_id, message.sender_id, encode_time(message.timestamp), payload_json, payload_hash, MAX_INBOUND_ATTEMPTS, encode_time(now)],
                )?;
                (tx.transaction.last_insert_rowid(), InboundDuplicate::Inserted)
            };
            advance_checkpoint(&tx.transaction, &message.channel_id, message.update_id, now)?;
            Ok((id, duplicate, message.update_id.saturating_add(1)))
        })
    }

    pub fn claim_oldest_inbound(
        &self,
        owner: &str,
        now: DateTime<Utc>,
        lease_duration: Duration,
    ) -> Result<Option<InboundClaim>, StateError> {
        if owner.is_empty() || lease_duration.is_zero() {
            return Err(StateError::Validation(
                "inbound lease owner and duration are required".into(),
            ));
        }
        let lease_ms = i64::try_from(lease_duration.as_millis())
            .map_err(|_| StateError::Validation("inbound lease duration is too large".into()))?;
        self.with_tx(|tx| {
            tx.transaction.execute(
                "UPDATE inbound_events SET status=CASE WHEN attempt_count>=max_attempts THEN 'failed' ELSE 'abandoned' END,lease_owner='',lease_expires_at=NULL,last_error=CASE WHEN last_error='' THEN 'processing lease expired' ELSE last_error END,updated_at=?1 WHERE status='processing' AND lease_expires_at IS NOT NULL AND julianday(lease_expires_at)<=julianday(?1)",
                [encode_time(now)],
            )?;
            let candidate: Option<i64> = tx.transaction.query_row(
                "SELECT id FROM inbound_events e WHERE e.id=(SELECT MIN(id) FROM inbound_events WHERE status<>'completed' AND NOT(status='failed' AND attempt_count>=max_attempts)) AND ((e.status IN ('received','abandoned')) OR (e.status='failed' AND e.attempt_count<e.max_attempts AND julianday(e.next_attempt_at)<=julianday(?1)))",
                [encode_time(now)],
                |row| row.get(0),
            ).optional()?;
            let Some(id) = candidate else { return Ok(None); };
            let expires = now + chrono::Duration::milliseconds(lease_ms);
            let changed = tx.transaction.execute(
                "UPDATE inbound_events SET status='processing',attempt_count=attempt_count+1,lease_owner=?1,lease_generation=lease_generation+1,lease_expires_at=?2,started_at=COALESCE(started_at,?3),updated_at=?3 WHERE id=?4 AND status IN ('received','abandoned','failed')",
                params![owner, encode_time(expires), encode_time(now), id],
            )?;
            if changed != 1 { return Ok(None); }
            let event = load_event(&tx.transaction, id)?;
            Ok(Some(InboundClaim { lease_generation: event.lease_generation, lease_owner: owner.into(), event }))
        })
    }

    pub fn refresh_inbound_lease(
        &self,
        claim: &InboundClaim,
        now: DateTime<Utc>,
        lease_duration: Duration,
    ) -> Result<(), StateError> {
        let lease_ms = i64::try_from(lease_duration.as_millis())
            .map_err(|_| StateError::Validation("inbound lease duration is too large".into()))?;
        let expires = now + chrono::Duration::milliseconds(lease_ms);
        self.require_claim_update(
            "UPDATE inbound_events SET lease_expires_at=?1,updated_at=?2 WHERE id=?3 AND status='processing' AND lease_owner=?4 AND lease_generation=?5 AND julianday(lease_expires_at)>julianday(?2)",
            params![encode_time(expires), encode_time(now), claim.event.id, claim.lease_owner, claim.lease_generation],
            "refresh inbound lease",
        )
    }

    pub fn assert_inbound_claim(
        &self,
        claim: &InboundClaim,
        now: DateTime<Utc>,
    ) -> Result<(), StateError> {
        self.assert_inbound_lease(
            claim.event.id,
            &claim.lease_owner,
            claim.lease_generation,
            now,
        )
    }

    pub fn assert_inbound_lease(
        &self,
        event_id: i64,
        lease_owner: &str,
        lease_generation: i64,
        now: DateTime<Utc>,
    ) -> Result<(), StateError> {
        let valid: bool = self.lock()?.query_row(
            "SELECT EXISTS(SELECT 1 FROM inbound_events WHERE id=?1 AND status='processing' AND lease_owner=?2 AND lease_generation=?3 AND julianday(lease_expires_at)>julianday(?4))",
            params![event_id, lease_owner, lease_generation, encode_time(now)],
            |row| row.get(0),
        )?;
        if valid {
            Ok(())
        } else {
            Err(StateError::Validation(
                "inbound processing lease was lost".into(),
            ))
        }
    }

    pub fn fail_inbound_claim(
        &self,
        claim: &InboundClaim,
        now: DateTime<Utc>,
        error: &str,
    ) -> Result<bool, StateError> {
        let permanent = claim.event.attempt_count >= claim.event.max_attempts;
        let delay = match claim.event.attempt_count {
            1 => 5,
            2 => 30,
            3 => 120,
            _ => 300,
        };
        let next = now + chrono::Duration::seconds(delay);
        let error = bounded_error(error);
        self.with_tx(|tx| {
            let changed = tx.transaction.execute(
                "UPDATE inbound_events SET status='failed',next_attempt_at=?1,lease_owner='',lease_expires_at=NULL,last_error=?2,updated_at=?3 WHERE id=?4 AND status='processing' AND lease_owner=?5 AND lease_generation=?6",
                params![encode_time(next), error, encode_time(now), claim.event.id, claim.lease_owner, claim.lease_generation],
            )?;
            if changed != 1 {
                return Err(StateError::Validation("fail inbound event: inbound lease was lost".into()));
            }
            if permanent {
                tx.transaction.execute(
                    "UPDATE response_traces SET status='failed',failure_stage='inbound',error=?1,completed_at=?2 WHERE inbound_event_id=?3 AND status<>'completed'",
                    params![error, encode_time(now), claim.event.id],
                )?;
            } else {
                tx.transaction.execute(
                    "UPDATE response_traces SET status='active',failure_stage='',error='',completed_at=NULL WHERE inbound_event_id=?1 AND status<>'completed'",
                    [claim.event.id],
                )?;
            }
            Ok(())
        })?;
        Ok(permanent)
    }

    pub fn complete_inbound_claim(
        &self,
        claim: &InboundClaim,
        now: DateTime<Utc>,
    ) -> Result<(), StateError> {
        self.require_claim_update(
            "UPDATE inbound_events SET status='completed',lease_owner='',lease_expires_at=NULL,last_error='',completed_at=?1,updated_at=?1 WHERE id=?2 AND status='processing' AND lease_owner=?3 AND lease_generation=?4",
            params![encode_time(now), claim.event.id, claim.lease_owner, claim.lease_generation],
            "complete inbound event",
        )
    }

    pub fn get_inbound_event(&self, id: i64) -> Result<InboundEvent, StateError> {
        load_event(&*self.lock()?, id)
    }

    fn require_claim_update<P: rusqlite::Params>(
        &self,
        sql: &str,
        params: P,
        operation: &str,
    ) -> Result<(), StateError> {
        if self.lock()?.execute(sql, params)? == 1 {
            Ok(())
        } else {
            Err(StateError::Validation(format!(
                "{operation}: inbound lease was lost"
            )))
        }
    }
}

impl StateTx<'_> {
    pub fn assert_inbound_claim(
        &self,
        event_id: i64,
        lease_owner: &str,
        lease_generation: i64,
        now: DateTime<Utc>,
    ) -> Result<(), StateError> {
        let valid: bool = self.transaction.query_row(
            "SELECT EXISTS(SELECT 1 FROM inbound_events WHERE id=?1 AND status='processing' AND lease_owner=?2 AND lease_generation=?3 AND julianday(lease_expires_at)>julianday(?4))",
            params![event_id, lease_owner, lease_generation, encode_time(now)],
            |row| row.get(0),
        )?;
        if valid {
            Ok(())
        } else {
            Err(StateError::Validation(
                "inbound processing lease was lost".into(),
            ))
        }
    }
}

fn advance_checkpoint(
    connection: &rusqlite::Connection,
    channel_id: &str,
    update_id: i64,
    now: DateTime<Utc>,
) -> Result<(), StateError> {
    connection.execute(
        "INSERT INTO channel_polling_checkpoints (channel_id,next_update_id,updated_at) VALUES (?1,?2,?3) ON CONFLICT(channel_id) DO UPDATE SET next_update_id=MAX(next_update_id,excluded.next_update_id),updated_at=excluded.updated_at",
        params![channel_id, update_id.saturating_add(1), encode_time(now)],
    )?;
    Ok(())
}

fn load_event(connection: &rusqlite::Connection, id: i64) -> Result<InboundEvent, StateError> {
    let raw = connection.query_row(
        "SELECT payload_json,status,attempt_count,max_attempts,next_attempt_at,lease_owner,lease_generation,lease_expires_at,last_error FROM inbound_events WHERE id=?1",
        [id],
        |row| Ok((row.get::<_, String>(0)?,row.get::<_, String>(1)?,row.get::<_, i64>(2)?,row.get::<_, i64>(3)?,row.get::<_, String>(4)?,row.get::<_, String>(5)?,row.get::<_, i64>(6)?,row.get::<_, Option<String>>(7)?,row.get::<_, String>(8)?)),
    )?;
    Ok(InboundEvent {
        id,
        message: serde_json::from_str(&raw.0).map_err(|error| {
            StateError::Validation(format!("decode inbound event {id}: {error}"))
        })?,
        status: InboundStatus::parse(&raw.1)?,
        attempt_count: raw.2,
        max_attempts: raw.3,
        next_attempt_at: decode_time(raw.4)?,
        lease_owner: raw.5,
        lease_generation: raw.6,
        lease_expires_at: raw.7.map(decode_time).transpose()?,
        last_error: raw.8,
    })
}

fn bounded_error(error: &str) -> String {
    if error.len() <= MAX_INBOUND_ERROR_BYTES {
        return error.into();
    }
    let mut end = MAX_INBOUND_ERROR_BYTES;
    while !error.is_char_boundary(end) {
        end -= 1;
    }
    error[..end].into()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn message(update_id: i64, message_id: i64, content: &str) -> InboundMessage {
        InboundMessage {
            channel_id: "telegram".into(),
            update_id,
            message_id,
            chat_id: 42,
            sender_id: 42,
            timestamp: DateTime::from_timestamp(1_700_000_000, 0).unwrap(),
            content: content.into(),
            reply: None,
        }
    }

    #[test]
    fn recording_is_atomic_deduplicated_and_persistent() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("state.sqlite");
        let now = DateTime::from_timestamp(1_700_000_100, 0).unwrap();
        let store = Store::new(&path).unwrap();
        let (id, disposition, next) = store
            .record_inbound_message(&message(10, 7, "hello"), now)
            .unwrap();
        assert_eq!(disposition, InboundDuplicate::Inserted);
        assert_eq!(next, 11);
        assert_eq!(store.channel_next_update_id("telegram").unwrap(), 11);
        drop(store);

        let store = Store::new(&path).unwrap();
        let (duplicate_id, disposition, _) = store
            .record_inbound_message(&message(10, 7, "hello"), now)
            .unwrap();
        assert_eq!(duplicate_id, id);
        assert_eq!(disposition, InboundDuplicate::Retryable);
        assert_eq!(store.get_inbound_event(id).unwrap().attempt_count, 0);

        let error = store
            .record_inbound_message(&message(12, 7, "changed"), now)
            .unwrap_err();
        assert!(error.to_string().contains("identity conflict"));
        assert_eq!(store.channel_next_update_id("telegram").unwrap(), 11);
    }

    #[test]
    fn failed_event_insert_cannot_advance_the_checkpoint() {
        let store = Store::new(":memory:").unwrap();
        store.lock().unwrap().execute_batch(
            "CREATE TRIGGER reject_inbound BEFORE INSERT ON inbound_events BEGIN SELECT RAISE(FAIL, 'injected'); END;",
        ).unwrap();
        let now = DateTime::from_timestamp(1_700_000_100, 0).unwrap();
        assert!(
            store
                .record_inbound_message(&message(10, 7, "hello"), now)
                .is_err()
        );
        assert_eq!(store.channel_next_update_id("telegram").unwrap(), 0);
    }

    #[test]
    fn leases_are_exclusive_fenced_and_abandoned_work_recovers() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("state.sqlite");
        let store = Store::new(&path).unwrap();
        let other_store = Store::new(&path).unwrap();
        let now = DateTime::from_timestamp(1_700_000_100, 0).unwrap();
        let (id, _, _) = store
            .record_inbound_message(&message(10, 7, "hello"), now)
            .unwrap();
        let first = store
            .claim_oldest_inbound("one", now, Duration::from_secs(60))
            .unwrap()
            .unwrap();
        store
            .record_inbound_message(&message(11, 8, "second"), now)
            .unwrap();
        assert_eq!(first.event.attempt_count, 1);
        assert!(
            other_store
                .claim_oldest_inbound("two", now, Duration::from_secs(60))
                .unwrap()
                .is_none()
        );
        assert_eq!(
            store
                .record_inbound_message(&message(10, 7, "hello"), now)
                .unwrap()
                .1,
            InboundDuplicate::Active
        );

        let recovered_at = now + chrono::Duration::seconds(61);
        let second = store
            .claim_oldest_inbound("two", recovered_at, Duration::from_secs(60))
            .unwrap()
            .unwrap();
        assert_eq!(second.event.id, id);
        assert_eq!(second.event.attempt_count, 2);
        assert!(store.assert_inbound_claim(&first, recovered_at).is_err());
        store.assert_inbound_claim(&second, recovered_at).unwrap();
        store.complete_inbound_claim(&second, recovered_at).unwrap();
        assert_eq!(
            store.get_inbound_event(id).unwrap().status,
            InboundStatus::Completed
        );
        let following = other_store
            .claim_oldest_inbound("two", recovered_at, Duration::from_secs(60))
            .unwrap()
            .unwrap();
        assert_eq!(following.event.message.message_id, 8);
        other_store
            .complete_inbound_claim(&following, recovered_at)
            .unwrap();
        assert_eq!(
            store
                .record_inbound_message(&message(10, 7, "hello"), recovered_at)
                .unwrap()
                .1,
            InboundDuplicate::Completed
        );
    }

    #[test]
    fn failures_back_off_and_stop_after_five_attempts() {
        let store = Store::new(":memory:").unwrap();
        let mut now = DateTime::from_timestamp(1_700_000_100, 0).unwrap();
        store
            .record_inbound_message(&message(10, 7, "hello"), now)
            .unwrap();
        for attempt in 1..=MAX_INBOUND_ATTEMPTS {
            let claim = store
                .claim_oldest_inbound("worker", now, Duration::from_secs(60))
                .unwrap()
                .unwrap();
            assert_eq!(claim.event.attempt_count, attempt);
            let permanent = store.fail_inbound_claim(&claim, now, "failed").unwrap();
            assert_eq!(permanent, attempt == MAX_INBOUND_ATTEMPTS);
            now += chrono::Duration::minutes(10);
        }
        assert!(
            store
                .claim_oldest_inbound("worker", now, Duration::from_secs(60))
                .unwrap()
                .is_none()
        );
        assert_eq!(
            store
                .record_inbound_message(&message(10, 7, "hello"), now)
                .unwrap()
                .1,
            InboundDuplicate::PermanentlyFailed
        );
    }
}
