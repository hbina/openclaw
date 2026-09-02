package config

import (
	"os"
	"path/filepath"
	"strings"
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
		"agents":{"defaults":{"historySearch":{"minScore":0.4}}},
			"channels":{"telegram":{"enabled":true,"ownerUserId":"123456789"}},
		"models":{
			"providers":{"openai":{"baseUrl":"http://127.0.0.1:8080/v1"}},
			"embeddings":{"baseUrl":"http://127.0.0.1:8081/v1","model":"default","indexId":"embeddinggemma-q8-v1","dimensions":768}
		}
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Channels.Telegram.Enabled {
		t.Fatal("expected Telegram to be enabled")
	}
	if cfg.Channels.Telegram.OwnerUserID != "123456789" {
		t.Fatalf("Telegram owner user ID = %q", cfg.Channels.Telegram.OwnerUserID)
	}
	if got := cfg.Models.Providers.OpenAI.BaseURL; got != "http://127.0.0.1:8080/v1" {
		t.Fatalf("OpenAI base URL = %q", got)
	}
	if got := cfg.Models.Embeddings.IndexID; got != "embeddinggemma-q8-v1" {
		t.Fatalf("embedding index id = %q", got)
	}
	if got := cfg.Agents.Defaults.HistorySearch.MinScore; got != 0.4 {
		t.Fatalf("history search minimum score = %v", got)
	}
}

func TestLoadConfigRequiresTelegramOwnerWhenEnabled(t *testing.T) {
	for _, owner := range []string{"", "someone", "0", "-1"} {
		t.Run(owner, func(t *testing.T) {
			path := writeTestFile(t, "openclaw.json", `{
				"channels":{"telegram":{"enabled":true,"ownerUserId":"`+owner+`"}},
				"models":{"embeddings":{"baseUrl":"http://127.0.0.1:8081/v1","model":"default","indexId":"id","dimensions":768}}
			}`)
			if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "ownerUserId") {
				t.Fatalf("LoadConfig error = %v, want ownerUserId validation", err)
			}
		})
	}
}

func TestLoadConfigMemoryMaintenance(t *testing.T) {
	path := writeTestFile(t, "openclaw.json", `{
		"agents":{"defaults":{"memoryMaintenance":{"enabled":true,"schedule":"15 2 * * *","timezone":"Asia/Kuala_Lumpur","batchSize":12}}},
		"channels":{"telegram":{"enabled":true,"ownerUserId":"100"}},
		"models":{"embeddings":{"baseUrl":"http://127.0.0.1:8081/v1","model":"default","indexId":"id","dimensions":768}}
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Agents.Defaults.MemoryMaintenance
	if !got.Enabled || got.Schedule != "15 2 * * *" || got.Timezone != "Asia/Kuala_Lumpur" || got.BatchSize != 12 {
		t.Fatalf("memory maintenance = %#v", got)
	}
}

func TestLoadConfigRejectsInvalidMemoryMaintenance(t *testing.T) {
	tests := []struct {
		name, value, want string
	}{
		{"schedule", `{"schedule":"sometimes"}`, "schedule"},
		{"timezone", `{"timezone":"Moon/Base"}`, "timezone"},
		{"batch", `{"batchSize":101}`, "batchSize"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeTestFile(t, "openclaw.json", `{
				"agents":{"defaults":{"memoryMaintenance":`+test.value+`}},
				"models":{"embeddings":{"baseUrl":"http://127.0.0.1:8081/v1","model":"default","indexId":"id","dimensions":768}}
			}`)
			if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadConfig error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadSecrets(t *testing.T) {
	path := writeTestFile(t, "secrets.json", `{
		"models":{"providers":{"openai":{"apiKey":"local-key"}},"embeddings":{"apiKey":"embedding-key"}},
		"channels":{"telegram":{"botToken":"telegram-token"}}
	}`)

	secrets, err := LoadSecrets(path)
	if err != nil {
		t.Fatalf("LoadSecrets: %v", err)
	}
	if secrets.Models.Providers.OpenAI.APIKey != "local-key" {
		t.Fatal("OpenAI key was not loaded")
	}
	if secrets.Models.Embeddings.APIKey != "embedding-key" {
		t.Fatal("embedding key was not loaded")
	}
	if secrets.Channels.Telegram.BotToken != "telegram-token" {
		t.Fatal("Telegram token was not loaded")
	}
}

func TestLoadConfigRejectsRemovedKeys(t *testing.T) {
	tests := []struct {
		name  string
		extra string
	}{
		{name: "soul", extra: `"agents":{"defaults":{"soul":"Be direct."}}`},
		{name: "identity", extra: `"agents":{"defaults":{"identity":"Jet"}}`},
		{name: "agent model", extra: `"agents":{"defaults":{"model":{"primary":"openai/default"}}}`},
		{name: "Discord", extra: `"channels":{"discord":{"enabled":true}}`},
		{name: "WhatsApp", extra: `"channels":{"whatsapp":{"enabled":true}}`},
		{name: "provider model list", extra: `"models":{"providers":{"openai":{"models":["default"]}},"embeddings":{"baseUrl":"http://127.0.0.1:8081/v1","model":"default","indexId":"id","dimensions":768}}`},
		{name: "Anthropic", extra: `"models":{"providers":{"anthropic":{"baseUrl":"http://127.0.0.1"}},"embeddings":{"baseUrl":"http://127.0.0.1:8081/v1","model":"default","indexId":"id","dimensions":768}}`},
		{name: "plugins", extra: `"plugins":{"enabled":true}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := `{"models":{"embeddings":{"baseUrl":"http://127.0.0.1:8081/v1","model":"default","indexId":"id","dimensions":768}},` +
				strings.TrimPrefix(test.extra, "{") + `}`
			path := writeTestFile(t, "openclaw.json", body)
			if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("LoadConfig error = %v, want unknown field", err)
			}
		})
	}
}

func TestLoadSecretsRejectsRemovedChannelKeys(t *testing.T) {
	path := writeTestFile(t, "secrets.json", `{
		"channels":{"telegram":{"botToken":"telegram-token"},"discord":{"botToken":"removed"}}
	}`)
	if _, err := LoadSecrets(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("LoadSecrets error = %v, want unknown field", err)
	}
}

func TestLoadConfigRejectsInvalidJSON(t *testing.T) {
	path := writeTestFile(t, "openclaw.json", `{`)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected invalid JSON error")
	}
}

func TestLoadConfigRequiresEmbeddingContract(t *testing.T) {
	tests := []struct {
		name       string
		embeddings string
		want       string
	}{
		{name: "missing URL", embeddings: `{"model":"default","indexId":"id","dimensions":768}`, want: "baseUrl"},
		{name: "relative URL", embeddings: `{"baseUrl":"/v1","model":"default","indexId":"id","dimensions":768}`, want: "absolute HTTP URL"},
		{name: "missing model", embeddings: `{"baseUrl":"http://127.0.0.1:8081/v1","indexId":"id","dimensions":768}`, want: ".model"},
		{name: "missing index id", embeddings: `{"baseUrl":"http://127.0.0.1:8081/v1","model":"default","dimensions":768}`, want: "indexId"},
		{name: "invalid dimensions", embeddings: `{"baseUrl":"http://127.0.0.1:8081/v1","model":"default","indexId":"id","dimensions":0}`, want: "dimensions"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeTestFile(t, "openclaw.json", `{
				"models":{"embeddings":`+test.embeddings+`}
			}`)
			_, err := LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadConfig error = %v, want %q", err, test.want)
			}
		})
	}
}
