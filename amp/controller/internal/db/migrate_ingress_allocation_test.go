package db

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/controller/migrations"
)

// TestMigrateIngressAllocationInvariants pins the three properties of 0007 that
// a round-trip walk cannot see, because each one is silent when broken and each
// one is the sort of thing a later "tidy-up" removes in good faith.
//
//  1. The slot index is UNIQUE among LIVE rows. Unique, because two live
//     endpoints sharing a (shard, port) is a connection reaching another
//     customer's database. Partial, because retiring an endpoint has to give the
//     port back — an index over every row would block a slot forever and leak
//     capacity a shard at a time.
//
//  2. `endpoint_id` carries NO foreign key, deliberately. Endpoints are
//     hard-deleted, so a cascading reference would take the allocation with them
//     and free the slot the instant the endpoint went away — defeating the
//     quarantine that stops a client with a cached address landing on whoever
//     takes it next. The absence is load-bearing, so it is asserted rather than
//     left to a comment nobody reads before adding the "missing" constraint.
//
//  3. `shard` DOES carry one, so a shard cannot be dropped while allocations —
//     quarantined ones included — still point at it and its DNS records may
//     still resolve.
func TestMigrateIngressAllocationInvariants(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AMP_TEST_DATABASE_URL not set; these invariants need a YugabyteDB")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	scratchURL, drop, err := ScratchDatabase(ctx, url, "amp_test_ingress_alloc")
	if err != nil {
		t.Fatalf("provision scratch database: %v", err)
	}
	t.Cleanup(drop)

	unlock, err := lockScratchProvisioning(ctx)
	if err != nil {
		t.Fatalf("take DDL lock: %v", err)
	}
	t.Cleanup(unlock)

	const schema = "controller" // the --db-schema default

	m, sqlDB, err := newMigrator(ctx, scratchURL, schema, migrations.FS)
	if err != nil {
		t.Fatalf("build migrator: %v", err)
	}
	defer sqlDB.Close()

	version := migrationVersion(t, "_ingress_allocation.up.sql")
	if err := m.Migrate(version); err != nil {
		t.Fatalf("migrate up to %d: %v", version, err)
	}

	pool, err := Connect(ctx, scratchURL, schema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx,
		`INSERT INTO amp_registry.ingress_shards
		     (name, family, state, hostname, port_lo, port_hi)
		 VALUES ('pgin-0', 'internal', 'ready', 'lb-0.example', 20000, 20049)`,
	); err != nil {
		t.Fatalf("seed shard: %v", err)
	}

	allocate := func(endpoint uuid.UUID, port int, dnsName string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO amp_registry.ingress_allocations (endpoint_id, shard, port, dns_name)
			 VALUES ($1::uuid, 'pgin-0', $2, $3)`,
			endpoint.String(), port, dnsName)
		return err
	}
	isUniqueViolation := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "23505"
	}

	first := uuid.New()
	if err := allocate(first, 20007, "ep-first.pg.example"); err != nil {
		t.Fatalf("first allocation: %v", err)
	}

	// (1a) Unique among live rows.
	if err := allocate(uuid.New(), 20007, "ep-second.pg.example"); !isUniqueViolation(err) {
		t.Fatalf("second live endpoint on (pgin-0, 20007) = %v, want a 23505 unique_violation: "+
			"two databases would be reachable on one address", err)
	}
	if err := allocate(uuid.New(), 20008, "ep-first.pg.example"); !isUniqueViolation(err) {
		t.Fatalf("second live endpoint under one dns_name = %v, want a 23505 unique_violation", err)
	}
	if err := allocate(first, 20009, "ep-first-again.pg.example"); !isUniqueViolation(err) {
		t.Fatalf("second live allocation for one endpoint = %v, want a 23505 unique_violation", err)
	}

	// (3) The shard reference holds while anything points at it.
	if _, err := pool.Exec(ctx,
		`DELETE FROM amp_registry.ingress_shards WHERE name = 'pgin-0'`); err == nil {
		t.Fatal("dropped a shard that still has allocations; its DNS records may still resolve")
	}

	// (1b) ...and partial, so releasing gives the slot back.
	if _, err := pool.Exec(ctx,
		`UPDATE amp_registry.ingress_allocations SET released_at = now() WHERE endpoint_id = $1::uuid`,
		first.String()); err != nil {
		t.Fatalf("release the first allocation: %v", err)
	}
	if err := allocate(uuid.New(), 20007, "ep-third.pg.example"); err != nil {
		t.Fatalf("re-allocating a released slot = %v, want success: an index over every row "+
			"rather than only live ones leaks a port per retire", err)
	}

	// The released row must still be there afterwards — it is what the quarantine
	// window is computed from.
	var released int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM amp_registry.ingress_allocations WHERE released_at IS NOT NULL`,
	).Scan(&released); err != nil {
		t.Fatalf("count released allocations: %v", err)
	}
	if released != 1 {
		t.Fatalf("released allocations = %d, want 1: the row a re-allocation replaced must survive "+
			"or there is nothing to quarantine against", released)
	}

	// (2) No foreign key on endpoint_id. Asserted against the catalog because the
	// property is an ABSENCE, and the next person to notice it will be holding a
	// patch that adds one.
	var endpointFKs int
	if err := pool.QueryRow(ctx,
		`SELECT count(*)
		   FROM pg_constraint c
		   JOIN pg_class t ON t.oid = c.conrelid
		   JOIN pg_namespace n ON n.oid = t.relnamespace
		   JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = ANY (c.conkey)
		  WHERE n.nspname = 'amp_registry'
		    AND t.relname = 'ingress_allocations'
		    AND c.contype = 'f'
		    AND a.attname = 'endpoint_id'`,
	).Scan(&endpointFKs); err != nil {
		t.Fatalf("inspect constraints: %v", err)
	}
	if endpointFKs != 0 {
		t.Fatalf("endpoint_id has %d foreign key(s), want 0: endpoints are hard-deleted, so a "+
			"reference frees the slot the moment the endpoint goes and the quarantine never runs",
			endpointFKs)
	}
}
