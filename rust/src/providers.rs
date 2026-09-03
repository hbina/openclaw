use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use reqwest::{Client, Method, StatusCode};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use thiserror::Error;
use tokio::sync::Notify;
use url::Url;

const MAX_RESPONSE_BYTES: usize = 16 << 20;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum MessageRole {
    User,
    Assistant,
    System,
    Tool,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct FunctionCall {
    pub name: String,
    pub arguments: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ToolCall {
    pub id: String,
    #[serde(rename = "type")]
    pub kind: String,
    pub function: FunctionCall,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Message {
    pub role: MessageRole,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub content: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tool_calls: Vec<ToolCall>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub tool_call_id: String,
}

impl Message {
    pub fn text(role: MessageRole, content: impl Into<String>) -> Self {
        Self {
            role,
            content: content.into(),
            tool_calls: Vec::new(),
            tool_call_id: String::new(),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct FunctionDefinition {
    pub name: String,
    pub description: String,
    pub parameters: Value,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ToolDefinition {
    #[serde(rename = "type")]
    pub kind: String,
    pub function: FunctionDefinition,
}

#[derive(Debug, Clone, Default)]
pub struct GenerateRequest {
    pub model: String,
    pub messages: Vec<Message>,
    pub tools: Vec<ToolDefinition>,
    pub tool_choice: String,
    pub max_tokens: u32,
    /// The exact sanitized body persisted before network I/O.
    pub wire_json: Vec<u8>,
}

#[derive(Debug, Clone)]
pub struct GenerateResponse {
    pub message: Message,
    pub finish_reason: String,
    pub raw_response: Vec<u8>,
    pub http_status: u16,
}

#[derive(Debug, Error)]
pub enum ProviderError {
    #[error("invalid local model configuration: {0}")]
    Configuration(String),
    #[error("local model request failed: {0}")]
    Request(#[from] reqwest::Error),
    #[error("local model response exceeds {MAX_RESPONSE_BYTES} bytes")]
    ResponseTooLarge,
    #[error("local model unexpected status {status}: {body}")]
    HttpStatus { status: u16, body: String },
    #[error("local model decode response: {source}")]
    Decode {
        source: serde_json::Error,
        status: u16,
        body: Vec<u8>,
    },
    #[error("local model returned no choices")]
    NoChoices { status: u16, body: Vec<u8> },
    #[error("local model reported invalid context size")]
    InvalidContextSize,
    #[error("embedding input must not be empty")]
    EmptyEmbeddingInput,
    #[error("embedding response count {actual} does not match input count {expected}")]
    EmbeddingCount { actual: usize, expected: usize },
    #[error("embedding response contains invalid index {0}")]
    EmbeddingIndex(usize),
    #[error("embedding dimension {actual} does not match configured {expected}")]
    EmbeddingDimensions { actual: usize, expected: usize },
    #[error("embedding contains a non-finite value")]
    NonFiniteEmbedding,
    #[error("embedding has zero norm")]
    ZeroNormEmbedding,
    #[error("serialize local model request: {0}")]
    Serialize(#[from] serde_json::Error),
}

#[async_trait]
pub trait Provider: Send + Sync {
    fn is_local_openai(&self) -> bool {
        false
    }

    fn marshal_generate_request(&self, request: &GenerateRequest)
    -> Result<Vec<u8>, ProviderError>;

    async fn generate(
        &self,
        request: &mut GenerateRequest,
    ) -> Result<GenerateResponse, ProviderError>;
}

#[async_trait]
pub trait Embedder: Send + Sync {
    async fn embed(&self, inputs: &[String]) -> Result<Vec<Vec<f32>>, ProviderError>;
    async fn tokenize(&self, content: &str) -> Result<Vec<i32>, ProviderError>;
    async fn detokenize(&self, tokens: &[i32]) -> Result<String, ProviderError>;
}

#[async_trait]
pub trait PromptSizer: Send + Sync {
    async fn context_size(&self) -> Result<u32, ProviderError>;
    async fn count_prompt_tokens(
        &self,
        messages: &[Message],
        tools: &[ToolDefinition],
    ) -> Result<usize, ProviderError>;
}

#[derive(Debug, Clone)]
pub struct OpenAiClient {
    api_key: String,
    base_url: String,
    server_url: String,
    client: Client,
}

impl OpenAiClient {
    pub fn new(
        api_key: impl Into<String>,
        base_url: impl AsRef<str>,
    ) -> Result<Self, ProviderError> {
        let (base_url, server_url) = local_urls(base_url.as_ref(), "local model base URL")?;
        Ok(Self {
            api_key: api_key.into().trim().to_owned(),
            base_url,
            server_url,
            client: Client::new(),
        })
    }

    pub async fn context_size(&self) -> Result<u32, ProviderError> {
        #[derive(Deserialize)]
        struct Props {
            default_generation_settings: GenerationSettings,
        }
        #[derive(Deserialize)]
        struct GenerationSettings {
            n_ctx: i64,
        }
        let props: Props = self
            .local_json(Method::GET, &format!("{}/props", self.server_url), None)
            .await?;
        u32::try_from(props.default_generation_settings.n_ctx)
            .ok()
            .filter(|size| *size > 0)
            .ok_or(ProviderError::InvalidContextSize)
    }

    pub async fn count_prompt_tokens(
        &self,
        messages: &[Message],
        tools: &[ToolDefinition],
    ) -> Result<usize, ProviderError> {
        #[derive(Deserialize)]
        struct Templated {
            prompt: String,
        }
        #[derive(Deserialize)]
        struct Tokenized {
            tokens: Vec<i32>,
        }
        let wire_messages: Vec<_> = messages.iter().map(wire_message).collect();
        let templated: Templated = self
            .local_json(
                Method::POST,
                &format!("{}/apply-template", self.server_url),
                Some(json!({ "messages": wire_messages })),
            )
            .await?;
        let tool_json = serde_json::to_string(tools)?;
        let tokenized: Tokenized = self
            .local_json(
                Method::POST,
                &format!("{}/tokenize", self.server_url),
                Some(json!({
                    "content": format!("{}\n{}", templated.prompt, tool_json),
                    "add_special": false,
                    "parse_special": true
                })),
            )
            .await?;
        Ok(tokenized.tokens.len())
    }

    async fn local_json<T: for<'de> Deserialize<'de>>(
        &self,
        method: Method,
        endpoint: &str,
        input: Option<Value>,
    ) -> Result<T, ProviderError> {
        let mut builder = self.client.request(method, endpoint);
        if let Some(input) = input {
            builder = builder.json(&input);
        }
        if !self.api_key.is_empty() {
            builder = builder.bearer_auth(&self.api_key);
        }
        let response = builder.send().await?;
        let status = response.status();
        let body = bounded_body(response).await?;
        if status != StatusCode::OK {
            return Err(ProviderError::HttpStatus {
                status: status.as_u16(),
                body: String::from_utf8_lossy(&body).trim().to_owned(),
            });
        }
        serde_json::from_slice(&body).map_err(|source| ProviderError::Decode {
            source,
            status: status.as_u16(),
            body,
        })
    }
}

#[async_trait]
impl PromptSizer for OpenAiClient {
    async fn context_size(&self) -> Result<u32, ProviderError> {
        Self::context_size(self).await
    }

    async fn count_prompt_tokens(
        &self,
        messages: &[Message],
        tools: &[ToolDefinition],
    ) -> Result<usize, ProviderError> {
        Self::count_prompt_tokens(self, messages, tools).await
    }
}

#[derive(Serialize)]
struct WireMessage<'a> {
    role: MessageRole,
    content: Option<&'a str>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    tool_calls: &'a Vec<ToolCall>,
    #[serde(skip_serializing_if = "String::is_empty")]
    tool_call_id: &'a String,
}

fn wire_message(message: &Message) -> WireMessage<'_> {
    WireMessage {
        role: message.role,
        content: (!message.content.is_empty() || message.tool_calls.is_empty())
            .then_some(message.content.as_str()),
        tool_calls: &message.tool_calls,
        tool_call_id: &message.tool_call_id,
    }
}

#[derive(Serialize)]
struct WireGenerateRequest<'a> {
    model: &'a str,
    messages: Vec<WireMessage<'a>>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    tools: &'a Vec<ToolDefinition>,
    #[serde(skip_serializing_if = "Option::is_none")]
    tool_choice: Option<&'a str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    parallel_tool_calls: Option<bool>,
    #[serde(skip_serializing_if = "is_zero")]
    max_tokens: u32,
}

fn is_zero(value: &u32) -> bool {
    *value == 0
}

#[derive(Deserialize)]
struct WireGenerateResponse {
    choices: Vec<WireChoice>,
}

#[derive(Deserialize)]
struct WireChoice {
    message: WireResponseMessage,
    finish_reason: String,
}

#[derive(Deserialize)]
struct WireResponseMessage {
    #[serde(default)]
    content: Option<String>,
    #[serde(default)]
    tool_calls: Vec<ToolCall>,
}

#[async_trait]
impl Provider for OpenAiClient {
    fn is_local_openai(&self) -> bool {
        true
    }

    fn marshal_generate_request(
        &self,
        request: &GenerateRequest,
    ) -> Result<Vec<u8>, ProviderError> {
        let tools_present = !request.tools.is_empty();
        Ok(serde_json::to_vec(&WireGenerateRequest {
            model: &request.model,
            messages: request.messages.iter().map(wire_message).collect(),
            tools: &request.tools,
            tool_choice: tools_present.then_some(if request.tool_choice.is_empty() {
                "auto"
            } else {
                &request.tool_choice
            }),
            parallel_tool_calls: tools_present.then_some(false),
            max_tokens: request.max_tokens,
        })?)
    }

    async fn generate(
        &self,
        request: &mut GenerateRequest,
    ) -> Result<GenerateResponse, ProviderError> {
        if request.wire_json.is_empty() {
            request.wire_json = self.marshal_generate_request(request)?;
        }
        let mut builder = self
            .client
            .post(format!("{}/chat/completions", self.base_url))
            .header(reqwest::header::CONTENT_TYPE, "application/json")
            .body(request.wire_json.clone());
        if !self.api_key.is_empty() {
            builder = builder.bearer_auth(&self.api_key);
        }
        let response = builder.send().await?;
        let status = response.status().as_u16();
        let body = bounded_body(response).await?;
        if status != 200 {
            return Err(ProviderError::HttpStatus {
                status,
                body: String::from_utf8_lossy(&body).into_owned(),
            });
        }
        let decoded: WireGenerateResponse =
            serde_json::from_slice(&body).map_err(|source| ProviderError::Decode {
                source,
                status,
                body: body.clone(),
            })?;
        let choice =
            decoded
                .choices
                .into_iter()
                .next()
                .ok_or_else(|| ProviderError::NoChoices {
                    status,
                    body: body.clone(),
                })?;
        Ok(GenerateResponse {
            message: Message {
                role: MessageRole::Assistant,
                content: choice.message.content.unwrap_or_default(),
                tool_calls: choice.message.tool_calls,
                tool_call_id: String::new(),
            },
            finish_reason: choice.finish_reason,
            raw_response: body,
            http_status: status,
        })
    }
}

#[derive(Debug, Clone)]
pub struct EmbeddingClient {
    api_key: String,
    base_url: String,
    server_url: String,
    model: String,
    dimensions: usize,
    client: Client,
}

impl EmbeddingClient {
    pub fn new(
        api_key: impl Into<String>,
        base_url: impl AsRef<str>,
        model: impl Into<String>,
        dimensions: usize,
    ) -> Result<Self, ProviderError> {
        let (base_url, server_url) = local_urls(base_url.as_ref(), "embedding base URL")?;
        let model = model.into().trim().to_owned();
        if model.is_empty() {
            return Err(ProviderError::Configuration(
                "embedding model is required".into(),
            ));
        }
        if dimensions == 0 {
            return Err(ProviderError::Configuration(
                "embedding dimensions must be positive".into(),
            ));
        }
        Ok(Self {
            api_key: api_key.into().trim().to_owned(),
            base_url,
            server_url,
            model,
            dimensions,
            client: Client::new(),
        })
    }

    async fn post_json<T: for<'de> Deserialize<'de>>(
        &self,
        endpoint: &str,
        value: Value,
    ) -> Result<T, ProviderError> {
        let mut builder = self.client.post(endpoint).json(&value);
        if !self.api_key.is_empty() {
            builder = builder.bearer_auth(&self.api_key);
        }
        let response = builder.send().await?;
        let status = response.status();
        let body = bounded_body(response).await?;
        if status != StatusCode::OK {
            return Err(ProviderError::HttpStatus {
                status: status.as_u16(),
                body: String::from_utf8_lossy(&body).trim().to_owned(),
            });
        }
        serde_json::from_slice(&body).map_err(|source| ProviderError::Decode {
            source,
            status: status.as_u16(),
            body,
        })
    }
}

#[derive(Deserialize)]
struct EmbeddingResponse {
    data: Vec<EmbeddingItem>,
}

#[derive(Deserialize)]
struct EmbeddingItem {
    index: usize,
    embedding: Vec<f64>,
}

#[async_trait]
impl Embedder for EmbeddingClient {
    async fn embed(&self, inputs: &[String]) -> Result<Vec<Vec<f32>>, ProviderError> {
        if inputs.is_empty() {
            return Err(ProviderError::EmptyEmbeddingInput);
        }
        let response: EmbeddingResponse = self
            .post_json(
                &format!("{}/embeddings", self.base_url),
                json!({
                    "model": self.model,
                    "input": inputs,
                    "encoding_format": "float"
                }),
            )
            .await?;
        if response.data.len() != inputs.len() {
            return Err(ProviderError::EmbeddingCount {
                actual: response.data.len(),
                expected: inputs.len(),
            });
        }
        let mut vectors: Vec<Option<Vec<f32>>> = vec![None; inputs.len()];
        for item in response.data {
            if item.index >= inputs.len() || vectors[item.index].is_some() {
                return Err(ProviderError::EmbeddingIndex(item.index));
            }
            if item.embedding.len() != self.dimensions {
                return Err(ProviderError::EmbeddingDimensions {
                    actual: item.embedding.len(),
                    expected: self.dimensions,
                });
            }
            if item.embedding.iter().any(|value| !value.is_finite()) {
                return Err(ProviderError::NonFiniteEmbedding);
            }
            let norm = item
                .embedding
                .iter()
                .map(|value| value * value)
                .sum::<f64>();
            if norm == 0.0 {
                return Err(ProviderError::ZeroNormEmbedding);
            }
            let norm = norm.sqrt();
            vectors[item.index] = Some(
                item.embedding
                    .into_iter()
                    .map(|value| (value / norm) as f32)
                    .collect(),
            );
        }
        Ok(vectors
            .into_iter()
            .map(|vector| vector.expect("all response indexes validated"))
            .collect())
    }

    async fn tokenize(&self, content: &str) -> Result<Vec<i32>, ProviderError> {
        #[derive(Deserialize)]
        struct Response {
            tokens: Vec<i32>,
        }
        let response: Response = self
            .post_json(
                &format!("{}/tokenize", self.server_url),
                json!({
                    "content": content,
                    "add_special": false,
                    "parse_special": true
                }),
            )
            .await?;
        Ok(response.tokens)
    }

    async fn detokenize(&self, tokens: &[i32]) -> Result<String, ProviderError> {
        #[derive(Deserialize)]
        struct Response {
            content: String,
        }
        let response: Response = self
            .post_json(
                &format!("{}/detokenize", self.server_url),
                json!({ "tokens": tokens }),
            )
            .await?;
        Ok(response.content)
    }
}

fn local_urls(value: &str, label: &str) -> Result<(String, String), ProviderError> {
    let base = value.trim().trim_end_matches('/');
    if base.is_empty() {
        return Err(ProviderError::Configuration(format!("{label} is required")));
    }
    let parsed = Url::parse(base)
        .ok()
        .filter(|url| matches!(url.scheme(), "http" | "https") && url.host_str().is_some())
        .ok_or_else(|| ProviderError::Configuration(format!("invalid {label}")))?;
    let base_url = parsed.as_str().trim_end_matches('/').to_owned();
    let server_url = base_url.strip_suffix("/v1").unwrap_or(&base_url).to_owned();
    Ok((base_url, server_url))
}

async fn bounded_body(mut response: reqwest::Response) -> Result<Vec<u8>, ProviderError> {
    if response
        .content_length()
        .is_some_and(|size| size > MAX_RESPONSE_BYTES as u64)
    {
        return Err(ProviderError::ResponseTooLarge);
    }
    let mut body = Vec::with_capacity(
        response
            .content_length()
            .unwrap_or(0)
            .min(MAX_RESPONSE_BYTES as u64) as usize,
    );
    while let Some(chunk) = response.chunk().await? {
        if body.len().saturating_add(chunk.len()) > MAX_RESPONSE_BYTES {
            return Err(ProviderError::ResponseTooLarge);
        }
        body.extend_from_slice(&chunk);
    }
    Ok(body)
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum WorkPriority {
    Foreground,
    Background,
}

/// Gives queued owner-facing calls precedence between background calls.
/// Calls already in flight finish normally; cancellation is represented by
/// dropping the acquire future or returned permit.
#[derive(Debug, Default)]
pub struct PriorityGate {
    state: Mutex<PriorityState>,
    changed: Notify,
}

#[derive(Debug, Default)]
struct PriorityState {
    active_foreground: usize,
    waiting_foreground: usize,
    background_active: bool,
}

impl PriorityGate {
    pub fn new() -> Arc<Self> {
        Arc::new(Self::default())
    }

    pub async fn acquire(self: &Arc<Self>, priority: WorkPriority) -> PriorityPermit {
        let mut waiting = WaitingForeground::new(self, priority);
        loop {
            let notified = self.changed.notified();
            {
                let mut state = self.state.lock().expect("priority gate mutex poisoned");
                let available = match priority {
                    WorkPriority::Foreground => !state.background_active,
                    WorkPriority::Background => {
                        !state.background_active
                            && state.active_foreground == 0
                            && state.waiting_foreground == 0
                    }
                };
                if available {
                    match priority {
                        WorkPriority::Foreground => {
                            state.waiting_foreground -= 1;
                            state.active_foreground += 1;
                            waiting.registered = false;
                        }
                        WorkPriority::Background => state.background_active = true,
                    }
                    return PriorityPermit {
                        gate: Arc::clone(self),
                        priority,
                    };
                }
            }
            notified.await;
        }
    }
}

struct WaitingForeground<'a> {
    gate: &'a PriorityGate,
    registered: bool,
}

impl<'a> WaitingForeground<'a> {
    fn new(gate: &'a PriorityGate, priority: WorkPriority) -> Self {
        let registered = priority == WorkPriority::Foreground;
        if registered {
            gate.state
                .lock()
                .expect("priority gate mutex poisoned")
                .waiting_foreground += 1;
            gate.changed.notify_waiters();
        }
        Self { gate, registered }
    }
}

impl Drop for WaitingForeground<'_> {
    fn drop(&mut self) {
        if self.registered {
            self.gate
                .state
                .lock()
                .expect("priority gate mutex poisoned")
                .waiting_foreground -= 1;
            self.gate.changed.notify_waiters();
        }
    }
}

pub struct PriorityPermit {
    gate: Arc<PriorityGate>,
    priority: WorkPriority,
}

impl Drop for PriorityPermit {
    fn drop(&mut self) {
        let mut state = self
            .gate
            .state
            .lock()
            .expect("priority gate mutex poisoned");
        match self.priority {
            WorkPriority::Foreground => state.active_foreground -= 1,
            WorkPriority::Background => state.background_active = false,
        }
        drop(state);
        self.gate.changed.notify_waiters();
    }
}

pub struct PriorityProvider {
    base: Arc<dyn Provider>,
    gate: Arc<PriorityGate>,
    priority: WorkPriority,
}

impl PriorityProvider {
    pub fn new(base: Arc<dyn Provider>, gate: Arc<PriorityGate>, priority: WorkPriority) -> Self {
        Self {
            base,
            gate,
            priority,
        }
    }
}

#[async_trait]
impl Provider for PriorityProvider {
    fn is_local_openai(&self) -> bool {
        self.base.is_local_openai()
    }

    fn marshal_generate_request(
        &self,
        request: &GenerateRequest,
    ) -> Result<Vec<u8>, ProviderError> {
        self.base.marshal_generate_request(request)
    }

    async fn generate(
        &self,
        request: &mut GenerateRequest,
    ) -> Result<GenerateResponse, ProviderError> {
        let _permit = self.gate.acquire(self.priority).await;
        self.base.generate(request).await
    }
}

pub struct PriorityEmbedder {
    base: Arc<dyn Embedder>,
    gate: Arc<PriorityGate>,
    priority: WorkPriority,
}

pub struct PriorityPromptSizer {
    base: Arc<dyn PromptSizer>,
    gate: Arc<PriorityGate>,
    priority: WorkPriority,
}

impl PriorityPromptSizer {
    pub fn new(
        base: Arc<dyn PromptSizer>,
        gate: Arc<PriorityGate>,
        priority: WorkPriority,
    ) -> Self {
        Self {
            base,
            gate,
            priority,
        }
    }
}

#[async_trait]
impl PromptSizer for PriorityPromptSizer {
    async fn context_size(&self) -> Result<u32, ProviderError> {
        let _permit = self.gate.acquire(self.priority).await;
        self.base.context_size().await
    }

    async fn count_prompt_tokens(
        &self,
        messages: &[Message],
        tools: &[ToolDefinition],
    ) -> Result<usize, ProviderError> {
        let _permit = self.gate.acquire(self.priority).await;
        self.base.count_prompt_tokens(messages, tools).await
    }
}

impl PriorityEmbedder {
    pub fn new(base: Arc<dyn Embedder>, gate: Arc<PriorityGate>, priority: WorkPriority) -> Self {
        Self {
            base,
            gate,
            priority,
        }
    }
}

#[async_trait]
impl Embedder for PriorityEmbedder {
    async fn embed(&self, inputs: &[String]) -> Result<Vec<Vec<f32>>, ProviderError> {
        let _permit = self.gate.acquire(self.priority).await;
        self.base.embed(inputs).await
    }

    async fn tokenize(&self, content: &str) -> Result<Vec<i32>, ProviderError> {
        let _permit = self.gate.acquire(self.priority).await;
        self.base.tokenize(content).await
    }

    async fn detokenize(&self, tokens: &[i32]) -> Result<String, ProviderError> {
        let _permit = self.gate.acquire(self.priority).await;
        self.base.detokenize(tokens).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    async fn serve_once(body: &'static str) -> (String, tokio::task::JoinHandle<Vec<u8>>) {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let worker = tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.unwrap();
            let mut request = Vec::new();
            let (header_end, content_length) = loop {
                let mut buffer = [0_u8; 4096];
                let read = stream.read(&mut buffer).await.unwrap();
                assert!(read > 0);
                request.extend_from_slice(&buffer[..read]);
                if let Some(position) = request.windows(4).position(|window| window == b"\r\n\r\n")
                {
                    let header_end = position + 4;
                    let headers = String::from_utf8_lossy(&request[..header_end]);
                    let content_length = headers
                        .lines()
                        .find_map(|line| {
                            line.split_once(':').and_then(|(name, value)| {
                                name.eq_ignore_ascii_case("content-length")
                                    .then(|| value.trim().parse::<usize>().unwrap())
                            })
                        })
                        .unwrap_or(0);
                    break (header_end, content_length);
                }
            };
            while request.len() - header_end < content_length {
                let mut buffer = [0_u8; 4096];
                let read = stream.read(&mut buffer).await.unwrap();
                assert!(read > 0);
                request.extend_from_slice(&buffer[..read]);
            }
            let response = format!(
                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                body.len()
            );
            stream.write_all(response.as_bytes()).await.unwrap();
            request
        });
        (format!("http://{address}"), worker)
    }

    #[test]
    fn wire_contract_preserves_structured_history() {
        let client = OpenAiClient::new("secret", "http://127.0.0.1:8080/v1/").unwrap();
        let mut request = GenerateRequest {
            model: "default".into(),
            messages: vec![
                Message::text(MessageRole::User, "remember espresso"),
                Message {
                    role: MessageRole::Assistant,
                    content: String::new(),
                    tool_calls: vec![ToolCall {
                        id: "call-1".into(),
                        kind: "function".into(),
                        function: FunctionCall {
                            name: "store_memory".into(),
                            arguments: "{}".into(),
                        },
                    }],
                    tool_call_id: String::new(),
                },
                Message {
                    role: MessageRole::Tool,
                    content: r#"{"stored":true}"#.into(),
                    tool_calls: vec![],
                    tool_call_id: "call-1".into(),
                },
            ],
            tools: vec![ToolDefinition {
                kind: "function".into(),
                function: FunctionDefinition {
                    name: "store_memory".into(),
                    description: "store durable memory".into(),
                    parameters: json!({"type": "object"}),
                },
            }],
            max_tokens: 4096,
            ..Default::default()
        };
        request.wire_json = client.marshal_generate_request(&request).unwrap();
        let value: Value = serde_json::from_slice(&request.wire_json).unwrap();
        assert_eq!(value["tool_choice"], "auto");
        assert_eq!(value["parallel_tool_calls"], false);
        assert_eq!(value["max_tokens"], 4096);
        assert!(value["messages"][1]["content"].is_null());
        assert_eq!(value["messages"][2]["tool_call_id"], "call-1");
    }

    #[test]
    fn clients_reject_invalid_configuration() {
        assert!(OpenAiClient::new("", "").is_err());
        assert!(EmbeddingClient::new("", "/v1", "default", 3).is_err());
        assert!(EmbeddingClient::new("", "http://localhost", "", 3).is_err());
        assert!(EmbeddingClient::new("", "http://localhost", "default", 0).is_err());
    }

    #[tokio::test]
    async fn empty_embedding_batch_is_rejected_without_io() {
        let client = EmbeddingClient::new("", "http://127.0.0.1:9", "default", 3).unwrap();
        assert!(matches!(
            client.embed(&[]).await,
            Err(ProviderError::EmptyEmbeddingInput)
        ));
    }

    #[tokio::test]
    async fn clients_execute_openai_wire_contract_and_normalize_embeddings() {
        let chat_body = r#"{"choices":[{"message":{"role":"assistant","content":"ok","tool_calls":[]},"finish_reason":"stop"}]}"#;
        let (chat_base, chat_worker) = serve_once(chat_body).await;
        let client = OpenAiClient::new("local-secret", format!("{chat_base}/v1")).unwrap();
        let mut request = GenerateRequest {
            model: "default".into(),
            messages: vec![Message::text(MessageRole::User, "hello")],
            max_tokens: 32,
            ..Default::default()
        };
        let response = client.generate(&mut request).await.unwrap();
        assert_eq!(response.message.content, "ok");
        assert_eq!(response.raw_response, chat_body.as_bytes());
        let request = String::from_utf8(chat_worker.await.unwrap()).unwrap();
        assert!(request.starts_with("POST /v1/chat/completions HTTP/1.1\r\n"));
        assert!(
            request
                .to_lowercase()
                .contains("authorization: bearer local-secret")
        );

        let embedding_body =
            r#"{"data":[{"index":1,"embedding":[0,3]},{"index":0,"embedding":[4,0]}]}"#;
        let (embedding_base, embedding_worker) = serve_once(embedding_body).await;
        let client =
            EmbeddingClient::new("", format!("{embedding_base}/v1"), "default", 2).unwrap();
        let vectors = client
            .embed(&["first".into(), "second".into()])
            .await
            .unwrap();
        assert_eq!(vectors, vec![vec![1.0, 0.0], vec![0.0, 1.0]]);
        let request = String::from_utf8(embedding_worker.await.unwrap()).unwrap();
        assert!(request.starts_with("POST /v1/embeddings HTTP/1.1\r\n"));
        assert!(request.contains(r#""encoding_format":"float""#));
    }

    #[tokio::test]
    async fn waiting_foreground_precedes_the_next_background() {
        let gate = PriorityGate::new();
        let first_background = gate.acquire(WorkPriority::Background).await;
        let (send, mut receive) = tokio::sync::mpsc::unbounded_channel();

        let foreground_gate = Arc::clone(&gate);
        let foreground_send = send.clone();
        let foreground = tokio::spawn(async move {
            let _permit = foreground_gate.acquire(WorkPriority::Foreground).await;
            foreground_send.send("foreground").unwrap();
        });
        tokio::task::yield_now().await;
        let background_gate = Arc::clone(&gate);
        let background = tokio::spawn(async move {
            let _permit = background_gate.acquire(WorkPriority::Background).await;
            send.send("background").unwrap();
        });
        drop(first_background);

        assert_eq!(receive.recv().await, Some("foreground"));
        assert_eq!(receive.recv().await, Some("background"));
        foreground.await.unwrap();
        background.await.unwrap();
    }

    #[tokio::test]
    async fn cancelled_foreground_wait_does_not_block_background() {
        let gate = PriorityGate::new();
        let first_background = gate.acquire(WorkPriority::Background).await;
        let waiting_gate = Arc::clone(&gate);
        let waiting = tokio::spawn(async move {
            let _permit = waiting_gate.acquire(WorkPriority::Foreground).await;
        });
        tokio::task::yield_now().await;
        waiting.abort();
        assert!(waiting.await.unwrap_err().is_cancelled());
        drop(first_background);
        tokio::time::timeout(
            std::time::Duration::from_secs(1),
            gate.acquire(WorkPriority::Background),
        )
        .await
        .expect("background remained blocked after cancellation");
    }
}
