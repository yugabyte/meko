package db

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Retry budget for the two ways YugabyteDB makes scratch provisioning flaky: a
// dropped keyspace whose name is not free yet, and DDL that serialization-aborts
// against DDL another test package is running concurrently (the catalog is
// cluster-wide, so separate databases do not separate them).
const (
	scratchAttempts = 6
	scratchBackoff  = 2 * time.Second
)

// ScratchDatabase creates a throwaway database named name on the server adminURL
// points at (dropping any leftover of the same name first) and returns a URL for
// it plus a func that drops it again.
//
// It exists for the Postgres-backed tests, which used to isolate themselves by
// migrating into a schema of their own. That stopped working once the migrations
// grew a second, fixed schema: the resource model follows --db-schema, but the
// task log is always `controller` (see TaskSchema), so two packages migrating two
// different resource schemas against one database would both try to create
// `controller.task` and the second would fail.
//
// Isolating by database instead is both correct and more faithful — every test now
// exercises the real schema names rather than a relocated copy. It is exported
// from non-test code so the four test packages that need it share one
// implementation, the same reason newMigrator is shared with the round-trip test.
//
// name must be a plain identifier; it is validated rather than escaped, because a
// database name cannot be parameterized in CREATE DATABASE.
func ScratchDatabase(ctx context.Context, adminURL, name string) (string, func(), error) {
	if !plainIdent(name) {
		return "", nil, fmt.Errorf("scratch database name %q is not a plain identifier", name)
	}
	scratchURL, err := withDatabase(adminURL, name)
	if err != nil {
		return "", nil, err
	}

	// DROP/CREATE run on the connection adminURL names, which must therefore be a
	// different database than the one being created.
	exec := func(ctx context.Context, stmts ...string) error {
		conn, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			return fmt.Errorf("connect to %s: %w", redactedHost(adminURL), err)
		}
		defer conn.Close(ctx)
		for _, stmt := range stmts {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}
		return nil
	}

	// Evict anything still connected before dropping. A previous run's pools are
	// not reaped synchronously, and DROP DATABASE refuses while a session is
	// attached (55006); waiting it out took longer than the retry budget in
	// practice, and there is nothing in a scratch database worth being polite to.
	evictStmt := `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
	               WHERE datname = ` + quoteLiteral(name) + ` AND pid <> pg_backend_pid()`
	dropStmt := "DROP DATABASE IF EXISTS " + quoteIdent(name)
	createStmt := "CREATE DATABASE " + quoteIdent(name)

	// Serialize against every other scratch operation on this machine: DDL on
	// YugabyteDB's cluster-wide catalog contends hard across the concurrent
	// processes `go test ./...` spawns. Held only around the DDL, never across a
	// caller's work, so the lock is never nested.
	unlock, err := lockScratchProvisioning(ctx)
	if err != nil {
		return "", nil, err
	}
	defer unlock()

	// Retry: a dropped keyspace's name is not free synchronously, and a previous
	// run's connections are not reaped synchronously either, so both the DROP and
	// the CREATE can transiently fail on a cluster that is otherwise fine.
	var lastErr error
	for attempt := range scratchAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", nil, ctx.Err()
			case <-time.After(scratchBackoff):
			}
		}
		if lastErr = exec(ctx, evictStmt, dropStmt, createStmt); lastErr == nil {
			break
		}
		if !isTransientProvisioning(lastErr) {
			return "", nil, fmt.Errorf("provision scratch database %q: %w", name, lastErr)
		}
	}
	if lastErr != nil {
		return "", nil, fmt.Errorf("provision scratch database %q: %w", name, lastErr)
	}

	drop := func() {
		dropCtx := context.WithoutCancel(ctx)
		// DROP DATABASE is DDL too, so it takes the same lock — a cleanup running
		// while another package migrates is exactly the collision this prevents.
		if unlock, err := lockScratchProvisioning(dropCtx); err == nil {
			defer unlock()
		}
		// Deliberately swallowed: a leftover scratch database is dropped by the
		// next run's DROP IF EXISTS, so failing here would only turn a passing
		// test red for a condition it cannot fix.
		_ = exec(dropCtx, evictStmt, dropStmt)
	}
	return scratchURL, drop, nil
}

