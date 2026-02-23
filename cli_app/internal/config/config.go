package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	LogDir string `yaml:"log_dir"`
}

// Dir returns the meko home directory: ~/var/meko
func Dir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, "var", "meko")
}

func path() string {
	return filepath.Join(Dir(), "config.yaml")
}

// Load reads the config from ~/var/meko/config.yaml.
// Returns a zero-value Config (not an error) when the file doesn't exist yet.
func Load() (*Config, error) {
	cfg := &Config{}

	data, err := os.ReadFile(path())
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config %s: %w", path(), err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path(), err)
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

	if err := os.WriteFile(path(), data, 0644); err != nil {
		return fmt.Errorf("write config %s: %w", path(), err)
	}
	return nil
}
