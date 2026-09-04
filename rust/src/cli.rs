use std::{
    io::Read,
    path::{Path, PathBuf},
    sync::Arc,
    time::Duration,
};

use chrono::{DateTime, SecondsFormat, Utc};
use clap::{Args, Parser, Subcommand, ValueEnum};
use serde::Deserialize;
use serde_json::json;
use tracing::debug;
use url::Url;

const MAX_GATEWAY_RESPONSE_BYTES: usize = 1 << 20;

use crate::{
    config::{Config, Secrets},
    gateway::RagService,
    maintenance::Maintainer,
    memory::{Provenance, Service},
    providers::{Embedder, EmbeddingClient, OpenAiClient, PromptSizer, Provider},
    state::{
        MaintenanceMode, Memory, MemoryFilter, MemoryKind, MemoryOrigin, MemorySource,
        MemoryStatus, Store, TraceFilter,
    },
};

#[derive(Debug, Parser)]
#[command(
    name = "openclaw",
    version,
    about = "Locally operated single-owner personal assistant",
    args_conflicts_with_subcommands = true
)]
struct Cli {
    /// Validate or initialize a SQLite state database and exit.
    #[arg(long, value_name = "DATABASE", hide = true)]
    check_state: Option<PathBuf>,

    #[command(subcommand)]
    command: Option<Command>,
}

#[derive(Debug, Subcommand)]
enum Command {
    /// Send one chat request to a running gateway.
    Chat(ChatArgs),
    /// Check whether a running gateway is healthy.
    Health(HealthArgs),
    /// Inspect persisted response traces.
    Trace(TraceArgs),
    /// Inspect and maintain the memory ledger.
    Memory(MemoryArgs),
}

#[derive(Debug, Args)]
struct HealthArgs {
    /// Base URL of the gateway.
    #[arg(long, value_name = "URL")]
    url: Option<String>,

    /// Request timeout in seconds, or with an `s` or `m` suffix.
    #[arg(long, value_parser = parse_duration, default_value = "5s")]
    timeout: Duration,
}

#[derive(Debug, Args)]
struct ChatArgs {
    /// Base URL of the gateway.
    #[arg(long, value_name = "URL")]
    url: Option<String>,

    /// Conversation sender key used by the local CLI route.
    #[arg(long, default_value = "cli-user", value_parser = parse_nonempty)]
    sender_id: String,

    /// Request timeout in seconds, or with an `s` or `m` suffix.
    #[arg(long, value_parser = parse_duration, default_value = "10m")]
    timeout: Duration,

    /// Print a JSON response containing the reply and trace ID.
    #[arg(long)]
    json: bool,

    /// Message text. When omitted, the message is read from standard input.
    #[arg(value_name = "MESSAGE")]
    message: Vec<String>,
}

#[derive(Debug, Args)]
struct TraceArgs {
    #[command(subcommand)]
    command: TraceCommand,
}

#[derive(Debug, Subcommand)]
enum TraceCommand {
    /// List response traces, newest first.
    List(TraceListArgs),
    /// Show one complete response trace.
    Show(TraceShowArgs),
}

#[derive(Debug, Args)]
struct TraceListArgs {
    #[command(flatten)]
    database: DatabaseArg,

    /// Maximum number of traces to return.
    #[arg(long, default_value_t = 20, value_parser = parse_trace_limit)]
    limit: usize,

    /// Filter by channel ID.
    #[arg(long, default_value = "")]
    channel: String,

    /// Filter by sender ID.
    #[arg(long, default_value = "")]
    sender: String,

    /// Filter by trace status.
    #[arg(long, default_value = "")]
    status: String,

    /// Filter by external message ID.
    #[arg(long, default_value = "")]
    message_id: String,

    /// Include only traces at or after this RFC3339 timestamp.
    #[arg(long, value_parser = parse_rfc3339)]
    since: Option<DateTime<Utc>>,

    /// Print JSON instead of the human-readable table.
    #[arg(long)]
    json: bool,
}

#[derive(Debug, Args)]
struct TraceShowArgs {
    #[command(flatten)]
    database: DatabaseArg,

    /// Trace ID to inspect.
    #[arg(long, value_parser = parse_positive_i64)]
    id: i64,

    /// Print JSON instead of the human-readable report.
    #[arg(long)]
    json: bool,
}

