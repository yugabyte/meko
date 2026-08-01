// Package db owns the YBDB/PostgreSQL connection pool and schema migrations.
// YBDB is the controller's only state store (amp-controller.md §10) and
// speaks the PostgreSQL wire protocol.
package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ResourceSchema is the schema holding the control-plane resource model: users,
// storage pools, projects, and the single `branches` / `endpoints` tables that
// both the product API and the storage-layer control surface read and write.
//
// It is hardcoded rather than configurable, the same way `amp_storage` is: these
// names are part of the architecture, not a deployment knob. --db-schema
// relocates only the controller's own tables (the task log), which is what the
// pool's search_path points at. Statements over the resource model are therefore
// schema-qualified, so they resolve regardless of that search_path.
const ResourceSchema = "amp_registry"

// Pool is the controller's connection pool, pinned to its control-plane schema.
type Pool struct {
	*pgxpool.Pool
}

// Connect opens a pool against url, with search_path pinned to schema so that
// the controller's own unqualified tables resolve there, isolated from tenant
// data. Everything outside it — the resource model (ResourceSchema) and the data
// plane (amp_storage) — is schema-qualified at the statement.
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

// quoteLiteral single-quotes a SQL string literal, for the few statements that
// cannot take a bind parameter. Same posture as quoteIdent: callers validate,
// this is defence in depth.
func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}
