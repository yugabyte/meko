// Package db owns the YBDB/PostgreSQL connection pool and schema migrations.
// YBDB is the controller's only state store (yblight-controller.md §10) and
// speaks the PostgreSQL wire protocol.
package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is the controller's connection pool, pinned to its control-plane schema.
type Pool struct {
	*pgxpool.Pool
}

// Connect opens a pool against url, with search_path pinned to schema so that
// unqualified control-plane tables resolve in their own schema, isolated from
// tenant data.
func Connect(ctx context.Context, url, schema string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Pool{pool}, nil
}

// Healthy reports whether the database is reachable (readiness probe).
func (p *Pool) Healthy(ctx context.Context) error {
	return p.Ping(ctx)
}

// quoteIdent double-quotes a SQL identifier. Callers must validate the input is
// a plain identifier (config does this for the schema name); the escaping here
// is defence in depth.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
