package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"strings"
)

// Config represents the canonical openclaw.json structure
type Config struct {
	Agents   AgentsConfig   `json:"agents"`
	Channels ChannelsConfig `json:"channels"`
	Models   ModelsConfig   `json:"models"`
}

type AgentsConfig struct {
	Defaults AgentDefaults `json:"defaults"`
}

type AgentDefaults struct {
	HistorySearch struct {
		MinScore float64 `json:"minScore"`
	} `json:"historySearch"`
}

type ChannelsConfig struct {
	Telegram ChannelEntry `json:"telegram"`
}

type ChannelEntry struct {
	Enabled bool `json:"enabled"`
}

type ModelsConfig struct {
	Providers  ProvidersConfig `json:"providers"`
	Embeddings EmbeddingConfig `json:"embeddings"`
}

type EmbeddingConfig struct {
	BaseURL    string `json:"baseUrl"`
	Model      string `json:"model"`
	IndexID    string `json:"indexId"`
	Dimensions int    `json:"dimensions"`
}

type ProvidersConfig struct {
	OpenAI OpenAIProviderConfig `json:"openai"`
}

type OpenAIProviderConfig struct {
	BaseURL string `json:"baseUrl"`
}

// Secrets represents the separate secrets configuration file
type Secrets struct {
	Models struct {
		Providers struct {
			OpenAI struct {
				APIKey string `json:"apiKey"`
			} `json:"openai"`
		} `json:"providers"`
		Embeddings struct {
			APIKey string `json:"apiKey"`
		} `json:"embeddings"`
	} `json:"models"`
	Channels struct {
		Telegram struct {
			BotToken string `json:"botToken"`
		} `json:"telegram"`
	} `json:"channels"`
}

// LoadConfig reads the public openclaw.json file
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := decodeStrictJSON(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config JSON: %w", err)
	}
	if cfg.Agents.Defaults.HistorySearch.MinScore == 0 {
		cfg.Agents.Defaults.HistorySearch.MinScore = 0.35
	}
	if math.IsNaN(cfg.Agents.Defaults.HistorySearch.MinScore) ||
		math.IsInf(cfg.Agents.Defaults.HistorySearch.MinScore, 0) ||
		cfg.Agents.Defaults.HistorySearch.MinScore < 0 ||
		cfg.Agents.Defaults.HistorySearch.MinScore > 1 {
		return nil, fmt.Errorf("agents.defaults.historySearch.minScore must be between 0 and 1")
	}
	cfg.Models.Embeddings.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.Models.Embeddings.BaseURL), "/")
	if cfg.Models.Embeddings.BaseURL == "" {
		return nil, fmt.Errorf("models.embeddings.baseUrl must be a non-empty local llama-server URL")
	}
	parsedURL, err := url.Parse(cfg.Models.Embeddings.BaseURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		return nil, fmt.Errorf("models.embeddings.baseUrl must be an absolute HTTP URL")
	}
	cfg.Models.Embeddings.Model = strings.TrimSpace(cfg.Models.Embeddings.Model)
	if cfg.Models.Embeddings.Model == "" {
		return nil, fmt.Errorf("models.embeddings.model must be a non-empty string")
	}
	cfg.Models.Embeddings.IndexID = strings.TrimSpace(cfg.Models.Embeddings.IndexID)
	if cfg.Models.Embeddings.IndexID == "" {
		return nil, fmt.Errorf("models.embeddings.indexId must be a non-empty stable model identifier")
	}
	if cfg.Models.Embeddings.Dimensions <= 0 {
		return nil, fmt.Errorf("models.embeddings.dimensions must be positive")
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
	if err := decodeStrictJSON(data, &sec); err != nil {
		return nil, fmt.Errorf("failed to parse secrets JSON: %w", err)
	}

	return &sec, nil
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}
