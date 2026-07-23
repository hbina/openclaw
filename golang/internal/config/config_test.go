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
		"agents":{"defaults":{"soul":"  Be direct.  ","identity":"\n Jet the fox. \t","model":{"primary":"openai/gpt-5.5"}}},
		"channels":{"telegram":{"enabled":true}},
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
	if !cfg.Plugins.Entries["memory-core"].Enabled {
		t.Fatal("expected memory-core plugin to be enabled")
	}
}

func TestLoadSecrets(t *testing.T) {
	path := writeTestFile(t, "secrets.json", `{
		"models":{"providers":{"openai":{"apiKey":"local-key"}}},
		"channels":{"telegram":{"botToken":"telegram-token"}}
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

func TestLoadConfigIgnoresRemovedChannelKeys(t *testing.T) {
	configPath := writeTestFile(t, "openclaw.json", `{
		"agents":{"defaults":{"soul":"Be direct.","identity":"Jet the fox."}},
		"channels":{
			"telegram":{"enabled":true},
			"discord":{"enabled":true},
			"whatsapp":{"enabled":true}
		}
	}`)
	if _, err := LoadConfig(configPath); err != nil {
		t.Fatalf("LoadConfig with removed channel keys: %v", err)
	}

	secretsPath := writeTestFile(t, "secrets.json", `{
		"channels":{
			"telegram":{"botToken":"telegram-token"},
			"discord":{"botToken":"ignored"},
			"whatsapp":{"session":"ignored"}
		}
	}`)
	secrets, err := LoadSecrets(secretsPath)
	if err != nil {
		t.Fatalf("LoadSecrets with removed channel keys: %v", err)
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
			path := writeTestFile(t, "openclaw.json", `{"agents":{"defaults":{`+test.persona+`}}}`)
			_, err := LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("LoadConfig error = %v, want field %q", err, test.wantError)
			}
		})
	}
}
