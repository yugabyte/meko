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
	dbPort := flag.Int("db-port", 5433, "database port")
	flag.Parse()

	slog.SetDefault(slog.New(
		slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}),
	))

	connStr := fmt.Sprintf(
		"postgresql://%s:%d/yugabyte?user=yugabyte&password=&sslmode=disable",
		*dbHost, *dbPort,
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

	details, err := db.RunMigrations(conn)
	if err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}
	for _, d := range details {
		slog.Info("migration", "detail", d)
	}

	return api.Run(api.ServerConfig{DB: conn, Port: *port})
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}
