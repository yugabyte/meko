// Package config loads amp controller configuration from environment
// variables, with command-line flags taking precedence.
package config

import (
	"flag"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"time"
)

// LogConfig controls the structured logger.
type LogConfig struct {
	Level  string // debug | info | warn | error
	Format string // json | text
}

// DBConfig points at the YBDB/PostgreSQL state store.
type DBConfig struct {
	URL    string // YSQL connection string (required)
	Schema string // schema that holds the control-plane tables
}

// TaskConfig tunes the task framework worker.
type TaskConfig struct {
	PollInterval time.Duration
	LeaseTTL     time.Duration
}

// StorageControllerConfig configures the storage controller that the amp
// controller runs as a managed subprocess (yblight-controller.md §11.1). The
// storage controller is a managed service: the controller spawns it, supervises
// it (restart on crash, clean stop on shutdown), and is its only caller.
type StorageControllerConfig struct {
	Enabled      bool          // run the storage controller as a managed subprocess
	BinPath      string        // path to the storage_controller binary
	ListenAddr   string        // host:port the storage controller serves HTTP on
	DatabaseURL  string        // universe connection for the storage controller; defaults to DB.URL
	Dev          bool          // pass --dev (run without auth); dev/local only
	ExtraArgs    []string      // extra raw args appended to the invocation (programmatic extension point; not flag-wired)
	StartTimeout time.Duration // how long to wait for the first readiness probe to pass
	StopTimeout  time.Duration // grace period after SIGTERM before SIGKILL
}

// Config is the fully-resolved controller configuration.
type Config struct {
	ListenAddr        string
	SigningKeyPath    string // optional; controller generates an ephemeral key if unset
	Log               LogConfig
	DB                DBConfig
	Task              TaskConfig
	StorageController StorageControllerConfig
}

var schemaRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Load resolves configuration from args (flags) layered over the environment.
func Load(args []string) (*Config, error) {
	fs := flag.NewFlagSet("amp", flag.ContinueOnError)
	c := &Config{}

	fs.StringVar(&c.ListenAddr, "listen", env("AMP_LISTEN", ":8080"), "HTTP listen address")
	fs.StringVar(&c.DB.URL, "database-url", env("AMP_DATABASE_URL", ""), "YBDB/PostgreSQL connection string (required)")
	fs.StringVar(&c.DB.Schema, "db-schema", env("AMP_DB_SCHEMA", "amp"), "schema that holds the control-plane tables")
	fs.StringVar(&c.Log.Level, "log-level", env("AMP_LOG_LEVEL", "info"), "log level: debug|info|warn|error")
	fs.StringVar(&c.Log.Format, "log-format", env("AMP_LOG_FORMAT", "json"), "log format: json|text")
	fs.StringVar(&c.SigningKeyPath, "signing-key", env("AMP_SIGNING_KEY_PATH", ""), "path to the control-plane signing key (optional)")
	fs.DurationVar(&c.Task.PollInterval, "task-poll-interval", envDuration("AMP_TASK_POLL_INTERVAL", 2*time.Second), "task worker poll interval")
	fs.DurationVar(&c.Task.LeaseTTL, "task-lease-ttl", envDuration("AMP_TASK_LEASE_TTL", 30*time.Second), "task ownership lease TTL")

	sc := &c.StorageController
	fs.BoolVar(&sc.Enabled, "storage-controller", envBool("AMP_STORAGE_CONTROLLER", false), "run the storage controller as a managed subprocess (§11.1)")
	fs.StringVar(&sc.BinPath, "storage-controller-bin", env("AMP_STORAGE_CONTROLLER_BIN", "storage_controller"), "storage_controller binary: a bare name is resolved next to the amp binary (the bundle), then PATH; a path is used verbatim")
	fs.StringVar(&sc.ListenAddr, "storage-controller-listen", env("AMP_STORAGE_CONTROLLER_LISTEN", "127.0.0.1:50051"), "host:port the managed storage controller serves on")
	fs.StringVar(&sc.DatabaseURL, "storage-controller-database-url", env("AMP_STORAGE_CONTROLLER_DATABASE_URL", ""), "universe connection for the storage controller (defaults to --database-url)")
	fs.BoolVar(&sc.Dev, "storage-controller-dev", envBool("AMP_STORAGE_CONTROLLER_DEV", false), "run the storage controller in --dev mode (no auth; dev/local only)")
	fs.DurationVar(&sc.StartTimeout, "storage-controller-start-timeout", envDuration("AMP_STORAGE_CONTROLLER_START_TIMEOUT", 30*time.Second), "how long to wait for the storage controller to become ready")
	fs.DurationVar(&sc.StopTimeout, "storage-controller-stop-timeout", envDuration("AMP_STORAGE_CONTROLLER_STOP_TIMEOUT", 10*time.Second), "grace period after SIGTERM before the storage controller is killed")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	// The universe connection is required configuration: the controller refuses
	// to start without it (yblight-controller.md §9).
	if c.DB.URL == "" {
		return nil, fmt.Errorf("database URL is required (set --database-url or AMP_DATABASE_URL)")
	}
	if !schemaRe.MatchString(c.DB.Schema) {
		return nil, fmt.Errorf("invalid db schema %q: must be a plain SQL identifier", c.DB.Schema)
	}
	if c.Task.PollInterval <= 0 {
		return nil, fmt.Errorf("task poll interval must be positive")
	}
	if c.Task.LeaseTTL <= 0 {
		return nil, fmt.Errorf("task lease TTL must be positive")
	}
	if err := validateStorageController(sc, c.DB.URL); err != nil {
		return nil, err
	}
	return c, nil
}

// validateStorageController fills defaults and validates the managed
// storage-controller config. It is a no-op unless the service is enabled.
func validateStorageController(sc *StorageControllerConfig, universeURL string) error {
	if !sc.Enabled {
		return nil
	}
	if sc.DatabaseURL == "" {
		// Both persist to the same universe over YSQL (§11); reuse the
		// controller's connection unless one is given explicitly.
		sc.DatabaseURL = universeURL
	}
	if sc.BinPath == "" {
		return fmt.Errorf("storage controller binary path is required when --storage-controller is set")
	}
	if _, _, err := net.SplitHostPort(sc.ListenAddr); err != nil {
		return fmt.Errorf("invalid storage controller listen address %q: %w", sc.ListenAddr, err)
	}
	if sc.StartTimeout <= 0 || sc.StopTimeout <= 0 {
		return fmt.Errorf("storage controller start/stop timeouts must be positive")
	}
	return nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
