use std::{
    io::IsTerminal,
    net::SocketAddr,
    path::PathBuf,
    sync::Arc,
    time::{Duration, Instant},
};

use openclaw::channels::{Registry, TelegramAdapter};
use openclaw::config::{Config, Secrets};
use openclaw::gateway::{Agent, Gateway, RagService};
use openclaw::maintenance::Maintainer;
use openclaw::memory::Service as MemoryService;
use openclaw::providers::{
    Embedder, EmbeddingClient, OpenAiClient, PriorityEmbedder, PriorityGate, PriorityPromptSizer,
    PriorityProvider, PromptSizer, Provider, WorkPriority,
};
use openclaw::state::Store;
use tracing::{debug, error, info, warn};
use tracing_subscriber::{EnvFilter, fmt::time::SystemTime};

const DEFAULT_LOG_FILTER: &str = "warn,openclaw=debug";

#[tokio::main]
async fn main() {
    init_logging();
    if let Err(error) = run().await {
        error!(error = %error, "OpenClaw stopped with an error");
        std::process::exit(1);
    }
}

fn init_logging() {
    let filter = match std::env::var("RUST_LOG") {
        Ok(value) => EnvFilter::try_new(value).unwrap_or_else(|error| {
            eprintln!("Ignoring invalid RUST_LOG value ({error}); using {DEFAULT_LOG_FILTER}");
            EnvFilter::new(DEFAULT_LOG_FILTER)
        }),
        Err(std::env::VarError::NotPresent) => EnvFilter::new(DEFAULT_LOG_FILTER),
        Err(error) => {
            eprintln!("Ignoring invalid RUST_LOG value ({error}); using {DEFAULT_LOG_FILTER}");
            EnvFilter::new(DEFAULT_LOG_FILTER)
        }
    };
    tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_timer(SystemTime)
        .with_ansi(std::io::stderr().is_terminal())
        .with_writer(std::io::stderr)
        .compact()
        .init();
}

