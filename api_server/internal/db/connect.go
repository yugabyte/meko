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

//go:embed migrations/system/*.sql
var systemMigrationsFS embed.FS

//go:embed migrations/data/*.sql
var dataMigrationsFS embed.FS

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

type DBPurpose string

const (
	DBPurposeSystem DBPurpose = "system"
	DBPurposeData   DBPurpose = "data"
)

func RunMigrations(systemDB, dataDB *sql.DB) ([]string, error) {
	dataM, err := getMigrator(dataDB, DBPurposeData, dataMigrationsFS, "migrations/data")
	if err != nil {
		slog.Error("failed to get migrator", "purpose", DBPurposeData, "error", err)
		return nil, err
	}
	defer dataM.Close()
	systemM, err := getMigrator(systemDB, DBPurposeSystem, systemMigrationsFS, "migrations/system")
	if err != nil {
		slog.Error("failed to get migrator", "purpose", DBPurposeSystem, "error", err)
		return nil, err
	}
	defer systemM.Close()

	var details []string
	d, err := applyMigrator(dataM, DBPurposeData)
	if err != nil {
		return nil, err
	}
	details = append(details, d)
	d, err = applyMigrator(systemM, DBPurposeSystem)
	if err != nil {
		return nil, err
	}
	details = append(details, d)
	return details, nil
}

func getMigrator(conn *sql.DB, purpose DBPurpose, fs embed.FS, rootFs string) (*migrate.Migrate, error) {
	src, err := iofs.New(fs, rootFs)
	if err != nil {
		return nil, fmt.Errorf("failed to load embedded %s data: %w", purpose, err)
	}
	driver, err := postgres.WithInstance(conn, getPostgresConfig(purpose))
	if err != nil {
		return nil, fmt.Errorf("failed to %s migration driver: %w", purpose, err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "yugabyte", driver)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize %s migrator: %w", purpose, err)
	}
	return m, nil
}

func getPostgresConfig(purpose DBPurpose) *postgres.Config {
	switch purpose {
	case DBPurposeSystem:
		return &postgres.Config{
			MigrationsTable: fmt.Sprintf("%s_%s", postgres.DefaultMigrationsTable, "system"),
		}
	default:
		return &postgres.Config{}
	}
}

func applyMigrator(m *migrate.Migrate, purpose DBPurpose) (string, error) {
	slog.Info("running database migrations", "purpose", purpose)
	err := m.Up()
	if err != nil && err != migrate.ErrNoChange {
		slog.Error("migration failed", "purpose", purpose, "error", err)
		return "", fmt.Errorf("%s migration failed: %w", purpose, err)
	}

	version, dirty, vErr := m.Version()
	if vErr != nil {
		return "", fmt.Errorf("failed to get %s migration version: %w", purpose, vErr)
	}
	if dirty {
		slog.Error("migration dirty state", "purpose", purpose, "version", version)
		return "", fmt.Errorf(
			"%s migration is in a dirty state at version %d", purpose, version,
		)
	}

	var details string
	if err == migrate.ErrNoChange {
		slog.Info("schema already up to date", "purpose", purpose, "version", version)
		details = fmt.Sprintf("%s schema up to date (version %d)", purpose, version)
	} else {
		slog.Info("schema migrated", "purpose", purpose, "version", version)
		details = fmt.Sprintf("Migrated %s schema to version %d", purpose, version)
	}
	return details, nil
}

// SanitizeUUID replaces hyphens with underscores so a UUID can be used
// in SQL identifiers.
func SanitizeUUID(uuid string) string {
	return strings.ReplaceAll(uuid, "-", "_")
}
