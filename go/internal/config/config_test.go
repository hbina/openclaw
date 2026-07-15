package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	path := writeTestFile(t, "openclaw.json", `{
		"agents":{"defaults":{"model":{"primary":"openai/gpt-5.5"}}},
		"channels":{"telegram":{"enabled":true},"whatsapp":{"enabled":false},"discord":{"enabled":true}},
		"models":{"providers":{"openai":{"baseUrl":"http://127.0.0.1:8080/v1","models":["default"]}}},
		"plugins":{"enabled":true,"entries":{"memory-core":{"enabled":true}}}
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.Agents.Defaults.Model.Primary; got != "openai/gpt-5.5" {
		t.Fatalf("primary model = %q", got)
	}
	if !cfg.Channels.Telegram.Enabled || !cfg.Channels.Discord.Enabled {
		t.Fatal("expected retained channels to be enabled")
	}
	if got := cfg.Models.Providers.OpenAI.BaseURL; got != "http://127.0.0.1:8080/v1" {
		t.Fatalf("OpenAI base URL = %q", got)
	}
	if !cfg.Plugins.Entries["memory-core"].Enabled {
		t.Fatal("expected memory-core plugin to be enabled")
	}
}

func TestLoadSecrets(t *testing.T) {
	path := writeTestFile(t, "secrets.json", `{
		"models":{"providers":{"openai":{"apiKey":"local-key"}}},
		"channels":{"telegram":{"botToken":"telegram-token"},"discord":{"botToken":"discord-token"}}
	}`)

	secrets, err := LoadSecrets(path)
	if err != nil {
		t.Fatalf("LoadSecrets: %v", err)
	}
	if secrets.Models.Providers.OpenAI.APIKey != "local-key" {
		t.Fatal("OpenAI key was not loaded")
	}
	if secrets.Channels.Telegram.BotToken != "telegram-token" {
		t.Fatal("Telegram token was not loaded")
	}
}

func TestLoadConfigRejectsInvalidJSON(t *testing.T) {
	path := writeTestFile(t, "openclaw.json", `{`)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected invalid JSON error")
	}
}
