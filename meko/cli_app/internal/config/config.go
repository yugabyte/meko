package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	LogDir string `yaml:"log_dir"`
}

// Credentials holds the tokens and metadata persisted after a
// successful `meko login`.
type Credentials struct {
	AccessToken  string    `json:"access_token"`
	IDToken      string    `json:"id_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresIn    int32     `json:"expires_in"`
	Email        string    `json:"email"`
	APIBaseURL   string    `json:"api_base_url"`
	LoggedInAt   time.Time `json:"logged_in_at"`
}

// Dir returns the meko home directory: ~/var/meko
func Dir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, "var", "meko")
}

func configPath() string {
	return filepath.Join(Dir(), "config.yaml")
}

func credentialsPath() string {
	return filepath.Join(Dir(), "credentials.json")
}

// Load reads the config from ~/var/meko/config.yaml.
// Returns a zero-value Config (not an error) when the file doesn't exist yet.
func Load() (*Config, error) {
	cfg := &Config{}

	data, err := os.ReadFile(configPath())
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config %s: %w", configPath(), err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", configPath(), err)
	}
	return cfg, nil
}

// Save writes the config to ~/var/meko/config.yaml, creating the
// directory if it doesn't exist.
func (c *Config) Save() error {
	if err := os.MkdirAll(Dir(), 0755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("serialize config: %w", err)
	}

	if err := os.WriteFile(configPath(), data, 0644); err != nil {
		return fmt.Errorf("write config %s: %w", configPath(), err)
	}
	return nil
}

// SaveCredentials writes tokens to ~/var/meko/credentials.json with
// restrictive permissions (0600).
func SaveCredentials(creds *Credentials) error {
	if err := os.MkdirAll(Dir(), 0755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize credentials: %w", err)
	}

	p := credentialsPath()
	if err := os.WriteFile(p, data, 0600); err != nil {
		return fmt.Errorf("write credentials %s: %w", p, err)
	}

	slog.Info("credentials saved", "path", p, "email", creds.Email)
	return nil
}

// LoadCredentials reads stored tokens from ~/var/meko/credentials.json.
// Returns nil (not an error) when the file does not exist.
func LoadCredentials() (*Credentials, error) {
	p := credentialsPath()
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read credentials %s: %w", p, err)
	}

	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("parse credentials %s: %w", p, err)
	}

	slog.Debug("credentials loaded", "email", creds.Email)
	return &creds, nil
}

// DeleteCredentials removes the credentials file. Returns nil if the
// file does not exist.
func DeleteCredentials() error {
	p := credentialsPath()
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove credentials %s: %w", p, err)
	}
	slog.Info("credentials deleted", "path", p)
	return nil
}
