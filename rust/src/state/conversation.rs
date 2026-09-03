use chrono::{DateTime, Utc};
use rusqlite::{Row, params};

use super::{StateError, StateTx, Store, decode_time};

pub const CONTENT_TEXT: &str = "text";
pub const CONTENT_INBOUND_MESSAGE: &str = "inbound_message";
pub const CONTENT_SCHEDULED_REMINDER: &str = "scheduled_reminder";
pub const CONTENT_TOOL_CALL: &str = "tool_call";
pub const CONTENT_TOOL_RESULT: &str = "tool_result";
pub const AUDIENCE_CONVERSATION: &str = "conversation";
pub const AUDIENCE_INTERNAL: &str = "internal";

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ConversationTurn {
    pub id: i64,
    pub channel_id: String,
    pub sender_id: String,
    pub role: String,
    pub content_type: String,
    pub audience: String,
    pub content: String,
    pub created_at: DateTime<Utc>,
}

impl Store {
    pub fn save_conversation_message(
        &self,
        channel_id: &str,
        sender_id: &str,
        role: &str,
        content_type: &str,
        content: &str,
    ) -> Result<i64, StateError> {
        self.with_tx(|tx| {
            tx.save_conversation_message(
                channel_id,
                sender_id,
                role,
                content_type,
                AUDIENCE_CONVERSATION,
                content,
            )
        })
    }

    pub fn get_conversation_history(
        &self,
        channel_id: &str,
        sender_id: &str,
    ) -> Result<Vec<ConversationTurn>, StateError> {
        let connection = self.lock()?;
        collect_history(
            &connection,
            "SELECT id, channel_id, sender_id, role, content_type, audience, content, created_at FROM conversation_history WHERE channel_id=?1 AND sender_id=?2 AND audience='conversation' ORDER BY id ASC",
            params![channel_id, sender_id],
        )
    }

    pub fn get_all_conversation_history(&self) -> Result<Vec<ConversationTurn>, StateError> {
        let connection = self.lock()?;
        collect_history(
            &connection,
            "SELECT id, channel_id, sender_id, role, content_type, audience, content, created_at FROM conversation_history WHERE audience='conversation' ORDER BY id ASC",
            [],
        )
    }
}

impl StateTx<'_> {
    pub fn save_conversation_message(
        &self,
        channel_id: &str,
        sender_id: &str,
        role: &str,
        content_type: &str,
        audience: &str,
        content: &str,
    ) -> Result<i64, StateError> {
        if !matches!(audience, AUDIENCE_CONVERSATION | AUDIENCE_INTERNAL) {
            return Err(StateError::Validation(format!(
                "unsupported conversation audience {audience:?}"
            )));
        }
        self.transaction.execute(
            "INSERT INTO conversation_history (channel_id, sender_id, role, content_type, audience, content) VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
            params![channel_id, sender_id, role, content_type, audience, content],
        )?;
        Ok(self.transaction.last_insert_rowid())
    }
}

fn collect_history<P: rusqlite::Params>(
    connection: &rusqlite::Connection,
    sql: &str,
    params: P,
) -> Result<Vec<ConversationTurn>, StateError> {
    let mut statement = connection.prepare(sql)?;
    let raw = statement
        .query_map(params, raw_turn)?
        .collect::<Result<Vec<_>, _>>()?;
    raw.into_iter().map(ConversationTurn::try_from).collect()
}

type RawTurn = (i64, String, String, String, String, String, String, String);

fn raw_turn(row: &Row<'_>) -> rusqlite::Result<RawTurn> {
    Ok((
        row.get(0)?,
        row.get(1)?,
        row.get(2)?,
        row.get(3)?,
        row.get(4)?,
        row.get(5)?,
        row.get(6)?,
        row.get(7)?,
    ))
}

impl TryFrom<RawTurn> for ConversationTurn {
    type Error = StateError;

    fn try_from(raw: RawTurn) -> Result<Self, Self::Error> {
        Ok(Self {
            id: raw.0,
            channel_id: raw.1,
            sender_id: raw.2,
            role: raw.3,
            content_type: raw.4,
            audience: raw.5,
            content: raw.6,
            created_at: decode_time(raw.7)?,
        })
    }
}
