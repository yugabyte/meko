package db

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestMigrateStoragePoolNameIsUniqueAmongLivePools pins the schema's pool-name policy,
// which is an index rather than a column constraint and so fails silently when
// wrong. `storage_pools_name_live` is the only thing keeping names unique: two
// live pools must not share one, a deleted pool must free its name, and any number
// of deleted pools may keep the same one — a whole-table UNIQUE would hold a
// deleted pool's name forever.
func TestMigrateStoragePoolNameIsUniqueAmongLivePools(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AMP_TEST_DATABASE_URL not set; the index needs a YugabyteDB")
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
	if err := Migrate(ctx, scratchURL, schema); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := Connect(ctx, scratchURL, schema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	owner := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO amp_registry.users (id) VALUES ($1::uuid)`,
		owner.String()); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	insert := func(name string) (uuid.UUID, error) {
		id := uuid.New()
		_, err := pool.Exec(ctx,
			`INSERT INTO amp_registry.storage_pools (id, name, owner_user_id)
			 VALUES ($1::uuid, $2, $3::uuid)`,
			id.String(), name, owner.String())
		return id, err
	}
	tombstone := func(id uuid.UUID) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`UPDATE amp_registry.storage_pools SET dropped_at = now() WHERE id = $1::uuid`,
			id.String()); err != nil {
			t.Fatalf("tombstone pool %s: %v", id, err)
		}
	}
	isUniqueViolation := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "23505"
	}

	first, err := insert("orders")
	if err != nil {
		t.Fatalf("first live pool: %v", err)
	}
	if _, err := insert("orders"); !isUniqueViolation(err) {
		t.Fatalf("a second live pool named 'orders' = %v, want a 23505 unique_violation", err)
	}

	// Deleting frees the name.
	tombstone(first)
	second, err := insert("orders")
	if err != nil {
		t.Fatalf("re-using a deleted pool's name: %v", err)
	}

	// And a name deleted twice does not collide with its own history.
	tombstone(second)
	if _, err := insert("orders"); err != nil {
		t.Fatalf("a third 'orders', with two deleted ones behind it: %v", err)
	}
}