#[derive(Debug, Args)]
struct MemoryArgs {
    #[command(subcommand)]
    command: MemoryCommand,
}

#[derive(Debug, Subcommand)]
enum MemoryCommand {
    /// Show active and deleted memory counts.
    Status(DatabaseArg),
    /// List memory records.
    List(MemoryListArgs),
    /// Show one memory record.
    Get(MemoryIdArgs),
    /// Add a memory record and its embedding.
    Add(MemoryAddArgs),
    /// Update a memory record and its embedding.
    Update(MemoryUpdateArgs),
    /// Mark a memory record as deleted.
    Remove(MemoryIdArgs),
    /// Search memory using the configured local models.
    Search(MemorySearchArgs),
    /// Rebuild memory and conversation indexes.
    Reindex(ConfiguredMemoryArgs),
    /// Inspect or run background memory consolidation.
    Maintenance(MemoryMaintenanceArgs),
}

#[derive(Debug, Args)]
struct DatabaseArg {
    /// Path to the canonical SQLite database.
    #[arg(long, value_name = "PATH")]
    database: PathBuf,
}

#[derive(Debug, Args)]
struct ConfiguredMemoryArgs {
    #[command(flatten)]
    database: DatabaseArg,

    /// Directory containing openclaw.json and secrets.json.
    #[arg(long, value_name = "DIR")]
    config_dir: PathBuf,
}

#[derive(Debug, Clone, Copy, ValueEnum)]
enum MemoryKindArg {
    Profile,
    Durable,
    Daily,
}

impl From<MemoryKindArg> for MemoryKind {
    fn from(value: MemoryKindArg) -> Self {
        match value {
            MemoryKindArg::Profile => Self::Profile,
            MemoryKindArg::Durable => Self::Durable,
            MemoryKindArg::Daily => Self::Daily,
        }
    }
}

#[derive(Debug, Clone, Copy, ValueEnum)]
enum MemoryStatusArg {
    Active,
    Deleted,
    All,
}

impl From<MemoryStatusArg> for MemoryStatus {
    fn from(value: MemoryStatusArg) -> Self {
        match value {
            MemoryStatusArg::Active => Self::Active,
            MemoryStatusArg::Deleted => Self::Deleted,
            MemoryStatusArg::All => Self::All,
        }
    }
}

#[derive(Debug, Args)]
struct MemoryListArgs {
    #[command(flatten)]
    database: DatabaseArg,

    /// Filter by memory kind.
    #[arg(long, value_enum)]
    kind: Option<MemoryKindArg>,

    /// Filter by lifecycle status.
    #[arg(long, value_enum, default_value_t = MemoryStatusArg::Active)]
    status: MemoryStatusArg,

    /// Maximum number of memories to return.
    #[arg(long, default_value_t = 20)]
    limit: usize,
}

#[derive(Debug, Args)]
struct MemoryIdArgs {
    #[command(flatten)]
    database: DatabaseArg,

    /// Memory ID.
    #[arg(long, value_parser = parse_positive_i64)]
    id: i64,
}

#[derive(Debug, Args)]
struct MemoryAddArgs {
    #[command(flatten)]
    configured: ConfiguredMemoryArgs,

    /// Memory category.
    #[arg(long, value_enum)]
    kind: MemoryKindArg,

    /// Memory text to persist.
    #[arg(long, value_parser = parse_nonempty)]
    content: String,
}

#[derive(Debug, Args)]
struct MemoryUpdateArgs {
    #[command(flatten)]
    configured: ConfiguredMemoryArgs,

    /// Memory ID to update.
    #[arg(long, value_parser = parse_positive_i64)]
    id: i64,

    /// Replacement category; defaults to the existing category.
    #[arg(long, value_enum)]
    kind: Option<MemoryKindArg>,

    /// Replacement memory text.
    #[arg(long, value_parser = parse_nonempty)]
    content: String,
}

#[derive(Debug, Args)]
struct MemorySearchArgs {
    #[command(flatten)]
    configured: ConfiguredMemoryArgs,

    /// Query passed to local semantic and keyword recall.
    #[arg(long, value_parser = parse_nonempty)]
    query: String,

    /// Maximum number of results.
    #[arg(long, default_value_t = 5)]
    max_results: usize,
}

#[derive(Debug, Args)]
struct MemoryMaintenanceArgs {
    #[command(subcommand)]
    command: MemoryMaintenanceCommand,
}

