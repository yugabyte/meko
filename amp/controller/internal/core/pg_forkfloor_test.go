package core

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
	"github.com/yugabyte/amp/controller/internal/registry"
)

// TestPgForkFloor covers a branch's floor (fork_lsn) against a real
// Postgres/YSQL database: a root's is written once and only by the first
// writer, every fork insert refuses a point below its parent's, and the
// backfill marks exactly the roots that already had a writer.
//
// Set AMP_TEST_DATABASE_URL to run; skipped otherwise so `go test ./...` stays
// green without a database.
func TestPgForkFloor(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AMP_TEST_DATABASE_URL (e.g. postgres://yugabyte@host:5433/yb_storage?sslmode=disable) to run")
	}
	const schema = "controller" // the --db-schema default
	provCtx, provCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer provCancel()
	scratchURL, drop, err := db.ScratchMigrated(provCtx, url, "amp_test_core_fork_floor", schema)
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

	// newRoot provisions a cluster and its default branch, and returns them.
	newRoot := func(t *testing.T) (poolID, clusterID, rootID uuid.UUID) {
		t.Helper()
		poolID, clusterID = seedCoreCluster(t, ctx, pool)
		rootID = uuid.New()
		if err := b.EnsureCluster(ctx, poolID, clusterID, rootID, "main", 18, "", nil, nil); err != nil {
			t.Fatalf("EnsureCluster: %v", err)
		}
		return poolID, clusterID, rootID
	}
	forkLSN := func(t *testing.T, branchID uuid.UUID) *string {
		t.Helper()
		var lsn *string
		if err := pool.QueryRow(ctx,
			`SELECT fork_lsn::text FROM amp_registry.branches WHERE branch_id = $1::uuid`,
			branchID.String()).Scan(&lsn); err != nil {
			t.Fatalf("read fork_lsn: %v", err)
		}
		return lsn
	}
	pending := func(t *testing.T, branchID uuid.UUID) bool {
		t.Helper()
		p, err := b.RootForkLSNPending(ctx, branchID)
		if err != nil {
			t.Fatalf("RootForkLSNPending: %v", err)
		}
		return p
	}

	t.Run("SetRootForkLSN is first writer wins and ignores forks", func(t *testing.T) {
		poolID, clusterID, rootID := newRoot(t)
		forkID := uuid.New()
		if err := b.EnsureBranch(ctx, rootID, clusterID, forkID, "dev", "0/100", 18); err != nil {
			t.Fatalf("EnsureBranch: %v", err)
		}
		if !pending(t, rootID) || pending(t, forkID) || pending(t, uuid.New()) {
			t.Fatal("RootForkLSNPending: want true for the new root only")
		}
		root, err := b.GetBranch(ctx, poolID, clusterID, rootID)
		if err != nil {
			t.Fatalf("GetBranch: %v", err)
		}
		if !root.PasswordRequired() {
			t.Error("a new root does not require a password")
		}

		for _, lsn := range []string{"0/16B3748", "0/FFFFFFF"} {
			if err := b.SetRootForkLSN(ctx, rootID, lsn); err != nil {
				t.Fatalf("SetRootForkLSN(%s): %v", lsn, err)
			}
		}
		if got := forkLSN(t, rootID); got == nil || *got != "0/16B3748" {
			t.Errorf("root floor = %v, want the first writer's 0/16B3748", got)
		}
		if pending(t, rootID) {
			t.Error("root still pending after its floor was set")
		}
		root, err = b.GetBranch(ctx, poolID, clusterID, rootID)
		if err != nil {
			t.Fatalf("GetBranch: %v", err)
		}
		if root.ForkLSN != "0/16B3748" || root.PasswordRequired() {
			t.Errorf("root read back ForkLSN %q, PasswordRequired %v; want 0/16B3748, false", root.ForkLSN, root.PasswordRequired())
		}
		cl, err := b.GetCluster(ctx, poolID, clusterID)
		if err != nil {
			t.Fatalf("GetCluster: %v", err)
		}
		if cl.PasswordRequired() {
			t.Error("cluster still requires a password after its root's floor was set")
		}

		if err := b.SetRootForkLSN(ctx, forkID, "0/999999"); err != nil {
			t.Fatalf("SetRootForkLSN(fork): %v", err)
		}
		if got := forkLSN(t, forkID); got == nil || *got != "0/100" {
			t.Errorf("fork fork_lsn = %v, want 0/100 untouched", got)
		}
		// An absent branch is a no-op, not an error.
		if err := b.SetRootForkLSN(ctx, uuid.New(), "0/1"); err != nil {
			t.Errorf("SetRootForkLSN(absent): %v", err)
		}
	})

	t.Run("SetRootForkLSN skips a tombstoned root", func(t *testing.T) {
		_, _, rootID := newRoot(t)
		if _, err := pool.Exec(ctx, `UPDATE amp_registry.branches SET dropped_at = now() WHERE branch_id = $1::uuid`,
			rootID.String()); err != nil {
			t.Fatalf("tombstone: %v", err)
		}
		if pending(t, rootID) {
			t.Error("a tombstoned root reads as pending")
		}
		if err := b.SetRootForkLSN(ctx, rootID, "0/1"); err != nil {
			t.Fatalf("SetRootForkLSN: %v", err)
		}
		if got := forkLSN(t, rootID); got != nil {
			t.Errorf("tombstoned root floor = %q, want NULL", *got)
		}
	})

	t.Run("every fork insert refuses a point below the parent's floor", func(t *testing.T) {
		poolID, clusterID, rootID := newRoot(t)
		store := registry.NewStore(pool)
		// The root's branch_state seed, which a registry fork's GC-cutoff check needs.
		if err := store.RecordBranch(ctx, clusterID, rootID, registry.BranchCreateRecord{
			BranchName: "main", PgVersion: 18, StartLSN: "0/0",
		}); err != nil {
			t.Fatalf("RecordBranch(root): %v", err)
		}
		if err := b.SetRootForkLSN(ctx, rootID, "0/100"); err != nil {
			t.Fatalf("SetRootForkLSN: %v", err)
		}

		// "0/FF" sorts after "0/100" as text; pg_lsn compares it as a number.
		if err := b.EnsureBranch(ctx, rootID, clusterID, uuid.New(), "below", "0/FF", 18); !errors.Is(err, ErrForkBelowFloor) {
			t.Errorf("EnsureBranch below the floor = %v, want ErrForkBelowFloor", err)
		}
		if _, err := b.CreateBranch(ctx, poolID, clusterID, rootID, "below-sync", "0/FF"); !errors.Is(err, ErrForkBelowFloor) {
			t.Errorf("CreateBranch below the floor = %v, want ErrForkBelowFloor", err)
		}
		err := store.RecordBranch(ctx, clusterID, uuid.New(), registry.BranchCreateRecord{
			BranchName: "below-recipe", PgVersion: 18, AncestorBranchID: &rootID, StartLSN: "0/FF",
		})
		if !errors.Is(err, registry.ErrForkBelowFloor) || !errors.Is(err, registry.ErrInvalid) {
			t.Errorf("RecordBranch below the floor = %v, want registry.ErrForkBelowFloor (an ErrInvalid)", err)
		}

		// At the floor is allowed on every path.
		at := uuid.New()
		if err := b.EnsureBranch(ctx, rootID, clusterID, at, "at", "0/100", 18); err != nil {
			t.Fatalf("EnsureBranch at the floor: %v", err)
		}
		if _, err := b.CreateBranch(ctx, poolID, clusterID, rootID, "at-sync", "0/100"); err != nil {
			t.Errorf("CreateBranch at the floor: %v", err)
		}
		recipe := uuid.New()
		if err := store.RecordBranch(ctx, clusterID, recipe, registry.BranchCreateRecord{
			BranchName: "at-recipe", PgVersion: 18, AncestorBranchID: &rootID, StartLSN: "0/100",
		}); err != nil {
			t.Errorf("RecordBranch at the floor: %v", err)
		}

		// A fork's own floor is its fork_lsn.
		if err := b.EnsureBranch(ctx, at, clusterID, uuid.New(), "below-fork", "0/50", 18); !errors.Is(err, ErrForkBelowFloor) {
			t.Errorf("EnsureBranch below a fork's fork_lsn = %v, want ErrForkBelowFloor", err)
		}

		// A replay of an id that exists stays a no-op, even though its fork point
		// is now below a floor that rose after it was inserted.
		if _, err := pool.Exec(ctx, `UPDATE amp_registry.branches SET fork_lsn = '0/500' WHERE branch_id = $1::uuid`,
			rootID.String()); err != nil {
			t.Fatalf("raise the floor: %v", err)
		}
		if err := b.EnsureBranch(ctx, rootID, clusterID, at, "at", "0/100", 18); err != nil {
			t.Errorf("EnsureBranch replay: %v, want a no-op", err)
		}
		if err := store.RecordBranch(ctx, clusterID, recipe, registry.BranchCreateRecord{
			BranchName: "at-recipe", PgVersion: 18, AncestorBranchID: &rootID, StartLSN: "0/100",
		}); err != nil {
			t.Errorf("RecordBranch replay: %v, want a no-op", err)
		}

		// An absent parent is still not found, not below a floor.
		if err := b.EnsureBranch(ctx, uuid.New(), clusterID, uuid.New(), "orphan", "0/1", 18); !errors.Is(err, ErrNotFound) {
			t.Errorf("EnsureBranch with no parent = %v, want ErrNotFound", err)
		}
	})

	t.Run("a root with no floor can be forked anywhere", func(t *testing.T) {
		_, clusterID, rootID := newRoot(t)
		if err := b.EnsureBranch(ctx, rootID, clusterID, uuid.New(), "dev", "0/1", 18); err != nil {
			t.Errorf("EnsureBranch off a root with no floor: %v", err)
		}
	})

	// addStep adds a create_endpoint step to a task in clusterID. result is the
	// step's result JSON, empty for none.
	addStep := func(t *testing.T, clusterID, branchID uuid.UUID, role, status, result string) {
		t.Helper()
		taskID := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO tasks (id, kind, status, cluster_id) VALUES ($1::uuid, 'create_endpoint', 'succeeded', $2::uuid)`,
			taskID.String(), clusterID.String()); err != nil {
			t.Fatalf("insert task: %v", err)
		}
		args := `{"endpoint_id":"` + uuid.NewString() + `","cluster_id":"` + clusterID.String() +
			`","branch_id":"` + branchID.String() + `","role":"` + role + `","pg_version":18}`
		var res *string
		if result != "" {
			res = &result
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO task_steps (task_id, kind, status, args, result)
			 VALUES ($1::uuid, 'create_endpoint', $2::step_status, $3::jsonb, $4::jsonb)`,
			taskID.String(), status, args, res); err != nil {
			t.Fatalf("insert step: %v", err)
		}
	}

	t.Run("BackfillRootForkLSN marks exactly the roots that had a writer", func(t *testing.T) {
		_, writerCluster, writerRoot := newRoot(t)
		addStep(t, writerCluster, writerRoot, "read_write", "succeeded", "")

		// A create that waited and saw the catalog already had a password.
		_, alreadyCluster, alreadyRoot := newRoot(t)
		addStep(t, alreadyCluster, alreadyRoot, "read_write", "succeeded", `{"compute_password":"already_set"}`)

		// Creates that waited and could not see a password land are no evidence
		// the catalog has one, until a later writer's is seen.
		_, unknownCluster, unknownRoot := newRoot(t)
		addStep(t, unknownCluster, unknownRoot, "read_write", "succeeded", `{"compute_password":"unknown"}`)
		_, unsetCluster, unsetRoot := newRoot(t)
		addStep(t, unsetCluster, unsetRoot, "read_write", "succeeded", `{"compute_password":"unset"}`)
		_, failedOutcomeCluster, failedOutcomeRoot := newRoot(t)
		addStep(t, failedOutcomeCluster, failedOutcomeRoot, "read_write", "succeeded", `{"compute_password":"failed"}`)
		_, noLSNCluster, noLSNRoot := newRoot(t)
		addStep(t, noLSNCluster, noLSNRoot, "read_write", "succeeded", `{"compute_password":"applied_no_lsn"}`)
		_, laterCluster, laterRoot := newRoot(t)
		addStep(t, laterCluster, laterRoot, "read_write", "succeeded", `{"compute_password":"unknown"}`)
		addStep(t, laterCluster, laterRoot, "read_write", "succeeded", `{"compute_password":"already_set"}`)

		// An endpoint image older than GET /v1/remote-auth applies the password
		// but cannot say so.
		_, unsupportedCluster, unsupportedRoot := newRoot(t)
		addStep(t, unsupportedCluster, unsupportedRoot, "read_write", "succeeded", `{"compute_password":"unsupported"}`)

		_, failedCluster, failedRoot := newRoot(t)
		addStep(t, failedCluster, failedRoot, "read_write", "failed", "")

		_, readerCluster, readerRoot := newRoot(t)
		addStep(t, readerCluster, readerRoot, "read_only", "succeeded", "")

		_, forkCluster, forkParent := newRoot(t)
		forkID := uuid.New()
		if err := b.EnsureBranch(ctx, forkParent, forkCluster, forkID, "dev", "0/100", 18); err != nil {
			t.Fatalf("EnsureBranch: %v", err)
		}
		addStep(t, forkCluster, forkID, "read_write", "succeeded", "")

		_, droppedCluster, droppedRoot := newRoot(t)
		addStep(t, droppedCluster, droppedRoot, "read_write", "succeeded", "")
		if _, err := pool.Exec(ctx, `UPDATE amp_registry.branches SET dropped_at = now() WHERE branch_id = $1::uuid`,
			droppedRoot.String()); err != nil {
			t.Fatalf("tombstone: %v", err)
		}

		_, flooredCluster, flooredRoot := newRoot(t)
		addStep(t, flooredCluster, flooredRoot, "read_write", "succeeded", "")
		if err := b.SetRootForkLSN(ctx, flooredRoot, "0/5000"); err != nil {
			t.Fatalf("SetRootForkLSN: %v", err)
		}

		// The scratch database is shared by the subtests above, and none of their
		// roots has a create_endpoint step, so these are the only candidates.
		n, err := b.BackfillRootForkLSN(ctx)
		if err != nil {
			t.Fatalf("BackfillRootForkLSN: %v", err)
		}
		if n != 5 {
			t.Errorf("BackfillRootForkLSN updated %d rows, want 5", n)
		}
		for name, id := range map[string]uuid.UUID{
			"a succeeded writer":                   writerRoot,
			"a writer that found a password":       alreadyRoot,
			"a later writer that found a password": laterRoot,
			"a writer on an older endpoint image":  unsupportedRoot,
			"a writer that read no lsn":            noLSNRoot,
		} {
			if got := forkLSN(t, id); got == nil || *got != "0/0" {
				t.Errorf("root with %s: fork_lsn = %v, want 0/0", name, got)
			}
		}
		for name, id := range map[string]uuid.UUID{
			"only a failed writer":        failedRoot,
			"only a reader":               readerRoot,
			"a fork's writer":             forkParent,
			"a tombstone":                 droppedRoot,
			"a writer of unknown outcome": unknownRoot,
			"a writer with no password":   unsetRoot,
			"a writer that failed to set": failedOutcomeRoot,
		} {
			if got := forkLSN(t, id); got != nil {
				t.Errorf("root with %s: fork_lsn = %q, want NULL", name, *got)
			}
		}
		if got := forkLSN(t, forkID); got == nil || *got != "0/100" {
			t.Errorf("fork fork_lsn = %v, want 0/100 untouched", got)
		}
		if got := forkLSN(t, flooredRoot); got == nil || *got != "0/5000" {
			t.Errorf("root with a floor: fork_lsn = %v, want 0/5000 untouched", got)
		}

		if n, err := b.BackfillRootForkLSN(ctx); err != nil || n != 0 {
			t.Errorf("second BackfillRootForkLSN = %d, %v; want 0, nil", n, err)
		}
	})

	// After the full backfill, which would take this subtest's roots.
	t.Run("BackfillRootForkLSNOf marks only the root it is given", func(t *testing.T) {
		_, cluster, root := newRoot(t)
		addStep(t, cluster, root, "read_write", "succeeded", "")
		_, otherCluster, otherRoot := newRoot(t)
		addStep(t, otherCluster, otherRoot, "read_write", "succeeded", "")
		_, unknownCluster, unknownRoot := newRoot(t)
		addStep(t, unknownCluster, unknownRoot, "read_write", "succeeded", `{"compute_password":"unknown"}`)

		if ok, err := b.BackfillRootForkLSNOf(ctx, root); err != nil || !ok {
			t.Fatalf("BackfillRootForkLSNOf = %v, %v; want true, nil", ok, err)
		}
		if got := forkLSN(t, root); got == nil || *got != "0/0" {
			t.Errorf("fork_lsn = %v, want 0/0", got)
		}
		if got := forkLSN(t, otherRoot); got != nil {
			t.Errorf("another root's fork_lsn = %q, want NULL", *got)
		}
		if ok, err := b.BackfillRootForkLSNOf(ctx, root); err != nil || ok {
			t.Errorf("second BackfillRootForkLSNOf = %v, %v; want false, nil", ok, err)
		}
		if ok, err := b.BackfillRootForkLSNOf(ctx, unknownRoot); err != nil || ok {
			t.Errorf("BackfillRootForkLSNOf of a writer of unknown outcome = %v, %v; want false, nil", ok, err)
		}
	})
}
