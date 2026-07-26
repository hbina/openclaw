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
		"agents":{"defaults":{"soul":"  Be direct.  ","identity":"\n Jet the fox. \t"}},
		"channels":{"telegram":{"enabled":true}},
		"models":{
			"providers":{"openai":{"baseUrl":"http://127.0.0.1:8080/v1"}},
			"embeddings":{"baseUrl":"http://127.0.0.1:8081/v1","model":"default","indexId":"embeddinggemma-q8-v1","dimensions":768}
		}
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.Agents.Defaults.Soul; got != "Be direct." {
		t.Fatalf("soul = %q", got)
	}
	if got := cfg.Agents.Defaults.Identity; got != "Jet the fox." {
		t.Fatalf("identity = %q", got)
	}
	if !cfg.Channels.Telegram.Enabled {
		t.Fatal("expected Telegram to be enabled")
	}
	if got := cfg.Models.Providers.OpenAI.BaseURL; got != "http://127.0.0.1:8080/v1" {
		t.Fatalf("OpenAI base URL = %q", got)
	}
	if got := cfg.Models.Embeddings.IndexID; got != "embeddinggemma-q8-v1" {
		t.Fatalf("embedding index id = %q", got)
	}
	if got := cfg.Agents.Defaults.HistorySearch.MinScore; got != 0.35 {
		t.Fatalf("history search minimum score = %v", got)
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
		{name: "agent model", extra: `"agents":{"defaults":{"soul":"Be direct.","identity":"Jet","model":{"primary":"openai/default"}}}`},
		{name: "Discord", extra: `"channels":{"discord":{"enabled":true}}`},
		{name: "WhatsApp", extra: `"channels":{"whatsapp":{"enabled":true}}`},
		{name: "provider model list", extra: `"models":{"providers":{"openai":{"models":["default"]}},"embeddings":{"baseUrl":"http://127.0.0.1:8081/v1","model":"default","indexId":"id","dimensions":768}}`},
		{name: "Anthropic", extra: `"models":{"providers":{"anthropic":{"baseUrl":"http://127.0.0.1"}},"embeddings":{"baseUrl":"http://127.0.0.1:8081/v1","model":"default","indexId":"id","dimensions":768}}`},
		{name: "plugins", extra: `"plugins":{"enabled":true}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := `{"agents":{"defaults":{"soul":"Be direct.","identity":"Jet"}},"models":{"embeddings":{"baseUrl":"http://127.0.0.1:8081/v1","model":"default","indexId":"id","dimensions":768}},` +
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

func TestLoadConfigRequiresPersona(t *testing.T) {
	tests := []struct {
		name      string
		persona   string
		wantError string
	}{
		{name: "missing soul", persona: `"identity":"Jet"`, wantError: "agents.defaults.soul"},
		{name: "empty soul", persona: `"soul":"","identity":"Jet"`, wantError: "agents.defaults.soul"},
		{name: "whitespace soul", persona: `"soul":"  \n\t","identity":"Jet"`, wantError: "agents.defaults.soul"},
		{name: "missing identity", persona: `"soul":"Be kind"`, wantError: "agents.defaults.identity"},
		{name: "empty identity", persona: `"soul":"Be kind","identity":""`, wantError: "agents.defaults.identity"},
		{name: "whitespace identity", persona: `"soul":"Be kind","identity":" \n\t "`, wantError: "agents.defaults.identity"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeTestFile(t, "openclaw.json", `{
				"agents":{"defaults":{`+test.persona+`}},
				"models":{"embeddings":{"baseUrl":"http://127.0.0.1:8081/v1","model":"default","indexId":"embeddinggemma-q8-v1","dimensions":768}}
			}`)
			_, err := LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("LoadConfig error = %v, want field %q", err, test.wantError)
			}
		})
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
				"agents":{"defaults":{"soul":"Be direct.","identity":"Jet"}},
				"models":{"embeddings":`+test.embeddings+`}
			}`)
			_, err := LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadConfig error = %v, want %q", err, test.want)
			}
		})
	}
}
