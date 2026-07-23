package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config represents the canonical openclaw.json structure
type Config struct {
	Agents   AgentsConfig   `json:"agents"`
	Channels ChannelsConfig `json:"channels"`
	Models   ModelsConfig   `json:"models"`
	Plugins  PluginsConfig  `json:"plugins"`
}

type AgentsConfig struct {
	Defaults AgentDefaults `json:"defaults"`
}

type AgentDefaults struct {
	Soul     string `json:"soul"`
	Identity string `json:"identity"`
	Model    struct {
		Primary string `json:"primary"`
	} `json:"model"`
}

type ChannelsConfig struct {
	Telegram ChannelEntry `json:"telegram"`
	WhatsApp ChannelEntry `json:"whatsapp"`
	Discord  ChannelEntry `json:"discord"`
}

type ChannelEntry struct {
	Enabled bool `json:"enabled"`
}

type ModelsConfig struct {
	Providers ProvidersConfig `json:"providers"`
}

type ProvidersConfig struct {
	OpenAI OpenAIProviderConfig `json:"openai"`
}

type OpenAIProviderConfig struct {
	BaseURL string   `json:"baseUrl"`
	Models  []string `json:"models"`
}

type PluginsConfig struct {
	Enabled bool                   `json:"enabled"`
	Entries map[string]PluginEntry `json:"entries"`
}

type PluginEntry struct {
	Enabled bool `json:"enabled"`
}

// Secrets represents the separate secrets configuration file
type Secrets struct {
	Models struct {
		Providers struct {
			OpenAI struct {
				APIKey string `json:"apiKey"`
			} `json:"openai"`
		} `json:"providers"`
	} `json:"models"`
	Channels struct {
		Telegram struct {
			BotToken string `json:"botToken"`
		} `json:"telegram"`
		Discord struct {
			BotToken string `json:"botToken"`
		} `json:"discord"`
	} `json:"channels"`
}

// LoadConfig reads the public openclaw.json file
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config JSON: %w", err)
	}
	cfg.Agents.Defaults.Soul = strings.TrimSpace(cfg.Agents.Defaults.Soul)
	if cfg.Agents.Defaults.Soul == "" {
		return nil, fmt.Errorf("agents.defaults.soul must be a non-empty string")
	}
	cfg.Agents.Defaults.Identity = strings.TrimSpace(cfg.Agents.Defaults.Identity)
	if cfg.Agents.Defaults.Identity == "" {
		return nil, fmt.Errorf("agents.defaults.identity must be a non-empty string")
	}

	return &cfg, nil
}

// LoadSecrets reads the private secrets file
func LoadSecrets(path string) (*Secrets, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read secrets file: %w", err)
	}

	var sec Secrets
	if err := json.Unmarshal(data, &sec); err != nil {
		return nil, fmt.Errorf("failed to parse secrets JSON: %w", err)
	}

	return &sec, nil
}
