package core

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
)

// TestPgListClustersIncludeFailed covers the failed-creation filter: a cluster
// whose create_cluster task failed (its row written mid-provisioning before a
// later step failed) is an orphan. ListClusters excludes it by default (usable
// clusters only), and includes it only when the caller passes includeFailed=true
// (an operator's fleet view, to see and delete it). Succeeded and
// still-in-progress clusters are always listed.
//
// Exercises the real PgBackend SQL against a Postgres/YSQL-compatible database.
// It provisions an isolated schema per run and drops it afterwards, so it is
// safe against any reachable DB (e.g. a meko-dev YB universe). Set
// AMP_TEST_DATABASE_URL to run; skipped otherwise so `go test ./...` stays green
// without a database.
func TestPgListClustersIncludeFailed(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AMP_TEST_DATABASE_URL (e.g. postgres://yugabyte@host:5433/yb_storage?sslmode=disable) to run")
	}
	// An isolated DATABASE per run, not an isolated schema: the migrations create
	// two fixed schemas (the resource model under --db-schema plus the always-
	// `controller` task log), so relocating one no longer isolates anything. A
	// throwaway database also lets the test use the production schema names.
	//
	// Provisioning gets a budget of its own, separate from the assertions below:
	// ScratchMigrated queues behind a machine-local lock that serializes DDL across
	// test packages, so how long it takes says nothing about this test's own
	// latency and should not eat into it.
	const schema = "controller" // the --db-schema default
	provCtx, provCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer provCancel()
	scratchURL, drop, err := db.ScratchMigrated(provCtx, url, "amp_test_core_clusters", schema)
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

	// --- seed: one pool, three clusters each with a default branch, each with a
	// create_cluster task in a different terminal/in-flight state. ---
	owner := uuid.New()
	poolID := uuid.New()
	mustExec(t, ctx, pool, `INSERT INTO amp_registry.users (id) VALUES ($1)`, owner)
	mustExec(t, ctx, pool, `INSERT INTO amp_registry.storage_pools (id, name, owner_user_id) VALUES ($1,$2,$3)`,
		poolID, "pool-core-clusters", owner)

	type seed struct {
		name       string
		taskStatus string
		id         uuid.UUID
	}
	seeds := []seed{
		{name: "ok", taskStatus: "succeeded", id: uuid.New()},
		{name: "failed", taskStatus: "failed", id: uuid.New()},
		{name: "inflight", taskStatus: "running", id: uuid.New()},
	}
	for _, s := range seeds {
		mustExec(t, ctx, pool, `INSERT INTO amp_registry.clusters (id, storage_pool_id, name) VALUES ($1,$2,$3)`,
			s.id, poolID, s.name)
		mustExec(t, ctx, pool, `INSERT INTO amp_registry.branches (cluster_id, branch_id, name, pg_version, is_default) VALUES ($1,$2,'main',18,true)`,
			s.id, uuid.New())
		mustExec(t, ctx, pool, `INSERT INTO tasks (kind, status, cluster_id) VALUES ('create_cluster',$1::task_status,$2)`,
			s.taskStatus, s.id)
	}

	b := NewPgBackend(pool)
	names := func(ps []Cluster) map[string]bool {
		m := map[string]bool{}
		for _, p := range ps {
			m[p.Name] = true
		}
		return m
	}

	// --- includeFailed=false (the default): the failed cluster is hidden. ---
	t.Run("excludes failed by default", func(t *testing.T) {
		clusters, total, err := b.ListClusters(ctx, poolID, 100, 0, false)
		if err != nil {
			t.Fatalf("ListClusters: %v", err)
		}
		got := names(clusters)
		if got["failed"] {
			t.Errorf("failed-creation cluster should be excluded with includeFailed=false, but it was returned")
		}
		if !got["ok"] || !got["inflight"] {
			t.Errorf("succeeded + in-progress clusters should be listed, got %v", got)
		}
		if total != 2 || len(clusters) != 2 {
			t.Errorf("total=%d len=%d, want 2/2 (failed excluded from the count too)", total, len(clusters))
		}
	})

	// --- includeFailed=true (opt-in): all three, including the failed one. ---
	t.Run("includes failed when opted in", func(t *testing.T) {
		clusters, total, err := b.ListClusters(ctx, poolID, 100, 0, true)
		if err != nil {
			t.Fatalf("ListClusters: %v", err)
		}
		got := names(clusters)
		if !got["failed"] || !got["ok"] || !got["inflight"] {
			t.Errorf("all three clusters should be listed with includeFailed=true, got %v", got)
		}
		if total != 3 || len(clusters) != 3 {
			t.Errorf("total=%d len=%d, want 3/3 (failed included)", total, len(clusters))
		}
	})
}

func mustExec(t *testing.T, ctx context.Context, pool *db.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
