// Package config loads Meko's settings. Precedence: flags, then MEKO_* environment
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

// DefaultPort is next to AgentK's 7777.
const DefaultPort = 7778

// Config is every setting.
type Config struct {
	Server             Server             `toml:"server"`
	Log                Log                `toml:"log"`
	MemoryEngine       MemoryEngine       `toml:"memory_engine"`
	ConversationSearch ConversationSearch `toml:"conversation_search"`
	Secrets            Secrets            `toml:"secrets"`
	Update             Update             `toml:"update"`
	KnowledgeBase      KnowledgeBase      `toml:"knowledge_base"`
	WebUI              WebUI              `toml:"web_ui"`
}

// WebUI is the [web_ui] table: where the built web UI is. Empty means the usual place
// (internal/webui).
type WebUI struct {
	Dir string `toml:"dir"` // also MEKO_WEB_UI_DIR
}

// KnowledgeBase is the [knowledge_base] table.
type KnowledgeBase struct {
	// MaxDistance is the farthest (cosine distance, 0 to 2) a knowledge-base result may be from the
	// query for context_search to keep it. 0 means the default, tuned for the embedding model
	// (internal/memory).
	MaxDistance float64 `toml:"max_distance"`
}

// Update is the [update] table: how a release install updates itself.
type Update struct {
	// Channel is "stable" (the default) or "off" (no checks at all). Also MEKO_UPDATE_CHANNEL.
	Channel string `toml:"channel"`
	// Auto installs an update when one is found, while Meko runs as a service (default true). With
	// false, updates are only reported (meko status), and meko update installs them. Also
	// MEKO_UPDATE_AUTO.
	Auto bool `toml:"auto"`
	// IntervalHours is how often to check (default 6).
	IntervalHours float64 `toml:"interval_hours"`
	// Repo is the GitHub repo releases come from (default yugabyte/agent-k-local). Also
	// MEKO_UPDATE_REPO.
	Repo string `toml:"repo"`
}

// Secrets is the [secrets] table: where the client token and other secrets are kept.
type Secrets struct {
	// Store is "keychain" (the default on macOS) or "file" (the default elsewhere: files in the data
	// folder, readable only by their owner). Also MEKO_SECRETS. Changing it doesn't move secrets: the
	// client token is made again, and clients set up with the old one need the new one.
	Store string `toml:"store"`
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
	// Auth is "token" (the default: /mcp and the data routes need the client or admin token) or
	// "none" (they need no credential; every local user can read and write the memories). Also
	// MEKO_AUTH.
	Auth string `toml:"auth"`
}

// Auth modes.
const (
	AuthToken = "token"
	AuthNone  = "none"
)

// MemoryEngine is the [memory_engine] table: where the memory engine and its Node are. Empty means the usual
// places (internal/memoryengine).
type MemoryEngine struct {
	Node string `toml:"node"` // also MEKO_NODE
	Dir  string `toml:"dir"`  // the built memory engine; also MEKO_MEMORY_ENGINE_DIR
}

// Log is the [log] table.
type Log struct {
	Level string `toml:"level"`
}

// Default is the configuration with nothing set.
func Default() Config {
	return Config{Server: Server{Port: DefaultPort, Auth: AuthToken}, Log: Log{Level: "info"},
		Update: Update{Channel: "stable", Auto: true, IntervalHours: 6, Repo: "yugabyte/agent-k-local"}}
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
	if v := env("MEKO_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("MEKO_PORT=%q isn't a port number", v)
		}
		c.Server.Port = p
	}
	if v := env("MEKO_AUTH"); v != "" {
		c.Server.Auth = v
	}
	if v := env("MEKO_LOG_LEVEL"); v != "" {
		c.Log.Level = v
	}
	if v := env("MEKO_NODE"); v != "" {
		c.MemoryEngine.Node = v
	}
	if v := env("MEKO_UPDATE_CHANNEL"); v != "" {
		c.Update.Channel = v
	}
	if v := env("MEKO_UPDATE_AUTO"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("MEKO_UPDATE_AUTO=%q isn't true or false", v)
		}
		c.Update.Auto = b
	}
	if v := env("MEKO_UPDATE_REPO"); v != "" {
		c.Update.Repo = v
	}
	if v := env("MEKO_SECRETS"); v != "" {
		c.Secrets.Store = v
	}
	if v := env("MEKO_MEMORY_ENGINE_DIR"); v != "" {
		c.MemoryEngine.Dir = v
	}
	if v := env("MEKO_WEB_UI_DIR"); v != "" {
		c.WebUI.Dir = v
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
	// An unknown mode is refused, so a typo never turns auth off.
	if c.Server.Auth != AuthToken && c.Server.Auth != AuthNone {
		return fmt.Errorf("server.auth %q isn't one of %q, %q", c.Server.Auth, AuthToken, AuthNone)
	}
	if s := c.Secrets.Store; s != "" && s != "keychain" && s != "file" {
		return fmt.Errorf("secrets.store %q isn't one of \"keychain\", \"file\"", s)
	}
	if ch := c.Update.Channel; ch != "stable" && ch != "off" {
		return fmt.Errorf("update.channel %q isn't one of \"stable\", \"off\"", ch)
	}
	if h := c.Update.IntervalHours; h < 0.1 || h > 24*30 {
		return fmt.Errorf("update.interval_hours %g is out of range: use 0.1 to 720", h)
	}
	if _, err := c.Log.SlogLevel(); err != nil {
		return err
	}
	if d := c.ConversationSearch.MaxDistance; d < 0 || d > 2 {
		return fmt.Errorf("conversation_search.max_distance %g is out of range: use 0 to 2", d)
	}
	if d := c.KnowledgeBase.MaxDistance; d < 0 || d > 2 {
		return fmt.Errorf("knowledge_base.max_distance %g is out of range: use 0 to 2", d)
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
