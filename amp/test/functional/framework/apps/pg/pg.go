// Package pg is the Postgres client app: a thin, pooled pgx wrapper with the
// query and readiness helpers tests reach for. It is a client library app, not
// a full App-lifecycle object.
package pg

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConnInfo describes how to reach a Postgres endpoint.
type ConnInfo struct {
	Host     string
	Port     int    // 0 → 5432
	User     string // "" → postgres
	Password string
	Database string // "" → postgres
	SSLMode  string // "" → disable
}

func (ci ConnInfo) port() int {
	if ci.Port == 0 {
		return 5432
	}
	return ci.Port
}

func (ci ConnInfo) user() string {
	if ci.User == "" {
		return "postgres"
	}
	return ci.User
}

func (ci ConnInfo) database() string {
	if ci.Database == "" {
		return "postgres"
	}
	return ci.Database
}

func (ci ConnInfo) sslMode() string {
	if ci.SSLMode == "" {
		return "disable"
	}
	return ci.SSLMode
}

// DSN renders a key=value connection string with defaults applied.
func (ci ConnInfo) DSN() string {
	kv := [][2]string{
		{"host", ci.Host},
		{"port", strconv.Itoa(ci.port())},
		{"user", ci.user()},
		{"dbname", ci.database()},
		{"sslmode", ci.sslMode()},
	}
	if ci.Password != "" {
		kv = append(kv, [2]string{"password", ci.Password})
	}
	out := ""
	for i, p := range kv {
		if i > 0 {
			out += " "
		}
		out += p[0] + "=" + quoteDSN(p[1])
	}
	return out
}

// quoteDSN single-quotes a value if it contains a space or is empty.
func quoteDSN(v string) string {
	if v == "" {
		return "''"
	}
	for _, c := range v {
		if c == ' ' || c == '\'' || c == '\\' {
			esc := ""
			for _, r := range v {
				if r == '\'' || r == '\\' {
					esc += "\\"
				}
				esc += string(r)
			}
			return "'" + esc + "'"
		}
	}
	return v
}

// Client is a pooled Postgres connection.
type Client struct {
	pool *pgxpool.Pool
}

// Connect opens a connection pool to the endpoint described by ci.
func Connect(ctx context.Context, ci ConnInfo) (*Client, error) {
	pool, err := pgxpool.New(ctx, ci.DSN())
	if err != nil {
		return nil, fmt.Errorf("connect %s:%d: %w", ci.Host, ci.port(), err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping %s:%d: %w", ci.Host, ci.port(), err)
	}
	return &Client{pool: pool}, nil
}

// Session is one Postgres backend, held for as long as the caller keeps it.
//
// A [Client] is a pool: it hands each query whichever connection is free and
// quietly replaces one it finds dead, which is what a test wants almost always
// and exactly wrong when the question is whether a connection SURVIVED
// something. Through a pool, a compute that restarted mid-test still answers —
// the pool reconnected. A Session is the same methods over one pinned
// connection, so a server that went away surfaces as an error instead.
type Session interface {
	Exec(ctx context.Context, sql string, args ...any) (int64, error)
	QueryInt(ctx context.Context, sql string, args ...any) (int64, error)
	QueryString(ctx context.Context, sql string, args ...any) (string, error)
	Close()
}

// Pin takes one connection out of the pool and keeps it.
//
// Close returns it. Every query on the result runs on that one backend, so its
// session state (a temp table, a `SET`) persists and its death is visible.
func (c *Client) Pin(ctx context.Context) (Session, error) {
	conn, err := c.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("pin a connection: %w", err)
	}
	return &pinned{conn: conn}, nil
}

type pinned struct {
	conn *pgxpool.Conn
}

func (p *pinned) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	tag, err := p.conn.Exec(ctx, sql, args...)
	if err != nil {
		return 0, fmt.Errorf("exec %q: %w", truncate(sql), err)
	}
	return tag.RowsAffected(), nil
}

