// Package testdb builds throwaway PostgreSQL schemas for the tests. It is imported only by _test files.
//
// A test gets a schema of its own holding the tables of api_server migration 000029 (the event log) and,
// on request, the bookkeeping tables the handlers write, so the SQL runs for real. The YugabyteDB-only key
// syntax (HASH, ASC) is left out so it runs on plain PostgreSQL.
package testdb

import (
	"crypto/rand"
	"database/sql"
	_ "embed" // bookkeeping.sql
	"encoding/hex"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq" // registers the "postgres" driver
)

//go:embed bookkeeping.sql
var bookkeepingSQL string

// Placeholder is the schema name inside SQL passed to DB.Exec and DB.Query.
const Placeholder = "__S__"

// EventLogSQL creates the event log tables in the schema that Placeholder stands for: events with monthly
// partitions around today plus a default partition, event_consumers seeded for shards 0 to 15,
// event_dead_letters and event_applied.
const EventLogSQL = `
CREATE TABLE __S__.events (
    shard smallint NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    id uuid NOT NULL, type text NOT NULL, event_schema_version smallint NOT NULL DEFAULT 1, payload jsonb NOT NULL,
    PRIMARY KEY (shard, created_at, id)) PARTITION BY RANGE (created_at);
CREATE TABLE __S__.events_default PARTITION OF __S__.events DEFAULT;
DO $$
DECLARE lo timestamp;
BEGIN
  FOR i IN -2..5 LOOP
    lo := date_trunc('month', now() AT TIME ZONE 'UTC') + make_interval(months => i);
    EXECUTE format('CREATE TABLE __S__.%I PARTITION OF __S__.events FOR VALUES FROM (%L) TO (%L)',
      'events_' || to_char(lo, 'YYYY_MM'), (lo AT TIME ZONE 'UTC')::text,
      ((lo + interval '1 month') AT TIME ZONE 'UTC')::text);
  END LOOP;
END $$;
CREATE TABLE __S__.event_consumers (
    consumer text NOT NULL, shard smallint NOT NULL, offset_ts timestamptz NOT NULL, offset_id uuid NOT NULL,
    lease_owner uuid, lease_expires_at timestamptz, updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, shard));
INSERT INTO __S__.event_consumers (consumer, shard, offset_ts, offset_id)
    SELECT 'analytics', s, now() - interval '1 day', '00000000-0000-0000-0000-000000000000' FROM generate_series(0, 15) s;
CREATE TABLE __S__.event_dead_letters (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(), consumer text NOT NULL, shard smallint NOT NULL,
    event_id uuid NOT NULL, event_created_at timestamptz NOT NULL, type text NOT NULL,
    event_schema_version smallint NOT NULL, payload jsonb NOT NULL, error text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now());
CREATE INDEX ON __S__.event_dead_letters (created_at);
CREATE TABLE __S__.event_applied (
    consumer text NOT NULL, shard smallint NOT NULL, created_at timestamptz NOT NULL, event_id uuid NOT NULL,
    PRIMARY KEY (shard, consumer, created_at, event_id));
`

// BookkeepingSQL creates the tables the handlers write, in the schema that Placeholder stands for.
func BookkeepingSQL() string { return strings.ReplaceAll(bookkeepingSQL, "meko_system", Placeholder) }

// DB is a throwaway schema and an administrative connection to its database.
type DB struct {
	T      *testing.T
	DSN    string
	Schema string
	Admin  *sql.DB
}

// New creates a schema from the given SQL (written with Placeholder) and drops it when the test ends.
// The test is skipped when EVENTS_TEST_DSN is not set.
func New(t *testing.T, ddl ...string) *DB {
	t.Helper()
	dsn := os.Getenv("EVENTS_TEST_DSN")
	if dsn == "" {
		t.Skip("EVENTS_TEST_DSN not set")
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	d := &DB{T: t, DSN: dsn, Schema: "_ev_" + hex.EncodeToString(b), Admin: admin}
	// Registered before any DDL runs, so a schema whose setup fails (YugabyteDB can refuse DDL that races another
	// session's) is still dropped and does not pile up in the database.
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP SCHEMA IF EXISTS " + d.Schema + " CASCADE")
		_ = admin.Close()
	})
	d.Exec("CREATE SCHEMA " + Placeholder)
	for _, q := range ddl {
		d.Exec(q)
	}
	return d
}

// SQL replaces Placeholder with this DB's schema name.
func (d *DB) SQL(q string) string { return strings.ReplaceAll(q, Placeholder, d.Schema) }

// Exec runs a statement (or several, when there are no arguments) with the placeholder replaced.
func (d *DB) Exec(q string, args ...any) {
	d.T.Helper()
	if _, err := d.Admin.Exec(d.SQL(q), args...); err != nil {
		d.T.Fatal(err)
	}
}
