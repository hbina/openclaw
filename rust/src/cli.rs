use std::{collections::HashMap, io::Read, path::Path, sync::Arc, time::Duration};

use chrono::{DateTime, SecondsFormat, Utc};
use serde::Deserialize;
use serde_json::json;
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

pub async fn run(arguments: &[String]) -> Result<bool, Box<dyn std::error::Error>> {
    let Some(command) = arguments.first().map(String::as_str) else {
        return Ok(false);
    };
    match command {
        "chat" => chat(&arguments[1..]).await?,
        "health" => health(&arguments[1..]).await?,
        "trace" => trace(&arguments[1..])?,
        "memory" => memory(&arguments[1..]).await?,
        _ => return Ok(false),
    }
    Ok(true)
}

async fn health(arguments: &[String]) -> Result<(), Box<dyn std::error::Error>> {
    let parsed = ParsedArgs::parse(arguments, &[])?;
    parsed.reject_unknown(&["url", "timeout"])?;
    if !parsed.positionals.is_empty() {
        return Err("health does not accept positional arguments".into());
    }
    let base = parsed
        .option("url")
        .map(str::to_owned)
        .or_else(|| std::env::var("OPENCLAW_GATEWAY_URL").ok())
        .unwrap_or_else(|| "http://127.0.0.1:18789".into());
    let endpoint = gateway_endpoint(&base, "healthz")?;
    let timeout = parsed
        .option("timeout")
        .map(parse_duration)
        .transpose()?
        .unwrap_or(Duration::from_secs(5));
    let response = reqwest::Client::builder()
        .timeout(timeout)
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

struct ParsedArgs {
    options: HashMap<String, String>,
    switches: Vec<String>,
    positionals: Vec<String>,
}

impl ParsedArgs {
    fn parse(arguments: &[String], boolean_flags: &[&str]) -> Result<Self, String> {
        let mut result = Self {
            options: HashMap::new(),
            switches: Vec::new(),
            positionals: Vec::new(),
        };
        let mut index = 0;
        while index < arguments.len() {
            let argument = &arguments[index];
            if !argument.starts_with("--") {
                result.positionals.push(argument.clone());
                index += 1;
                continue;
            }
            let name = argument.trim_start_matches("--");
            if boolean_flags.contains(&name) {
                result.switches.push(name.into());
                index += 1;
                continue;
            }
            let value = arguments
                .get(index + 1)
                .filter(|value| !value.starts_with("--"))
                .ok_or_else(|| format!("--{name} requires a value"))?;
            if result.options.insert(name.into(), value.clone()).is_some() {
                return Err(format!("--{name} was provided more than once"));
            }
            index += 2;
        }
        Ok(result)
    }

    fn option(&self, name: &str) -> Option<&str> {
        self.options.get(name).map(String::as_str)
    }

    fn required(&self, name: &str) -> Result<&str, String> {
        self.option(name)
            .filter(|value| !value.trim().is_empty())
            .ok_or_else(|| format!("--{name} is required"))
    }

    fn enabled(&self, name: &str) -> bool {
        self.switches.iter().any(|item| item == name)
    }

    fn reject_unknown(&self, allowed: &[&str]) -> Result<(), String> {
        if let Some(name) = self
            .options
            .keys()
            .find(|name| !allowed.contains(&name.as_str()))
        {
            return Err(format!("unknown option --{name}"));
        }
        Ok(())
    }
}

async fn chat(arguments: &[String]) -> Result<(), Box<dyn std::error::Error>> {
    let parsed = ParsedArgs::parse(arguments, &["json"])?;
    parsed.reject_unknown(&["url", "sender-id", "timeout"])?;
    let sender = parsed.option("sender-id").unwrap_or("cli-user").trim();
    if sender.is_empty() {
        return Err("--sender-id must not be empty".into());
    }
    let message = if parsed.positionals.is_empty() {
        let mut input = String::new();
        std::io::stdin().read_to_string(&mut input)?;
        input.trim().to_owned()
    } else {
        parsed.positionals.join(" ").trim().to_owned()
    };
    if message.is_empty() {
        return Err("message is required as arguments or stdin".into());
    }
    let base = parsed
        .option("url")
        .map(str::to_owned)
        .or_else(|| std::env::var("OPENCLAW_GATEWAY_URL").ok())
        .unwrap_or_else(|| "http://127.0.0.1:18789".into());
    let endpoint = gateway_endpoint(&base, "chat")?;
    let timeout = parsed
        .option("timeout")
        .map(parse_duration)
        .transpose()?
        .unwrap_or(Duration::from_secs(10 * 60));
    let mut response = reqwest::Client::builder()
        .timeout(timeout)
        .build()?
        .post(endpoint)
        .json(&json!({"sender_id":sender,"message":message}))
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
    if parsed.enabled("json") {
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

fn trace(arguments: &[String]) -> Result<(), Box<dyn std::error::Error>> {
    let (command, rest) = arguments
        .split_first()
        .ok_or("usage: openclaw trace list|show [options]")?;
    let parsed = ParsedArgs::parse(rest, &["json"])?;
    match command.as_str() {
        "list" => {
            parsed.reject_unknown(&[
                "database",
                "limit",
                "channel",
                "sender",
                "status",
                "message-id",
                "since",
            ])?;
            let limit = parsed.option("limit").unwrap_or("20").parse::<usize>()?;
            if !(1..=1000).contains(&limit) {
                return Err("--limit must be between 1 and 1000".into());
            }
            let since = parsed
                .option("since")
                .map(DateTime::parse_from_rfc3339)
                .transpose()?
                .map(|value| value.with_timezone(&Utc));
            let store = Store::open_read_only(parsed.required("database")?)?;
            let items = store.list_response_traces(&TraceFilter {
                limit,
                channel_id: parsed.option("channel").unwrap_or("").into(),
                sender_id: parsed.option("sender").unwrap_or("").into(),
                status: parsed.option("status").unwrap_or("").into(),
                external_message_id: parsed.option("message-id").unwrap_or("").into(),
                since,
            })?;
            if parsed.enabled("json") {
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
        "show" => {
            parsed.reject_unknown(&["database", "id"])?;
            let id = parsed.required("id")?.parse::<i64>()?;
            if id < 1 {
                return Err("--id must be positive".into());
            }
            let store = Store::open_read_only(parsed.required("database")?)?;
            let report = store.get_trace_report(id)?;
            let value = json!({"trace":report.trace,"events":report.events});
            if parsed.enabled("json") {
                println!("{}", serde_json::to_string_pretty(&value)?);
            } else {
                println!("Trace {id} ({})", value["trace"]["status"]);
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
        other => return Err(format!("unknown trace command {other:?}").into()),
    }
    Ok(())
}

async fn memory(arguments: &[String]) -> Result<(), Box<dyn std::error::Error>> {
    let (command, rest) = arguments.split_first().ok_or(
        "usage: openclaw memory <status|add|list|get|search|update|remove|reindex|maintenance>",
    )?;
    if command == "maintenance" {
        return memory_maintenance(rest).await;
    }
    let parsed = ParsedArgs::parse(rest, &[])?;
    match command.as_str() {
        "status" => {
            parsed.reject_unknown(&["database"])?;
            let store = open_store(&parsed)?;
            let (active, deleted) = store.memory_counts()?;
            println!("Memory ledger: {active} active, {deleted} deleted");
        }
        "list" => {
            parsed.reject_unknown(&["database", "kind", "status", "limit"])?;
            let store = open_store(&parsed)?;
            let status = parse_status(parsed.option("status").unwrap_or("active"))?;
            let kind = parsed.option("kind").map(parse_kind).transpose()?;
            let limit = parsed.option("limit").unwrap_or("20").parse()?;
            for item in store.list_memories(MemoryFilter {
                kind,
                status,
                limit,
            })? {
                print_memory(&item);
            }
        }
        "get" => {
            parsed.reject_unknown(&["database", "id"])?;
            let store = open_store(&parsed)?;
            print_memory(&store.get_memory(parsed.required("id")?.parse()?)?);
        }
        "remove" => {
            parsed.reject_unknown(&["database", "id"])?;
            let store = open_store(&parsed)?;
            let id = parsed.required("id")?.parse()?;
            let item = store.with_tx(|tx| tx.remove_memory(id, Utc::now()))?;
            println!(
                "Memory ID {} is deleted and no longer recalled; revisions remain available.",
                item.id
            );
        }
        "add" | "update" | "search" | "reindex" => {
            configured_memory_command(command, &parsed).await?;
        }
        other => return Err(format!("unknown memory command {other:?}").into()),
    }
    Ok(())
}

async fn configured_memory_command(
    command: &str,
    parsed: &ParsedArgs,
) -> Result<(), Box<dyn std::error::Error>> {
    let (store, service, config, embedder) = configured_memory(parsed)?;
    match command {
        "add" => {
            parsed.reject_unknown(&["database", "config-dir", "kind", "content"])?;
            let write = service
                .prepare_write(
                    parse_kind(parsed.required("kind")?)?,
                    parsed.required("content")?,
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
        "update" => {
            parsed.reject_unknown(&["database", "config-dir", "id", "kind", "content"])?;
            let id = parsed.required("id")?.parse()?;
            let current = store.get_memory(id)?;
            let kind = parsed
                .option("kind")
                .map(parse_kind)
                .transpose()?
                .unwrap_or(current.kind);
            let write = service
                .prepare_write(kind, parsed.required("content")?, operator_provenance())
                .await?;
            print_memory(&store.with_tx(|tx| tx.update_memory(id, &write))?);
        }
        "search" => {
            parsed.reject_unknown(&["database", "config-dir", "query", "max-results"])?;
            let chat = configured_chat(parsed.required("config-dir")?, &config)?;
            let limit = parsed.option("max-results").unwrap_or("5").parse()?;
            for (index, item) in service
                .search_with_model(&chat, parsed.required("query")?, limit)
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
        "reindex" => {
            parsed.reject_unknown(&["database", "config-dir"])?;
            let memories = service.prepare_reindex().await?;
            let chat = Arc::new(configured_chat(parsed.required("config-dir")?, &config)?);
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
        _ => unreachable!(),
    }
    Ok(())
}

async fn memory_maintenance(arguments: &[String]) -> Result<(), Box<dyn std::error::Error>> {
    let (command, rest) = arguments
        .split_first()
        .ok_or("usage: openclaw memory maintenance <status|preview|run|candidates>")?;
    let parsed = ParsedArgs::parse(rest, &[])?;
    match command.as_str() {
        "status" => {
            parsed.reject_unknown(&["database"])?;
            let store = open_store(&parsed)?;
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
        "candidates" => {
            parsed.reject_unknown(&["database", "run-id"])?;
            let store = open_store(&parsed)?;
            let id = parsed.required("run-id")?.parse()?;
            for item in store.list_maintenance_candidates(id)? {
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
        "preview" | "run" => {
            parsed.reject_unknown(&["database", "config-dir"])?;
            let (store, service, config, _) = configured_memory(&parsed)?;
            let chat: Arc<dyn Provider> =
                Arc::new(configured_chat(parsed.required("config-dir")?, &config)?);
            let maintainer = Maintainer::new(
                Arc::clone(&store),
                service,
                chat,
                &config.channels.telegram.owner_user_id,
                config.agents.defaults.memory_maintenance.batch_size as usize,
                &config.agents.defaults.memory_maintenance.schedule,
                &config.agents.defaults.memory_maintenance.timezone,
            )?;
            let mode = if command == "preview" {
                MaintenanceMode::Preview
            } else {
                MaintenanceMode::Apply
            };
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
        }
        other => return Err(format!("unknown memory maintenance command {other:?}").into()),
    }
    Ok(())
}

type ConfiguredMemory = (Arc<Store>, Arc<Service>, Config, Arc<dyn Embedder>);

fn configured_memory(parsed: &ParsedArgs) -> Result<ConfiguredMemory, Box<dyn std::error::Error>> {
    let store = Arc::new(Store::new(parsed.required("database")?)?);
    let directory = parsed.required("config-dir")?;
    let config = Config::load(Path::new(directory).join("openclaw.json"))?;
    let secrets = Secrets::load(Path::new(directory).join("secrets.json")).unwrap_or_default();
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
    directory: &str,
    config: &Config,
) -> Result<OpenAiClient, Box<dyn std::error::Error>> {
    let secrets = Secrets::load(Path::new(directory).join("secrets.json")).unwrap_or_default();
    Ok(OpenAiClient::new(
        &secrets.models.providers.openai.api_key,
        &config.models.providers.openai.base_url,
    )?)
}

fn open_store(parsed: &ParsedArgs) -> Result<Store, Box<dyn std::error::Error>> {
    Ok(Store::new(parsed.required("database")?)?)
}

fn operator_provenance() -> Provenance {
    Provenance {
        origin: MemoryOrigin::Owner,
        source: MemorySource::Operator,
        source_history_id: None,
        source_trace_id: None,
    }
}

fn parse_kind(value: &str) -> Result<MemoryKind, String> {
    match value {
        "profile" => Ok(MemoryKind::Profile),
        "durable" => Ok(MemoryKind::Durable),
        "daily" => Ok(MemoryKind::Daily),
        _ => Err("invalid --kind".into()),
    }
}

fn parse_status(value: &str) -> Result<MemoryStatus, String> {
    match value {
        "active" => Ok(MemoryStatus::Active),
        "deleted" => Ok(MemoryStatus::Deleted),
        "all" => Ok(MemoryStatus::All),
        _ => Err("invalid --status".into()),
    }
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
            .map(|value| Duration::from_secs(value * 60))
            .map_err(|_| "invalid --timeout".into());
    }
    value
        .parse::<u64>()
        .map(Duration::from_secs)
        .map_err(|_| "invalid --timeout".into())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn argument_parser_rejects_duplicates_and_missing_values() {
        assert!(ParsedArgs::parse(&["--id".into()], &[]).is_err());
        assert!(
            ParsedArgs::parse(&["--id".into(), "1".into(), "--id".into(), "2".into()], &[])
                .is_err()
        );
        let parsed = ParsedArgs::parse(&["--json".into(), "hello".into()], &["json"]).unwrap();
        assert!(parsed.enabled("json"));
        assert_eq!(parsed.positionals, vec!["hello"]);
    }
}
