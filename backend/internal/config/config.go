// Package config loads Meko Local's settings. Precedence: flags, then MEKO_LOCAL_* environment
// variables, then config.toml in the data folder, then defaults.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultPort is next to AgentK Local's 7777.
const DefaultPort = 7778

// Config is every setting.
type Config struct {
	Server             Server             `toml:"server"`
	Log                Log                `toml:"log"`
	Engine             Engine             `toml:"engine"`
	ConversationSearch ConversationSearch `toml:"conversation_search"`
}

// ConversationSearch is the [conversation_search] table.
type ConversationSearch struct {
	// MaxDistance is the farthest (cosine distance, 0 to 2) a past turn may be from the query to
	// be a hit. 0 means the default, tuned for the embedding model (internal/memory).
	MaxDistance float64 `toml:"max_distance"`
}

// Server is the [server] table.
type Server struct {
	Port int `toml:"port"`
}

// Engine is the [engine] table: where the memory engine and its Node are. Empty means the usual
// places (internal/engine).
type Engine struct {
	Node string `toml:"node"` // also MEKO_LOCAL_NODE
	Dir  string `toml:"dir"`  // the built engine; also MEKO_LOCAL_ENGINE_DIR
}

// Log is the [log] table.
type Log struct {
	Level string `toml:"level"`
}

// Default is the configuration with nothing set.
func Default() Config {
	return Config{Server: Server{Port: DefaultPort}, Log: Log{Level: "info"}}
}

// Overrides are settings given as flags; zero values mean unset.
type Overrides struct {
	Port     int
	LogLevel string
}

// Load reads config.toml at path (missing is fine), then applies env (os.Getenv in production)
// and the flag overrides.
func Load(path string, env func(string) string, o Overrides) (Config, error) {
	c := Default()
	md, err := toml.DecodeFile(path, &c)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return Config{}, fmt.Errorf("%s: %w", path, err)
	default:
		if extra := md.Undecoded(); len(extra) > 0 {
			keys := make([]string, len(extra))
			for i, k := range extra {
				keys[i] = k.String()
			}
			return Config{}, fmt.Errorf("%s: unknown setting %s", path, strings.Join(keys, ", "))
		}
	}
	if v := env("MEKO_LOCAL_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("MEKO_LOCAL_PORT=%q isn't a port number", v)
		}
		c.Server.Port = p
	}
	if v := env("MEKO_LOCAL_LOG_LEVEL"); v != "" {
		c.Log.Level = v
	}
	if v := env("MEKO_LOCAL_NODE"); v != "" {
		c.Engine.Node = v
	}
	if v := env("MEKO_LOCAL_ENGINE_DIR"); v != "" {
		c.Engine.Dir = v
	}
	if o.Port != 0 {
		c.Server.Port = o.Port
	}
	if o.LogLevel != "" {
		c.Log.Level = o.LogLevel
	}
	return c, c.validate()
}

func (c Config) validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("port %d is out of range: use 1 to 65535", c.Server.Port)
	}
	if _, err := c.Log.SlogLevel(); err != nil {
		return err
	}
	if d := c.ConversationSearch.MaxDistance; d < 0 || d > 2 {
		return fmt.Errorf("conversation_search.max_distance %g is out of range: use 0 to 2", d)
	}
	return nil
}

// SlogLevel turns the level name into a slog level.
func (l Log) SlogLevel() (slog.Level, error) {
	switch strings.ToLower(l.Level) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("log level %q isn't one of debug, info, warn, error", l.Level)
}