#[derive(Debug, Subcommand)]
enum MemoryMaintenanceCommand {
    /// Show maintenance checkpoint, lease, and latest-run state.
    Status(DatabaseArg),
    /// Extract and evaluate candidates without mutating memory.
    Preview(ConfiguredMemoryArgs),
    /// Run maintenance and apply accepted candidates.
    Run(ConfiguredMemoryArgs),
    /// List candidates recorded for one maintenance run.
    Candidates(MaintenanceCandidatesArgs),
}

#[derive(Debug, Args)]
struct MaintenanceCandidatesArgs {
    #[command(flatten)]
    database: DatabaseArg,

    /// Maintenance run ID.
    #[arg(long, value_parser = parse_positive_i64)]
    run_id: i64,
}

pub async fn run(arguments: &[String]) -> Result<bool, Box<dyn std::error::Error>> {
    let cli = Cli::try_parse_from(
        std::iter::once("openclaw").chain(arguments.iter().map(String::as_str)),
    )?;
    if let Some(database) = cli.check_state {
        debug!(database = %database.display(), "checking state schema");
        Store::new(database)?;
        println!("State schema check passed");
        return Ok(true);
    }
    match cli.command {
        Some(Command::Chat(arguments)) => chat(arguments).await?,
        Some(Command::Health(arguments)) => health(arguments).await?,
        Some(Command::Trace(arguments)) => trace(arguments)?,
        Some(Command::Memory(arguments)) => memory(arguments).await?,
        None => return Ok(false),
    }
    Ok(true)
}

async fn health(arguments: HealthArgs) -> Result<(), Box<dyn std::error::Error>> {
    let base = arguments
        .url
        .or_else(|| std::env::var("OPENCLAW_GATEWAY_URL").ok())
        .unwrap_or_else(|| "http://127.0.0.1:18789".into());
    let endpoint = gateway_endpoint(&base, "healthz")?;
    let response = reqwest::Client::builder()
        .timeout(arguments.timeout)
        .build()?
        .get(endpoint)
        .send()
        .await?;
    if !response.status().is_success() {
        return Err(format!("gateway health check returned {}", response.status()).into());
    }
    Ok(())
}

fn gateway_endpoint(base: &str, path: &str) -> Result<Url, Box<dyn std::error::Error>> {
    let mut endpoint = Url::parse(base.trim())?;
    if !matches!(endpoint.scheme(), "http" | "https")
        || endpoint.host_str().is_none()
        || endpoint.query().is_some()
        || endpoint.fragment().is_some()
    {
        return Err("--url must be an absolute HTTP URL without query or fragment".into());
    }
    endpoint.set_path(&format!("{}/{path}", endpoint.path().trim_end_matches('/')));
    Ok(endpoint)
}

async fn chat(arguments: ChatArgs) -> Result<(), Box<dyn std::error::Error>> {
    let message = if arguments.message.is_empty() {
        let mut input = String::new();
        std::io::stdin().read_to_string(&mut input)?;
        input.trim().to_owned()
    } else {
        arguments.message.join(" ").trim().to_owned()
    };
    if message.is_empty() {
        return Err("message is required as arguments or stdin".into());
    }
    let base = arguments
        .url
        .or_else(|| std::env::var("OPENCLAW_GATEWAY_URL").ok())
        .unwrap_or_else(|| "http://127.0.0.1:18789".into());
    let endpoint = gateway_endpoint(&base, "chat")?;
    let mut response = reqwest::Client::builder()
        .timeout(arguments.timeout)
        .build()?
        .post(endpoint)
        .json(&json!({"sender_id":arguments.sender_id,"message":message}))
        .send()
        .await?;
    let status = response.status();
    let trace = response
        .headers()
        .get("X-OpenClaw-Trace-ID")
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.parse::<i64>().ok());
    if response
        .content_length()
        .is_some_and(|size| size > MAX_GATEWAY_RESPONSE_BYTES as u64)
    {
        return Err(format!("chat response exceeds {MAX_GATEWAY_RESPONSE_BYTES} bytes").into());
    }
    let mut body = Vec::with_capacity(
        response
            .content_length()
            .unwrap_or(0)
            .min(MAX_GATEWAY_RESPONSE_BYTES as u64) as usize,
    );
    while let Some(chunk) = response.chunk().await? {
        if body.len().saturating_add(chunk.len()) > MAX_GATEWAY_RESPONSE_BYTES {
            return Err(format!("chat response exceeds {MAX_GATEWAY_RESPONSE_BYTES} bytes").into());
        }
        body.extend_from_slice(&chunk);
    }
    if !status.is_success() {
        return Err(format!(
            "gateway returned {}: {}{}",
            status,
            String::from_utf8_lossy(&body).trim(),
            trace
                .map(|id| format!(" (trace ID {id})"))
                .unwrap_or_default()
        )
        .into());
    }
    let trace = trace.ok_or("chat response is missing X-OpenClaw-Trace-ID")?;
    #[derive(Deserialize)]
    struct ChatResponse {
        reply: String,
    }
    let output: ChatResponse = serde_json::from_slice(&body)?;
    if output.reply.trim().is_empty() {
        return Err("chat response contains an empty reply".into());
    }
    if arguments.json {
        println!(
            "{}",
            serde_json::to_string(&json!({"reply":output.reply,"trace_id":trace}))?
        );
    } else {
        println!("{}", output.reply);
        eprintln!("Trace ID: {trace}");
    }
    Ok(())
}