func (p *pinned) QueryInt(ctx context.Context, sql string, args ...any) (int64, error) {
	var v int64
	if err := p.conn.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		return 0, fmt.Errorf("query int %q: %w", truncate(sql), err)
	}
	return v, nil
}

func (p *pinned) QueryString(ctx context.Context, sql string, args ...any) (string, error) {
	var v string
	if err := p.conn.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		return "", fmt.Errorf("query string %q: %w", truncate(sql), err)
	}
	return v, nil
}

// Close releases the connection back to the pool. Safe on a connection whose
// server went away: pgxpool discards it rather than reusing it.
func (p *pinned) Close() {
	if p.conn != nil {
		p.conn.Release()
		p.conn = nil
	}
}

// Close releases the pool.
func (c *Client) Close() {
	if c.pool != nil {
		c.pool.Close()
	}
}

// Pool returns the underlying pgxpool.Pool, for apps that need the pgx
// interface directly rather than this package's thin wrappers (e.g.
// workloadgen.Querier is satisfied by *pgxpool.Pool via structural typing).
func (c *Client) Pool() *pgxpool.Pool { return c.pool }

// Exec runs a statement and returns the rows affected.
func (c *Client) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	tag, err := c.pool.Exec(ctx, sql, args...)
	if err != nil {
		return 0, fmt.Errorf("exec %q: %w", truncate(sql), err)
	}
	return tag.RowsAffected(), nil
}

// CopyFrom streams rows into a table with the PostgreSQL COPY protocol and
// returns how many the server accepted. One COPY is one statement, so a
// killed compute must leave either all of these rows or none.
func (c *Client) CopyFrom(ctx context.Context, table string, columns []string, src pgx.CopyFromSource) (int64, error) {
	n, err := c.pool.CopyFrom(ctx, pgx.Identifier{table}, columns, src)
	if err != nil {
		return n, fmt.Errorf("copy into %s: %w", table, err)
	}
	return n, nil
}

// Query runs a query and calls scan for each row. scan receives the pgx.Rows.
func (c *Client) Query(ctx context.Context, scan func(pgx.Rows) error, sql string, args ...any) error {
	rows, err := c.pool.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("query %q: %w", truncate(sql), err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return fmt.Errorf("scan %q: %w", truncate(sql), err)
		}
	}
	return rows.Err()
}

// QueryInt runs a query returning a single int64.
func (c *Client) QueryInt(ctx context.Context, sql string, args ...any) (int64, error) {
	var v int64
	if err := c.pool.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		return 0, fmt.Errorf("query int %q: %w", truncate(sql), err)
	}
	return v, nil
}

// QueryString runs a query returning a single string.
func (c *Client) QueryString(ctx context.Context, sql string, args ...any) (string, error) {
	var v string
	if err := c.pool.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		return "", fmt.Errorf("query string %q: %w", truncate(sql), err)
	}
	return v, nil
}

// WaitReady polls until SELECT 1 succeeds against ci, ctx is cancelled, or the
// timeout elapses. It opens a short-lived connection per attempt so it works
// before the endpoint accepts connections.
func WaitReady(ctx context.Context, ci ConnInfo, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	wctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var last error
	for {
		select {
		case <-wctx.Done():
			if last != nil {
				return fmt.Errorf("postgres not ready at %s:%d: %w (last: %w)", ci.Host, ci.port(), wctx.Err(), last)
			}
			return fmt.Errorf("postgres not ready at %s:%d: %w", ci.Host, ci.port(), wctx.Err())
		default:
		}
		last = tryOnce(wctx, ci)
		if last == nil {
			return nil
		}
		select {
		case <-wctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func tryOnce(ctx context.Context, ci ConnInfo) error {
	attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(attempt, ci.DSN())
	if err != nil {
		return err
	}
	defer conn.Close(attempt)
	var v int
	return conn.QueryRow(attempt, "SELECT 1").Scan(&v)
}

// Addr is the host:port of the endpoint, defaults applied.
func (ci ConnInfo) Addr() string {
	return net.JoinHostPort(ci.Host, strconv.Itoa(ci.port()))
}

func truncate(s string) string {
	const max = 120
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
