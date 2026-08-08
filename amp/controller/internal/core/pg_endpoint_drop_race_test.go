package core

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
)

// TestPgEndpointVsBranchDropSerializes covers the interlock between creating an
// endpoint on a branch and dropping that branch. Both sides used to decide on a
// READ — the endpoint insert only had the FK, which a tombstoned branch still
// satisfies, and the drop checked `NOT EXISTS (endpoints)` — so under snapshot
// isolation neither saw the other's write and both could commit, leaving a live
// endpoint on a tombstoned branch. Nothing enumerates that endpoint to retire it
// (every cascade reader filters `dropped_at IS NULL`) while its compute keeps
// serving, and branch-drop GC then reclaims the branch's pages and WAL out from
// under it. EnsureEndpointRecord now WRITES the branch's row, so the two
// serialize.
//
// It exercises the real pair — PgBackend.EnsureEndpointRecord against
// PgBackend.DeleteBranch — rather than either side's SQL in isolation, because
// the property under test is exactly that those two statements conflict.
//
// Set AMP_TEST_DATABASE_URL to run; skipped otherwise so `go test ./...` stays
// green without a database.
func TestPgEndpointVsBranchDropSerializes(t *testing.T) {
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
	scratchURL, drop, err := db.ScratchMigrated(provCtx, url, "amp_test_core_endpoint_drop", schema)
	if err != nil {
		t.Fatalf("provision scratch database: %v", err)
	}
	t.Cleanup(drop)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx, scratchURL, schema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	b := NewPgBackend(pool)
	owner, err := b.CreateUser(ctx, false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	sp, err := b.CreateStoragePool(ctx, owner.ID, "endpoint-drop-race-pool")
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	clusterID, defaultBranchID := uuid.New(), uuid.New()
	if err := b.EnsureCluster(ctx, sp.ID, clusterID, defaultBranchID, "endpoint-drop-race", 18); err != nil {
		t.Fatalf("ensure cluster: %v", err)
	}

	// Every race gets its own branch, forked off the default one: the drop refuses
	// a branch with a live child, so the branch under test must be a leaf.
	newBranch := func() uuid.UUID {
		t.Helper()
		id := uuid.New()
		if err := b.EnsureBranch(ctx, defaultBranchID, clusterID, id, "race-"+id.String(), "0/16B8", 18); err != nil {
			t.Fatalf("ensure branch %s: %v", id, err)
		}
		return id
	}
	// Liveness through the backend's own accessor rather than a hand-copied
	// predicate: getBranchByID already means "present and not tombstoned", and
	// reusing it keeps the test from drifting from branchColumns.
	liveBranch := func(id uuid.UUID) bool {
		t.Helper()
		_, err := b.getBranchByID(ctx, clusterID, id)
		if err != nil && !errors.Is(err, ErrNotFound) {
			t.Fatalf("probe branch %s: %v", id, err)
		}
		return err == nil
	}
	endpointRows := func(branchID uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM amp_registry.endpoints WHERE branch_id = $1::uuid`,
			branchID.String()).Scan(&n); err != nil {
			t.Fatalf("count endpoints: %v", err)
		}
		return n
	}

	// --- Uncontended: an endpoint on an already-tombstoned branch is refused, and
	// the record rolls back with the interlock — no row is left behind. The FK
	// alone would have accepted it: the tombstoned branch's row is still there.
	branchID := newBranch()
	if err := b.DeleteBranch(ctx, clusterID, branchID); err != nil {
		t.Fatalf("tombstone branch: %v", err)
	}
	if err := b.EnsureEndpointRecord(ctx, uuid.New(), clusterID, branchID, RoleReadWrite, "", nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("endpoint on a tombstoned branch: want ErrNotFound, got %v", err)
	}
	if n := endpointRows(branchID); n != 0 {
		t.Fatalf("refused endpoint left %d row(s) on a tombstoned branch", n)
	}

	// --- Contended: race an endpoint create against a drop of its branch. Either
	// order is legal and either side may lose with a retryable conflict; what must
	// never hold is the conjunction — a tombstoned branch with an endpoint on it.
	// Asserting the invariant rather than a winner is what keeps this non-flaky
	// whichever way the scheduler goes.
	//
	// All pairs run at once rather than one round at a time, which is load-bearing
	// rather than just faster: the window lives inside the create's transaction, so
	// a sequential loop reproduces the skew only occasionally where this reproduces
	// it every run.
	const pairs = 64
	type race struct {
		branchID, endpointID uuid.UUID
		createErr, dropErr   error
	}
	races := make([]race, pairs)
	for i := range races {
		races[i] = race{branchID: newBranch(), endpointID: uuid.New()}
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range races {
		r := &races[i]
		wg.Go(func() {
			<-start
			r.createErr = b.EnsureEndpointRecord(ctx, r.endpointID, clusterID, r.branchID, RoleReadWrite, "", nil, nil)
		})
		wg.Go(func() {
			<-start
			r.dropErr = b.DeleteBranch(ctx, clusterID, r.branchID)
		})
	}
	close(start)
	wg.Wait()

	for i, r := range races {
		if !liveBranch(r.branchID) && endpointRows(r.branchID) > 0 {
			t.Errorf("pair %d: branch %s tombstoned with endpoint %s still on it (create err %v, drop err %v)",
				i, r.branchID, r.endpointID, r.createErr, r.dropErr)
		}
		// A create that reported failure must not have left a record behind either:
		// the endpoint is recorded as a whole or not at all.
		if r.createErr != nil && endpointRows(r.branchID) > 0 {
			t.Errorf("pair %d: create failed (%v) but left an endpoint row on branch %s",
				i, r.createErr, r.branchID)
		}
	}
}
