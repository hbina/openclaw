use std::{
    collections::HashMap,
    future::Future,
    pin::Pin,
    sync::{Arc, Mutex},
    time::Duration,
};

use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use thiserror::Error;

const MAX_TELEGRAM_RESPONSE_BYTES: usize = 16 << 20;

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

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Message {
    pub channel_id: String,
    pub sender_id: String,
    pub message_id: String,
    pub content: String,
    pub reply: Option<ReplyContext>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DeliveryReceipt {
    pub message_id: String,
}

pub type Handler =
    Arc<dyn Fn(Message) -> Pin<Box<dyn Future<Output = Result<(), String>> + Send>> + Send + Sync>;

#[async_trait]
pub trait Channel: Send + Sync {
    fn id(&self) -> &str;
    async fn start(&self, handler: Handler) -> Result<(), ChannelError>;
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
        self.channels.insert(channel.id().to_owned(), channel);
    }

    pub fn get(&self, id: &str) -> Result<Arc<dyn Channel>, ChannelError> {
        self.channels
            .get(id)
            .cloned()
            .ok_or_else(|| ChannelError::NotFound(id.to_owned()))
    }

    pub async fn start_all(&self, handler: Handler) -> Result<(), ChannelError> {
        for channel in self.channels.values() {
            channel.start(Arc::clone(&handler)).await?;
        }
        Ok(())
    }

    pub async fn stop_all(&self) -> Result<(), ChannelError> {
        let mut first_error = None;
        for channel in self.channels.values() {
            if let Err(error) = channel.stop().await
                && first_error.is_none()
            {
                first_error = Some(error);
            }
        }
        first_error.map_or(Ok(()), Err)
    }
}

pub struct TelegramAdapter {
    client: reqwest::Client,
    api_base: String,
    owner_user_id: String,
    shutdown: Mutex<Option<tokio::sync::watch::Sender<bool>>>,
    worker: Mutex<Option<tokio::task::JoinHandle<()>>>,
}

impl TelegramAdapter {
    pub fn new(token: &str, owner_user_id: &str) -> Result<Self, ChannelError> {
        if token.trim().is_empty() {
            return Err(ChannelError::Operation(
                "telegram bot token must not be empty".into(),
            ));
        }
        let owner_user_id = owner_user_id.trim();
        if owner_user_id
            .parse::<i64>()
            .ok()
            .filter(|id| *id > 0)
            .is_none()
        {
            return Err(ChannelError::Operation(
                "telegram owner user id must be a positive integer".into(),
            ));
        }
        Ok(Self {
            client: reqwest::Client::new(),
            api_base: format!("https://api.telegram.org/bot{}/", token.trim()),
            owner_user_id: owner_user_id.into(),
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
        owner_user_id: String,
        bot_id: i64,
        handler: Handler,
        mut shutdown: tokio::sync::watch::Receiver<bool>,
    ) {
        let mut offset = 0_i64;
        loop {
            if *shutdown.borrow() {
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
                _ = shutdown.changed() => return,
                result = request => match result {
                    Ok(updates) => updates,
                    Err(error) => {
                        eprintln!("Telegram polling error: {error}");
                        tokio::time::sleep(Duration::from_secs(1)).await;
                        continue;
                    }
                }
            };
            for update in updates {
                offset = offset.max(update.update_id + 1);
                let Some(message) = update.message else {
                    continue;
                };
                let Some(text) = message.text.as_deref() else {
                    continue;
                };
                let Ok(inbound) = telegram_inbound_message(&message, text, bot_id) else {
                    continue;
                };
                if inbound.sender_id != owner_user_id {
                    continue;
                }
                let handling = handler(inbound);
                tokio::select! {
                    changed = shutdown.changed() => {
                        if changed.is_err() || *shutdown.borrow() {
                            return;
                        }
                    }
                    result = handling => {
                        if let Err(error) = result {
                            eprintln!("Telegram message handling failed: {error}");
                        }
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

    async fn start(&self, handler: Handler) -> Result<(), ChannelError> {
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
        let me: TelegramUser =
            Self::call(&self.client, &self.api_base, "getMe", serde_json::json!({})).await?;
        let (sender, receiver) = tokio::sync::watch::channel(false);
        let worker = tokio::spawn(Self::polling_loop(
            self.client.clone(),
            self.api_base.clone(),
            self.owner_user_id.clone(),
            me.id,
            handler,
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
        Ok(())
    }

    async fn stop(&self) -> Result<(), ChannelError> {
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
        let sent: TelegramMessage = Self::call(
            &self.client,
            &self.api_base,
            "sendMessage",
            serde_json::json!({"chat_id": recipient, "text": content}),
        )
        .await?;
        Ok(DeliveryReceipt {
            message_id: sent.message_id.to_string(),
        })
    }
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

#[derive(Deserialize)]
struct TelegramQuote {
    text: String,
}

#[derive(Deserialize)]
struct TelegramMessage {
    message_id: i64,
    from: Option<TelegramUser>,
    text: Option<String>,
    caption: Option<String>,
    reply_to_message: Option<Box<TelegramMessage>>,
    quote: Option<TelegramQuote>,
}

fn telegram_inbound_message(
    message: &TelegramMessage,
    text: &str,
    bot_id: i64,
) -> Result<Message, ChannelError> {
    let sender = message.from.as_ref().ok_or_else(|| {
        ChannelError::Operation("telegram text message is missing a sender".into())
    })?;
    let mut inbound = Message {
        channel_id: "telegram".into(),
        sender_id: sender.id.to_string(),
        message_id: message.message_id.to_string(),
        content: text.into(),
        reply: None,
    };
    if let Some(reply) = &message.reply_to_message {
        let author = match reply.from.as_ref().map(|user| user.id) {
            Some(id) if id == bot_id => ReplyAuthor::Assistant,
            Some(id) if id == sender.id => ReplyAuthor::User,
            _ => ReplyAuthor::Other,
        };
        let body = reply
            .text
            .as_deref()
            .or(reply.caption.as_deref())
            .unwrap_or("")
            .trim()
            .to_owned();
        inbound.reply = Some(ReplyContext {
            message_id: reply.message_id.to_string(),
            author,
            content_unavailable: body.is_empty(),
            body,
            selected_text: message
                .quote
                .as_ref()
                .map(|quote| quote.text.trim().to_owned())
                .unwrap_or_default(),
        });
    }
    Ok(inbound)
}

#[cfg(test)]
mod telegram_tests {
    use super::*;

    #[test]
    fn validates_owner_and_maps_reply_context() {
        assert!(TelegramAdapter::new("token", "not-an-id").is_err());
        let message = TelegramMessage {
            message_id: 9,
            from: Some(TelegramUser { id: 42 }),
            text: Some("What about this?".into()),
            caption: None,
            reply_to_message: Some(Box::new(TelegramMessage {
                message_id: 7,
                from: Some(TelegramUser { id: 100 }),
                text: Some("Earlier answer".into()),
                caption: None,
                reply_to_message: None,
                quote: None,
            })),
            quote: Some(TelegramQuote {
                text: "answer".into(),
            }),
        };
        let inbound = telegram_inbound_message(&message, "What about this?", 100).unwrap();
        assert_eq!(inbound.sender_id, "42");
        let reply = inbound.reply.unwrap();
        assert_eq!(reply.author, ReplyAuthor::Assistant);
        assert_eq!(reply.selected_text, "answer");
    }
}