fn trace(arguments: TraceArgs) -> Result<(), Box<dyn std::error::Error>> {
    match arguments.command {
        TraceCommand::List(arguments) => {
            let store = Store::open_read_only(&arguments.database.database)?;
            let items = store.list_response_traces(&TraceFilter {
                limit: arguments.limit,
                channel_id: arguments.channel,
                sender_id: arguments.sender,
                status: arguments.status,
                external_message_id: arguments.message_id,
                since: arguments.since,
            })?;
            if arguments.json {
                println!("{}", serde_json::to_string_pretty(&items)?);
            } else {
                println!("TRACE\tTIME\tTRIGGER\tROUTE\tSTATUS\tRESPONSE");
                for item in items {
                    let mut preview = item
                        .final_content
                        .split_whitespace()
                        .collect::<Vec<_>>()
                        .join(" ");
                    if preview.chars().count() > 80 {
                        preview = format!("{}...", preview.chars().take(77).collect::<String>());
                    }
                    println!(
                        "{}\t{}\t{}\t{}/{}\t{}\t{}",
                        item.id,
                        item.started_at.to_rfc3339_opts(SecondsFormat::Secs, true),
                        item.trigger_type,
                        item.channel_id,
                        item.sender_id,
                        item.status,
                        preview
                    );
                }
            }
        }
        TraceCommand::Show(arguments) => {
            let store = Store::open_read_only(&arguments.database.database)?;
            let report = store.get_trace_report(arguments.id)?;
            let value = json!({"trace":report.trace,"events":report.events});
            if arguments.json {
                println!("{}", serde_json::to_string_pretty(&value)?);
            } else {
                println!("Trace {} ({})", arguments.id, value["trace"]["status"]);
                println!(
                    "Route: {}/{}",
                    value["trace"]["channel_id"], value["trace"]["sender_id"]
                );
                println!("Trigger: {}", value["trace"]["trigger_type"]);
                println!("Input: {}", value["trace"]["input"]);
                for event in value["events"].as_array().into_iter().flatten() {
                    println!(
                        "{}. {} [{}]",
                        event["sequence_no"], event["kind"], event["status"]
                    );
                    println!("{}", serde_json::to_string_pretty(&event["detail"])?);
                }
            }
        }
    }
    Ok(())
}

