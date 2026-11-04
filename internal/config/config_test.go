package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	tempDir := t.TempDir()
	configFile := filepath.Join(tempDir, "config.yaml")

	yamlContent := `
telegram:
  bot_token: "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11"
  allowed_user_id: 987654321

projects:
  coachly:
    path: "/tmp/test-coachly"
    default_model: "gemini-3.8-flash-high"
    auto_approve: false

models:
  - id: "gemini-3.8-flash-high"
    display_name: "Gemini 3.8 Flash (High)"
  - id: "claude-sonnet-4-6"
    display_name: "Claude Sonnet 4.6"

defaults:
  project: "coachly"
  verbosity: "normal"
`
	if err := os.WriteFile(configFile, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := Load(configFile)
	if err != nil {
		t.Fatalf("unexpected error loading config: %v", err)
	}

	if cfg.Telegram.BotToken != "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11" {
		t.Errorf("expected bot token 123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11, got %s", cfg.Telegram.BotToken)
	}
	if cfg.Telegram.AllowedUserID != 987654321 {
		t.Errorf("expected allowed user id 987654321, got %d", cfg.Telegram.AllowedUserID)
	}
	if p, ok := cfg.Projects["coachly"]; !ok || p.Path != "/tmp/test-coachly" {
		t.Errorf("expected project coachly path /tmp/test-coachly, got %+v", p)
	}
	if cfg.Limits.CardDebounceSeconds != 3 {
		t.Errorf("expected default CardDebounceSeconds to be 3, got %d", cfg.Limits.CardDebounceSeconds)
	}
	if cfg.Defaults.Verbosity != "normal" {
		t.Errorf("expected verbosity 'normal', got %s", cfg.Defaults.Verbosity)
	}
}

func TestLoadConfigValidation(t *testing.T) {
	tempDir := t.TempDir()
	configFile := filepath.Join(tempDir, "invalid.yaml")

	// Missing bot_token
	yamlContent := `
telegram:
  allowed_user_id: 12345
projects:
  test:
    path: "/tmp"
`
	if err := os.WriteFile(configFile, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, err := Load(configFile)
	if err == nil {
		t.Fatalf("expected error due to missing bot_token, got nil")
	}
}
