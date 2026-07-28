package mcp

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PgStore is a Postgres-backed Store for a single datapack database.
type PgStore struct{ pool *pgxpool.Pool }

// NewPgStore connects to the datapack database and ensures the memories table.
func NewPgStore(ctx context.Context, dsn string) (*PgStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(cctx); err != nil {
		pool.Close()
		return nil, err
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS memories (
		id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		text       text NOT NULL,
		created_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		pool.Close()
		return nil, err
	}
	return &PgStore{pool: pool}, nil
}

func (p *PgStore) Add(ctx context.Context, text string) (Memory, error) {
	var m Memory
	err := p.pool.QueryRow(ctx, `INSERT INTO memories (text) VALUES ($1) RETURNING id, text, created_at`, text).
		Scan(&m.ID, &m.Text, &m.CreatedAt)
	return m, err
}

func (p *PgStore) List(ctx context.Context) ([]Memory, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, text, created_at FROM memories ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Memory{}
	for rows.Next() {
		var m Memory
		if err := rows.Scan(&m.ID, &m.Text, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (p *PgStore) Close() error {
	p.pool.Close()
	return nil
}
