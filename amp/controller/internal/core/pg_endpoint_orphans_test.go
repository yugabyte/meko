package core

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
)

// TestPgFailedCreateEndpoints covers the GC sweeper's endpoint selection: an
// endpoint whose creating task failed before the endpoint was ever addressable is
// an orphan, and while its row sits there it holds the branch's single-writer
// slot. The cases below are the ones the predicate has to tell apart — most of
// them are endpoints attached to a failed task that must NOT be reclaimed.
//
// Exercises the real PgBackend SQL against a Postgres/YSQL-compatible database,
// on a throwaway database per run (see TestPgListClustersIncludeFailed for why a
// whole database rather than a schema). Set AMP_TEST_DATABASE_URL to run; skipped
// otherwise so `go test ./...` stays green without a database.
func TestPgFailedCreateEndpoints(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AMP_TEST_DATABASE_URL (e.g. postgres://yugabyte@host:5433/yb_storage?sslmode=disable) to run")
	}
	const schema = "controller"
	provCtx, provCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer provCancel()
	scratchURL, drop, err := db.ScratchMigrated(provCtx, url, "amp_test_core_endpoint_orphans", schema)
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

	// The sweeper's cutoff in this test, and two timestamps either side of it: a
	// failure "old" enough to be past the grace window, and one still inside it.
	var (
		cutoff = time.Now().Add(-30 * time.Minute)
		old    = cutoff.Add(-time.Hour)
		recent = cutoff.Add(time.Minute)
	)

	owner, poolID := uuid.New(), uuid.New()
	mustExec(t, ctx, pool, `INSERT INTO amp_registry.users (id) VALUES ($1)`, owner)
	mustExec(t, ctx, pool, `INSERT INTO amp_registry.storage_pools (id, name, owner_user_id) VALUES ($1,$2,$3)`,
		poolID, "pool-endpoint-orphans", owner)

	// Two clusters: one created successfully, one whose own create_cluster failed
	// (so the cluster pass owns everything under it).
	okCluster, failedCluster := uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"ok": okCluster, "failed": failedCluster} {
		mustExec(t, ctx, pool, `INSERT INTO amp_registry.clusters (id, storage_pool_id, name) VALUES ($1,$2,$3)`,
			id, poolID, name)
	}
	mustExec(t, ctx, pool, `INSERT INTO tasks (kind, status, cluster_id, updated_at) VALUES ('create_cluster','succeeded',$1,$2)`,
		okCluster, old)
	mustExec(t, ctx, pool, `INSERT INTO tasks (kind, status, cluster_id, updated_at) VALUES ('create_cluster','failed',$1,$2)`,
		failedCluster, old)

	// One branch per endpoint: endpoints_one_writer_per_branch allows a single
	// read_write endpoint per branch, and read_write is the role whose leaked row
	// wedges the branch, so that is the role every case here uses.
	branch := func(cluster uuid.UUID, name string, dropped bool) uuid.UUID {
		id := uuid.New()
		mustExec(t, ctx, pool,
			`INSERT INTO amp_registry.branches (cluster_id, branch_id, name, pg_version, dropped_at)
			 VALUES ($1,$2,$3,18,$4)`,
			cluster, id, name, nullTime(dropped))
		return id
	}
	// endpoint records one endpoint plus the task that created it: a task of kind
	// taskKind in status taskStatus, carrying the create_endpoint step whose args
	// name this endpoint. That step — not the task kind — is what the selection
	// keys off, which is what lets it see endpoints created by a `branch` task as
	// well as by a standalone `create_endpoint`.
	endpoint := func(cluster, branchID uuid.UUID, name, address, taskKind, taskStatus string, failedAt time.Time) uuid.UUID {
		id := uuid.New()
		mustExec(t, ctx, pool,
			`INSERT INTO amp_registry.endpoints (id, branch_id, role, name, address)
			 VALUES ($1,$2,'read_write',$3,$4)`,
			id, branchID, name, nullString(address))
		taskID := uuid.New()
		mustExec(t, ctx, pool,
			`INSERT INTO tasks (id, kind, status, cluster_id, target_ref, updated_at)
			 VALUES ($1,$2,$3::task_status,$4,$5,$6)`,
			taskID, taskKind, taskStatus, cluster, id.String(), failedAt)
		mustExec(t, ctx, pool,
			`INSERT INTO task_steps (task_id, kind, args) VALUES ($1,'create_endpoint',$2::jsonb)`,
			taskID, fmt.Sprintf(`{"endpoint_id":%q}`, id))
		return id
	}

	// --- the two that must be reclaimed ---
	// A standalone scale-up whose deploy failed.
	scaleUp := endpoint(okCluster, branch(okCluster, "b-scale-up", false), "ep-scale-up", "", "create_endpoint", "failed", old)
	// A `with_writer` fork: the branch was recorded, the endpoint never came up.
	// Its endpoint id is in no task's target_ref — only in the step's args — so
	// this is the case a task-kind-keyed predicate would miss.
	forkWriter := endpoint(okCluster, branch(okCluster, "b-fork", false), "ep-fork", "", "branch", "failed", old)

	// --- the ones that must be left alone, and why ---
	addressed := endpoint(okCluster, branch(okCluster, "b-addressed", false), "ep-addressed", "10.0.0.1:5433", "create_endpoint", "failed", old)
	succeeded := endpoint(okCluster, branch(okCluster, "b-succeeded", false), "ep-succeeded", "10.0.0.2:5433", "create_endpoint", "succeeded", old)
	inFlight := endpoint(okCluster, branch(okCluster, "b-inflight", false), "ep-inflight", "", "create_endpoint", "running", old)
	withinGrace := endpoint(okCluster, branch(okCluster, "b-grace", false), "ep-grace", "", "create_endpoint", "failed", recent)
	onFailedCluster := endpoint(failedCluster, branch(failedCluster, "b-on-failed-cluster", false), "ep-on-failed-cluster", "", "create_endpoint", "failed", old)
	onDroppedBranch := endpoint(okCluster, branch(okCluster, "b-dropped", true), "ep-dropped", "", "create_endpoint", "failed", old)

	// A teardown already in flight: selecting it again would spend a task to no
	// effect and log a conflict every tick.
	deleting := endpoint(okCluster, branch(okCluster, "b-deleting", false), "ep-deleting", "", "create_endpoint", "failed", old)
	mustExec(t, ctx, pool,
		`INSERT INTO tasks (kind, status, cluster_id, target_ref) VALUES ('delete_endpoint','pending',$1,$2)`,
		okCluster, deleting.String())

	b := NewPgBackend(pool)

	t.Run("selects only the reclaimable endpoints", func(t *testing.T) {
		got, err := b.FailedCreateEndpoints(ctx, cutoff, 100)
		if err != nil {
			t.Fatalf("FailedCreateEndpoints: %v", err)
		}
		selected := map[uuid.UUID]OrphanEndpoint{}
		for _, o := range got {
			selected[o.EndpointID] = o
		}
		for _, want := range []struct {
			id     uuid.UUID
			reason string
		}{
			{scaleUp, "a standalone create_endpoint that failed before recording an address"},
			{forkWriter, "a with_writer fork's endpoint, named only in the create_endpoint step's args"},
		} {
			if _, ok := selected[want.id]; !ok {
				t.Errorf("endpoint %s not selected, but it is an orphan: %s", want.id, want.reason)
			}
		}
		for _, skip := range []struct {
			id     uuid.UUID
			reason string
		}{
			{addressed, "it recorded an address, so it may be serving"},
			{succeeded, "its create succeeded"},
			{inFlight, "its create is still running"},
			{withinGrace, "it failed inside the grace window"},
			{onFailedCluster, "its cluster is a failed-create orphan, and delete_cluster cascades"},
			{onDroppedBranch, "its branch is tombstoned, so the cascade already covered it"},
			{deleting, "a delete_endpoint is already pending"},
		} {
			if _, ok := selected[skip.id]; ok {
				t.Errorf("endpoint %s was selected but must be left alone: %s", skip.id, skip.reason)
			}
		}
		if len(got) != 2 {
			t.Errorf("selected %d endpoints, want exactly the 2 orphans", len(got))
		}
	})

	t.Run("reports the ids the teardown is addressed by", func(t *testing.T) {
		got, err := b.FailedCreateEndpoints(ctx, cutoff, 100)
		if err != nil {
			t.Fatalf("FailedCreateEndpoints: %v", err)
		}
		for _, o := range got {
			if o.ClusterID != okCluster {
				t.Errorf("endpoint %s: cluster_id = %s, want %s — delete_endpoint is addressed by it",
					o.EndpointID, o.ClusterID, okCluster)
			}
			if o.BranchID == uuid.Nil || o.FailedAt.IsZero() {
				t.Errorf("endpoint %s: branch_id=%s failed_at=%v, both are logged for the operator",
					o.EndpointID, o.BranchID, o.FailedAt)
			}
		}
	})

	t.Run("limit caps the batch and a non-positive limit selects nothing", func(t *testing.T) {
		got, err := b.FailedCreateEndpoints(ctx, cutoff, 1)
		if err != nil {
			t.Fatalf("FailedCreateEndpoints: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("selected %d with limit 1, want 1", len(got))
		}
		if got, err := b.FailedCreateEndpoints(ctx, cutoff, 0); err != nil || len(got) != 0 {
			t.Errorf("limit 0: got %d rows, err %v; want 0 rows and no query", len(got), err)
		}
	})

	// The precheck answers the same question for one id, minus the grace window —
	// so an endpoint that is reclaimable but still inside its grace window
	// answers true here, and only selection holds it back.
	t.Run("precheck matches selection, ignoring the grace window", func(t *testing.T) {
		for _, c := range []struct {
			id   uuid.UUID
			want bool
			why  string
		}{
			{scaleUp, true, "selected, so still reclaimable"},
			{withinGrace, true, "reclaimable; only the window, which the precheck drops, held it back"},
			{addressed, false, "it recorded an address"},
			{deleting, false, "a delete_endpoint is already pending"},
			{onFailedCluster, false, "the cluster pass owns it"},
			{uuid.New(), false, "no such endpoint"},
		} {
			got, err := b.IsFailedCreateEndpoint(ctx, c.id)
			if err != nil {
				t.Fatalf("IsFailedCreateEndpoint(%s): %v", c.id, err)
			}
			if got != c.want {
				t.Errorf("IsFailedCreateEndpoint(%s) = %v, want %v: %s", c.id, got, c.want, c.why)
			}
		}
	})
}

// nullTime turns "this row is tombstoned" into the dropped_at value to insert.
func nullTime(dropped bool) any {
	if !dropped {
		return nil
	}
	return time.Now()
}

// nullString keeps an empty address NULL rather than ”, which is what the
// selection's "never recorded an address" test means.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
