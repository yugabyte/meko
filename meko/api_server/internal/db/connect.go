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
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// PoolConfig controls connection recycling for pools opened by
// ConnectWithRetry. Zero disables the corresponding limit.
type PoolConfig struct {
	MaxLifetime time.Duration
	MaxIdleTime time.Duration
}

func (c PoolConfig) validate() error {
	if c.MaxLifetime < 0 {
		return fmt.Errorf("max connection lifetime must not be negative")
	}
	if c.MaxIdleTime < 0 {
		return fmt.Errorf("max connection idle time must not be negative")
	}
	return nil
}

// One statement per file for anything that writes rows, and never DDL beside DML:
// golang-migrate Execs a migration whole, which the simple query protocol runs as a
// single implicit transaction, and YugabyteDB cannot retry a statement inside one
// (yugabyte-db#21833). A conflict with a live writer then aborts the migration and
// leaves the version dirty instead of being retried away -- CLOUDGA-36984's backfill
// of a table agents heartbeat into every 10s, while the api-server replicas it is
// replacing still serve them. The two 000001s and the two other multi-statement
// backfills predate the rule and are grandfathered by
// TestBackfillMigrationsAreSingleStatement.
//
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
	poolConfig PoolConfig,
) (*sql.DB, error) {
	if err := poolConfig.validate(); err != nil {
		return nil, fmt.Errorf("invalid pool configuration: %w", err)
	}

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

		conn.SetConnMaxLifetime(poolConfig.MaxLifetime)
		conn.SetConnMaxIdleTime(poolConfig.MaxIdleTime)

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

// DefaultHashPartitions is the partition count of every hash-partitioned data
// table. A data migration reads the count only when it creates a table, so an
// existing table keeps the count it was created with.
const DefaultHashPartitions = 128

// Partition names are LPAD(i, 3), which truncates a fourth digit.
const maxHashPartitions = 1000

const hashPartitionsSetting = "meko.hash_partitions"

func RunMigrations(systemDB, dataDB *sql.DB, scope MigrationScope, hashPartitions int) ([]string, error) {
	var details []string

	if scope.Data {
		if hashPartitions < 1 || hashPartitions > maxHashPartitions {
			return nil, fmt.Errorf("hash partitions must be between 1 and %d, got %d", maxHashPartitions, hashPartitions)
		}
		dataM, err := getDataMigrator(dataDB, hashPartitions)
		if err != nil {
			slog.Error("failed to get migrator", "purpose", DBPurposeData, "error", err)
			return nil, err
		}
		defer dataM.Close()
		if _, err := recoverInterruptedFirstDataMigration(context.Background(), dataDB); err != nil {
			return nil, err
		}
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

// recoverInterruptedFirstDataMigration lets a first install survive its migrate
// container being killed during data migration 1. golang-migrate marks a version
// dirty before running it and refuses to run anything while that mark is set, so
// without this every restart fails with "Dirty database version 1" until someone
// forces the version by hand (MEKO-831).
//
// Clearing the row is what `migrate force` to the nil version does; migration 1 is
// then rerun from the top, which 000001 is written to tolerate.
//
// This runs BEFORE golang-migrate takes its advisory lock, so it is not ordered behind
// another replica that is genuinely midway through migration 1: it clears that
// replica's in-flight dirty mark too. That is harmless rather than prevented. The
// replica keeps running and rewrites the row at each step, and this process's own
// m.Up() then blocks on the lock and finds the finished state (or, if the other replica
// died, reruns a migration 1 that is safe to rerun).
//
// Only version 1, on purpose. Later migrations have not been audited for being safe
// to rerun, so a dirty version past 1 is still left for a person to look at. The table
// comes from the same config the data migrator is built with, so it cannot drift from it.
func recoverInterruptedFirstDataMigration(ctx context.Context, db *sql.DB) (bool, error) {
	res, err := db.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM %s WHERE version = 1 AND dirty", migrationsTableRef(getPostgresConfig(DBPurposeData))))
	if err != nil {
		return false, fmt.Errorf("failed to check %s migration 1 for an interrupted run: %w", DBPurposeData, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to check %s migration 1 for an interrupted run: %w", DBPurposeData, err)
	}
	if n > 0 {
		slog.Warn("data migration 1 was interrupted on an earlier start; rerunning it",
			"purpose", DBPurposeData)
	}
	return n > 0, nil
}

// migrationsTableRef is the quoted, schema-qualified name of the table golang-migrate
// keeps its version in for cfg, mirroring how the postgres driver resolves it: an empty
// MigrationsTable means the default, and an empty SchemaName means the search path.
func migrationsTableRef(cfg *postgres.Config) string {
	table := cfg.MigrationsTable
	if table == "" {
		table = postgres.DefaultMigrationsTable
	}
	ref := quoteIdentifier(table)
	if cfg.SchemaName != "" {
		ref = quoteIdentifier(cfg.SchemaName) + "." + ref
	}
	return ref
}

func quoteIdentifier(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
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

func getDataMigrator(db *sql.DB, hashPartitions int) (*migrate.Migrate, error) {
	src, err := iofs.New(dataMigrationsFS, "migrations/data")
	if err != nil {
		return nil, fmt.Errorf("failed to load embedded %s data: %w", DBPurposeData, err)
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s migration session: %w", DBPurposeData, err)
	}
	driver, err := newDataDriver(ctx, conn, hashPartitions)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	m, err := migrate.NewWithInstance("iofs", src, "yugabyte", driver)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize %s migrator: %w", DBPurposeData, err)
	}
	return m, nil
}

// golang-migrate applies every migration on the one session it is given, so a
// session setting reaches all of them. Taking a *sql.Conn rather than the pool is
// what keeps the SET and the migrations on that one session.
func newDataDriver(ctx context.Context, conn *sql.Conn, hashPartitions int) (database.Driver, error) {
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET %s = %d", hashPartitionsSetting, hashPartitions)); err != nil {
		return nil, fmt.Errorf("failed to set %s: %w", hashPartitionsSetting, err)
	}
	driver, err := postgres.WithConnection(ctx, conn, getPostgresConfig(DBPurposeData))
	if err != nil {
		return nil, fmt.Errorf("failed to %s migration driver: %w", DBPurposeData, err)
	}
	return driver, nil
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
