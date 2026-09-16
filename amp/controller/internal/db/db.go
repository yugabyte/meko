// Package db owns the YBDB/PostgreSQL connection pool and schema migrations.
// YBDB is the controller's only state store (amp-controller.md §10) and
// speaks the PostgreSQL wire protocol.
package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ResourceSchema is the schema holding the control-plane resource model: users,
// storage pools, clusters, and the single `branches` / `endpoints` tables that
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

// Bounds that keep a backing reschedule from stranding the pool on sockets whose
// peer no longer exists (#961).
//
// When the backing pod is replaced, the pool still holds ESTABLISHED sockets to
// the old pod IP. Nothing closed them — the peer vanished rather than sending a
// RST — so a write to one blocks in kernel retransmit for minutes. pgx does set
// most of these bounds, but at values sized for connection hygiene rather than
// for a peer that disappeared (1h lifetime, 30m idle, 1m health check); only
// PingTimeout is genuinely unbounded, and it is the one on the critical path.
// The failure is quiet: the caller sees a bare `context deadline exceeded` with
// no address in it, because the operation never reached a dial.
//
// Observed on run 44551e5d: from 23:03:26 to 23:05:07 — where the collected
// controller log ends — every /readyz took exactly its 1s probe budget and every
// metrics scrape exactly its 5s, and no dial was logged in that window at all.
// Each call discovered one dead socket, spent its whole deadline on it,
// destroyed it and gave up, so the pool drained one connection per caller rather
// than all at once and kubernetes had the controller out of its Service
// throughout. The data plane was already serving: the test committed rows
// through the endpoints 52s before /readyz went green.
//
// The log stops before the recovery, so it does not settle everything. #961 also
// quotes a GC sweep at 23:05:29 — past the end of this log — dialing the departed
// IP by name, 2m09s after the replacement pod came up. Whatever still resolved to
// that address then is a separate path, and these bounds do not close it; they
// only cap each attempt at connectTimeout.
const (
	// connectTimeout bounds a single dial. pgxpool already forces 2 minutes when
	// the config leaves it unset (pgxpool/pool.go:277), so the dial was capped —
	// just two orders of magnitude above anything useful, and close enough to the
	// observed outage to be worth naming rather than leaving implied. 5s is
	// ysql_client's DEFAULT_CONNECT_TIMEOUT_MS, so both languages give a dial the
	// same budget.
	connectTimeout = 5 * time.Second

	// pingTimeout bounds the liveness ping pgxpool runs on acquire — the only one
	// of these pgx leaves genuinely unbounded, so one dead socket consumed the
	// caller's entire budget.
	//
	// It is a backstop, not the primary detector: TCP_USER_TIMEOUT (dialer.go)
	// fails a departed peer's sockets in the kernel within 3s, after which a ping
	// errors immediately and the pool drains in one acquire loop. That leaves the
	// bound free to be generous, which it needs to be — the default ShouldPing
	// fires on any connection idle over 1s, i.e. every /readyz probe at
	// periodSeconds 5, so a tight bound would have a merely-slow store destroy and
	// re-dial its whole idle pool at the moment it can least afford it. A tablet
	// leader election on an RF=3 backing routinely outruns a few hundred
	// milliseconds; 2s clears that while still bounding the pathological case.
	pingTimeout = 2 * time.Second

	// maxConnIdleTime and healthCheckPeriod tighten the background health check
	// that already runs, from pgx's 30m/1m to 2m/15s, so stale idle connections
	// are reaped off the critical path on a cadence that matters during an
	// outage. That check is purely time-based (it never pings), so it cannot
	// itself hang on a dead socket.
	maxConnIdleTime   = 2 * time.Minute
	healthCheckPeriod = 15 * time.Second

	// maxConnLifetime bounds staleness overall, and the jitter keeps a pool — and
	// a fleet — from recycling in lockstep. pgx defaults the lifetime to 1h and
	// the jitter to zero. Both numbers here are the compute side's:
	// 15 minutes is ysql_client's DEFAULT_CONNECTION_LIFETIME_MS, and a quarter
	// of the base reproduces the 0–25% envelope its jittered() spreads over.
	// pgxpool draws uniformly from [0, jitter) and adds it, so the distribution
	// matches as well as the bound does.
	//
	// The compute side needs the bound for a reason this pool does not share —
	// the YB smart driver discovers its topology once per connection, so a
	// connection outliving a tserver rename keeps dialing names that no longer
	// resolve (#811). pgx has no such cache. Sharing the number anyway costs one
	// reconnect per connection per quarter hour and means there is a single
	// answer to "how stale can a connection get".
	maxConnLifetime       = 15 * time.Minute
	maxConnLifetimeJitter = maxConnLifetime / 4
)

// The bounds above all work by having a caller wait and then give up, so the
// pool sheds dead connections one call at a time. dialer.go adds the other half:
// socket options that let the KERNEL fail every dead connection at once, which
// is what turns recovery from "one per probe" into a single window.

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
	applyPoolBounds(cfg)
	if err := installClientCertReload(cfg, url); err != nil {
		return nil, err
	}

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

// applyPoolBounds installs the bounds above. A DSN that sets connect_timeout
// keeps its value as the dial deadline, but the dialer is replaced either way:
// it is what carries the socket-level bounds, and those cannot be expressed as a
// connection-string setting. Deadline and dialer are built from the same value
// so the pair cannot disagree.
func applyPoolBounds(cfg *pgxpool.Config) {
	dialTimeout := cfg.ConnConfig.ConnectTimeout
	if dialTimeout == 0 {
		dialTimeout = connectTimeout
		cfg.ConnConfig.ConnectTimeout = dialTimeout
	}
	cfg.ConnConfig.DialFunc = dialFuncWithTimeout(dialTimeout)
	cfg.PingTimeout = pingTimeout
	cfg.MaxConnIdleTime = maxConnIdleTime
	cfg.HealthCheckPeriod = healthCheckPeriod
	cfg.MaxConnLifetime = maxConnLifetime
	cfg.MaxConnLifetimeJitter = maxConnLifetimeJitter
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
