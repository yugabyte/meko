// Package db provides database connection utilities and migration support.
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

// ConnectWithRetry opens a database connection, retrying on failure.
func ConnectWithRetry(
	connString string,
	maxRetries int,
	retryDelay time.Duration,
) (*sql.DB, error) {
	var conn *sql.DB
	var err error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		slog.Debug("opening database connection",
			"attempt", attempt, "max_retries", maxRetries)

		conn, err = sql.Open("postgres", connString)
		if err != nil {
			slog.Warn("sql.Open failed", "attempt", attempt, "error", err)
			if attempt < maxRetries {
				time.Sleep(retryDelay)
				continue
			}
			return nil, fmt.Errorf(
				"failed to open database after %d attempts: %w",
				maxRetries, err,
			)
		}

		ctx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		err = conn.PingContext(ctx)
		cancel()

		if err != nil {
			if closeErr := conn.Close(); closeErr != nil {
				slog.Warn("failed to close db after ping failure",
					"error", closeErr)
			}
			slog.Warn("database ping failed",
				"attempt", attempt, "error", err)
			if attempt < maxRetries {
				time.Sleep(retryDelay)
				continue
			}
			return nil, fmt.Errorf(
				"failed to connect to database after %d attempts: %w",
				maxRetries, err,
			)
		}

		slog.Info("database connection established", "attempt", attempt)
		return conn, nil
	}

	return nil, fmt.Errorf(
		"failed to connect after %d attempts", maxRetries,
	)
}

// RunMigrations applies all pending SQL migrations from the embedded FS.
func RunMigrations(conn *sql.DB) ([]string, error) {
	slog.Info("running database migrations")

	source, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf(
			"failed to read embedded migrations: %w", err,
		)
	}

	driver, err := postgres.WithInstance(conn, &postgres.Config{})
	if err != nil {
		return nil, fmt.Errorf(
			"failed to create migration driver: %w", err,
		)
	}

	m, err := migrate.NewWithInstance(
		"iofs", source, "yugabyte", driver,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to initialize migrator: %w", err,
		)
	}

	err = m.Up()
	if err != nil && err != migrate.ErrNoChange {
		slog.Error("migration failed", "error", err)
		return nil, fmt.Errorf("migration failed: %w", err)
	}

	version, dirty, vErr := m.Version()
	if vErr != nil {
		return nil, fmt.Errorf("failed to get migration version: %w", vErr)
	}
	if dirty {
		slog.Error("migration dirty state", "version", version)
		return nil, fmt.Errorf(
			"migration is in a dirty state at version %d", version,
		)
	}

	var details []string
	if err == migrate.ErrNoChange {
		slog.Info("schema already up to date", "version", version)
		details = append(details,
			fmt.Sprintf("Schema up to date (version %d)", version))
	} else {
		slog.Info("schema migrated", "version", version)
		details = append(details,
			fmt.Sprintf("Migrated schema to version %d", version))
	}

	return details, nil
}

// SanitizeUUID replaces hyphens with underscores so a UUID can be used
// in SQL identifiers.
func SanitizeUUID(uuid string) string {
	return strings.ReplaceAll(uuid, "-", "_")
}
