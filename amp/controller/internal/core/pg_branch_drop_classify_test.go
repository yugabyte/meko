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

// TestPgDeleteBranchVanishingBlocker pins the one thing PgBackend.DeleteBranch
// must not do when its tombstone UPDATE moves nothing: classify the no-op by
// re-checking the blockers. That runs in a transaction of its own, so a blocker
// that vanishes in the gap reports success on a branch that is still live —
// endpoints already destroyed, no tombstone for GC to act on, and no retry,
// because the step Succeeded (#563).
//
// The blocker is a live child fork that its own drop task tombstones, the trace
// from the issue. The endpoint guard shares this classify, but cannot be driven
// into the gap from outside: YugabyteDB takes read locks for the rows a DML reads,
// so a concurrent `DELETE FROM endpoints` queues behind the tombstone UPDATE and
// commits only after the classify has run.
//
// Nothing outside DeleteBranch can observe the boundary between its two statements
// — the first changed no rows, so it leaves nothing to wait on — so the child's
// tombstone is committed concurrently on a delay swept across it. The sweep then
// checks that it really straddled the boundary: a run where every round landed on
// the same side of it exercised nothing, and says so rather than passing.
//
// Set AMP_TEST_DATABASE_URL to run; skipped otherwise so `go test ./...` stays
// green without a database.
func TestPgDeleteBranchVanishingBlocker(t *testing.T) {
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
	scratchURL, drop, err := db.ScratchMigrated(provCtx, url, "amp_test_core_branch_classify", schema)
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
	sp, err := b.CreateStoragePool(ctx, owner.ID, "branch-classify-pool")
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	clusterID, defaultBranchID := uuid.New(), uuid.New()
	if err := b.EnsureCluster(ctx, sp.ID, clusterID, defaultBranchID, "branch-classify", 18, "", nil, nil); err != nil {
		t.Fatalf("ensure cluster: %v", err)
	}

	newBranch := func(parent uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if err := b.EnsureBranch(ctx, parent, clusterID, id, "classify-"+id.String(), "0/16B8", 18); err != nil {
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

	// The child's tombstone is offset from the drop by `round*step - lead`, so the
	// sweep opens before the parent's tombstone UPDATE and closes well after its
	// classify. Fine steps over a wide span rather than a delay tuned to one
	// machine: the window is a single round trip wide and sits wherever the day's
	// latency puts it.
	const (
		rounds = 140
		step   = 50 * time.Microsecond
		lead   = 2 * time.Millisecond
	)
	var tombstoned, refused int
	for round := range rounds {
		parent := newBranch(defaultBranchID)
		child := newBranch(parent)

		dropped := make(chan error, 1)
		var wg sync.WaitGroup
		wg.Go(func() {
			time.Sleep(lead)
			dropped <- b.DeleteBranch(ctx, clusterID, parent)
		})
		wg.Go(func() {
			time.Sleep(time.Duration(round) * step)
			// The child's own drop task, reduced to the write that matters.
			if _, err := pool.Exec(ctx,
				`UPDATE amp_registry.branches SET dropped_at = now() WHERE branch_id = $1::uuid`,
				child.String()); err != nil {
				t.Errorf("round %d: tombstone child %s: %v", round, child, err)
			}
		})
		wg.Wait()

		// Only the conjunction is a failure. Every offset outside the window yields
		// a legal outcome — the tombstone lands, or it is refused as a retryable
		// conflict, or a guard read loses to the child's write and surfaces as a
		// serialization error — and asserting the invariant rather than one of those
		// is what keeps the sweep non-flaky.
		err := <-dropped
		switch live := liveBranch(parent); {
		case err == nil && live:
			t.Fatalf("round %d: reported success on branch %s, still live", round, parent)
		case err == nil:
			tombstoned++
		case errors.Is(err, ErrConflict):
			refused++
		}
	}
	// Both outcomes present ⇒ some rounds committed the child's tombstone before the
	// parent's UPDATE and others after its classify, so the sweep did cross the
	// boundary. Missing either one means it drifted off — a timing miss to retune,
	// not a pass.
	if tombstoned == 0 || refused == 0 {
		t.Fatalf("sweep never straddled the classify boundary (%d tombstoned, %d refused of %d rounds); retune step/lead",
			tombstoned, refused, rounds)
	}
}