async fn memory(arguments: MemoryArgs) -> Result<(), Box<dyn std::error::Error>> {
    match arguments.command {
        MemoryCommand::Status(arguments) => {
            let store = open_store(&arguments)?;
            let (active, deleted) = store.memory_counts()?;
            println!("Memory ledger: {active} active, {deleted} deleted");
        }
        MemoryCommand::List(arguments) => {
            let store = open_store(&arguments.database)?;
            for item in store.list_memories(MemoryFilter {
                kind: arguments.kind.map(Into::into),
                status: arguments.status.into(),
                limit: arguments.limit,
            })? {
                print_memory(&item);
            }
        }
        MemoryCommand::Get(arguments) => {
            let store = open_store(&arguments.database)?;
            print_memory(&store.get_memory(arguments.id)?);
        }
        MemoryCommand::Remove(arguments) => {
            let store = open_store(&arguments.database)?;
            let item = store.with_tx(|tx| tx.remove_memory(arguments.id, Utc::now()))?;
            println!(
                "Memory ID {} is deleted and no longer recalled; revisions remain available.",
                item.id
            );
        }
        MemoryCommand::Add(arguments) => {
            let (store, service, _, _) = configured_memory(&arguments.configured)?;
            let write = service
                .prepare_write(
                    arguments.kind.into(),
                    &arguments.content,
                    operator_provenance(),
                )
                .await?;
            let (item, stored) = store.with_tx(|tx| tx.store_memory(&write))?;
            println!(
                "Memory ID {} ({}, revision {}, stored={}): {}",
                item.id,
                kind_name(item.kind),
                item.revision_number,
                stored,
                item.content
            );
        }
        MemoryCommand::Update(arguments) => {
            let (store, service, _, _) = configured_memory(&arguments.configured)?;
            let current = store.get_memory(arguments.id)?;
            let kind = arguments.kind.map(Into::into).unwrap_or(current.kind);
            let write = service
                .prepare_write(kind, &arguments.content, operator_provenance())
                .await?;
            print_memory(&store.with_tx(|tx| tx.update_memory(arguments.id, &write))?);
        }
        MemoryCommand::Search(arguments) => {
            let (_, service, config, _) = configured_memory(&arguments.configured)?;
            let chat = configured_chat(&arguments.configured.config_dir, &config)?;
            for (index, item) in service
                .search_with_model(&chat, &arguments.query, arguments.max_results)
                .await?
                .iter()
                .enumerate()
            {
                println!(
                    "{}. Memory ID {} [{}] score {:.4}: {}",
                    index + 1,
                    item.memory.id,
                    kind_name(item.memory.kind),
                    item.combined_score,
                    item.memory.content
                );
            }
        }
        MemoryCommand::Reindex(arguments) => {
            let (store, service, config, embedder) = configured_memory(&arguments)?;
            let memories = service.prepare_reindex().await?;
            let chat = Arc::new(configured_chat(&arguments.config_dir, &config)?);
            let sizer: Arc<dyn PromptSizer> = chat;
            let rag = RagService::new(
                Arc::clone(&store),
                embedder,
                sizer,
                &config.models.embeddings.index_id,
                config.models.embeddings.dimensions as usize,
                config.agents.defaults.history_search.min_score,
            );
            let conversations = rag.prepare_reindex().await?;
            store.replace_derived_indexes(&memories, &conversations, Utc::now())?;
            println!(
                "Reindexed {} active memories and {} completed conversations.",
                memories.len(),
                conversations.len()
            );
        }
        MemoryCommand::Maintenance(arguments) => memory_maintenance(arguments).await?,
    }
    Ok(())
}

async fn memory_maintenance(
    arguments: MemoryMaintenanceArgs,
) -> Result<(), Box<dyn std::error::Error>> {
    match arguments.command {
        MemoryMaintenanceCommand::Status(arguments) => {
            let store = open_store(&arguments)?;
            let (status, latest) = store.maintenance_status()?;
            println!(
                "Memory maintenance checkpoint: history ID {}",
                status.checkpoint_history_id
            );
            if let Some(next) = status.next_run_at {
                println!(
                    "Next scheduled run: {}",
                    next.to_rfc3339_opts(SecondsFormat::Secs, true)
                );
            }
            if !status.lease_owner.is_empty() {
                println!("Worker lease: active");
            }
            if let Some(run) = latest {
                println!(
                    "Latest run: {} mode={:?} status={} stage={} range={}..{} processed={} candidates={} promoted={} rejected={}",
                    run.id,
                    run.mode,
                    run.status,
                    run.stage,
                    run.checkpoint_history_id,
                    run.highwater_history_id,
                    run.processed_history_id,
                    run.candidate_count,
                    run.promoted_count,
                    run.rejected_count
                );
                if !run.error.is_empty() {
                    println!("Latest error: {}", run.error);
                }
            } else {
                println!("Latest run: none");
            }
        }
        MemoryMaintenanceCommand::Candidates(arguments) => {
            let store = open_store(&arguments.database)?;
            for item in store.list_maintenance_candidates(arguments.run_id)? {
                println!(
                    "Candidate {} [{}/{}] action={}{} evidence={:?} scores=trust:{:.3} recency:{:.3} novelty:{:.3} contradiction:{:.3} reason={}\n{}",
                    item.id,
                    kind_name(item.kind),
                    item.status,
                    item.proposed_action,
                    item.target_memory_id
                        .map(|id| format!(" target={id}"))
                        .unwrap_or_default(),
                    item.evidence_history_ids,
                    item.trust_score,
                    item.recency_score,
                    item.novelty_score,
                    item.contradiction_score,
                    item.decision_reason,
                    item.content
                );
            }
        }
        MemoryMaintenanceCommand::Preview(arguments) => {
            run_memory_maintenance(&arguments, MaintenanceMode::Preview).await?
        }
        MemoryMaintenanceCommand::Run(arguments) => {
            run_memory_maintenance(&arguments, MaintenanceMode::Apply).await?
        }
    }
    Ok(())
}

