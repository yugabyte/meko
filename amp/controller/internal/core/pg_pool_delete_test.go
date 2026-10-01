package core

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
)

// TestPgStoragePoolDeleteRename covers what MemBackend cannot: pools are
// tombstoned rather than removed, so these assertions are all about the real
// schema — the partial unique index freeing a dropped pool's name, and a
// tombstoned cluster's surviving FK not blocking its pool's delete (#1083).
//
// Exercises the real PgBackend SQL against a Postgres/YSQL-compatible database.
// It provisions an isolated database per run and drops it afterwards. Set
// AMP_TEST_DATABASE_URL to run; skipped otherwise so `go test ./...` stays green
// without a database.
func TestPgStoragePoolDeleteRename(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AMP_TEST_DATABASE_URL (e.g. postgres://yugabyte@host:5433/yb_storage?sslmode=disable) to run")
	}
	const schema = "controller" // the --db-schema default
	provCtx, provCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer provCancel()
	scratchURL, drop, err := db.ScratchMigrated(provCtx, url, "amp_test_core_pool_delete", schema)
	if err != nil {
		t.Fatalf("provision scratch database: %v", err)
	}
	t.Cleanup(drop)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx, scratchURL, schema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	owner := uuid.New()
	mustExec(t, ctx, pool, `INSERT INTO amp_registry.users (id) VALUES ($1)`, owner)
	b := NewPgBackend(pool)

	t.Run("delete frees the name for a new pool", func(t *testing.T) {
		p, err := b.CreateStoragePool(ctx, owner, "recyclable")
		if err != nil {
			t.Fatalf("CreateStoragePool: %v", err)
		}
		if err := b.DeleteStoragePool(ctx, p.ID); err != nil {
			t.Fatalf("DeleteStoragePool: %v", err)
		}
		if _, err := b.GetStoragePool(ctx, p.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetStoragePool after delete = %v, want ErrNotFound", err)
		}
		// The tombstone kept the row, so this only works if storage_pools_name_live
		// scopes uniqueness to dropped_at IS NULL.
		reborn, err := b.CreateStoragePool(ctx, owner, "recyclable")
		if err != nil {
			t.Fatalf("re-create with the freed name: %v", err)
		}
		if reborn.ID == p.ID {
			t.Error("re-created pool reused the tombstoned pool's id")
		}
		t.Cleanup(func() { _ = b.DeleteStoragePool(ctx, reborn.ID) })
	})

	t.Run("a live cluster blocks the delete, a tombstoned one does not", func(t *testing.T) {
		p, err := b.CreateStoragePool(ctx, owner, "with-cluster")
		if err != nil {
			t.Fatalf("CreateStoragePool: %v", err)
		}
		c, err := b.CreateCluster(ctx, p.ID, "db", 18)
		if err != nil {
			t.Fatalf("CreateCluster: %v", err)
		}
		if err := b.DeleteStoragePool(ctx, p.ID); !errors.Is(err, ErrConflict) {
			t.Fatalf("DeleteStoragePool with a live cluster = %v, want ErrConflict", err)
		}
		if err := b.DeleteCluster(ctx, c.ID); err != nil {
			t.Fatalf("DeleteCluster: %v", err)
		}
		if err := b.DeleteStoragePool(ctx, p.ID); err != nil {
			t.Fatalf("DeleteStoragePool after the cluster was tombstoned: %v", err)
		}
		// The cluster row outlives both, still pointing at the tombstoned pool.
		var live bool
		if err := pool.QueryRow(ctx,
			`SELECT dropped_at IS NULL FROM amp_registry.clusters WHERE id = $1`, c.ID).Scan(&live); err != nil {
			t.Fatalf("read cluster tombstone: %v", err)
		}
		if live {
			t.Error("cluster row should still be tombstoned after its pool was deleted")
		}
	})

	t.Run("delete is not repeatable", func(t *testing.T) {
		p, err := b.CreateStoragePool(ctx, owner, "once")
		if err != nil {
			t.Fatalf("CreateStoragePool: %v", err)
		}
		if err := b.DeleteStoragePool(ctx, p.ID); err != nil {
			t.Fatalf("DeleteStoragePool: %v", err)
		}
		if err := b.DeleteStoragePool(ctx, p.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("second DeleteStoragePool = %v, want ErrNotFound", err)
		}
	})

	t.Run("an id held by a tombstoned pool is a conflict, not an idempotent re-create", func(t *testing.T) {
		id := uuid.New()
		if _, err := b.CreateStoragePoolWithID(ctx, id, owner, "explicit-id"); err != nil {
			t.Fatalf("CreateStoragePoolWithID: %v", err)
		}
		if err := b.DeleteStoragePool(ctx, id); err != nil {
			t.Fatalf("DeleteStoragePool: %v", err)
		}
		if _, err := b.CreateStoragePoolWithID(ctx, id, owner, "explicit-id-again"); !errors.Is(err, ErrConflict) {
			t.Errorf("re-create on a tombstoned id = %v, want ErrConflict", err)
		}
	})

	// The interlock EnsureCluster carries. Without it both statements commit —
	// the delete's guard is a read, and EnsureCluster writes only cluster/branch
	// rows, which a tombstoned pool's FK still satisfies — leaving a live cluster
	// under a tombstoned pool that no `dropped_at IS NULL` reader can see and no
	// cascade ever enumerates.
	t.Run("a cluster create racing a pool delete cannot land under a tombstone", func(t *testing.T) {
		p, err := b.CreateStoragePool(ctx, owner, "raced")
		if err != nil {
			t.Fatalf("CreateStoragePool: %v", err)
		}
		if err := b.DeleteStoragePool(ctx, p.ID); err != nil {
			t.Fatalf("DeleteStoragePool: %v", err)
		}
		// Serialized rather than concurrent: the delete has already committed, so
		// EnsureCluster's own liveness write is the only thing that can refuse it.
		// The concurrent ordering is what the write turns into a serialization
		// abort; this asserts the guard the abort falls back on.
		err = b.EnsureCluster(ctx, p.ID, uuid.New(), uuid.New(), "orphan", 18, "", nil, nil)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("EnsureCluster into a tombstoned pool = %v, want ErrNotFound", err)
		}
		var clusters int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM amp_registry.clusters WHERE storage_pool_id = $1`, p.ID).Scan(&clusters); err != nil {
			t.Fatalf("count clusters: %v", err)
		}
		if clusters != 0 {
			t.Errorf("tombstoned pool has %d cluster rows, want 0 — the insert was not rolled back", clusters)
		}
	})

	t.Run("rename releases the old name and refuses a live one", func(t *testing.T) {
		p, err := b.CreateStoragePool(ctx, owner, "before")
		if err != nil {
			t.Fatalf("CreateStoragePool: %v", err)
		}
		other, err := b.CreateStoragePool(ctx, owner, "taken")
		if err != nil {
			t.Fatalf("CreateStoragePool(taken): %v", err)
		}
		renamed, err := b.RenameStoragePool(ctx, p.ID, "after")
		if err != nil {
			t.Fatalf("RenameStoragePool: %v", err)
		}
		if renamed.Name != "after" || renamed.ID != p.ID || !renamed.CreatedAt.Equal(p.CreatedAt) {
			t.Errorf("renamed = %+v, want the same pool named after", renamed)
		}
		if _, err := b.CreateStoragePool(ctx, owner, "before"); err != nil {
			t.Errorf("the vacated name should be free: %v", err)
		}
		if _, err := b.RenameStoragePool(ctx, p.ID, "taken"); !errors.Is(err, ErrConflict) {
			t.Errorf("rename onto a live name = %v, want ErrConflict", err)
		}
		if _, err := b.RenameStoragePool(ctx, uuid.New(), "whatever"); !errors.Is(err, ErrNotFound) {
			t.Errorf("rename of a missing pool = %v, want ErrNotFound", err)
		}
		// A tombstoned pool's name is claimable by rename, not just by create.
		if err := b.DeleteStoragePool(ctx, other.ID); err != nil {
			t.Fatalf("DeleteStoragePool(taken): %v", err)
		}
		if _, err := b.RenameStoragePool(ctx, p.ID, "taken"); err != nil {
			t.Errorf("rename onto a tombstoned pool's name: %v", err)
		}
	})
}
