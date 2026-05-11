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
	"time"

	_ "github.com/lib/pq"

	"meko/api_server/internal/api"
	"meko/api_server/internal/db"
	"meko/api_server/internal/repository"
)

func run() error {

	// meko datapack api server configuration
	port := flag.Int("port", 25433, "API server listen port")

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

	// S3 configuration
	s3AuthMode := flag.String("s3-auth-mode", envOrDefault("S3_AUTH_MODE", "minio"), "S3 auth mode: minio, iam-user, iam-role")
	s3Endpoint := flag.String("s3-endpoint", envOrDefault("S3_ENDPOINT", "http://localhost:19094"), "S3-compatible endpoint URL (minio mode)")
	s3AccessKey := flag.String("s3-access-key", envOrDefault("S3_ACCESS_KEY", "meko"), "S3 access key (minio/iam-user modes)")
	s3SecretKey := flag.String("s3-secret-key", envOrDefault("S3_SECRET_KEY", "mekosecret"), "S3 secret key (minio/iam-user modes)")
	s3Bucket := flag.String("s3-bucket", envOrDefault("S3_BUCKET", "meko-kb-uploads"), "S3 bucket for KB file uploads")
	s3Region := flag.String("s3-region", envOrDefault("S3_REGION", "us-east-2"), "S3 region")
	flag.Parse()

	slog.SetDefault(slog.New(
		slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}),
	))

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
