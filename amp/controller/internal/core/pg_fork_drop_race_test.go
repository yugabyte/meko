package core

import (
	"context"
	"errors"
	neturl "net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
	"github.com/yugabyte/amp/controller/internal/registry"
)

// TestPgForkVsDropSerializes covers the interlock between forking a child off a
// branch and dropping that branch. Both sides used to decide on a READ — the fork
// SELECTed its parent's `dropped_at IS NULL`, the drop checked
// `NOT EXISTS (live children)` — so under snapshot isolation neither saw the
// other's write and both could commit, leaving a tombstoned parent with a live
// child. That state is not merely untidy: the child's pre-fork history lives
// under the parent's branch_id, so branch-drop GC would reclaim it out from under
// a live branch. The fork now writes the parent's row too, so the two conflict.
//
// It exercises the real pair — registry.RecordBranch against
// PgBackend.DeleteBranch — rather than either side's SQL in isolation, because
// the property under test is exactly that those two statements conflict.
//
// Set AMP_TEST_DATABASE_URL to run; skipped otherwise so `go test ./...` stays
// green without a database.
func TestPgForkVsDropSerializes(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AMP_TEST_DATABASE_URL (e.g. postgres://yugabyte@host:5433/yb_storage?sslmode=disable) to run")
	}
	// Concurrent fork/drop pairs. Enough overlap to land inside the window without
	// making the pool or the single-node YugabyteDB in CI the bottleneck.
	const pairs = 16

	// Provisioning gets a budget of its own: ScratchMigrated queues behind a lock
	// that serializes DDL across test packages, which says nothing about this
	// test's own latency.
	const schema = "controller" // the --db-schema default
	provCtx, provCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer provCancel()
	scratchURL, drop, err := db.ScratchMigrated(provCtx, url, "amp_test_core_fork_drop", schema)
	if err != nil {
		t.Fatalf("provision scratch database: %v", err)
	}
	t.Cleanup(drop)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// A connection for every goroutine in the fan-out below. pgxpool defaults to
	// max(4, NumCPU), which on a small runner would queue the pairs and leave the
	// race unexercised while the test still passed.
	u, err := neturl.Parse(scratchURL)
	if err != nil {
		t.Fatalf("parse scratch url: %v", err)
	}
	q := u.Query()
	q.Set("pool_max_conns", strconv.Itoa(2*pairs+2))
	u.RawQuery = q.Encode()

	pool, err := db.Connect(ctx, u.String(), schema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	store := registry.NewStore(pool)
	b := NewPgBackend(pool)
	clusterID := seedCoreCluster(t, ctx, pool)

	recordRoot := func(id uuid.UUID, name string) {
		t.Helper()
		if err := store.RecordBranch(ctx, clusterID, id, registry.BranchCreateRecord{
			BranchName: name, PgVersion: 18, StartLSN: "0/0",
		}); err != nil {
			t.Fatalf("RecordBranch(%s): %v", name, err)
		}
	}
	fork := func(child, parent uuid.UUID, name string) error {
		return store.RecordBranch(ctx, clusterID, child, registry.BranchCreateRecord{
			BranchName: name, PgVersion: 18,
			AncestorBranchID: &parent, StartLSN: "0/16B8",
		})
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
	stateRows := func(id uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM amp_storage.branch_state WHERE branch_id = $1`,
			id.String()).Scan(&n); err != nil {
			t.Fatalf("count branch_state: %v", err)
		}
		return n
	}

	// --- Uncontended: a fork off an already-tombstoned parent is refused, and
	// the whole registration rolls back — neither of the child's two rows lands.
	parent, child := uuid.New(), uuid.New()
	recordRoot(parent, "race-dropped-parent")
	if err := b.DeleteBranch(ctx, clusterID, parent); err != nil {
		t.Fatalf("tombstone parent: %v", err)
	}
	if err := fork(child, parent, "race-orphan"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("fork off a tombstoned parent: want ErrNotFound, got %v", err)
	}
	if liveBranch(child) {
		t.Fatal("refused fork left a live branch row")
	}
	if n := stateRows(child); n != 0 {
		t.Fatalf("refused fork left %d branch_state row(s)", n)
	}

	// --- Contended: race a fork against a drop of the same parent. Either order is
	// legal and either side may lose with a retryable conflict; what must never
	// hold is the conjunction — a tombstoned parent whose child is live. Asserting
	// the invariant rather than a winner is what keeps this non-flaky whichever way
	// the scheduler goes.
	//
	// The window is inside RecordBranch's transaction, which the test cannot pause
	// from outside, so coverage comes from overlap: every pair gets its own parent
	// and they all start together on one signal.
	type race struct {
		parent, child    uuid.UUID
		forkErr, dropErr error
	}
	races := make([]race, pairs)
	for i := range races {
		races[i] = race{parent: uuid.New(), child: uuid.New()}
		recordRoot(races[i].parent, "race-parent-"+races[i].parent.String())
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range races {
		r := &races[i]
		wg.Go(func() {
			<-start
			r.forkErr = fork(r.child, r.parent, "race-child-"+r.child.String())
		})
		wg.Go(func() {
			<-start
			r.dropErr = b.DeleteBranch(ctx, clusterID, r.parent)
		})
	}
	close(start)
	wg.Wait()

	for i, r := range races {
		parentLive, childLive := liveBranch(r.parent), liveBranch(r.child)
		if !parentLive && childLive {
			t.Errorf("pair %d: parent %s tombstoned while child %s is live (fork err %v, drop err %v)",
				i, r.parent, r.child, r.forkErr, r.dropErr)
		}
		// A fork that did not land must not have left the child's storage seed
		// behind either: the registration rolls back as a whole or not at all.
		if !childLive {
			if n := stateRows(r.child); n != 0 {
				t.Errorf("pair %d: child %s is not live but has %d branch_state row(s)", i, r.child, n)
			}
		}
	}
}
