package core

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
	"github.com/yugabyte/amp/controller/internal/registry"
)

// TestPgBranchExtensionsInheritance covers the externalized-extension branch
// state end-to-end against a real Postgres/YSQL database: the opt-in union
// (AddBranchExtensions), the durable read (BranchExtensions), and — the point of
// the review ask — that a FORK inherits its parent's opted-in set. The set lives
// on amp_registry.branches — the one branch table both surfaces share — and fork
// inheritance is a snapshot RecordBranch copies from the ancestor at create time.
//
// This is the "verify branching" integration coverage for extensions that
// yb_amp_local can't do end-to-end (the local Docker path has no ImageVolumes,
// so it can't actually CREATE EXTENSION); actual CREATE EXTENSION + fork on a
// mounted volume is covered by the GKE smoke test (deploy/smoke-test.sh,
// SMOKE_EXTENSIONS=1).
//
// Exercises the real PgBackend + registry.Store SQL against a throwaway DATABASE
// (not a schema: the migrations create two fixed schemas, so relocating one
// isolates nothing). Set AMP_TEST_DATABASE_URL to run; skipped otherwise so
// `go test ./...` stays green without a database.
func TestPgBranchExtensionsInheritance(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AMP_TEST_DATABASE_URL (e.g. postgres://yugabyte@host:5433/yb_storage?sslmode=disable) to run")
	}
	// Provisioning gets a budget of its own: ScratchMigrated queues behind a lock
	// that serializes DDL across test packages, which says nothing about this
	// test's own latency.
	const schema = "controller" // the --db-schema default
	provCtx, provCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer provCancel()
	scratchURL, drop, err := db.ScratchMigrated(provCtx, url, "amp_test_core_extensions", schema)
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

	store := registry.NewStore(pool)
	b := NewPgBackend(pool)

	// A branch's row is created by RecordBranch — the shared choke point both the
	// controller and yb_amp_local reach. Branches are cluster-scoped now, so the
	// cluster has to exist: the branch -> cluster FK is what enforces it.
	clusterID := seedCoreCluster(t, ctx, pool)
	rootID := uuid.New()
	rootHex := registry.FormatBranchID(rootID)

	recordBranch := func(id uuid.UUID, name string, ancestorHex *string, startLSN string) {
		t.Helper()
		if err := store.RecordBranch(ctx, clusterID, registry.FormatBranchID(id), registry.BranchCreateRecord{
			BranchName:       name,
			PgVersion:        18,
			AncestorBranchID: ancestorHex,
			StartLSN:         startLSN,
		}); err != nil {
			t.Fatalf("RecordBranch(%s): %v", name, err)
		}
	}
	mustExts := func(id uuid.UUID) []string {
		t.Helper()
		got, err := b.BranchExtensions(ctx, clusterID, id)
		if err != nil {
			t.Fatalf("BranchExtensions: %v", err)
		}
		return got
	}

	// --- root branch: no opt-in yet -> empty set. ---
	recordBranch(rootID, "main", nil, "0/0")
	if got := mustExts(rootID); len(got) != 0 {
		t.Fatalf("fresh branch extensions = %v, want none", got)
	}

	// --- opt in (idempotent absolute union). ---
	if err := b.AddBranchExtensions(ctx, clusterID, rootID, []string{"pgvector"}); err != nil {
		t.Fatalf("AddBranchExtensions: %v", err)
	}
	if got := mustExts(rootID); !reflect.DeepEqual(got, []string{"pgvector"}) {
		t.Fatalf("root extensions = %v, want [pgvector]", got)
	}

	// --- fork BEFORE any further opt-in: the child inherits the parent's set as
	// a snapshot RecordBranch copies from the ancestor. ---
	childID := uuid.New()
	recordBranch(childID, "b1", &rootHex, "0/16B8")
	if got := mustExts(childID); !reflect.DeepEqual(got, []string{"pgvector"}) {
		t.Fatalf("forked child extensions = %v, want inherited [pgvector]", got)
	}

	// --- child opts into more (a mix including hexabridge); the union is
	// sorted+deduped and the parent is unaffected (the snapshot is a copy, not a
	// live pointer). Sort is lexical: hexabridge < pgvector < rum. ---
	if err := b.AddBranchExtensions(ctx, clusterID, childID, []string{"rum", "pgvector", "hexabridge"}); err != nil {
		t.Fatalf("AddBranchExtensions(child): %v", err)
	}
	if got := mustExts(childID); !reflect.DeepEqual(got, []string{"hexabridge", "pgvector", "rum"}) {
		t.Fatalf("child extensions = %v, want sorted union [hexabridge pgvector rum]", got)
	}
	if got := mustExts(rootID); !reflect.DeepEqual(got, []string{"pgvector"}) {
		t.Fatalf("parent extensions = %v after child add, want unchanged [pgvector]", got)
	}

	// --- re-adding is idempotent (absolute union, no double-apply). ---
	if err := b.AddBranchExtensions(ctx, clusterID, childID, []string{"pgvector", "rum", "hexabridge"}); err != nil {
		t.Fatalf("AddBranchExtensions(idempotent): %v", err)
	}
	if got := mustExts(childID); !reflect.DeepEqual(got, []string{"hexabridge", "pgvector", "rum"}) {
		t.Fatalf("child extensions after idempotent re-add = %v, want [hexabridge pgvector rum]", got)
	}

	// --- a grandchild forked AFTER the child's extra opt-in inherits the fuller
	// set (including hexabridge); a second fork off the (still single-extension)
	// root does not. ---
	grandID := uuid.New()
	childHex := registry.FormatBranchID(childID)
	recordBranch(grandID, "b2", &childHex, "0/1C90")
	if got := mustExts(grandID); !reflect.DeepEqual(got, []string{"hexabridge", "pgvector", "rum"}) {
		t.Fatalf("grandchild extensions = %v, want inherited [hexabridge pgvector rum]", got)
	}
	sibID := uuid.New()
	recordBranch(sibID, "b3", &rootHex, "0/16B8")
	if got := mustExts(sibID); !reflect.DeepEqual(got, []string{"pgvector"}) {
		t.Fatalf("second root fork extensions = %v, want inherited [pgvector]", got)
	}

	// --- an absent branch reads empty and is a no-op to add (mirrors the
	// controller's degrade-to-defaults contract). ---
	absent := uuid.New()
	if got := mustExts(absent); len(got) != 0 {
		t.Fatalf("absent branch extensions = %v, want none", got)
	}
	if err := b.AddBranchExtensions(ctx, clusterID, absent, []string{"pgvector"}); err != nil {
		t.Fatalf("AddBranchExtensions(absent): %v", err)
	}
	if got := mustExts(absent); len(got) != 0 {
		t.Fatalf("absent branch still absent = %v, want none", got)
	}
}

// seedCoreCluster inserts the user -> pool -> cluster ladder a branch needs
// and returns the cluster id. Names carry the pool id because storage_pool names
// are unique cluster-wide and this database may be reused across tests.
func seedCoreCluster(t *testing.T, ctx context.Context, pool *db.Pool) uuid.UUID {
	t.Helper()
	owner, poolID, clusterID := uuid.New(), uuid.New(), uuid.New()
	name := "core-test-" + poolID.String()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO amp_registry.users (id) VALUES ($1::uuid)`, []any{owner.String()}},
		{`INSERT INTO amp_registry.storage_pools (id, name, owner_user_id) VALUES ($1::uuid, $2, $3::uuid)`,
			[]any{poolID.String(), name, owner.String()}},
		{`INSERT INTO amp_registry.clusters (id, storage_pool_id, name) VALUES ($1::uuid, $2::uuid, $3)`,
			[]any{clusterID.String(), poolID.String(), name}},
	} {
		if _, err := pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed %q: %v", stmt.sql, err)
		}
	}
	return clusterID
}
