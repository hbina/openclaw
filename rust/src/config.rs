use std::{fs::File, io, path::Path};

use chrono_tz::Tz;
use serde::Deserialize;
use url::Url;

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Config {
    pub agents: AgentsConfig,
    pub channels: ChannelsConfig,
    pub models: ModelsConfig,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct AgentsConfig {
    pub defaults: AgentDefaults,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct AgentDefaults {
    #[serde(rename = "historySearch")]
    pub history_search: HistorySearchConfig,
    #[serde(rename = "memoryMaintenance")]
    pub memory_maintenance: MaintenanceConfig,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct HistorySearchConfig {
    #[serde(rename = "minScore")]
    pub min_score: f64,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct MaintenanceConfig {
    pub enabled: bool,
    pub schedule: String,
    pub timezone: String,
    #[serde(rename = "batchSize")]
    pub batch_size: i32,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct ChannelsConfig {
    pub telegram: ChannelEntry,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct ChannelEntry {
    pub enabled: bool,
    #[serde(rename = "ownerUserId")]
    pub owner_user_id: String,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct ModelsConfig {
    pub providers: ProvidersConfig,
    pub embeddings: EmbeddingConfig,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct ProvidersConfig {
    pub openai: OpenAiProviderConfig,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct OpenAiProviderConfig {
    #[serde(rename = "baseUrl")]
    pub base_url: String,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct EmbeddingConfig {
    #[serde(rename = "baseUrl")]
    pub base_url: String,
    pub model: String,
    #[serde(rename = "indexId")]
    pub index_id: String,
    pub dimensions: i32,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Secrets {
    pub models: SecretModels,
    pub channels: SecretChannels,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct SecretModels {
    pub providers: SecretProviders,
    pub embeddings: SecretEmbedding,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct SecretProviders {
    pub openai: SecretOpenAi,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct SecretOpenAi {
    #[serde(rename = "apiKey")]
    pub api_key: String,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct SecretEmbedding {
    #[serde(rename = "apiKey")]
    pub api_key: String,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct SecretChannels {
    pub telegram: SecretTelegram,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct SecretTelegram {
    #[serde(rename = "botToken")]
    pub bot_token: String,
}

impl Config {
    pub fn load(path: impl AsRef<Path>) -> io::Result<Self> {
        let file = File::open(path)?;
        let mut config: Self = strict_json(file)?;
        config.validate().map_err(io::Error::other)?;
        Ok(config)
    }

    pub fn validate(&mut self) -> Result<(), String> {
        if self.agents.defaults.history_search.min_score == 0.0 {
            self.agents.defaults.history_search.min_score = 0.35;
        }
        let score = self.agents.defaults.history_search.min_score;
        if !score.is_finite() || !(0.0..=1.0).contains(&score) {
            return Err("agents.defaults.historySearch.minScore must be between 0 and 1".into());
        }

        let telegram = &mut self.channels.telegram;
        telegram.owner_user_id = telegram.owner_user_id.trim().to_owned();
        if telegram.enabled
            && telegram
                .owner_user_id
                .parse::<i64>()
                .ok()
                .filter(|id| *id > 0)
                .is_none()
        {
            return Err("channels.telegram.ownerUserId must be one positive numeric Telegram user ID when Telegram is enabled".into());
        }

        let maintenance = &mut self.agents.defaults.memory_maintenance;
        maintenance.schedule = maintenance.schedule.trim().to_owned();
        if maintenance.schedule.is_empty() {
            maintenance.schedule = "0 3 * * *".into();
        }
        crate::state::validate_standard_cron(&maintenance.schedule).map_err(|reason| {
            format!("agents.defaults.memoryMaintenance.schedule must be a five-field cron expression: {reason}")
        })?;
        maintenance.timezone = maintenance.timezone.trim().to_owned();
        if maintenance.timezone.is_empty() {
            maintenance.timezone = "Local".into();
        }
        if maintenance.timezone != "Local" && maintenance.timezone.parse::<Tz>().is_err() {
            return Err(
                "agents.defaults.memoryMaintenance.timezone must be an IANA timezone".into(),
            );
        }
        if maintenance.batch_size == 0 {
            maintenance.batch_size = 24;
        }
        if !(1..=100).contains(&maintenance.batch_size) {
            return Err(
                "agents.defaults.memoryMaintenance.batchSize must be between 1 and 100".into(),
            );
        }
        if maintenance.enabled && !telegram.enabled {
            return Err(
                "agents.defaults.memoryMaintenance requires the admitted Telegram owner channel"
                    .into(),
            );
        }

        let embeddings = &mut self.models.embeddings;
        embeddings.base_url = embeddings.base_url.trim().trim_end_matches('/').to_owned();
        if embeddings.base_url.is_empty() {
            return Err(
                "models.embeddings.baseUrl must be a non-empty local llama-server URL".into(),
            );
        }
        let url = Url::parse(&embeddings.base_url)
            .map_err(|_| "models.embeddings.baseUrl must be an absolute HTTP URL")?;
        if !matches!(url.scheme(), "http" | "https") || url.host_str().is_none() {
            return Err("models.embeddings.baseUrl must be an absolute HTTP URL".into());
        }
        embeddings.model = embeddings.model.trim().to_owned();
        if embeddings.model.is_empty() {
            return Err("models.embeddings.model must be a non-empty string".into());
        }
        embeddings.index_id = embeddings.index_id.trim().to_owned();
        if embeddings.index_id.is_empty() {
            return Err(
                "models.embeddings.indexId must be a non-empty stable model identifier".into(),
            );
        }
        if embeddings.dimensions <= 0 {
            return Err("models.embeddings.dimensions must be positive".into());
        }
        Ok(())
    }
}

impl Secrets {
    pub fn load(path: impl AsRef<Path>) -> io::Result<Self> {
        strict_json(File::open(path)?)
    }
}

fn strict_json<T: for<'de> Deserialize<'de>>(reader: impl io::Read) -> io::Result<T> {
    let mut deserializer = serde_json::Deserializer::from_reader(reader);
    let value = T::deserialize(&mut deserializer).map_err(io::Error::other)?;
    deserializer.end().map_err(io::Error::other)?;
    Ok(value)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn config(json: &str) -> Result<Config, String> {
        let mut value: Config = serde_json::from_str(json).map_err(|e| e.to_string())?;
        value.validate()?;
        Ok(value)
    }

    const EMBEDDINGS: &str = r#""embeddings":{"baseUrl":"http://127.0.0.1:8081/v1","model":"default","indexId":"id","dimensions":768}"#;

    #[test]
    fn loads_and_defaults_retained_configuration() {
        let value = config(&format!(r#"{{"models":{{{EMBEDDINGS}}}}}"#)).unwrap();
        assert_eq!(value.agents.defaults.history_search.min_score, 0.35);
        assert_eq!(
            value.agents.defaults.memory_maintenance.schedule,
            "0 3 * * *"
        );
        assert_eq!(value.agents.defaults.memory_maintenance.timezone, "Local");
        assert_eq!(value.agents.defaults.memory_maintenance.batch_size, 24);
    }

    #[test]
    fn validates_owner_and_embedding_contracts() {
        let error = config(&format!(
            r#"{{"channels":{{"telegram":{{"enabled":true,"ownerUserId":"someone"}}}},"models":{{{EMBEDDINGS}}}}}"#
        ))
        .unwrap_err();
        assert!(error.contains("ownerUserId"));

        let error = config(r#"{"models":{"embeddings":{"baseUrl":"/v1","model":"m","indexId":"id","dimensions":3}}}"#)
            .unwrap_err();
        assert!(error.contains("absolute HTTP URL"));
    }

    #[test]
    fn rejects_removed_fields() {
        let error = serde_json::from_str::<Config>(&format!(
            r#"{{"plugins":{{"enabled":true}},"models":{{{EMBEDDINGS}}}}}"#
        ))
        .unwrap_err();
        assert!(error.to_string().contains("unknown field"));
    }

    #[test]
    fn validates_maintenance_settings() {
        let value = config(&format!(r#"{{"agents":{{"defaults":{{"memoryMaintenance":{{"schedule":"15 2 * * *","timezone":"Asia/Kuala_Lumpur","batchSize":12}}}}}},"models":{{{EMBEDDINGS}}}}}"#)).unwrap();
        assert_eq!(value.agents.defaults.memory_maintenance.batch_size, 12);
        for maintenance in [
            r#"{"schedule":"sometimes"}"#,
            r#"{"timezone":"Moon/Base"}"#,
            r#"{"batchSize":101}"#,
        ] {
            let body = format!(
                r#"{{"agents":{{"defaults":{{"memoryMaintenance":{maintenance}}}}},"models":{{{EMBEDDINGS}}}}}"#
            );
            assert!(config(&body).is_err(), "accepted {maintenance}");
        }
    }
}
