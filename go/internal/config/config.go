package config

import (
	"encoding/json"
	"fmt"
	"os"
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
	Model struct {
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
	OpenAI    OpenAIProviderConfig    `json:"openai"`
	Anthropic AnthropicProviderConfig `json:"anthropic"`
}

type OpenAIProviderConfig struct {
	BaseURL string   `json:"baseUrl"`
	Models  []string `json:"models"`
}

type AnthropicProviderConfig struct {
	Models []string `json:"models"`
}

type PluginsConfig struct {
	Enabled bool                    `json:"enabled"`
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
			Anthropic struct {
				APIKey string `json:"apiKey"`
			} `json:"anthropic"`
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
