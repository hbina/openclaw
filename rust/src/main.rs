use std::{net::SocketAddr, path::PathBuf, sync::Arc, time::Duration};

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

#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!("OpenClaw failed: {error}");
        std::process::exit(1);
    }
}

async fn run() -> Result<(), Box<dyn std::error::Error>> {
    let arguments: Vec<_> = std::env::args().collect();
    if arguments.len() == 3 && arguments[1] == "--check-state" {
        Store::new(&arguments[2])?;
        println!("State schema check passed");
        return Ok(());
    }
    if openclaw::cli::run(&arguments[1..]).await? {
        return Ok(());
    }

    let data_dir = required_directory("OPENCLAW_DATA_DIR")?;
    let config_dir = required_directory("OPENCLAW_CONFIG_DIR")?;

    let config = Config::load(config_dir.join("openclaw.json"))?;
    let secrets = match Secrets::load(config_dir.join("secrets.json")) {
        Ok(secrets) => secrets,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            eprintln!("Warning: secrets.json not found: {error}");
            Secrets::default()
        }
        Err(error) => return Err(error.into()),
    };

    let store = Arc::new(Store::new(data_dir.join("openclaw-agent.sqlite"))?);
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
    tokio::time::timeout(Duration::from_secs(30), async {
        chat.context_size().await?;
        let vectors = embeddings
            .embed(&["openclaw readiness probe".into()])
            .await?;
        if vectors.len() != 1 {
            return Err(openclaw::providers::ProviderError::EmbeddingCount {
                actual: vectors.len(),
                expected: 1,
            });
        }
        Ok::<(), openclaw::providers::ProviderError>(())
    })
    .await
    .map_err(|_| "local model readiness probe timed out")??;

    let dimensions = config.models.embeddings.dimensions as usize;
    store.validate_derived_memory_state(&config.models.embeddings.index_id, dimensions)?;
    let gaps =
        store.count_conversation_index_gaps(&config.models.embeddings.index_id, 1, dimensions)?;
    if gaps != 0 {
        return Err(format!(
            "conversation index readiness check failed: {gaps} completed exchanges are unindexed; run memory reindex"
        )
        .into());
    }

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
        Some(Arc::new(Maintainer::new(
            Arc::clone(&store),
            service,
            background_chat,
            &config.channels.telegram.owner_user_id,
            config.agents.defaults.memory_maintenance.batch_size as usize,
            &config.agents.defaults.memory_maintenance.schedule,
            &config.agents.defaults.memory_maintenance.timezone,
        )?))
    } else {
        None
    };
    let gateway = Arc::new(Gateway::new(agent, channels, store, maintainer));
    let address = gateway_address()?;
    eprintln!("Starting OpenClaw Rust gateway on {address}");
    let (shutdown_tx, shutdown_rx) = tokio::sync::watch::channel(false);
    let mut server = tokio::spawn(gateway.serve(address, shutdown_rx));
    tokio::select! {
        result = &mut server => result??,
        signal = shutdown_signal() => {
            signal?;
            let _ = shutdown_tx.send(true);
            server.await??;
        }
    }
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
