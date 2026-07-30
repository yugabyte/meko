package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func ConnectWithRetry(connString string, maxRetries int, retryDelay time.Duration) (*sql.DB, error) {
	var db *sql.DB
	var err error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		slog.Debug("opening database connection", "attempt", attempt, "max_retries", maxRetries)
		db, err = sql.Open("postgres", connString)
		if err != nil {
			slog.Warn("sql.Open failed", "attempt", attempt, "error", err)
			if attempt < maxRetries {
				time.Sleep(retryDelay)
				continue
			}
			return nil, fmt.Errorf("failed to open database connection after %d attempts: %w", maxRetries, err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = db.PingContext(ctx)
		cancel()

		if err != nil {
			db.Close()
			slog.Warn("database ping failed", "attempt", attempt, "error", err)
			if attempt < maxRetries {
				time.Sleep(retryDelay)
				continue
			}
			return nil, fmt.Errorf("failed to connect to YugabyteDB after %d attempts: %w", maxRetries, err)
		}

		slog.Info("database connection established", "attempt", attempt)
		return db, nil
	}

	return nil, fmt.Errorf("failed to connect after %d attempts", maxRetries)
}

func RunMigrations(db *sql.DB) ([]string, error) {
	slog.Info("running database migrations")

	source, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("failed to read embedded migrations: %w", err)
	}

	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to create migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "yugabyte", driver)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize migrator: %w", err)
	}

	err = m.Up()
	if err != nil && err != migrate.ErrNoChange {
		slog.Error("migration failed", "error", err)
		return nil, fmt.Errorf("migration failed: %w", err)
	}

	version, dirty, _ := m.Version()

	if dirty {
		slog.Error("migration dirty state", "version", version)
		return nil, fmt.Errorf("migration is in a dirty state at version %d — manual intervention required", version)
	}

	var details []string
	if err == migrate.ErrNoChange {
		slog.Info("schema already up to date", "version", version)
		details = append(details, fmt.Sprintf("Schema up to date (version %d)", version))
	} else {
		slog.Info("schema migrated", "version", version)
		details = append(details, fmt.Sprintf("Migrated schema to version %d", version))
	}

	return details, nil
}


// EmbeddingDims returns the vector dimension for a given embedding model name.
// Defaults to 1536 for unrecognized models.
func EmbeddingDims(embedModel string) int {
	dims := map[string]int{
		"text-embedding-ada-002": 1536,
		"text-embedding-3-small": 1536,
		"text-embedding-3-large": 3072,
	}
	if d, ok := dims[embedModel]; ok {
		return d
	}
	return 1536
}

// sanitizeUUID replaces hyphens with underscores so a UUID can be used in SQL identifiers.
func sanitizeUUID(uuid string) string {
	return strings.ReplaceAll(uuid, "-", "_")
}