// ScratchMigrated provisions a throwaway database (ScratchDatabase) and applies
// the migrations to schema, returning its URL and a drop func.
//
// It retries the whole provision-and-migrate on a serialization abort, because
// YugabyteDB's catalog is cluster-wide: two test packages migrating two *different*
// scratch databases at the same time still contend, and `go test ./...` runs
// packages in parallel. Retrying from a fresh database rather than resuming is
// what makes this safe — a half-applied migration would leave golang-migrate's
// version table dirty, and there is nothing to preserve in a scratch database.
func ScratchMigrated(ctx context.Context, adminURL, name, schema string) (string, func(), error) {
	var lastErr error
	for attempt := range scratchAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", nil, ctx.Err()
			case <-time.After(scratchBackoff):
			}
		}
		scratchURL, drop, err := ScratchDatabase(ctx, adminURL, name)
		if err != nil {
			lastErr = err
			continue
		}
		if err := migrateSerialized(ctx, scratchURL, schema); err != nil {
			drop()
			lastErr = fmt.Errorf("migrate %s: %w", schema, err)
			if isTransientProvisioning(err) {
				continue
			}
			return "", nil, lastErr
		}
		return scratchURL, drop, nil
	}
	return "", nil, fmt.Errorf("provision migrated scratch database %q: %w", name, lastErr)
}

// migrateSerialized applies the migrations under the machine-local DDL lock, so
// two packages bringing up their own scratch databases do not run CREATE/DROP
// against YugabyteDB's shared catalog at the same time.
func migrateSerialized(ctx context.Context, url, schema string) error {
	unlock, err := lockScratchProvisioning(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return Migrate(ctx, url, schema)
}

// isTransientProvisioning reports whether err is one of the conditions a
// YugabyteDB cluster raises while it catches up with its own DDL, all of which
// clear on a retry:
//
//   - 40001 serialization_failure — concurrent DDL. The catalog is cluster-wide,
//     so two test packages migrating two different scratch databases contend.
//   - 55006 object_in_use — a previous run's connections are not reaped yet, so
//     DROP DATABASE is refused.
//   - 3F000 invalid_schema_name — a freshly created schema is not visible on the
//     next pooled connection yet.
//   - "already exists" — a dropped keyspace's name is not free yet.
//
// None of these mean the caller did anything wrong; failing on them would make a
// healthy cluster look broken.
func isTransientProvisioning(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40001", "55006", "3F000":
			return true
		}
	}
	// golang-migrate wraps the driver error in an unexported type that drops the
	// chain, so fall back to the text it does preserve.
	msg := err.Error()
	for _, marker := range []string{
		"40001", "55006", "3F000",
		"could not serialize access",
		"is being accessed by other users",
		"already exists",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// lockScratchProvisioning takes an exclusive machine-local lock, returning the
// release func. Best-effort: if the lock file cannot be opened at all, callers
// proceed unserialized rather than fail, since the lock is a throughput measure
// and the retry loop is the correctness one.
func lockScratchProvisioning(ctx context.Context) (func(), error) {
	path := filepath.Join(os.TempDir(), "amp-scratch-db.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		return func() {}, nil //nolint:nilerr // best effort; see doc comment
	}
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// withDatabase rewrites a connection URL to point at a different database.
func withDatabase(raw, name string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse database url: %w", err)
	}
	u.Path = "/" + name
	return u.String(), nil
}

// redactedHost renders just the host of a connection URL, for error messages that
// must not leak credentials.
func redactedHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "the configured database"
	}
	return u.Host
}

// plainIdent reports whether s is a lowercase-alphanumeric-plus-underscore
// identifier starting with a letter — the shape a scratch database name may take.
func plainIdent(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	const allowed = "abcdefghijklmnopqrstuvwxyz0123456789_"
	for _, r := range s {
		if !strings.ContainsRune(allowed, r) {
			return false
		}
	}
	return true
}
