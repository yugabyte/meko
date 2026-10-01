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

// TestPgBranchExtensions covers the externalized-extension state end-to-end
// against a real Postgres/YSQL database: the create-time write (EnsureCluster),
// the per-branch opt-in union (AddBranchExtensions), the durable per-branch read
// (BranchExtensions), and — the point of the model — that a FORK inherits its
// ancestor's set while a later add is individual to a branch. The set lives per
// branch on amp_registry.branches, the one branch table both surfaces share.
//
// This is the "verify extensions" integration coverage that yb_amp_local can't do
// end-to-end (the local Docker path has no ImageVolumes, so it can't actually
// CREATE EXTENSION); actual CREATE EXTENSION on a mounted volume is covered by the
// GKE smoke test (deploy/smoke-test.sh, SMOKE_EXTENSIONS=1).
//
// Exercises the real PgBackend + registry.Store SQL against a throwaway DATABASE
// (not a schema: the migrations create two fixed schemas, so relocating one
// isolates nothing). Set AMP_TEST_DATABASE_URL to run; skipped otherwise so
// `go test ./...` stays green without a database.
func TestPgBranchExtensions(t *testing.T) {
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

	// EnsureCluster creates the cluster + its default branch (which holds the
	// create-time extension set). The seeded cluster row is a no-op collision (ON
	// CONFLICT DO NOTHING); the default branch it inserts is the point.
	poolID, clusterID := seedCoreCluster(t, ctx, pool)
	rootID := uuid.New()
	if err := b.EnsureCluster(ctx, poolID, clusterID, rootID, "main", 18, "", nil, nil); err != nil {
		t.Fatalf("EnsureCluster: %v", err)
	}

	mustExts := func(clusterID, branchID uuid.UUID) []string {
		t.Helper()
		got, err := b.BranchExtensions(ctx, clusterID, branchID)
		if err != nil {
			t.Fatalf("BranchExtensions: %v", err)
		}
		return got
	}

	// --- fresh default branch: no opt-in yet -> empty set. ---
	if got := mustExts(clusterID, rootID); len(got) != 0 {
		t.Fatalf("fresh branch extensions = %v, want none", got)
	}

	// --- opt in on the default branch (idempotent absolute union, sorted). ---
	if err := b.AddBranchExtensions(ctx, clusterID, rootID, []string{"pgvector"}); err != nil {
		t.Fatalf("AddBranchExtensions: %v", err)
	}
	if got := mustExts(clusterID, rootID); !reflect.DeepEqual(got, []string{"pgvector"}) {
		t.Fatalf("branch extensions = %v, want [pgvector]", got)
	}

	// --- add more: the union is sorted+deduped. hexabridge < pgvector < rum. ---
	if err := b.AddBranchExtensions(ctx, clusterID, rootID, []string{"rum", "pgvector", "hexabridge"}); err != nil {
		t.Fatalf("AddBranchExtensions(union): %v", err)
	}
	if got := mustExts(clusterID, rootID); !reflect.DeepEqual(got, []string{"hexabridge", "pgvector", "rum"}) {
		t.Fatalf("branch extensions = %v, want sorted union [hexabridge pgvector rum]", got)
	}

	// --- re-adding is idempotent (absolute union, no double-apply). ---
	if err := b.AddBranchExtensions(ctx, clusterID, rootID, []string{"pgvector", "rum", "hexabridge"}); err != nil {
		t.Fatalf("AddBranchExtensions(idempotent): %v", err)
	}
	if got := mustExts(clusterID, rootID); !reflect.DeepEqual(got, []string{"hexabridge", "pgvector", "rum"}) {
		t.Fatalf("branch extensions after idempotent re-add = %v, want [hexabridge pgvector rum]", got)
	}

	// --- a fork INHERITS its ancestor's set at fork time. A fork needs a
	// materialized ancestor (a branch_state row), which RecordBranch creates for a
	// root; EnsureCluster's default branch has none, so record a root here, opt it
	// in, then fork off it. ---
	ancestorID := uuid.New()
	if err := store.RecordBranch(ctx, clusterID, ancestorID, registry.BranchCreateRecord{
		BranchName: "fork-root", PgVersion: 18, StartLSN: "0/0",
	}); err != nil {
		t.Fatalf("RecordBranch(root): %v", err)
	}
	if err := b.AddBranchExtensions(ctx, clusterID, ancestorID, []string{"hexabridge", "pgvector", "rum"}); err != nil {
		t.Fatalf("AddBranchExtensions(fork root): %v", err)
	}
	childID := uuid.New()
	if err := store.RecordBranch(ctx, clusterID, childID, registry.BranchCreateRecord{
		BranchName: "b1", PgVersion: 18, AncestorBranchID: &ancestorID, StartLSN: "0/16B8",
	}); err != nil {
		t.Fatalf("RecordBranch(fork): %v", err)
	}
	if got := mustExts(clusterID, childID); !reflect.DeepEqual(got, []string{"hexabridge", "pgvector", "rum"}) {
		t.Fatalf("fork inherited extensions = %v, want [hexabridge pgvector rum]", got)
	}

	// --- adding to the fork is individual: the ancestor is untouched. ---
	if err := b.AddBranchExtensions(ctx, clusterID, childID, []string{"age"}); err != nil {
		t.Fatalf("AddBranchExtensions(fork): %v", err)
	}
	if got := mustExts(clusterID, childID); !reflect.DeepEqual(got, []string{"age", "hexabridge", "pgvector", "rum"}) {
		t.Fatalf("fork extensions after add = %v, want [age hexabridge pgvector rum]", got)
	}
	if got := mustExts(clusterID, ancestorID); !reflect.DeepEqual(got, []string{"hexabridge", "pgvector", "rum"}) {
		t.Fatalf("ancestor extensions after fork add = %v, want unchanged [hexabridge pgvector rum]", got)
	}

	// --- create-time write: a second cluster created WITH extensions carries them
	// on its default branch from the start. ---
	cluster2, root2 := uuid.New(), uuid.New()
	if err := b.EnsureCluster(ctx, poolID, cluster2, root2, "main2-"+cluster2.String(), 18, "", []string{"pg_cron", "pgvector"}, nil); err != nil {
		t.Fatalf("EnsureCluster(with extensions): %v", err)
	}
	if got := mustExts(cluster2, root2); !reflect.DeepEqual(got, []string{"pg_cron", "pgvector"}) {
		t.Fatalf("cluster2 default-branch extensions = %v, want create-time [pg_cron pgvector]", got)
	}

	// --- an absent branch reads empty and is a no-op to add (mirrors the
	// controller's degrade-to-defaults contract). ---
	absentCluster, absentBranch := uuid.New(), uuid.New()
	if got := mustExts(absentCluster, absentBranch); len(got) != 0 {
		t.Fatalf("absent branch extensions = %v, want none", got)
	}
	if err := b.AddBranchExtensions(ctx, absentCluster, absentBranch, []string{"pgvector"}); err != nil {
		t.Fatalf("AddBranchExtensions(absent): %v", err)
	}
	if got := mustExts(absentCluster, absentBranch); len(got) != 0 {
		t.Fatalf("absent branch still absent = %v, want none", got)
	}
}

// seedCoreCluster inserts the user -> pool -> cluster ladder a branch needs
// and returns the pool and cluster ids. Names carry the pool id because
// storage_pool names are unique cluster-wide and this database may be reused
// across tests.
func seedCoreCluster(t *testing.T, ctx context.Context, pool *db.Pool) (poolID, clusterID uuid.UUID) {
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
	return poolID, clusterID
}