async fn run_memory_maintenance(
    arguments: &ConfiguredMemoryArgs,
    mode: MaintenanceMode,
) -> Result<(), Box<dyn std::error::Error>> {
    let (store, service, config, _) = configured_memory(arguments)?;
    let chat: Arc<dyn Provider> = Arc::new(configured_chat(&arguments.config_dir, &config)?);
    let maintainer = Maintainer::new(
        Arc::clone(&store),
        service,
        chat,
        &config.channels.telegram.owner_user_id,
        config.agents.defaults.memory_maintenance.batch_size as usize,
        &config.agents.defaults.memory_maintenance.schedule,
        &config.agents.defaults.memory_maintenance.timezone,
    )?;
    let result = maintainer.run(mode).await?;
    println!(
        "Maintenance run {} ({:?}) processed through history ID {}: {} candidates, {} promoted, {} rejected.",
        result.run_id,
        result.mode,
        result.processed_history_id,
        result.candidate_count,
        result.promoted_count,
        result.rejected_count
    );
    Ok(())
}

type ConfiguredMemory = (Arc<Store>, Arc<Service>, Config, Arc<dyn Embedder>);

fn configured_memory(
    arguments: &ConfiguredMemoryArgs,
) -> Result<ConfiguredMemory, Box<dyn std::error::Error>> {
    let store = Arc::new(Store::new(&arguments.database.database)?);
    let config = Config::load(arguments.config_dir.join("openclaw.json"))?;
    let secrets = Secrets::load(arguments.config_dir.join("secrets.json")).unwrap_or_default();
    let embedder: Arc<dyn Embedder> = Arc::new(EmbeddingClient::new(
        &secrets.models.embeddings.api_key,
        &config.models.embeddings.base_url,
        &config.models.embeddings.model,
        config.models.embeddings.dimensions as usize,
    )?);
    let service = Arc::new(Service::new(
        Arc::clone(&store),
        Arc::clone(&embedder),
        &config.models.embeddings.index_id,
        config.models.embeddings.dimensions as usize,
        config.agents.defaults.history_search.min_score,
    ));
    Ok((store, service, config, embedder))
}

fn configured_chat(
    directory: &Path,
    config: &Config,
) -> Result<OpenAiClient, Box<dyn std::error::Error>> {
    let secrets = Secrets::load(directory.join("secrets.json")).unwrap_or_default();
    Ok(OpenAiClient::new(
        &secrets.models.providers.openai.api_key,
        &config.models.providers.openai.base_url,
    )?)
}

fn open_store(arguments: &DatabaseArg) -> Result<Store, Box<dyn std::error::Error>> {
    Ok(Store::new(&arguments.database)?)
}

fn operator_provenance() -> Provenance {
    Provenance {
        origin: MemoryOrigin::Owner,
        source: MemorySource::Operator,
        source_history_id: None,
        source_trace_id: None,
    }
}

fn parse_nonempty(value: &str) -> Result<String, String> {
    let value = value.trim();
    if value.is_empty() {
        Err("value must not be empty".into())
    } else {
        Ok(value.into())
    }
}

fn parse_positive_i64(value: &str) -> Result<i64, String> {
    let value = value
        .parse::<i64>()
        .map_err(|_| "value must be a positive integer".to_owned())?;
    if value < 1 {
        return Err("value must be a positive integer".into());
    }
    Ok(value)
}

