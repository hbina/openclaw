use serde::{Deserialize, Serialize};

use crate::{
    channels::{ReplyAuthor, ReplyContext},
    providers::{Message, MessageRole},
    state::{
        CONTENT_INBOUND_MESSAGE, CONTENT_SCHEDULED_REMINDER, CONTENT_TEXT, CONTENT_TOOL_CALL,
        CONTENT_TOOL_RESULT, ConversationTurn,
    },
    tools::ToolResult,
};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PersistedInboundMessage {
    pub content: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reply: Option<ReplyContext>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PersistedScheduledReminder {
    pub reminder_id: i64,
    pub message: String,
    pub scheduled_for: String,
}

pub fn render_inbound_message(inbound: &PersistedInboundMessage) -> Result<String, String> {
    let Some(reply) = &inbound.reply else {
        return Ok(inbound.content.clone());
    };
    let author = match reply.author {
        ReplyAuthor::User => "user",
        ReplyAuthor::Assistant => "assistant",
        ReplyAuthor::Other => "other",
    };
    let body = if reply.body.trim().is_empty() {
        if !reply.content_unavailable {
            return Err("reply body is empty without content-unavailable marker".into());
        }
        "[non-text Telegram message; content unavailable]"
    } else {
        if reply.content_unavailable {
            return Err("reply body conflicts with content-unavailable marker".into());
        }
        reply.body.trim()
    };
    let mut rendered = format!("Reply context:\nAuthor: {author}\nMessage:\n{body}");
    if !reply.selected_text.trim().is_empty() {
        rendered.push_str(&format!(
            "\n\nSelected text:\n{}",
            reply.selected_text.trim()
        ));
    }
    rendered.push_str(&format!("\n\nCurrent user message:\n{}", inbound.content));
    Ok(rendered)
}

pub fn render_scheduled_reminder(reminder: &PersistedScheduledReminder) -> Result<String, String> {
    if reminder.reminder_id < 1 {
        return Err("scheduled reminder id must be positive".into());
    }
    if reminder.message.trim().is_empty() {
        return Err("scheduled reminder message must not be empty".into());
    }
    if reminder.scheduled_for.trim().is_empty() {
        return Err("scheduled reminder occurrence is required".into());
    }
    Ok(format!(
        "Scheduled reminder event:\nReminder ID: {}\nScheduled for: {}\nStored message:\n{}",
        reminder.reminder_id,
        reminder.scheduled_for,
        reminder.message.trim()
    ))
}

pub fn history_message(turn: &ConversationTurn) -> Result<Message, String> {
    match turn.content_type.as_str() {
        CONTENT_TEXT => {
            let role = parse_role(&turn.role)?;
            if !matches!(
                role,
                MessageRole::User | MessageRole::Assistant | MessageRole::System
            ) {
                return Err(format!("invalid text role {:?}", turn.role));
            }
            Ok(Message::text(role, &turn.content))
        }
        CONTENT_INBOUND_MESSAGE => {
            if turn.role != "user" {
                return Err(format!("invalid inbound message role {:?}", turn.role));
            }
            let inbound: PersistedInboundMessage = serde_json::from_str(&turn.content)
                .map_err(|error| format!("decode inbound message: {error}"))?;
            Ok(Message::text(
                MessageRole::User,
                render_inbound_message(&inbound)?,
            ))
        }
        CONTENT_SCHEDULED_REMINDER => {
            if turn.role != "user" {
                return Err(format!("invalid scheduled reminder role {:?}", turn.role));
            }
            let reminder: PersistedScheduledReminder = serde_json::from_str(&turn.content)
                .map_err(|error| format!("decode scheduled reminder: {error}"))?;
            Ok(Message::text(
                MessageRole::User,
                render_scheduled_reminder(&reminder)?,
            ))
        }
        CONTENT_TOOL_CALL => {
            let message: Message = serde_json::from_str(&turn.content)
                .map_err(|error| format!("decode tool call: {error}"))?;
            if message.role != MessageRole::Assistant || message.tool_calls.is_empty() {
                return Err("invalid assistant tool-call payload".into());
            }
            Ok(message)
        }
        CONTENT_TOOL_RESULT => {
            let result: ToolResult = serde_json::from_str(&turn.content)
                .map_err(|error| format!("decode tool result: {error}"))?;
            if result.tool_call_id.is_empty() {
                return Err("tool result is missing tool_call_id".into());
            }
            Ok(result.message())
        }
        other => Err(format!("unknown content type {other:?}")),
    }
}

pub fn reconstruct_history(turns: &[ConversationTurn]) -> Result<Vec<Message>, String> {
    let start = turns.iter().position(is_user_turn).unwrap_or(turns.len());
    let turns = &turns[start..];
    let mut messages = Vec::with_capacity(turns.len());
    let mut index = 0;
    while index < turns.len() {
        let message = history_message(&turns[index])
            .map_err(|error| format!("load structured history row {}: {error}", turns[index].id))?;
        if message.tool_calls.is_empty() {
            if message.role == MessageRole::Tool {
                return Err(format!(
                    "load structured history row {}: tool result without assistant call",
                    turns[index].id
                ));
            }
            messages.push(message);
            index += 1;
            continue;
        }
        let mut call_ids: std::collections::HashSet<_> = message
            .tool_calls
            .iter()
            .map(|call| call.id.clone())
            .collect();
        if turns.len() - index - 1 < call_ids.len() {
            break;
        }
        let mut sequence = vec![message];
        let mut complete = true;
        for offset in 1..=call_ids.len() {
            let Ok(result) = history_message(&turns[index + offset]) else {
                complete = false;
                break;
            };
            if result.role != MessageRole::Tool || !call_ids.remove(&result.tool_call_id) {
                complete = false;
                break;
            }
            sequence.push(result);
        }
        if !complete || !call_ids.is_empty() {
            break;
        }
        index += sequence.len();
        messages.extend(sequence);
    }
    Ok(messages)
}

pub(crate) fn is_user_turn(turn: &ConversationTurn) -> bool {
    turn.role == "user"
        && matches!(
            turn.content_type.as_str(),
            CONTENT_TEXT | CONTENT_INBOUND_MESSAGE | CONTENT_SCHEDULED_REMINDER
        )
}

fn parse_role(role: &str) -> Result<MessageRole, String> {
    match role {
        "user" => Ok(MessageRole::User),
        "assistant" => Ok(MessageRole::Assistant),
        "system" => Ok(MessageRole::System),
        "tool" => Ok(MessageRole::Tool),
        _ => Err(format!("invalid message role {role:?}")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::providers::{FunctionCall, ToolCall};
    use chrono::Utc;

    fn turn(id: i64, role: &str, kind: &str, content: String) -> ConversationTurn {
        ConversationTurn {
            id,
            channel_id: "telegram".into(),
            sender_id: "owner".into(),
            role: role.into(),
            content_type: kind.into(),
            audience: "conversation".into(),
            content,
            created_at: Utc::now(),
        }
    }

    #[test]
    fn renders_reply_context_and_rejects_conflicts() {
        let inbound = PersistedInboundMessage {
            content: "explain this".into(),
            reply: Some(ReplyContext {
                message_id: "7".into(),
                author: ReplyAuthor::Assistant,
                body: "earlier answer".into(),
                selected_text: "answer".into(),
                content_unavailable: false,
            }),
        };
        let rendered = render_inbound_message(&inbound).unwrap();
        assert!(rendered.contains("Author: assistant"));
        assert!(rendered.contains("Selected text:\nanswer"));
    }

    #[test]
    fn incomplete_tool_sequence_is_not_replayed() {
        let user = turn(1, "user", CONTENT_TEXT, "hello".into());
        let call = Message {
            role: MessageRole::Assistant,
            content: String::new(),
            tool_calls: vec![ToolCall {
                id: "call-1".into(),
                kind: "function".into(),
                function: FunctionCall {
                    name: "add_task".into(),
                    arguments: "{}".into(),
                },
            }],
            tool_call_id: String::new(),
        };
        let call = turn(
            2,
            "assistant",
            CONTENT_TOOL_CALL,
            serde_json::to_string(&call).unwrap(),
        );
        assert_eq!(reconstruct_history(&[user, call]).unwrap().len(), 1);
    }

    #[test]
    fn complete_tool_sequence_preserves_exact_call_id() {
        let user = turn(1, "user", CONTENT_TEXT, "hello".into());
        let call_message = Message {
            role: MessageRole::Assistant,
            content: String::new(),
            tool_calls: vec![ToolCall {
                id: "call-1".into(),
                kind: "function".into(),
                function: FunctionCall {
                    name: "add_task".into(),
                    arguments: "{}".into(),
                },
            }],
            tool_call_id: String::new(),
        };
        let result = ToolResult {
            tool_call_id: "call-1".into(),
            name: "add_task".into(),
            content: "{}".into(),
            is_error: false,
        };
        let turns = [
            user,
            turn(
                2,
                "assistant",
                CONTENT_TOOL_CALL,
                serde_json::to_string(&call_message).unwrap(),
            ),
            turn(
                3,
                "tool",
                CONTENT_TOOL_RESULT,
                serde_json::to_string(&result).unwrap(),
            ),
            turn(4, "assistant", CONTENT_TEXT, "done".into()),
        ];
        let messages = reconstruct_history(&turns).unwrap();
        assert_eq!(messages.len(), 4);
        assert_eq!(messages[2].tool_call_id, "call-1");
    }
}
