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

// Control-plane mode needs only the fleet registry, so it gets its own set in
// portable SQL rather than the system sequence, which reaches 000008 only after
// applying YugabyteDB-only DDL. Lets a control plane run on a plain Postgres.
//
//go:embed migrations/controlplane/*.sql
var controlPlaneMigrationsFS embed.FS

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
	DBPurposeSystem       DBPurpose = "system"
	DBPurposeData         DBPurpose = "data"
	DBPurposeControlPlane DBPurpose = "controlplane"
)

// mekoSystemSchema holds every table the control-plane set creates.
const mekoSystemSchema = "meko_system"

// MigrationScope selects which migration sets RunMigrations applies.
type MigrationScope struct {
	System bool
	Data   bool
	// ControlPlane applies the fleet-registry set against the system DB. Separate
	// from System on purpose: it is the only set that runs on plain Postgres.
	ControlPlane bool
}

// AllMigrations runs both the data-plane and meko_system migration sets.
var AllMigrations = MigrationScope{System: true, Data: true}

// ControlPlaneMigrations runs only the fleet registry — what --control-plane-mode
// needs, and nothing that requires YugabyteDB.
var ControlPlaneMigrations = MigrationScope{ControlPlane: true}

func RunMigrations(systemDB, dataDB *sql.DB, scope MigrationScope) ([]string, error) {
	var details []string

	if scope.Data {
		dataM, err := getMigrator(dataDB, DBPurposeData, dataMigrationsFS, "migrations/data")
		if err != nil {
			slog.Error("failed to get migrator", "purpose", DBPurposeData, "error", err)
			return nil, err
		}
		defer dataM.Close()
		d, err := applyMigrator(dataM, DBPurposeData)
		if err != nil {
			return nil, err
		}
		details = append(details, d)
	}

	if scope.System {
		systemM, err := getMigrator(systemDB, DBPurposeSystem, systemMigrationsFS, "migrations/system")
		if err != nil {
			slog.Error("failed to get migrator", "purpose", DBPurposeSystem, "error", err)
			return nil, err
		}
		defer systemM.Close()
		d, err := applyMigrator(systemM, DBPurposeSystem)
		if err != nil {
			return nil, err
		}
		details = append(details, d)
	}

	if scope.ControlPlane {
		cpM, err := getMigrator(systemDB, DBPurposeControlPlane, controlPlaneMigrationsFS, "migrations/controlplane")
		if err != nil {
			slog.Error("failed to get migrator", "purpose", DBPurposeControlPlane, "error", err)
			return nil, err
		}
		defer cpM.Close()
		d, err := applyMigrator(cpM, DBPurposeControlPlane)
		if err != nil {
			return nil, err
		}
		details = append(details, d)
	}

	return details, nil
}

func getMigrator(conn *sql.DB, purpose DBPurpose, fs embed.FS, rootFs string) (*migrate.Migrate, error) {
	src, err := iofs.New(fs, rootFs)
	if err != nil {
		return nil, fmt.Errorf("failed to load embedded %s data: %w", purpose, err)
	}
	// postgres.WithInstance creates the version table as it builds the driver,
	// before m.Up() runs anything, so a schema the config pins it to has to exist
	// already -- controlplane/000001 creating it is too late.
	if purpose == DBPurposeControlPlane {
		if _, err = conn.Exec("CREATE SCHEMA IF NOT EXISTS " + mekoSystemSchema); err != nil {
			return nil, fmt.Errorf("failed to create %s schema: %w", mekoSystemSchema, err)
		}
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
	// Left wherever the search path puts it: this set shares its database with the
	// data-plane tables, so moving its version table into meko_system would turn a
	// drop of that schema into a whole-platform reset.
	case DBPurposeSystem:
		return &postgres.Config{
			MigrationsTable: fmt.Sprintf("%s_%s", postgres.DefaultMigrationsTable, "system"),
		}
	// Its own tracking table, so neither set sees the other's version. In
	// meko_system, the schema this set creates and owns, so DROP SCHEMA meko_system
	// CASCADE is a complete reset instead of one that strands a version row claiming
	// the migrations are still applied.
	//
	// This set cannot run over a database that already ran system/000008: that set's
	// environments table survives this set's IF NOT EXISTS create in its older shape,
	// which has no uuid for environment_api_keys' foreign key to reference.
	//
	// Control-plane mode selects this set alone, so the two do not meet in any
	// configuration that exists today -- but that is a property of which scopes the
	// flags happen to build, not an enforced one: RunMigrations receives one handle
	// for both sets, so a scope with System and ControlPlane both set would apply
	// them to the same database.
	// TODO(CLOUDGA-35739): reject that combination in RunMigrations.
	case DBPurposeControlPlane:
		return &postgres.Config{
			MigrationsTable: fmt.Sprintf("%s_%s", postgres.DefaultMigrationsTable, "controlplane"),
			SchemaName:      mekoSystemSchema,
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
