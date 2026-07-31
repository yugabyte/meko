package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/yugabyte/amp/controller/migrations"
)

// Migrate applies all embedded migrations to schema, creating the schema first
// if needed. It is safe to run on every startup: already-applied migrations are
// skipped.
func Migrate(ctx context.Context, url, schema string) error {
	m, sqlDB, err := newMigrator(ctx, url, schema)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// newMigrator builds a golang-migrate instance over the embedded migrations,
// bound to schema (created first if needed). The caller owns the returned
// *sql.DB and must Close it. Shared by Migrate and the up/down round-trip test.
func newMigrator(ctx context.Context, url, schema string) (*migrate.Migrate, *sql.DB, error) {
	connCfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, nil, fmt.Errorf("parse database url: %w", err)
	}
	if connCfg.RuntimeParams == nil {
		connCfg.RuntimeParams = map[string]string{}
	}
	connCfg.RuntimeParams["search_path"] = schema

	// OpenDB takes the config directly; unlike sql.Open("pgx",
	// RegisterConnConfig(cfg)) it doesn't stash the config in a never-freed
	// global map, so newMigrator is safe to call repeatedly (tests, reconnects).
	sqlDB := stdlib.OpenDB(*connCfg)
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, nil, fmt.Errorf("ping migration connection: %w", err)
	}

	// The migrations table lives in schema, so the schema must exist before the
	// migrate driver initialises. CREATE SCHEMA IF NOT EXISTS is idempotent.
	if _, err := sqlDB.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoteIdent(schema)); err != nil {
		sqlDB.Close()
		return nil, nil, fmt.Errorf("ensure schema %q: %w", schema, err)
	}

	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		sqlDB.Close()
		return nil, nil, fmt.Errorf("load embedded migrations: %w", err)
	}

	driver, err := migratepgx.WithInstance(sqlDB, &migratepgx.Config{
		SchemaName:      schema,
		MigrationsTable: "schema_migrations",
	})
	if err != nil {
		sqlDB.Close()
		return nil, nil, fmt.Errorf("init migrate driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "pgx", driver)
	if err != nil {
		sqlDB.Close()
		return nil, nil, fmt.Errorf("init migrator: %w", err)
	}
	return m, sqlDB, nil
}
