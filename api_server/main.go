// Package main is the entry point for the meko datapack API server.
//
//	@title			Meko Datapack API
//	@version		1.0
//	@description	REST API for managing datapacks, agents, and knowledge bases in the meko platform.
//	@host			localhost:25433
//	@BasePath		/api/v1
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	_ "github.com/lib/pq"

	"meko/api_server/internal/api"
	"meko/api_server/internal/db"
)

func run() error {
	port := flag.Int("port", 25433, "API server listen port")
	dbHost := flag.String("db-host", "127.0.0.1", "database host")
	dbUserName := flag.String("db-user-name", "yugabyte", "database user name")
	dbPassword := flag.String("db-password", "", "database password")
	dbPort := flag.Int("db-port", 5433, "database port")
	migrateOnly := flag.Bool("migrate-only", false, "run migrations and exit without starting the server")
	cognitoRegion := flag.String("cognito-region", "", "AWS region for Cognito User Pool")
	cognitoPoolID := flag.String("cognito-pool-id", "", "Cognito User Pool ID")
	cognitoClientID := flag.String("cognito-client-id", "", "Cognito App Client ID")
	cognitoClientSecret := flag.String("cognito-client-secret", "", "Cognito App Client secret")
	cognitoDomain := flag.String("cognito-domain", "", "Cognito hosted UI domain (e.g. myapp.auth.us-east-2.amazoncognito.com)")
	crdNamespace := flag.String("crd-namespace", "meko-tenants", "Kubernetes namespace for MekoUserAccount CRDs")
	langfuseHost := flag.String("langfuse-host", os.Getenv("LANGFUSE_HOST"), "Langfuse base URL (e.g. http://localhost:3000)")
	langfuseAdminAPIKey := flag.String("langfuse-admin-api-key", os.Getenv("LANGFUSE_ADMIN_API_KEY"), "Langfuse admin API key used for per-account org/project provisioning")
	flag.Parse()

	slog.SetDefault(slog.New(
		slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}),
	))

	connStr := fmt.Sprintf(
		"postgresql://%s:%d/yugabyte?user=%s&password=%s&sslmode=disable",
		*dbHost, *dbPort, *dbUserName, *dbPassword,
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
		DB:                  conn,
		Port:                *port,
		CognitoRegion:       *cognitoRegion,
		CognitoPoolID:       *cognitoPoolID,
		CognitoClientID:     *cognitoClientID,
		CognitoClientSecret: *cognitoClientSecret,
		CognitoDomain:       *cognitoDomain,
		CRDNamespace:        *crdNamespace,
		LangfuseHost:        *langfuseHost,
		LangfuseAdminAPIKey: *langfuseAdminAPIKey,
	})
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}