fn parse_trace_limit(value: &str) -> Result<usize, String> {
    let value = value
        .parse::<usize>()
        .map_err(|_| "value must be an integer between 1 and 1000".to_owned())?;
    if !(1..=1000).contains(&value) {
        return Err("value must be between 1 and 1000".into());
    }
    Ok(value)
}

fn parse_rfc3339(value: &str) -> Result<DateTime<Utc>, String> {
    DateTime::parse_from_rfc3339(value)
        .map(|value| value.with_timezone(&Utc))
        .map_err(|_| "value must be an RFC3339 timestamp".into())
}

fn kind_name(kind: MemoryKind) -> &'static str {
    match kind {
        MemoryKind::Profile => "profile",
        MemoryKind::Durable => "durable",
        MemoryKind::Daily => "daily",
    }
}

fn status_name(status: MemoryStatus) -> &'static str {
    match status {
        MemoryStatus::Active => "active",
        MemoryStatus::Deleted => "deleted",
        MemoryStatus::All => "all",
    }
}

fn print_memory(item: &Memory) {
    let deleted = item
        .deleted_at
        .map(|value| {
            format!(
                " deleted={}",
                value.to_rfc3339_opts(SecondsFormat::Secs, true)
            )
        })
        .unwrap_or_default();
    println!(
        "Memory ID {} [{}/{}] revision {} observed={} updated={}{}\n{}",
        item.id,
        kind_name(item.kind),
        status_name(item.status),
        item.revision_number,
        item.observed_at.to_rfc3339_opts(SecondsFormat::Secs, true),
        item.updated_at.to_rfc3339_opts(SecondsFormat::Secs, true),
        deleted,
        item.content
    );
}

fn parse_duration(value: &str) -> Result<Duration, String> {
    if let Some(seconds) = value.strip_suffix('s') {
        return seconds
            .parse::<u64>()
            .map(Duration::from_secs)
            .map_err(|_| "invalid --timeout".into());
    }
    if let Some(minutes) = value.strip_suffix('m') {
        return minutes
            .parse::<u64>()
            .ok()
            .and_then(|value| value.checked_mul(60))
            .map(Duration::from_secs)
            .ok_or_else(|| "invalid --timeout".into());
    }
    value
        .parse::<u64>()
        .map(Duration::from_secs)
        .map_err(|_| "invalid --timeout".into())
}

#[cfg(test)]
mod tests {
    use super::*;
    use clap::{CommandFactory, error::ErrorKind};

    #[test]
    fn clap_command_tree_is_valid() {
        Cli::command().debug_assert();
    }

    #[test]
    fn clap_parser_rejects_duplicates_missing_values_and_unknown_commands() {
        assert!(Cli::try_parse_from(["openclaw", "trace", "show", "--id"]).is_err());
        assert!(
            Cli::try_parse_from([
                "openclaw",
                "trace",
                "show",
                "--database",
                "state.sqlite",
                "--id",
                "1",
                "--id",
                "2"
            ])
            .is_err()
        );
        assert!(Cli::try_parse_from(["openclaw", "unknown"]).is_err());
    }

    #[test]
    fn clap_parser_preserves_chat_arguments_and_defaults() {
        let parsed = Cli::try_parse_from([
            "openclaw",
            "chat",
            "--json",
            "--sender-id",
            "debugging",
            "hello",
            "there",
        ])
        .unwrap();
        let Some(Command::Chat(chat)) = parsed.command else {
            panic!("expected chat command");
        };
        assert!(chat.json);
        assert_eq!(chat.sender_id, "debugging");
        assert_eq!(chat.message, ["hello", "there"]);
        assert_eq!(chat.timeout, Duration::from_secs(10 * 60));
    }

    #[test]
    fn clap_parser_supports_server_default_help_and_hidden_state_check() {
        assert!(Cli::try_parse_from(["openclaw"]).unwrap().command.is_none());
        let help = Cli::try_parse_from(["openclaw", "memory", "--help"]).unwrap_err();
        assert_eq!(help.kind(), ErrorKind::DisplayHelp);
        let parsed =
            Cli::try_parse_from(["openclaw", "--check-state", "/tmp/openclaw-state.sqlite"])
                .unwrap();
        assert_eq!(
            parsed.check_state.as_deref(),
            Some(Path::new("/tmp/openclaw-state.sqlite"))
        );
    }
}
