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

// TestMigrateStoragePoolNameLiveCoversExistingPools exercises what
// TestMigrateRoundTrip cannot: `storage_pools_name_live` built over pools that
// were already there. That walk runs against an EMPTY database, so the index has
// nothing to backfill and a build that skipped every pre-existing row would ship
// unnoticed — and this pair lands on a deployment with pools already in it.
//
// Two properties, both silent when broken. The index must COVER rows that predate
// it: once 0015 drops the table-level UNIQUE it is the only thing keeping pool
// names unique, so an index missing the existing rows lets two live pools take one
// name. And its predicate must actually be partial, or a delete frees no name and
// the whole policy is inert.
//
// This does NOT assert the 0015/0016 split — it passes with the two merged (see
// 0016's header for why the split is kept regardless).
func TestMigrateStoragePoolNameLiveCoversExistingPools(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AMP_TEST_DATABASE_URL not set; the index backfill needs a YugabyteDB")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	scratchURL, drop, err := ScratchDatabase(ctx, url, "amp_test_pool_name_live")
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

	// The migrator pins itself to one connection and holds it for the session, so
	// seeding/reading on the same *sql.DB would block forever. Each phase gets its
	// own pool, closed before the next migration step.
	withPool := func(what string, fn func(pool *Pool)) {
		t.Helper()
		pool, err := Connect(ctx, scratchURL, schema)
		if err != nil {
			t.Fatalf("connect (%s): %v", what, err)
		}
		defer pool.Close()
		fn(pool)
	}

	m, sqlDB, err := newMigrator(ctx, scratchURL, schema, migrations.FS)
	if err != nil {
		t.Fatalf("build migrator: %v", err)
	}
	defer sqlDB.Close()

	tombstoneVersion := migrationVersion(t, "_storage_pool_tombstone.up.sql")
	indexVersion := migrationVersion(t, "_storage_pool_name_live.up.sql")
	if indexVersion <= tombstoneVersion {
		t.Fatalf("the index migration (%d) must follow the tombstone one (%d)", indexVersion, tombstoneVersion)
	}

	if err := m.Migrate(indexVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", indexVersion, err)
	}
	// Back over both, so the seeded pools are in the shape a real deployment has
	// when this pair is about to run: name UNIQUE, no dropped_at.
	if err := m.Steps(-2); err != nil {
		t.Fatalf("step back over %d and %d: %v", indexVersion, tombstoneVersion, err)
	}
	if v, _, err := m.Version(); err != nil || v >= tombstoneVersion {
		t.Fatalf("after stepping back: version=%d err=%v, want below %d", v, err, tombstoneVersion)
	}

	owner := uuid.New()
	existing := uuid.New()
	withPool("seed", func(pool *Pool) {
		if _, err := pool.Exec(ctx, `INSERT INTO amp_registry.users (id) VALUES ($1::uuid)`,
			owner.String()); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO amp_registry.storage_pools (id, name, owner_user_id)
			 VALUES ($1::uuid, 'pre-existing', $2::uuid)`,
			existing.String(), owner.String()); err != nil {
			t.Fatalf("seed pool: %v", err)
		}
	})

	if err := m.Steps(2); err != nil {
		t.Fatalf("re-apply %d and %d: %v", tombstoneVersion, indexVersion, err)
	}

	withPool("assert", func(pool *Pool) {
		insert := func(name string) error {
			_, err := pool.Exec(ctx,
				`INSERT INTO amp_registry.storage_pools (id, name, owner_user_id)
				 VALUES ($1::uuid, $2, $3::uuid)`,
				uuid.New().String(), name, owner.String())
			return err
		}

		// The index must SEE the row that predates it.
		err := insert("pre-existing")
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("second live pool named 'pre-existing' = %v, want a 23505 unique_violation "+
				"(the index did not cover the pre-existing row)", err)
		}

		// And the predicate must actually be partial, or delete frees no name.
		if _, err := pool.Exec(ctx,
			`UPDATE amp_registry.storage_pools SET dropped_at = now() WHERE id = $1::uuid`,
			existing.String()); err != nil {
			t.Fatalf("tombstone the seeded pool: %v", err)
		}
		if err := insert("pre-existing"); err != nil {
			t.Fatalf("re-using a tombstoned pool's name: %v", err)
		}
	})
}
