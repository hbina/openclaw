use std::{
    collections::HashMap,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use async_trait::async_trait;
use chrono::{DateTime, Utc};
use pulldown_cmark::{Event, Parser, Tag};
use serde::{Deserialize, Serialize};
use thiserror::Error;
use tracing::{debug, error, info, trace, warn};

use crate::state::{InboundDuplicate, Store};

const MAX_TELEGRAM_RESPONSE_BYTES: usize = 16 << 20;
const MAX_TELEGRAM_TEXT_BYTES: usize = 16 << 10;
const MAX_TELEGRAM_REPLY_BYTES: usize = 16 << 10;
const MAX_TELEGRAM_QUOTE_BYTES: usize = 4 << 10;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum ReplyAuthor {
    User,
    Assistant,
    Other,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ReplyContext {
    pub message_id: String,
    pub author: ReplyAuthor,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub body: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub selected_text: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub content_unavailable: bool,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct InboundMessage {
    pub channel_id: String,
    pub update_id: i64,
    pub message_id: i64,
    pub chat_id: i64,
    pub sender_id: i64,
    pub timestamp: DateTime<Utc>,
    pub content: String,
    pub reply: Option<ReplyContext>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DeliveryReceipt {
    pub message_id: String,
}

#[async_trait]
pub trait Channel: Send + Sync {
    fn id(&self) -> &str;
    async fn start(&self) -> Result<(), ChannelError>;
    async fn stop(&self) -> Result<(), ChannelError>;
    async fn send_message(
        &self,
        recipient_id: &str,
        content: &str,
    ) -> Result<DeliveryReceipt, ChannelError>;
}

#[derive(Debug, Error)]
pub enum ChannelError {
    #[error("channel {0:?} not found")]
    NotFound(String),
    #[error("channel operation failed: {0}")]
    Operation(String),
}

#[derive(Default)]
pub struct Registry {
    channels: HashMap<String, Arc<dyn Channel>>,
}

impl Registry {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn register(&mut self, channel: Arc<dyn Channel>) {
        debug!(channel = channel.id(), "registering channel");
        self.channels.insert(channel.id().to_owned(), channel);
    }

    pub fn get(&self, id: &str) -> Result<Arc<dyn Channel>, ChannelError> {
        self.channels
            .get(id)
            .cloned()
            .ok_or_else(|| ChannelError::NotFound(id.to_owned()))
    }

    pub async fn start_all(&self) -> Result<(), ChannelError> {
        debug!(channel_count = self.channels.len(), "starting channels");
        for channel in self.channels.values() {
            debug!(channel = channel.id(), "starting channel");
            channel.start().await?;
            info!(channel = channel.id(), "channel started");
        }
        Ok(())
    }

    pub async fn stop_all(&self) -> Result<(), ChannelError> {
        let mut first_error = None;
        for channel in self.channels.values() {
            debug!(channel = channel.id(), "stopping channel");
            match channel.stop().await {
                Ok(()) => info!(channel = channel.id(), "channel stopped"),
                Err(error) => {
                    error!(channel = channel.id(), error = %error, "channel failed to stop");
                    if first_error.is_none() {
                        first_error = Some(error);
                    }
                }
            }
        }
        first_error.map_or(Ok(()), Err)
    }
}

pub struct TelegramAdapter {
    client: reqwest::Client,
    api_base: String,
    owner_user_id: i64,
    store: Arc<Store>,
    shutdown: Mutex<Option<tokio::sync::watch::Sender<bool>>>,
    worker: Mutex<Option<tokio::task::JoinHandle<()>>>,
}

impl TelegramAdapter {
    pub fn new(token: &str, owner_user_id: &str, store: Arc<Store>) -> Result<Self, ChannelError> {
        if token.trim().is_empty() {
            return Err(ChannelError::Operation(
                "telegram bot token must not be empty".into(),
            ));
        }
        let owner_user_id = owner_user_id
            .trim()
            .parse::<i64>()
            .ok()
            .filter(|id| *id > 0)
            .ok_or_else(|| {
                ChannelError::Operation("telegram owner user id must be a positive integer".into())
            })?;
        Ok(Self {
            client: reqwest::Client::new(),
            api_base: format!("https://api.telegram.org/bot{}/", token.trim()),
            owner_user_id,
            store,
            shutdown: Mutex::new(None),
            worker: Mutex::new(None),
        })
    }

    async fn call<T: for<'de> Deserialize<'de>>(
        client: &reqwest::Client,
        api_base: &str,
        method: &str,
        payload: serde_json::Value,
    ) -> Result<T, ChannelError> {
        let started = Instant::now();
        trace!(method, "Telegram API request started");
        let mut response = client
            .post(format!("{api_base}{method}"))
            .json(&payload)
            .send()
            .await
            // reqwest errors can include the request URL. Telegram embeds the
            // bot token in that URL, so never propagate the source text.
            .map_err(|_| ChannelError::Operation(format!("telegram {method} request failed")))?;
        let status = response.status();
        if response
            .content_length()
            .is_some_and(|size| size > MAX_TELEGRAM_RESPONSE_BYTES as u64)
        {
            return Err(ChannelError::Operation(format!(
                "telegram {method} response exceeded {MAX_TELEGRAM_RESPONSE_BYTES} bytes"
            )));
        }
        let mut body = Vec::with_capacity(
            response
                .content_length()
                .unwrap_or(0)
                .min(MAX_TELEGRAM_RESPONSE_BYTES as u64) as usize,
        );
        while let Some(chunk) = response.chunk().await.map_err(|_| {
            ChannelError::Operation(format!("telegram {method} response read failed"))
        })? {
            if body.len().saturating_add(chunk.len()) > MAX_TELEGRAM_RESPONSE_BYTES {
                return Err(ChannelError::Operation(format!(
                    "telegram {method} response exceeded {MAX_TELEGRAM_RESPONSE_BYTES} bytes"
                )));
            }
            body.extend_from_slice(&chunk);
        }
        trace!(
            method,
            status = status.as_u16(),
            response_bytes = body.len(),
            elapsed_ms = started.elapsed().as_millis(),
            "Telegram API request completed"
        );
        let result: TelegramResponse<T> = serde_json::from_slice(&body).map_err(|error| {
            ChannelError::Operation(format!("telegram {method} response was invalid: {error}"))
        })?;
        if !status.is_success() || !result.ok {
            return Err(ChannelError::Operation(format!(
                "telegram {method} failed with status {}: {}",
                status.as_u16(),
                result.description.unwrap_or_else(|| "unknown error".into())
            )));
        }
        result.result.ok_or_else(|| {
            ChannelError::Operation(format!("telegram {method} response omitted result"))
        })
    }

    async fn polling_loop(
        client: reqwest::Client,
        api_base: String,
        owner_user_id: i64,
        bot_id: i64,
        store: Arc<Store>,
        mut shutdown: tokio::sync::watch::Receiver<bool>,
    ) {
        info!(bot_id, "Telegram polling worker started");
        let mut offset = match store.channel_next_update_id("telegram") {
            Ok(offset) => offset,
            Err(error) => {
                error!(error = %error, "failed to load Telegram polling checkpoint");
                return;
            }
        };
        loop {
            if *shutdown.borrow() {
                info!("Telegram polling worker stopping");
                return;
            }
            let request = Self::call::<Vec<TelegramUpdate>>(
                &client,
                &api_base,
                "getUpdates",
                serde_json::json!({
                    "offset": offset,
                    "timeout": 10,
                    "allowed_updates": ["message"]
                }),
            );
            let updates = tokio::select! {
                _ = shutdown.changed() => {
                    info!("Telegram polling worker stopping");
                    return;
                },
                result = request => match result {
                    Ok(updates) => updates,
                    Err(error) => {
                        warn!(error = %error, "Telegram polling failed; retrying");
                        tokio::time::sleep(Duration::from_secs(1)).await;
                        continue;
                    }
                }
            };
            if !updates.is_empty() {
                debug!(update_count = updates.len(), "Telegram updates received");
            }
            for update in updates {
                let inbound = match telegram_inbound_message(&update, bot_id, owner_user_id) {
                    Ok(Some(inbound)) => inbound,
                    Ok(None) => {
                        trace!(
                            update_id = update.update_id,
                            "ignoring unsupported Telegram update"
                        );
                        match store.advance_channel_checkpoint(
                            "telegram",
                            update.update_id,
                            Utc::now(),
                        ) {
                            Ok(next) => offset = offset.max(next),
                            Err(error) => {
                                error!(update_id = update.update_id, error = %error, "failed to persist Telegram checkpoint");
                                break;
                            }
                        }
                        continue;
                    }
                    Err(error) => {
                        warn!(update_id = update.update_id, error = %error, "rejected Telegram message");
                        match store.advance_channel_checkpoint(
                            "telegram",
                            update.update_id,
                            Utc::now(),
                        ) {
                            Ok(next) => offset = offset.max(next),
                            Err(error) => {
                                error!(update_id = update.update_id, error = %error, "failed to persist rejected Telegram checkpoint");
                                break;
                            }
                        }
                        continue;
                    }
                };
                info!(
                    update_id = inbound.update_id,
                    message_id = inbound.message_id,
                    chat_id = inbound.chat_id,
                    message_bytes = inbound.content.len(),
                    "accepted Telegram owner private message"
                );
                match store.record_inbound_message(&inbound, Utc::now()) {
                    Ok((event_id, duplicate, next)) => {
                        offset = offset.max(next);
                        match duplicate {
                            InboundDuplicate::Inserted => {
                                info!(event_id, "Telegram message durably queued")
                            }
                            InboundDuplicate::Completed => {
                                debug!(event_id, "completed Telegram duplicate ignored")
                            }
                            InboundDuplicate::Active => {
                                debug!(event_id, "active Telegram duplicate ignored")
                            }
                            InboundDuplicate::Retryable => {
                                debug!(event_id, "retryable Telegram duplicate retained")
                            }
                            InboundDuplicate::PermanentlyFailed => {
                                warn!(event_id, "permanently failed Telegram duplicate ignored")
                            }
                        }
                    }
                    Err(error) => {
                        error!(update_id = inbound.update_id, error = %error, "failed to durably queue Telegram message");
                        break;
                    }
                }
            }
        }
    }
}

#[async_trait]
impl Channel for TelegramAdapter {
    fn id(&self) -> &str {
        "telegram"
    }

    async fn start(&self) -> Result<(), ChannelError> {
        if self
            .worker
            .lock()
            .map_err(|_| ChannelError::Operation("telegram worker lock poisoned".into()))?
            .is_some()
        {
            return Err(ChannelError::Operation(
                "telegram channel is already started".into(),
            ));
        }
        debug!("validating Telegram bot credentials");
        let me: TelegramUser =
            Self::call(&self.client, &self.api_base, "getMe", serde_json::json!({})).await?;
        let (sender, receiver) = tokio::sync::watch::channel(false);
        let worker = tokio::spawn(Self::polling_loop(
            self.client.clone(),
            self.api_base.clone(),
            self.owner_user_id,
            me.id,
            Arc::clone(&self.store),
            receiver,
        ));
        *self
            .shutdown
            .lock()
            .map_err(|_| ChannelError::Operation("telegram shutdown lock poisoned".into()))? =
            Some(sender);
        *self
            .worker
            .lock()
            .map_err(|_| ChannelError::Operation("telegram worker lock poisoned".into()))? =
            Some(worker);
        info!(bot_id = me.id, "Telegram adapter started");
        Ok(())
    }

    async fn stop(&self) -> Result<(), ChannelError> {
        debug!("stopping Telegram adapter");
        if let Some(sender) = self
            .shutdown
            .lock()
            .map_err(|_| ChannelError::Operation("telegram shutdown lock poisoned".into()))?
            .take()
        {
            let _ = sender.send(true);
        }
        let worker = self
            .worker
            .lock()
            .map_err(|_| ChannelError::Operation("telegram worker lock poisoned".into()))?
            .take();
        if let Some(worker) = worker {
            worker
                .await
                .map_err(|error| ChannelError::Operation(error.to_string()))?;
        }
        info!("Telegram adapter stopped");
        Ok(())
    }

    async fn send_message(
        &self,
        recipient_id: &str,
        content: &str,
    ) -> Result<DeliveryReceipt, ChannelError> {
        let recipient = recipient_id
            .parse::<i64>()
            .map_err(|_| ChannelError::Operation("invalid Telegram recipient ID".into()))?;
        debug!(
            recipient_id,
            message_bytes = content.len(),
            "sending Telegram message"
        );
        let (text, entities) = telegram_bold_entities(content);
        let sent: TelegramMessage = Self::call(
            &self.client,
            &self.api_base,
            "sendMessage",
            serde_json::json!({"chat_id": recipient, "text": text, "entities": entities}),
        )
        .await?;
        info!(
            recipient_id,
            message_id = sent.message_id,
            "Telegram message sent"
        );
        Ok(DeliveryReceipt {
            message_id: sent.message_id.to_string(),
        })
    }
}

#[derive(Debug, PartialEq, Eq, Serialize)]
struct TelegramTextEntity {
    #[serde(rename = "type")]
    kind: &'static str,
    offset: usize,
    length: usize,
}

// Keep all non-bold Markdown as written. Telegram's MarkdownV2 differs from
// CommonMark and can reject an entire message when punctuation is unescaped.
fn telegram_bold_entities(content: &str) -> (String, Vec<TelegramTextEntity>) {
    let mut markers = Vec::new();
    let mut spans = Vec::new();
    for (event, range) in Parser::new(content).into_offset_iter() {
        if let Event::Start(Tag::Strong) = event {
            // OffsetIter gives the entire strong span for both Start and End.
            // CommonMark strong delimiters are two '*' or two '_' characters.
            if range.end >= range.start + 4 {
                let opening = content.get(range.start..range.start + 2);
                let closing = content.get(range.end - 2..range.end);
                if matches!(opening, Some("**" | "__")) && opening == closing {
                    markers.push(range.start..range.start + 2);
                    markers.push(range.end - 2..range.end);
                    spans.push((range.start + 2, range.end - 2));
                }
            }
        }
    }
    markers.sort_by_key(|range| range.start);
    markers.dedup();

    let mut text = String::with_capacity(content.len());
    let mut cursor = 0;
    for marker in &markers {
        text.push_str(&content[cursor..marker.start]);
        cursor = marker.end;
    }
    text.push_str(&content[cursor..]);

    let mut entities = Vec::with_capacity(spans.len());
    for (start, end) in spans {
        let offset = utf16_position_without_markers(content, &markers, start);
        let length = utf16_position_without_markers(content, &markers, end) - offset;
        if length > 0 {
            entities.push(TelegramTextEntity {
                kind: "bold",
                offset,
                length,
            });
        }
    }
    entities.sort_by_key(|entity| entity.offset);
    let mut merged: Vec<TelegramTextEntity> = Vec::with_capacity(entities.len());
    for entity in entities {
        if let Some(previous) = merged.last_mut() {
            let previous_end = previous.offset + previous.length;
            if entity.offset <= previous_end {
                previous.length = previous_end.max(entity.offset + entity.length) - previous.offset;
                continue;
            }
        }
        merged.push(entity);
    }
    (text, merged)
}

fn utf16_position_without_markers(
    content: &str,
    markers: &[std::ops::Range<usize>],
    position: usize,
) -> usize {
    let mut offset = content[..position].encode_utf16().count();
    for marker in markers {
        if marker.end <= position {
            offset -= content[marker.clone()].encode_utf16().count();
        }
    }
    offset
}

#[derive(Deserialize)]
struct TelegramResponse<T> {
    ok: bool,
    result: Option<T>,
    description: Option<String>,
}

#[derive(Deserialize)]
struct TelegramUpdate {
    update_id: i64,
    message: Option<TelegramMessage>,
}

#[derive(Deserialize)]
struct TelegramUser {
    id: i64,
}

#[derive(Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
enum TelegramChatType {
    Private,
    Group,
    Supergroup,
    Channel,
    #[serde(other)]
    Unsupported,
}

#[derive(Debug, Deserialize)]
struct TelegramChat {
    id: i64,
    #[serde(rename = "type")]
    kind: TelegramChatType,
}

#[derive(Deserialize)]
struct TelegramQuote {
    text: String,
}

#[derive(Deserialize)]
struct TelegramMessage {
    message_id: i64,
    from: Option<TelegramUser>,
    chat: TelegramChat,
    date: i64,
    text: Option<String>,
    caption: Option<String>,
    reply_to_message: Option<Box<TelegramMessage>>,
    quote: Option<TelegramQuote>,
}

fn telegram_inbound_message(
    update: &TelegramUpdate,
    bot_id: i64,
    owner_user_id: i64,
) -> Result<Option<InboundMessage>, ChannelError> {
    let Some(message) = update.message.as_ref() else {
        return Ok(None);
    };
    let Some(text) = message.text.as_deref() else {
        return Ok(None);
    };
    let sender = message.from.as_ref().ok_or_else(|| {
        ChannelError::Operation("telegram text message is missing a sender".into())
    })?;
    if message.chat.kind != TelegramChatType::Private
        || sender.id != owner_user_id
        || message.chat.id != owner_user_id
    {
        return Err(ChannelError::Operation(
            "telegram message did not satisfy the private owner chat contract".into(),
        ));
    }
    let timestamp = DateTime::from_timestamp(message.date, 0)
        .filter(|value| value.timestamp() > 0)
        .ok_or_else(|| ChannelError::Operation("telegram message timestamp is invalid".into()))?;
    let content = normalize_telegram_text(text, MAX_TELEGRAM_TEXT_BYTES, "message")?;
    if content.trim().is_empty() {
        return Err(ChannelError::Operation(
            "telegram message text must not be empty".into(),
        ));
    }
    let mut inbound = InboundMessage {
        channel_id: "telegram".into(),
        update_id: update.update_id,
        sender_id: sender.id,
        chat_id: message.chat.id,
        message_id: message.message_id,
        timestamp,
        content,
        reply: None,
    };
    if let Some(reply) = &message.reply_to_message {
        let author = match reply.from.as_ref().map(|user| user.id) {
            Some(id) if id == bot_id => ReplyAuthor::Assistant,
            Some(id) if id == sender.id => ReplyAuthor::User,
            _ => ReplyAuthor::Other,
        };
        let raw_body = reply
            .text
            .as_deref()
            .or(reply.caption.as_deref())
            .unwrap_or("");
        let body = normalize_telegram_text(raw_body, MAX_TELEGRAM_REPLY_BYTES, "reply")?;
        let selected_text = normalize_telegram_text(
            message
                .quote
                .as_ref()
                .map(|quote| quote.text.as_str())
                .unwrap_or(""),
            MAX_TELEGRAM_QUOTE_BYTES,
            "quote",
        )?;
        inbound.reply = Some(ReplyContext {
            message_id: reply.message_id.to_string(),
            author,
            content_unavailable: body.trim().is_empty(),
            body,
            selected_text,
        });
    }
    Ok(Some(inbound))
}

fn normalize_telegram_text(
    value: &str,
    max_bytes: usize,
    field: &str,
) -> Result<String, ChannelError> {
    let normalized = value.replace("\r\n", "\n").replace('\r', "\n");
    if normalized.len() > max_bytes {
        return Err(ChannelError::Operation(format!(
            "telegram {field} exceeded {max_bytes} bytes"
        )));
    }
    if normalized
        .chars()
        .any(|character| character.is_control() && !matches!(character, '\n' | '\t'))
    {
        return Err(ChannelError::Operation(format!(
            "telegram {field} contained an unsafe control character"
        )));
    }
    Ok(normalized)
}

#[cfg(test)]
mod telegram_tests {
    use super::*;

    #[test]
    fn outgoing_bold_uses_telegram_entities_and_preserves_lists() {
        let source = "**Technical & Quant Research (High Intensity)**\n* **Chlistalla paper** (Task 12)\n* **Papers in your ChatGPT share link** (Task 26)";
        let (text, entities) = telegram_bold_entities(source);
        assert_eq!(
            text,
            "Technical & Quant Research (High Intensity)\n* Chlistalla paper (Task 12)\n* Papers in your ChatGPT share link (Task 26)"
        );
        assert_eq!(entities.len(), 3);
        assert_eq!(entities[0].offset, 0);
        assert_eq!(
            entities[0].length,
            "Technical & Quant Research (High Intensity)".len()
        );
        assert_eq!(
            entities[1].offset,
            "Technical & Quant Research (High Intensity)\n* ".len()
        );
        assert_eq!(entities[1].length, "Chlistalla paper".len());
        assert_eq!(
            entities[2].length,
            "Papers in your ChatGPT share link".len()
        );
        assert!(entities.iter().all(|entity| entity.kind == "bold"));
    }

    #[test]
    fn outgoing_bold_counts_utf16_and_ignores_code_and_escapes() {
        let source =
            "👩‍💻 **漢字** and __é__\n`**code**` \\**literal** **unfinished\n```\n**fenced**\n```";
        let (text, entities) = telegram_bold_entities(source);
        assert_eq!(
            text,
            "👩‍💻 漢字 and é\n`**code**` \\**literal** **unfinished\n```\n**fenced**\n```"
        );
        assert_eq!(entities.len(), 2);
        assert_eq!(entities[0].offset, "👩‍💻 ".encode_utf16().count());
        assert_eq!(entities[0].length, "漢字".encode_utf16().count());
        assert_eq!(entities[1].offset, "👩‍💻 漢字 and ".encode_utf16().count());
        assert_eq!(entities[1].length, "é".encode_utf16().count());
        assert_eq!(
            telegram_bold_entities("plain text"),
            ("plain text".into(), vec![])
        );
    }

    #[test]
    fn outgoing_bold_handles_nested_and_adjacent_markdown() {
        for source in [
            "**one** **two**",
            "**bold _italic_ text**",
            "***bold and italic***",
            "****four stars****",
            "a **bold** word and * list marker",
        ] {
            let (text, entities) = telegram_bold_entities(source);
            assert!(text.len() <= source.len());
            for entity in entities {
                assert!(entity.length > 0);
                assert!(entity.offset + entity.length <= text.encode_utf16().count());
            }
        }
        let (text, entities) = telegram_bold_entities("****four stars****");
        assert_eq!(text, "four stars");
        assert_eq!(
            entities,
            vec![TelegramTextEntity {
                kind: "bold",
                offset: 0,
                length: "four stars".len(),
            }]
        );
    }

    #[test]
    fn fixtures_enforce_private_owner_chat_and_map_reply_context() {
        let store = Arc::new(Store::new(":memory:").unwrap());
        assert!(TelegramAdapter::new("token", "not-an-id", store).is_err());
        let updates: Vec<TelegramUpdate> =
            serde_json::from_str(include_str!("../testdata/telegram_updates.json")).unwrap();

        let inbound = telegram_inbound_message(&updates[0], 100, 42)
            .unwrap()
            .unwrap();
        assert_eq!(inbound.update_id, 1001);
        assert_eq!(inbound.message_id, 9);
        assert_eq!(inbound.chat_id, 42);
        assert_eq!(inbound.sender_id, 42);
        assert_eq!(inbound.content, "What about this?\nThanks\n");
        let reply = inbound.reply.unwrap();
        assert_eq!(reply.author, ReplyAuthor::Assistant);
        assert_eq!(reply.selected_text, "answer");

        assert!(
            updates[1..8]
                .iter()
                .all(|update| telegram_inbound_message(update, 100, 42).is_err())
        );
        let handler_inputs: Vec<_> = updates[1..8]
            .iter()
            .filter_map(|update| telegram_inbound_message(update, 100, 42).ok().flatten())
            .collect();
        assert!(handler_inputs.is_empty());
        assert!(
            telegram_inbound_message(&updates[8], 100, 42)
                .unwrap()
                .is_none()
        );
    }

    #[test]
    fn canonical_message_round_trips_and_serializes_deterministically() {
        let updates: Vec<TelegramUpdate> =
            serde_json::from_str(include_str!("../testdata/telegram_updates.json")).unwrap();
        let inbound = telegram_inbound_message(&updates[0], 100, 42)
            .unwrap()
            .unwrap();
        let encoded = serde_json::to_string(&inbound).unwrap();
        assert_eq!(
            serde_json::from_str::<InboundMessage>(&encoded).unwrap(),
            inbound
        );
        assert_eq!(
            encoded,
            r#"{"channel_id":"telegram","update_id":1001,"message_id":9,"chat_id":42,"sender_id":42,"timestamp":"2026-09-05T00:00:00Z","content":"What about this?\nThanks\n","reply":{"message_id":"7","author":"assistant","body":"Earlier answer","selected_text":"answer"}}"#
        );
    }

    #[test]
    fn normalization_enforces_boundaries_and_preserves_unicode() {
        assert_eq!(
            normalize_telegram_text("a\r\nb\rc\t", 8, "message").unwrap(),
            "a\nb\nc\t"
        );
        assert!(normalize_telegram_text("\0", 8, "message").is_err());
        assert!(normalize_telegram_text("\u{7f}", 8, "message").is_err());
        assert!(normalize_telegram_text("\u{85}", 8, "message").is_err());
        assert_eq!(
            normalize_telegram_text("👩‍💻 e\u{301} 漢字 مرحبا", 64, "message").unwrap(),
            "👩‍💻 e\u{301} 漢字 مرحبا"
        );
        assert!(normalize_telegram_text(&"x".repeat(8), 8, "message").is_ok());
        assert!(normalize_telegram_text(&"x".repeat(9), 8, "message").is_err());
    }

    #[test]
    fn ingress_enforces_current_reply_and_quote_limits() {
        let mut updates: Vec<TelegramUpdate> =
            serde_json::from_str(include_str!("../testdata/telegram_updates.json")).unwrap();
        let update = &mut updates[0];
        let message = update.message.as_mut().unwrap();

        message.text = Some("x".repeat(MAX_TELEGRAM_TEXT_BYTES));
        assert!(telegram_inbound_message(update, 100, 42).is_ok());
        update.message.as_mut().unwrap().text = Some("x".repeat(MAX_TELEGRAM_TEXT_BYTES + 1));
        assert!(telegram_inbound_message(update, 100, 42).is_err());

        let message = update.message.as_mut().unwrap();
        message.text = Some("current".into());
        message.reply_to_message.as_mut().unwrap().text =
            Some("x".repeat(MAX_TELEGRAM_REPLY_BYTES));
        assert!(telegram_inbound_message(update, 100, 42).is_ok());
        update
            .message
            .as_mut()
            .unwrap()
            .reply_to_message
            .as_mut()
            .unwrap()
            .text = Some("x".repeat(MAX_TELEGRAM_REPLY_BYTES + 1));
        assert!(telegram_inbound_message(update, 100, 42).is_err());

        let message = update.message.as_mut().unwrap();
        message.reply_to_message.as_mut().unwrap().text = Some("reply".into());
        message.quote.as_mut().unwrap().text = "x".repeat(MAX_TELEGRAM_QUOTE_BYTES);
        assert!(telegram_inbound_message(update, 100, 42).is_ok());
        update
            .message
            .as_mut()
            .unwrap()
            .quote
            .as_mut()
            .unwrap()
            .text = "x".repeat(MAX_TELEGRAM_QUOTE_BYTES + 1);
        assert!(telegram_inbound_message(update, 100, 42).is_err());
    }

    #[test]
    fn ingress_rejects_empty_control_and_invalid_timestamp_content() {
        let mut updates: Vec<TelegramUpdate> =
            serde_json::from_str(include_str!("../testdata/telegram_updates.json")).unwrap();
        let update = &mut updates[0];

        update.message.as_mut().unwrap().text = Some(" \n\t".into());
        assert!(telegram_inbound_message(update, 100, 42).is_err());
        update.message.as_mut().unwrap().text = Some("unsafe\0text".into());
        assert!(telegram_inbound_message(update, 100, 42).is_err());

        let message = update.message.as_mut().unwrap();
        message.text = Some("current".into());
        message.reply_to_message.as_mut().unwrap().text = Some("unsafe\u{85}reply".into());
        assert!(telegram_inbound_message(update, 100, 42).is_err());

        let message = update.message.as_mut().unwrap();
        message.reply_to_message.as_mut().unwrap().text = Some("reply".into());
        message.quote.as_mut().unwrap().text = "unsafe\u{7f}quote".into();
        assert!(telegram_inbound_message(update, 100, 42).is_err());

        let message = update.message.as_mut().unwrap();
        message.quote.as_mut().unwrap().text = "quote".into();
        message.date = 0;
        assert!(telegram_inbound_message(update, 100, 42).is_err());
    }

    #[test]
    fn malformed_wire_fields_fail_before_admission() {
        for raw in [
            r#"{"update_id":1,"message":{"message_id":1,"from":{"id":42},"date":1,"text":"x"}}"#,
            r#"{"update_id":1,"message":{"message_id":1,"from":{"id":42},"chat":{"id":42,"type":"private"},"text":"x"}}"#,
            r#"{"message":{"message_id":1,"from":{"id":42},"chat":{"id":42,"type":"private"},"date":1,"text":"x"}}"#,
        ] {
            assert!(serde_json::from_str::<TelegramUpdate>(raw).is_err());
        }
    }

    #[test]
    fn rejected_chat_fixtures_never_create_an_admitted_message() {
        let updates: Vec<TelegramUpdate> =
            serde_json::from_str(include_str!("../testdata/telegram_updates.json")).unwrap();

        for update in &updates[1..8] {
            assert!(!matches!(
                telegram_inbound_message(update, 100, 42),
                Ok(Some(_))
            ));
        }

        assert!(matches!(
            telegram_inbound_message(&updates[0], 100, 42),
            Ok(Some(_))
        ));
    }
}
