use std::{net::SocketAddr, sync::Arc, time::Duration};

use serde::Deserialize;
use serde_json::json;
use thiserror::Error;
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
    sync::{Semaphore, watch},
    task::JoinSet,
};

use crate::{
    channels::{ChannelError, Handler, Registry},
    maintenance::Maintainer,
    state::{StateError, Store},
};

use super::{Agent, AgentError, ChatInput, PreparedResponse};

const MAX_HEADER_BYTES: usize = 16 << 10;
const MAX_REQUEST_BYTES: usize = 1 << 20;
const MAX_MESSAGE_BYTES: usize = 64 << 10;
const MAX_SENDER_BYTES: usize = 256;
const MAX_CONNECTIONS: usize = 32;
const REQUEST_TIMEOUT: Duration = Duration::from_secs(10 * 60);
const SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(5);
const REMINDER_POLL_INTERVAL: Duration = Duration::from_secs(60);

#[derive(Debug, Error)]
pub enum GatewayError {
    #[error("gateway I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("gateway channel failed: {0}")]
    Channel(#[from] ChannelError),
    #[error("gateway state failed: {0}")]
    State(#[from] StateError),
    #[error("gateway agent failed: {0}")]
    Agent(#[from] AgentError),
    #[error("gateway bind address must be loopback, got {0}")]
    UnsafeBind(SocketAddr),
}

pub struct Gateway {
    agent: Arc<Agent>,
    channels: Arc<Registry>,
    store: Arc<Store>,
    maintainer: Option<Arc<Maintainer>>,
    connection_limit: Arc<Semaphore>,
}

impl Gateway {
    pub fn new(
        agent: Arc<Agent>,
        channels: Arc<Registry>,
        store: Arc<Store>,
        maintainer: Option<Arc<Maintainer>>,
    ) -> Self {
        Self {
            agent,
            channels,
            store,
            maintainer,
            connection_limit: Arc::new(Semaphore::new(MAX_CONNECTIONS)),
        }
    }

    pub async fn serve(
        self: Arc<Self>,
        address: SocketAddr,
        mut shutdown: watch::Receiver<bool>,
    ) -> Result<(), GatewayError> {
        if !address.ip().is_loopback() {
            return Err(GatewayError::UnsafeBind(address));
        }
        let listener = TcpListener::bind(address).await?;
        let agent = Arc::clone(&self.agent);
        let handler: Handler = Arc::new(move |message| {
            let agent = Arc::clone(&agent);
            Box::pin(async move {
                agent
                    .handle_message(&message)
                    .await
                    .map_err(|error| error.to_string())
            })
        });
        self.channels.start_all(handler).await?;

        let reminder_gateway = Arc::clone(&self);
        let reminder_shutdown = shutdown.clone();
        let mut reminder_task = tokio::spawn(async move {
            reminder_gateway.reminder_loop(reminder_shutdown).await;
        });
        let mut maintenance_task = self.maintainer.as_ref().map(|maintainer| {
            let maintainer = Arc::clone(maintainer);
            let maintenance_shutdown = shutdown.clone();
            tokio::spawn(async move { maintainer.run_loop(maintenance_shutdown).await })
        });
        let mut connections = JoinSet::new();
        loop {
            tokio::select! {
                changed = shutdown.changed() => {
                    if changed.is_err() || *shutdown.borrow() {
                        break;
                    }
                }
                accepted = listener.accept() => {
                    let (stream, peer) = accepted?;
                    let Ok(permit) = Arc::clone(&self.connection_limit).try_acquire_owned() else {
                        connections.spawn(async move {
                            let mut stream = stream;
                            let _ = write_error(&mut stream, 503, "busy", "gateway is busy", None).await;
                        });
                        continue;
                    };
                    let gateway = Arc::clone(&self);
                    connections.spawn(async move {
                        let _permit = permit;
                        if let Err(error) = tokio::time::timeout(
                            REQUEST_TIMEOUT,
                            gateway.handle_connection(stream, peer),
                        ).await {
                            eprintln!("HTTP request timed out: {error}");
                        }
                    });
                }
                Some(result) = connections.join_next(), if !connections.is_empty() => {
                    if let Err(error) = result {
                        eprintln!("HTTP connection task failed: {error}");
                    }
                }
            }
        }
        if tokio::time::timeout(SHUTDOWN_TIMEOUT, &mut reminder_task)
            .await
            .is_err()
        {
            reminder_task.abort();
            let _ = reminder_task.await;
        }
        if let Some(task) = &mut maintenance_task
            && tokio::time::timeout(SHUTDOWN_TIMEOUT, &mut *task)
                .await
                .is_err()
        {
            task.abort();
            let _ = task.await;
        }
        self.channels.stop_all().await?;
        let _ = tokio::time::timeout(SHUTDOWN_TIMEOUT, async {
            while connections.join_next().await.is_some() {}
        })
        .await;
        Ok(())
    }

    async fn reminder_loop(self: Arc<Self>, mut shutdown: watch::Receiver<bool>) {
        let mut interval = tokio::time::interval(REMINDER_POLL_INTERVAL);
        interval.tick().await;
        loop {
            tokio::select! {
                changed = shutdown.changed() => {
                    if changed.is_err() || *shutdown.borrow() {
                        return;
                    }
                }
                _ = interval.tick() => {
                    match self.store.fetch_due_reminders() {
                        Ok(reminders) => {
                            for reminder in reminders {
                                if let Err(error) = self.agent.deliver_reminder(&reminder).await {
                                    eprintln!("Failed to deliver reminder {}: {error}", reminder.id);
                                }
                            }
                        }
                        Err(error) => eprintln!("Failed to fetch due reminders: {error}"),
                    }
                }
            }
        }
    }

    async fn handle_connection(
        &self,
        mut stream: TcpStream,
        peer: SocketAddr,
    ) -> Result<(), std::io::Error> {
        if !peer.ip().is_loopback() {
            return write_error(
                &mut stream,
                403,
                "forbidden",
                "HTTP access is limited to the local machine",
                None,
            )
            .await;
        }
        let request = match read_request(&mut stream).await {
            Ok(request) => request,
            Err(error) => {
                return write_error(&mut stream, error.status, error.code, error.message, None)
                    .await;
            }
        };
        match (request.method.as_str(), request.path.as_str()) {
            ("GET", "/healthz") => {
                write_response(&mut stream, 200, "text/plain; charset=utf-8", b"OK", None).await
            }
            ("POST", "/chat") => self.handle_chat(&mut stream, &request.body).await,
            (_, "/healthz" | "/chat") => {
                write_error(
                    &mut stream,
                    405,
                    "method_not_allowed",
                    "method not allowed",
                    None,
                )
                .await
            }
            _ => write_error(&mut stream, 404, "not_found", "endpoint not found", None).await,
        }
    }

    async fn handle_chat(&self, stream: &mut TcpStream, body: &[u8]) -> Result<(), std::io::Error> {
        let request: ChatRequest = match strict_json(body) {
            Ok(request) => request,
            Err(_) => {
                return write_error(
                    stream,
                    400,
                    "invalid_json",
                    "request body must be one valid JSON object",
                    None,
                )
                .await;
            }
        };
        let message = request.message.trim();
        if message.is_empty() || message.len() > MAX_MESSAGE_BYTES {
            return write_error(
                stream,
                400,
                "invalid_message",
                "message must contain between 1 and 65536 bytes",
                None,
            )
            .await;
        }
        let sender = if request.sender_id.trim().is_empty() {
            "cli-user"
        } else {
            request.sender_id.trim()
        };
        if sender.len() > MAX_SENDER_BYTES {
            return write_error(
                stream,
                400,
                "invalid_sender",
                "sender_id must not exceed 256 bytes",
                None,
            )
            .await;
        }
        let prepared = match self
            .agent
            .prepare_chat_with_trace(ChatInput {
                channel_id: "cli".into(),
                sender_id: sender.into(),
                message_id: String::new(),
                content: message.into(),
                reply: None,
            })
            .await
        {
            Ok(prepared) => prepared,
            Err(error) => {
                eprintln!("Chat generation failed: {}", error.source);
                return write_error(
                    stream,
                    500,
                    "agent_error",
                    "the assistant could not produce a response",
                    error.trace_id,
                )
                .await;
            }
        };
        self.deliver_http(stream, prepared).await
    }

    async fn deliver_http(
        &self,
        stream: &mut TcpStream,
        prepared: PreparedResponse,
    ) -> Result<(), std::io::Error> {
        let delivery_id = match self.store.prepare_delivery(
            prepared.trace_id,
            prepared.output_event_id,
            "cli",
            &prepared.sender_id,
            &prepared.content,
        ) {
            Ok(id) => id,
            Err(error) => {
                let _ = self.store.finish_trace(
                    prepared.trace_id,
                    "failed",
                    "delivery",
                    Some(&error.to_string()),
                );
                return write_error(
                    stream,
                    500,
                    "delivery_error",
                    "the response could not be prepared for delivery",
                    Some(prepared.trace_id),
                )
                .await;
            }
        };
        if let Err(error) = self.store.mark_delivery_attempting(delivery_id) {
            let _ = self.store.finish_trace(
                prepared.trace_id,
                "failed",
                "delivery",
                Some(&error.to_string()),
            );
            return write_error(
                stream,
                500,
                "delivery_error",
                "the response could not be delivered",
                Some(prepared.trace_id),
            )
            .await;
        }
        let encoded = serde_json::to_vec(&json!({"reply": prepared.content}))
            .expect("chat response is serializable");
        if let Err(error) = write_response(
            stream,
            200,
            "application/json",
            &encoded,
            Some(prepared.trace_id),
        )
        .await
        {
            let _ = self
                .store
                .fail_delivery(prepared.trace_id, delivery_id, &error.to_string());
            return Err(error);
        }
        if let Err(error) = self.store.complete_delivery_indexed(
            prepared.trace_id,
            delivery_id,
            "http",
            "cli",
            &prepared.sender_id,
            &prepared.content,
            prepared.start_history_id,
            &prepared.chunks,
        ) {
            eprintln!(
                "Trace {} response was written but finalization failed: {error}",
                prepared.trace_id
            );
        }
        Ok(())
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ChatRequest {
    #[serde(default)]
    sender_id: String,
    message: String,
}

struct HttpRequest {
    method: String,
    path: String,
    body: Vec<u8>,
}

struct RequestError {
    status: u16,
    code: &'static str,
    message: &'static str,
}

async fn read_request(stream: &mut TcpStream) -> Result<HttpRequest, RequestError> {
    let mut bytes = Vec::new();
    let header_end = loop {
        if bytes.len() >= MAX_HEADER_BYTES {
            return Err(RequestError {
                status: 431,
                code: "headers_too_large",
                message: "request headers exceed 16384 bytes",
            });
        }
        let mut buffer = [0_u8; 4096];
        let read = stream.read(&mut buffer).await.map_err(|_| RequestError {
            status: 400,
            code: "read_error",
            message: "request could not be read",
        })?;
        if read == 0 {
            return Err(RequestError {
                status: 400,
                code: "incomplete_request",
                message: "request headers are incomplete",
            });
        }
        bytes.extend_from_slice(&buffer[..read]);
        if let Some(position) = bytes.windows(4).position(|window| window == b"\r\n\r\n") {
            break position + 4;
        }
    };
    let headers = std::str::from_utf8(&bytes[..header_end]).map_err(|_| RequestError {
        status: 400,
        code: "invalid_headers",
        message: "request headers are not valid ASCII",
    })?;
    let mut lines = headers.split("\r\n");
    let request_line: Vec<_> = lines
        .next()
        .unwrap_or_default()
        .split_whitespace()
        .collect();
    if request_line.len() != 3 || request_line[2] != "HTTP/1.1" {
        return Err(RequestError {
            status: 400,
            code: "invalid_request_line",
            message: "request line must use HTTP/1.1",
        });
    }
    if request_line[1].contains('?') {
        return Err(RequestError {
            status: 400,
            code: "query_not_supported",
            message: "query strings are not supported",
        });
    }
    let method = request_line[0].to_owned();
    let path = request_line[1].to_owned();
    let mut content_length = 0_usize;
    let mut has_content_length = false;
    for line in lines.filter(|line| !line.is_empty()) {
        let Some((name, value)) = line.split_once(':') else {
            return Err(RequestError {
                status: 400,
                code: "invalid_headers",
                message: "request contains a malformed header",
            });
        };
        if name.eq_ignore_ascii_case("transfer-encoding") {
            return Err(RequestError {
                status: 400,
                code: "unsupported_transfer_encoding",
                message: "transfer-encoding is not supported",
            });
        }
        if name.eq_ignore_ascii_case("content-length") {
            if has_content_length {
                return Err(RequestError {
                    status: 400,
                    code: "duplicate_content_length",
                    message: "request must contain exactly one content-length header",
                });
            }
            has_content_length = true;
            content_length = value.trim().parse().map_err(|_| RequestError {
                status: 400,
                code: "invalid_content_length",
                message: "content-length must be a non-negative integer",
            })?;
        }
    }
    if content_length > MAX_REQUEST_BYTES {
        return Err(RequestError {
            status: 413,
            code: "body_too_large",
            message: "request body exceeds 1048576 bytes",
        });
    }
    while bytes.len() - header_end < content_length {
        let remaining = content_length - (bytes.len() - header_end);
        let mut buffer = vec![0_u8; remaining.min(4096)];
        let read = stream.read(&mut buffer).await.map_err(|_| RequestError {
            status: 400,
            code: "read_error",
            message: "request body could not be read",
        })?;
        if read == 0 {
            return Err(RequestError {
                status: 400,
                code: "incomplete_body",
                message: "request body is incomplete",
            });
        }
        bytes.extend_from_slice(&buffer[..read]);
    }
    Ok(HttpRequest {
        method,
        path,
        body: bytes[header_end..header_end + content_length].to_vec(),
    })
}

fn strict_json<T: for<'de> Deserialize<'de>>(body: &[u8]) -> Result<T, serde_json::Error> {
    let mut decoder = serde_json::Deserializer::from_slice(body);
    let value = T::deserialize(&mut decoder)?;
    decoder.end()?;
    Ok(value)
}

async fn write_error(
    stream: &mut TcpStream,
    status: u16,
    code: &str,
    message: &str,
    trace_id: Option<i64>,
) -> Result<(), std::io::Error> {
    let body = serde_json::to_vec(&json!({"error":{"code":code,"message":message}}))
        .expect("error response is serializable");
    write_response(stream, status, "application/json", &body, trace_id).await
}

async fn write_response(
    stream: &mut TcpStream,
    status: u16,
    content_type: &str,
    body: &[u8],
    trace_id: Option<i64>,
) -> Result<(), std::io::Error> {
    let reason = match status {
        200 => "OK",
        400 => "Bad Request",
        403 => "Forbidden",
        404 => "Not Found",
        405 => "Method Not Allowed",
        413 => "Payload Too Large",
        431 => "Request Header Fields Too Large",
        500 => "Internal Server Error",
        503 => "Service Unavailable",
        _ => "Error",
    };
    let trace = trace_id
        .map(|id| format!("X-OpenClaw-Trace-ID: {id}\r\n"))
        .unwrap_or_default();
    let headers = format!(
        "HTTP/1.1 {status} {reason}\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\n{trace}Connection: close\r\n\r\n",
        body.len()
    );
    stream.write_all(headers.as_bytes()).await?;
    stream.write_all(body).await?;
    stream.shutdown().await
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::providers::{
        Embedder, GenerateRequest, GenerateResponse, Message, MessageRole, Provider, ProviderError,
    };
    use async_trait::async_trait;

    struct ReplyProvider;

    #[async_trait]
    impl Provider for ReplyProvider {
        fn marshal_generate_request(
            &self,
            _request: &GenerateRequest,
        ) -> Result<Vec<u8>, ProviderError> {
            Ok(br#"{"request":true}"#.to_vec())
        }

        async fn generate(
            &self,
            _request: &mut GenerateRequest,
        ) -> Result<GenerateResponse, ProviderError> {
            Ok(GenerateResponse {
                message: Message::text(MessageRole::Assistant, "done"),
                finish_reason: "stop".into(),
                raw_response: br#"{"reply":"done"}"#.to_vec(),
                http_status: 200,
            })
        }
    }

    struct UnusedEmbedder;

    #[async_trait]
    impl Embedder for UnusedEmbedder {
        async fn embed(&self, _inputs: &[String]) -> Result<Vec<Vec<f32>>, ProviderError> {
            panic!("embedding is disabled for this test")
        }

        async fn tokenize(&self, _content: &str) -> Result<Vec<i32>, ProviderError> {
            panic!("embedding is disabled for this test")
        }

        async fn detokenize(&self, _tokens: &[i32]) -> Result<String, ProviderError> {
            panic!("embedding is disabled for this test")
        }
    }

    #[test]
    fn chat_json_is_strict_and_bounded_by_validation() {
        assert!(strict_json::<ChatRequest>(br#"{"message":"hello"}"#).is_ok());
        assert!(strict_json::<ChatRequest>(br#"{"message":"hello","extra":true}"#).is_err());
        assert!(strict_json::<ChatRequest>(br#"{"message":"hello"} false"#).is_err());
    }

    #[test]
    fn refuses_non_loopback_bind_addresses() {
        let error = GatewayError::UnsafeBind("0.0.0.0:18789".parse().unwrap());
        assert!(error.to_string().contains("must be loopback"));
    }

    #[tokio::test]
    async fn chat_endpoint_completes_delivery_and_returns_trace_id() {
        let directory = tempfile::tempdir().unwrap();
        let store = Arc::new(Store::new(directory.path().join("state.sqlite")).unwrap());
        let channels = Arc::new(Registry::new());
        let agent = Arc::new(
            Agent::new(
                Arc::new(ReplyProvider),
                Arc::clone(&channels),
                Arc::clone(&store),
                "UTC",
                Arc::new(UnusedEmbedder),
                "embed-v1",
                2,
                0.35,
                None,
            )
            .unwrap(),
        );
        let gateway = Arc::new(Gateway::new(agent, channels, Arc::clone(&store), None));
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let server = tokio::spawn(async move {
            let (stream, peer) = listener.accept().await.unwrap();
            gateway.handle_connection(stream, peer).await.unwrap();
        });
        let mut client = TcpStream::connect(address).await.unwrap();
        let body = br#"{"sender_id":"owner","message":"hello"}"#;
        client
            .write_all(
                format!(
                    "POST /chat HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                )
                .as_bytes(),
            )
            .await
            .unwrap();
        client.write_all(body).await.unwrap();
        let mut response = Vec::new();
        client.read_to_end(&mut response).await.unwrap();
        server.await.unwrap();

        let response = String::from_utf8(response).unwrap();
        assert!(response.starts_with("HTTP/1.1 200 OK\r\n"));
        assert!(response.contains("X-OpenClaw-Trace-ID: 1\r\n"));
        assert!(response.ends_with(r#"{"reply":"done"}"#));
        let traces = store
            .list_response_traces(&crate::state::TraceFilter {
                limit: 10,
                ..Default::default()
            })
            .unwrap();
        assert_eq!(traces.len(), 1);
        assert_eq!(traces[0].status, "completed");
        assert_eq!(
            store
                .get_conversation_history("cli", "owner")
                .unwrap()
                .len(),
            2
        );
    }
}
