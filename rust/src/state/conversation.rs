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
    pub conversation_id: String,
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
        conversation_id: &str,
        role: &str,
        content_type: &str,
        content: &str,
    ) -> Result<i64, StateError> {
        self.with_tx(|tx| {
            tx.save_conversation_message(
                channel_id,
                sender_id,
                conversation_id,
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
        conversation_id: &str,
    ) -> Result<Vec<ConversationTurn>, StateError> {
        let connection = self.lock()?;
        collect_history(
            &connection,
            "SELECT id, channel_id, sender_id, conversation_id, role, content_type, audience, content, created_at FROM conversation_history WHERE channel_id=?1 AND conversation_id=?2 AND audience='conversation' ORDER BY id ASC",
            params![channel_id, conversation_id],
        )
    }

    pub fn get_all_conversation_history(&self) -> Result<Vec<ConversationTurn>, StateError> {
        let connection = self.lock()?;
        collect_history(
            &connection,
            "SELECT id, channel_id, sender_id, conversation_id, role, content_type, audience, content, created_at FROM conversation_history WHERE audience='conversation' ORDER BY id ASC",
            [],
        )
    }

    #[allow(clippy::too_many_arguments)]
    pub fn save_inbound_conversation_message(
        &self,
        inbound_event_id: i64,
        channel_id: &str,
        sender_id: &str,
        conversation_id: &str,
        content: &str,
    ) -> Result<i64, StateError> {
        self.with_tx(|tx| {
            tx.transaction.execute(
                "INSERT OR IGNORE INTO conversation_history (channel_id,sender_id,conversation_id,role,content_type,audience,content,inbound_event_id) VALUES (?1,?2,?3,'user',?4,?5,?6,?7)",
                params![channel_id,sender_id,conversation_id,CONTENT_INBOUND_MESSAGE,AUDIENCE_CONVERSATION,content,inbound_event_id],
            )?;
            tx.transaction.query_row(
                "SELECT id FROM conversation_history WHERE inbound_event_id=?1 AND channel_id=?2 AND sender_id=?3 AND conversation_id=?4 AND content=?5",
                params![inbound_event_id,channel_id,sender_id,conversation_id,content],
                |row| row.get(0),
            ).map_err(StateError::from)
        })
    }

    #[allow(clippy::too_many_arguments)]
    pub fn save_conversation_message_for_event(
        &self,
        source_trace_event_id: i64,
        channel_id: &str,
        sender_id: &str,
        conversation_id: &str,
        role: &str,
        content_type: &str,
        audience: &str,
        content: &str,
    ) -> Result<i64, StateError> {
        self.with_tx(|tx| {
            tx.save_conversation_message_for_event(
                source_trace_event_id,
                channel_id,
                sender_id,
                conversation_id,
                role,
                content_type,
                audience,
                content,
            )
        })
    }
}

impl StateTx<'_> {
    #[allow(clippy::too_many_arguments)]
    pub fn save_conversation_message(
        &self,
        channel_id: &str,
        sender_id: &str,
        conversation_id: &str,
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
            "INSERT INTO conversation_history (channel_id, sender_id, conversation_id, role, content_type, audience, content) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
            params![channel_id, sender_id, conversation_id, role, content_type, audience, content],
        )?;
        Ok(self.transaction.last_insert_rowid())
    }

    #[allow(clippy::too_many_arguments)]
    pub fn save_conversation_message_for_event(
        &self,
        source_trace_event_id: i64,
        channel_id: &str,
        sender_id: &str,
        conversation_id: &str,
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
            "INSERT OR IGNORE INTO conversation_history (channel_id,sender_id,conversation_id,role,content_type,audience,content,source_trace_event_id) VALUES (?1,?2,?3,?4,?5,?6,?7,?8)",
            params![channel_id,sender_id,conversation_id,role,content_type,audience,content,source_trace_event_id],
        )?;
        self.transaction.query_row(
            "SELECT id FROM conversation_history WHERE source_trace_event_id=?1 AND channel_id=?2 AND sender_id=?3 AND conversation_id=?4 AND role=?5 AND content_type=?6 AND audience=?7 AND content=?8",
            params![source_trace_event_id,channel_id,sender_id,conversation_id,role,content_type,audience,content],
            |row| row.get(0),
        ).map_err(StateError::from)
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

type RawTurn = (
    i64,
    String,
    String,
    String,
    String,
    String,
    String,
    String,
    String,
);

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
        row.get(8)?,
    ))
}

impl TryFrom<RawTurn> for ConversationTurn {
    type Error = StateError;

    fn try_from(raw: RawTurn) -> Result<Self, Self::Error> {
        Ok(Self {
            id: raw.0,
            channel_id: raw.1,
            sender_id: raw.2,
            conversation_id: raw.3,
            role: raw.4,
            content_type: raw.5,
            audience: raw.6,
            content: raw.7,
            created_at: decode_time(raw.8)?,
        })
    }
}
