// Package main is the entry point for the meko datapack API server.
//
//	@title			Meko Datapack API
//	@version		1.0
//	@description	REST API for managing datapacks, agents, and knowledge bases in the meko platform.
//	@host			localhost:25433
//	@BasePath		/api/v1
//
//	@securityDefinitions.apikey	BearerAuth
//	@in							header
//	@name						Authorization
//	@description				JWT access token from /auth/login. Format: Bearer {token}
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"

	"meko/api_server/internal/api"
	"meko/api_server/internal/db"
	"meko/api_server/internal/repository"
)

func run() error {

	// meko datapack api server configuration
	port := flag.Int("port", 25433, "API server listen port")

	// Logging level. Accepts debug, info, warn, error (case-insensitive).
	// Set via flag or LOG_LEVEL env. Defaults to info.
	logLevel := flag.String("log-level",
		envOrDefault("LOG_LEVEL", "info"),
		"slog level: debug, info, warn, error")

	// database configuration
	dbHost := flag.String("db-host", "127.0.0.1", "database host")
	dbUserName := flag.String("db-user-name", "yugabyte", "database user name")
	dbPassword := flag.String("db-password", "", "database password")
	dbPort := flag.Int("db-port", 5433, "database port")
	dbName := flag.String("db-name", "yugabyte", "database name")
	dbSSLMode := flag.String("db-sslmode", "disable", "database sslmode (disable, require, verify-ca, verify-full)")

	// migrate only flag
	migrateOnly := flag.Bool("migrate-only", false, "run migrations and exit without starting the server")

	// Cognito configuration
	cognitoRegion := flag.String("cognito-region", "", "AWS region for Cognito User Pool")
	cognitoPoolID := flag.String("cognito-pool-id", "", "Cognito User Pool ID")
	cognitoClientID := flag.String("cognito-client-id", "", "Cognito App Client ID")
	cognitoClientSecret := flag.String("cognito-client-secret", "", "Cognito App Client secret")
	cognitoDomain := flag.String("cognito-domain", "", "Cognito hosted UI domain (e.g. myapp.auth.us-east-2.amazoncognito.com)")

	// Kubernetes namespace for MekoUserAccount CRDs
	crdNamespace := flag.String("crd-namespace", "meko-tenants", "Kubernetes namespace for MekoUserAccount CRDs")

	// Langfuse configuration
	langfuseHost := flag.String("langfuse-host", os.Getenv("LANGFUSE_HOST"), "Langfuse base URL (e.g. http://localhost:3000)")
	langfuseAdminAPIKey := flag.String("langfuse-admin-api-key", os.Getenv("LANGFUSE_ADMIN_API_KEY"), "Langfuse admin API key used for per-account org/project provisioning")

	// Langfuse Postgres connection — optional, only needed for read-only
	// observability features (e.g. listing conversations) that read Langfuse
	// trace/observation data directly. When unset, those endpoints return 503.
	langfuseDBHost := flag.String("langfuse-db-host", os.Getenv("LANGFUSE_DB_HOST"), "Langfuse Postgres host (optional)")
	langfuseDBPort := flag.Int("langfuse-db-port", envIntOrDefault("LANGFUSE_DB_PORT", 5433), "Langfuse Postgres port")
	langfuseDBName := flag.String("langfuse-db-name", envOrDefault("LANGFUSE_DB_NAME", "langfuse"), "Langfuse Postgres database name")
	langfuseDBUser := flag.String("langfuse-db-user", envOrDefault("LANGFUSE_DB_USER", "yugabyte"), "Langfuse Postgres user")
	langfuseDBPassword := flag.String("langfuse-db-password", os.Getenv("LANGFUSE_DB_PASSWORD"), "Langfuse Postgres password")
	langfuseDBSSLMode := flag.String("langfuse-db-sslmode", envOrDefault("LANGFUSE_DB_SSLMODE", "disable"), "Langfuse Postgres sslmode (disable, require, verify-ca, verify-full)")

	openAIAPIKey := flag.String("openai-api-key", os.Getenv("OPENAI_API_KEY"), "OpenAI API key")
	openAILLMModel := flag.String("openai-llm-model", os.Getenv("OPEN_AI_LLM_MODEL"), "OpenAI LLM model (default: gpt-4o)")
	openAIEmbeddingModel := flag.String("openai-embedding-model", os.Getenv("OPEN_AI_EMBEDDING_MODEL"), "OpenAI embedding model (default: text-embedding-3-small)")

	// When true (default), the memory/learning list endpoints skip the OpenAI
	// summary + embedding pre-step and order by meko_created_at DESC. Set to
	// false to restore the legacy vector-similarity sort.
	memoryListSkipVectorSort := flag.Bool("memory-list-skip-vector-sort", envBoolOrDefault("MEMORY_LIST_SKIP_VECTOR_SORT", true), "Skip OpenAI vector embedding for memory/learning list and sort by created_at DESC")
	deployEnv := flag.String("deploy-env", envOrDefault("MEKO_DEPLOY_ENV", "prod"), "deployment environment: prod or dev")

	// MCP internal validation endpoint shared secret. When empty, the
	// /api/v1/internal/meko-tokens/validate endpoint returns 503.
	mcpInternalSecret := flag.String("mcp-internal-secret",
		os.Getenv("MCP_INTERNAL_SECRET"),
		"shared secret for /internal/meko-tokens/validate (MCP server -> api_server). Empty disables the endpoint.")

	// MCP PAT last_used_at flush cadence. The validator coalesces
	// touches in memory and the background goroutine drains them on
	// this interval. Lower = fresher UI / more DB writes; higher =
	// staler UI / fewer DB writes. Values < 1s are clamped at startup.
	mcpPATFlushInterval := flag.Duration("mcp-pat-flush-interval",
		envDurationOrDefault("MCP_PAT_FLUSH_INTERVAL", 30*time.Second),
		"cadence for flushing MCP PAT last_used_at touches to the DB (e.g. 30s, 2m). Values <1s are clamped to 1s.")

	// Per-PAT rate limit. Applied only on routes that opted into PAT
	// acceptance (currently the 7 DatapackHandler routes); JWT
	// requests are untouched. perMin <= 0 disables the limiter.
	patRateLimitPerMin := flag.Int("pat-rate-limit-per-min",
		envIntOrDefault("PAT_RATE_LIMIT_PER_MIN", 60),
		"max requests per minute per PAT on opted-in routes (<=0 disables)")
	patRateLimitBurst := flag.Int("pat-rate-limit-burst",
		envIntOrDefault("PAT_RATE_LIMIT_BURST", 10),
		"burst allowance on top of --pat-rate-limit-per-min")

	// S3 configuration
	s3AuthMode := flag.String("s3-auth-mode", envOrDefault("S3_AUTH_MODE", "minio"), "S3 auth mode: minio, iam-user, iam-role")
	s3Endpoint := flag.String("s3-endpoint", envOrDefault("S3_ENDPOINT", "http://localhost:19094"), "S3-compatible endpoint URL (minio mode)")
	s3AccessKey := flag.String("s3-access-key", envOrDefault("S3_ACCESS_KEY", "meko"), "S3 access key (minio/iam-user modes)")
	s3SecretKey := flag.String("s3-secret-key", envOrDefault("S3_SECRET_KEY", "mekosecret"), "S3 secret key (minio/iam-user modes)")
	s3Bucket := flag.String("s3-bucket", envOrDefault("S3_BUCKET", "meko-kb-uploads"), "S3 bucket for KB file uploads")
	s3Region := flag.String("s3-region", envOrDefault("S3_REGION", "us-east-2"), "S3 region")
	flag.Parse()

	resolvedLogLevel := parseLogLevel(*logLevel)
	slog.SetDefault(slog.New(
		slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: resolvedLogLevel,
		}),
	))
	slog.Info("logger configured",
		"requested", *logLevel, "effective", resolvedLogLevel)

	connStr := fmt.Sprintf(
		"postgresql://%s:%d/%s?user=%s&password=%s&sslmode=%s",
		*dbHost, *dbPort, *dbName, *dbUserName, *dbPassword, *dbSSLMode,
	)

	conn, err := db.ConnectWithRetry(connStr, 5, 3*time.Second)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			slog.Error("failed to close database", "error", closeErr)
		}
	}()

	// Optional Langfuse Postgres connection — only attempted when --langfuse-db-host
	// (or LANGFUSE_DB_HOST) is set. Disabled cleanly otherwise; observability
	// endpoints that need it will return 503.
	var langfuseDBConn *sql.DB
	if *langfuseDBHost != "" {
		langfuseConnStr := fmt.Sprintf(
			"postgresql://%s:%d/%s?user=%s&password=%s&sslmode=%s",
			*langfuseDBHost, *langfuseDBPort, *langfuseDBName, *langfuseDBUser, *langfuseDBPassword, *langfuseDBSSLMode,
		)
		langfuseDBConn, err = db.ConnectWithRetry(langfuseConnStr, 3, 2*time.Second)
		if err != nil {
			slog.Warn("failed to connect to langfuse database; observability features disabled",
				"host", *langfuseDBHost, "error", err)
			langfuseDBConn = nil
		} else {
			slog.Info("langfuse database connected", "host", *langfuseDBHost, "db", *langfuseDBName)
			defer func() {
				if closeErr := langfuseDBConn.Close(); closeErr != nil {
					slog.Error("failed to close langfuse database", "error", closeErr)
				}
			}()
		}
	}

	if *migrateOnly {
		details, err := db.RunMigrations(conn)
		if err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
		for _, d := range details {
			slog.Info("migration", "detail", d)
		}
		slog.Info("migrations complete, exiting")
		return nil
	}

	slog.Info("provisioning config", "crd_namespace", *crdNamespace)

	if *mcpInternalSecret == "" {
		slog.Warn("mcp internal validate endpoint disabled (no --mcp-internal-secret / MCP_INTERNAL_SECRET configured)")
	} else {
		slog.Info("mcp internal validate endpoint enabled")
	}

	return api.Run(api.ServerConfig{
		DB:                       conn,
		Port:                     *port,
		CognitoRegion:            *cognitoRegion,
		CognitoPoolID:            *cognitoPoolID,
		CognitoClientID:          *cognitoClientID,
		CognitoClientSecret:      *cognitoClientSecret,
		CognitoDomain:            *cognitoDomain,
		CRDNamespace:             *crdNamespace,
		LangfuseHost:             *langfuseHost,
		LangfuseAdminAPIKey:      *langfuseAdminAPIKey,
		LangfuseDB:               langfuseDBConn,
		OpenAIAPIKey:             *openAIAPIKey,
		OpenAILLMModel:           *openAILLMModel,
		OpenAIEmbeddingModel:     *openAIEmbeddingModel,
		MemoryListSkipVectorSort: *memoryListSkipVectorSort,
		S3AuthMode:               *s3AuthMode,
		S3Endpoint:               *s3Endpoint,
		S3AccessKey:              *s3AccessKey,
		S3SecretKey:              *s3SecretKey,
		S3Bucket:                 *s3Bucket,
		S3Region:                 *s3Region,
		MCPServerURL:             mcpServerURLForEnv(repository.ParseDeployEnv(*deployEnv)),
		DeployEnv:                repository.ParseDeployEnv(*deployEnv),
		MCPInternalSecret:        *mcpInternalSecret,
		MCPPATFlushInterval:      *mcpPATFlushInterval,
		PATRateLimitPerMin:       *patRateLimitPerMin,
		PATRateLimitBurst:        *patRateLimitBurst,
	})
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envBoolOrDefault(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

const (
	mcpServerURLProd = "https://mcp.mekodata.ai/mcp"
	mcpServerURLDev  = "https://mcp.mekodev.com/mcp"
)

func mcpServerURLForEnv(env repository.DeployEnv) string {
	if env == repository.DeployEnvDev {
		return mcpServerURLDev
	}
	return mcpServerURLProd
}

// parseLogLevel maps a CLI/env string to a slog.Level. Unknown
// values fall back to Info with a warn log so a typo never silences
// the process or crashes boot.
func parseLogLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "info", "":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		slog.Warn("unknown log level, defaulting to info",
			"requested", raw)
		return slog.LevelInfo
	}
}

// envDurationOrDefault parses the named env var as a Go duration
// (e.g. "30s", "2m", "500ms"). On parse error we log and fall back
// to the default rather than crashing the process -- a bad value in
// a deployment manifest should not block boot.
func envDurationOrDefault(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		//nolint:gosec // G706: key/value are operator-supplied env config, not attacker input
		slog.Warn("invalid duration env var, using default",
			"key", key, "value", v, "error", err, "default", fallback)
		return fallback
	}
	return d
}
