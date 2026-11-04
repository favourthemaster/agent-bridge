package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Telegram TelegramConfig           `yaml:"telegram"`
	Projects map[string]ProjectConfig `yaml:"projects"`
	Models   []ModelConfig            `yaml:"models"`
	Limits   LimitsConfig             `yaml:"limits"`
	Storage  StorageConfig            `yaml:"storage"`
	Defaults DefaultsConfig           `yaml:"defaults"`
}

type TelegramConfig struct {
	BotToken      string `yaml:"bot_token"`
	AllowedUserID int64  `yaml:"allowed_user_id"`
}

type ProjectConfig struct {
	Path         string `yaml:"path"`
	DefaultModel string `yaml:"default_model"`
	AutoApprove  bool   `yaml:"auto_approve"`
}

type ModelConfig struct {
	ID          string `yaml:"id"`
	DisplayName string `yaml:"display_name"`
}

type LimitsConfig struct {
	MaxFileSizeMB          int `yaml:"max_file_size_mb"`
	InboxRetentionDays     int `yaml:"inbox_retention_days"`
	ApprovalTimeoutMinutes int `yaml:"approval_timeout_minutes"`
	CardDebounceSeconds    int `yaml:"card_debounce_seconds"`
}

type StorageConfig struct {
	DBPath string `yaml:"db_path"`
}

type DefaultsConfig struct {
	Project   string `yaml:"project"`
	Verbosity string `yaml:"verbosity"` // quiet, normal, verbose
}

// Load loads the configuration from a YAML file, merging environment variables.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	cfg := &Config{
		Limits: LimitsConfig{
			MaxFileSizeMB:          20,
			InboxRetentionDays:     7,
			ApprovalTimeoutMinutes: 10,
			CardDebounceSeconds:    3,
		},
		Defaults: DefaultsConfig{
			Verbosity: "normal",
		},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing YAML config: %w", err)
	}

	// Environment variable overrides
	if token := os.Getenv("AGENT_BRIDGE_TELEGRAM_TOKEN"); token != "" {
		cfg.Telegram.BotToken = token
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// Resolve storage DB path default
	if cfg.Storage.DBPath == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			cfg.Storage.DBPath = filepath.Join(home, ".config", "agent-bridge", "bridge.db")
		} else {
			cfg.Storage.DBPath = "bridge.db"
		}
	} else {
		cfg.Storage.DBPath = expandPath(cfg.Storage.DBPath)
	}

	// Resolve and clean project paths
	for name, p := range cfg.Projects {
		p.Path = expandPath(p.Path)
		cfg.Projects[name] = p
	}

	return cfg, nil
}

func (c *Config) Validate() error {
	if strings.TrimSpace(c.Telegram.BotToken) == "" {
		return fmt.Errorf("telegram.bot_token is required (or set AGENT_BRIDGE_TELEGRAM_TOKEN)")
	}
	if c.Telegram.AllowedUserID == 0 {
		return fmt.Errorf("telegram.allowed_user_id must be set to your numeric Telegram user ID")
	}
	if len(c.Projects) == 0 {
		return fmt.Errorf("at least one project must be configured under 'projects'")
	}
	for name, p := range c.Projects {
		if strings.TrimSpace(p.Path) == "" {
			return fmt.Errorf("project %q must have a non-empty path", name)
		}
	}
	v := strings.ToLower(c.Defaults.Verbosity)
	if v != "quiet" && v != "normal" && v != "verbose" {
		c.Defaults.Verbosity = "normal"
	}
	return nil
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return filepath.Clean(path)
}