async fn run() -> Result<(), Box<dyn std::error::Error>> {
    let arguments: Vec<_> = std::env::args().collect();
    debug!(
        version = env!("CARGO_PKG_VERSION"),
        command = arguments.get(1).map_or("serve", String::as_str),
        "OpenClaw process started"
    );
    if arguments.len() == 3 && arguments[1] == "--check-state" {
        debug!(database = %arguments[2], "checking state schema");
        Store::new(&arguments[2])?;
        println!("State schema check passed");
        return Ok(());
    }
    if openclaw::cli::run(&arguments[1..]).await? {
        return Ok(());
    }

    let data_dir = required_directory("OPENCLAW_DATA_DIR")?;
    let config_dir = required_directory("OPENCLAW_CONFIG_DIR")?;
    debug!(
        config_dir = %config_dir.display(),
        data_dir = %data_dir.display(),
        "loading runtime configuration"
    );

    let config = Config::load(config_dir.join("openclaw.json"))?;
    debug!(
        telegram_enabled = config.channels.telegram.enabled,
        memory_maintenance_enabled = config.agents.defaults.memory_maintenance.enabled,
        embedding_model = %config.models.embeddings.model,
        embedding_dimensions = config.models.embeddings.dimensions,
        "configuration loaded"
    );
    let secrets = match Secrets::load(config_dir.join("secrets.json")) {
        Ok(secrets) => secrets,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            warn!(error = %error, "secrets file not found; continuing with empty secrets");
            Secrets::default()
        }
        Err(error) => return Err(error.into()),
    };

    let database_path = data_dir.join("openclaw-agent.sqlite");
    let store = Arc::new(Store::new(&database_path)?);
    debug!(database = %database_path.display(), "SQLite state opened");
    let chat = Arc::new(OpenAiClient::new(
        &secrets.models.providers.openai.api_key,
        &config.models.providers.openai.base_url,
    )?);
    let embeddings = Arc::new(EmbeddingClient::new(
        &secrets.models.embeddings.api_key,
        &config.models.embeddings.base_url,
        &config.models.embeddings.model,
        config.models.embeddings.dimensions as usize,
    )?);
    info!(timeout_seconds = 30, "checking local model readiness");
    let readiness_started = Instant::now();
    let (context_size, observed_dimensions) =
        tokio::time::timeout(Duration::from_secs(30), async {
            let context_size = chat.context_size().await?;
            let vectors = embeddings
                .embed(&["openclaw readiness probe".into()])
                .await?;
            if vectors.len() != 1 {
                return Err(openclaw::providers::ProviderError::EmbeddingCount {
                    actual: vectors.len(),
                    expected: 1,
                });
            }
            Ok::<_, openclaw::providers::ProviderError>((context_size, vectors[0].len()))
        })
        .await
        .map_err(|_| "local model readiness probe timed out")??;
    info!(
        elapsed_ms = readiness_started.elapsed().as_millis(),
        context_size,
        embedding_dimensions = observed_dimensions,
        "local models are ready"
    );

    let dimensions = config.models.embeddings.dimensions as usize;
    debug!("validating derived memory and conversation indexes");
    store.validate_derived_memory_state(&config.models.embeddings.index_id, dimensions)?;
    let gaps =
        store.count_conversation_index_gaps(&config.models.embeddings.index_id, 1, dimensions)?;
    if gaps != 0 {
        return Err(format!(
            "conversation index readiness check failed: {gaps} completed exchanges are unindexed; run memory reindex"
        )
        .into());
    }
    debug!(conversation_index_gaps = gaps, "derived state is ready");

    let chat_gate = PriorityGate::new();
    let embedding_gate = PriorityGate::new();
    let chat_provider: Arc<dyn Provider> = chat.clone();
    let embedding_provider: Arc<dyn Embedder> = embeddings;
    let foreground_chat: Arc<dyn Provider> = Arc::new(PriorityProvider::new(
        Arc::clone(&chat_provider),
        Arc::clone(&chat_gate),
        WorkPriority::Foreground,
    ));
    let background_chat: Arc<dyn Provider> = Arc::new(PriorityProvider::new(
        chat_provider,
        Arc::clone(&chat_gate),
        WorkPriority::Background,
    ));
    let foreground_embedding: Arc<dyn Embedder> = Arc::new(PriorityEmbedder::new(
        Arc::clone(&embedding_provider),
        Arc::clone(&embedding_gate),
        WorkPriority::Foreground,
    ));
    let background_embedding: Arc<dyn Embedder> = Arc::new(PriorityEmbedder::new(
        embedding_provider,
        embedding_gate,
        WorkPriority::Background,
    ));
    let raw_prompt_sizer: Arc<dyn PromptSizer> = chat;
    let prompt_sizer: Arc<dyn PromptSizer> = Arc::new(PriorityPromptSizer::new(
        raw_prompt_sizer,
        Arc::clone(&chat_gate),
        WorkPriority::Foreground,
    ));

    let mut channels = Registry::new();
    if config.channels.telegram.enabled {
        if secrets.channels.telegram.bot_token.trim().is_empty() {
            return Err(
                "Telegram is enabled but channels.telegram.botToken is missing from secrets.json"
                    .into(),
            );
        }
        channels.register(Arc::new(TelegramAdapter::new(
            &secrets.channels.telegram.bot_token,
            &config.channels.telegram.owner_user_id,
        )?));
        info!("Telegram channel configured");
    } else {
        debug!("Telegram channel disabled");
    }
    let channels = Arc::new(channels);
    let rag = Arc::new(RagService::new(
        Arc::clone(&store),
        Arc::clone(&foreground_embedding),
        prompt_sizer,
        &config.models.embeddings.index_id,
        dimensions,
        config.agents.defaults.history_search.min_score,
    ));
    let agent = Arc::new(Agent::new(
        foreground_chat,
        Arc::clone(&channels),
        Arc::clone(&store),
        "Local",
        foreground_embedding,
        &config.models.embeddings.index_id,
        dimensions,
        config.agents.defaults.history_search.min_score,
        Some(rag),
    )?);
    let maintainer = if config.agents.defaults.memory_maintenance.enabled {
        let service = Arc::new(MemoryService::new(
            Arc::clone(&store),
            background_embedding,
            &config.models.embeddings.index_id,
            dimensions,
            config.agents.defaults.history_search.min_score,
        ));
        let maintainer = Arc::new(Maintainer::new(
            Arc::clone(&store),
            service,
            background_chat,
            &config.channels.telegram.owner_user_id,
            config.agents.defaults.memory_maintenance.batch_size as usize,
            &config.agents.defaults.memory_maintenance.schedule,
            &config.agents.defaults.memory_maintenance.timezone,
        )?);
        info!(
            schedule = %config.agents.defaults.memory_maintenance.schedule,
            timezone = %config.agents.defaults.memory_maintenance.timezone,
            "memory maintenance enabled"
        );
        Some(maintainer)
    } else {
        debug!("memory maintenance disabled");
        None
    };
    let gateway = Arc::new(Gateway::new(agent, channels, store, maintainer));
    let address = gateway_address()?;
    info!(%address, "starting OpenClaw Rust gateway");
    let (shutdown_tx, shutdown_rx) = tokio::sync::watch::channel(false);
    let mut server = tokio::spawn(gateway.serve(address, shutdown_rx));
    tokio::select! {
        result = &mut server => result??,
        signal = shutdown_signal() => {
            signal?;
            info!("shutdown signal received");
            let _ = shutdown_tx.send(true);
            server.await??;
        }
    }
    info!("OpenClaw shutdown complete");
    Ok(())
}

fn required_directory(name: &str) -> Result<PathBuf, String> {
    std::env::var(name)
        .ok()
        .map(|value| value.trim().to_owned())
        .filter(|value| !value.is_empty())
        .map(PathBuf::from)
        .ok_or_else(|| format!("{name} must be set"))
}

fn gateway_address() -> Result<SocketAddr, String> {
    if let Ok(value) = std::env::var("OPENCLAW_HTTP_ADDR") {
        return value
            .trim()
            .parse()
            .map_err(|_| "OPENCLAW_HTTP_ADDR must be a loopback IP socket address".into());
    }
    let port = std::env::var("PORT").unwrap_or_else(|_| "18789".into());
    format!("127.0.0.1:{}", port.trim())
        .parse()
        .map_err(|_| "PORT must be a valid TCP port".into())
}

#[cfg(unix)]
async fn shutdown_signal() -> Result<(), std::io::Error> {
    use tokio::signal::unix::{SignalKind, signal};
    let mut terminate = signal(SignalKind::terminate())?;
    tokio::select! {
        result = tokio::signal::ctrl_c() => result,
        _ = terminate.recv() => Ok(()),
    }
}

#[cfg(not(unix))]
async fn shutdown_signal() -> Result<(), std::io::Error> {
    tokio::signal::ctrl_c().await
}
