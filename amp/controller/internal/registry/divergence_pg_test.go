package registry

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
)

// TestPgProductDeleteIsVisibleToStorageSurface is the regression test for the bug
// this whole unification exists to kill.
//
// There used to be two branch tables: the product model's `branch` and a separate
// flat registry. The product delete path (the delete_branch task step ->
// PgBackend.DeleteBranch) only ever touched the first one — nothing in it called
// the registry's BranchDrop, whose only callers were the HTTP route and
// yb_amp_local. So `DELETE /api/v1/.../branches/{id}` left the registry twin
// un-tombstoned: the branch stayed live on `GET /control/v1/.../branches`
// forever, kept resolving by name, and kept inflating the branch gauge.
//
// With one table the drift is not merely fixed, it is unrepresentable — the two
// surfaces read and write the same row. This test pins that: it deletes through
// the *product* path and asserts through the *storage-layer* one.
func TestPgProductDeleteIsVisibleToStorageSurface(t *testing.T) {
	ctx := t.Context()
	pool := migratedPool(t)
	store := NewStore(pool)
	backend := core.NewPgBackend(pool)

	projectID := seedProject(t, pool)
	branchUUID := uuid.New()
	branchID := FormatBranchID(branchUUID)

	if err := store.RecordBranch(ctx, projectID, branchID, BranchCreateRecord{
		BranchName: "divergence", PgVersion: 18, StartLSN: "0/0",
	}); err != nil {
		t.Fatalf("record branch: %v", err)
	}

	countLive := func(what string) int {
		t.Helper()
		branches, err := store.ListBranches(ctx, projectID)
		if err != nil {
			t.Fatalf("%s: list branches: %v", what, err)
		}
		n := 0
		for _, b := range branches {
			if b.BranchID == branchID {
				n++
			}
		}
		return n
	}
	resolvesByName := func(what string) bool {
		t.Helper()
		names, err := store.ListBranchNames(ctx, projectID)
		if err != nil {
			t.Fatalf("%s: list branch names: %v", what, err)
		}
		for _, e := range names {
			if e.BranchID == branchID {
				return true
			}
		}
		return false
	}

	if got := countLive("pre-delete"); got != 1 {
		t.Fatalf("pre-delete: storage surface lists the branch %d times, want 1", got)
	}
	if !resolvesByName("pre-delete") {
		t.Fatal("pre-delete: branch does not resolve by name")
	}
	before, err := store.RegistryCounts(ctx)
	if err != nil {
		t.Fatalf("count branches: %v", err)
	}

	// The product path — the one that used to skip the registry entirely.
	if err := backend.DeleteBranch(ctx, projectID, branchUUID); err != nil {
		t.Fatalf("product-path delete: %v", err)
	}

	if got := countLive("post-delete"); got != 0 {
		t.Fatalf("post-delete: storage surface still lists the branch %d times, want 0", got)
	}
	if resolvesByName("post-delete") {
		t.Fatal("post-delete: dropped branch still resolves by name")
	}
	after, err := store.RegistryCounts(ctx)
	if err != nil {
		t.Fatalf("recount branches: %v", err)
	}
	if after != before-1 {
		t.Fatalf("branch gauge = %d after deleting one of %d, want %d", after, before, before-1)
	}

	// The row is kept, not deleted: a branch id is never reused, and its pages and
	// WAL in amp_storage outlive the drop.
	var dropped bool
	if err := pool.QueryRow(ctx,
		`SELECT dropped_at IS NOT NULL FROM amp_registry.branches
		  WHERE project_id = $1::uuid AND branch_id = $2::uuid`,
		projectID.String(), branchUUID.String()).Scan(&dropped); err != nil {
		t.Fatalf("read back the tombstone: %v", err)
	}
	if !dropped {
		t.Fatal("product-path delete removed the row instead of tombstoning it")
	}
}

// TestPgListBranchesToleratesNullForkLSN pins a defect the fold introduced and
// the integration suite caught: `ListBranches` scans `fork_lsn` into a plain
// string, but a ROOT branch has no fork point.
//
// The old flat registry never hit this — every writer into it passed an explicit
// start_lsn of "0/0". The product path does not: EnsureProject creates a
// project's default branch with `fork_lsn` left NULL. Once the two tables became
// one, the launcher's very first `GET /control/v1/.../branches` scanned that NULL
// and 500'd, which took down cluster bring-up.
//
// The branch here is created the way the product path creates a default branch —
// a direct insert with no fork_lsn — precisely so this does not silently start
// testing the RecordBranch path, which always supplies one.
func TestPgListBranchesToleratesNullForkLSN(t *testing.T) {
	ctx := t.Context()
	pool := migratedPool(t)
	projectID := seedProject(t, pool)

	branchUUID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO amp_registry.branches (project_id, branch_id, name, pg_version, is_default)
		 VALUES ($1::uuid, $2::uuid, 'main', 18, true)`,
		projectID.String(), branchUUID.String()); err != nil {
		t.Fatalf("seed root branch: %v", err)
	}

	branches, err := NewStore(pool).ListBranches(ctx, projectID)
	if err != nil {
		t.Fatalf("list branches with a NULL fork_lsn: %v", err)
	}
	if len(branches) != 1 {
		t.Fatalf("listed %d branches, want 1", len(branches))
	}
	got := branches[0]
	if got.BranchID != FormatBranchID(branchUUID) {
		t.Errorf("branch id = %q, want %q", got.BranchID, FormatBranchID(branchUUID))
	}
	// "0/0" is what the old registry stored for a root, and what the launcher's
	// bootstrap arm expects to see.
	if got.StartLSN != "0/0" {
		t.Errorf("root branch start_lsn = %q, want %q", got.StartLSN, "0/0")
	}
	if got.AncestorBranchID != nil {
		t.Errorf("root branch ancestor = %v, want nil", *got.AncestorBranchID)
	}
}

// TestPgTombstoneGuards covers the four holes review found in the tombstone
// redesign. They share one theme: an UPDATE that never writes `branch_id` leaves
// every RESTRICT foreign key unexamined, so guarantees the old hard DELETE got
// from the database now have to be written out explicitly — and a tombstoned row
// still satisfies an FK, so "the parent exists" stopped meaning "the parent is
// alive".
func TestPgTombstoneGuards(t *testing.T) {
	ctx := t.Context()
	pool := migratedPool(t)
	store := NewStore(pool)
	backend := core.NewPgBackend(pool)
	projectID := seedProject(t, pool)

	// A live root branch with a live endpoint on it.
	rootID := uuid.New()
	if err := store.RecordBranch(ctx, projectID, FormatBranchID(rootID), BranchCreateRecord{
		BranchName: "guards-root", PgVersion: 18, StartLSN: "0/0",
	}); err != nil {
		t.Fatalf("record root: %v", err)
	}
	endpointID := uuid.New()
	if err := backend.EnsureEndpointRecord(ctx, endpointID, projectID, rootID, "read_write", nil); err != nil {
		t.Fatalf("create product endpoint: %v", err)
	}

	// (3) A branch with a live endpoint cannot be tombstoned. The old DELETE got
	// this from the endpoints FK; the UPDATE has to assert it.
	if err := backend.DeleteBranch(ctx, projectID, rootID); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("tombstone a branch with a live endpoint = %v, want ErrConflict", err)
	}
	// (3) Same for the project-wide cascade.
	if err := backend.DeleteProject(ctx, projectID); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("tombstone a project with a live endpoint = %v, want ErrConflict", err)
	}

	// (2) BranchDrop must not touch a product endpoint (spec IS NULL): this
	// surface owns launcher rows only, and deleting a deployed endpoint's record
	// would strand its running instance.
	if err := store.BranchDrop(ctx, projectID, FormatBranchID(rootID)); err != nil {
		t.Fatalf("BranchDrop: %v", err)
	}
	var survived bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM amp_registry.endpoints WHERE id = $1::uuid)`,
		endpointID.String()).Scan(&survived); err != nil {
		t.Fatalf("read back the product endpoint: %v", err)
	}
	if !survived {
		t.Fatal("BranchDrop deleted a product endpoint's row, stranding its instance")
	}

	// (1) A fork may not be created under a tombstoned parent. The parent's row
	// survives the drop, so the FK is satisfied — only an explicit liveness filter
	// stops the async create_branch step from resurrecting the lineage.
	//
	// BranchDrop above tombstoned the branch (it only skipped the endpoint), so
	// the parent is dropped-but-present, which is exactly the state under test.
	if err := backend.EnsureBranch(ctx, rootID, projectID, uuid.New(),
		"guards-child", "0/16B8", 18); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("fork under a tombstoned parent = %v, want ErrNotFound", err)
	}

	// (4) An endpoint handle taken by another project must not be silently
	// reparented: endpoints_name is global, so the ON CONFLICT arm needs its own
	// project scoping.
	otherProject := seedProject(t, pool)
	otherBranch := uuid.New()
	if err := store.RecordBranch(ctx, otherProject, FormatBranchID(otherBranch), BranchCreateRecord{
		BranchName: "guards-other", PgVersion: 18, StartLSN: "0/0",
	}); err != nil {
		t.Fatalf("record other-project branch: %v", err)
	}
	const handle = "ep-contested"
	if err := store.EndpointCreate(ctx, otherProject, ComputeEndpoint{
		EndpointID: handle, BranchID: FormatBranchID(otherBranch), Spec: []byte(`{"a":1}`),
	}); err != nil {
		t.Fatalf("create endpoint in the other project: %v", err)
	}

	thirdProject := seedProject(t, pool)
	thirdBranch := uuid.New()
	if err := store.RecordBranch(ctx, thirdProject, FormatBranchID(thirdBranch), BranchCreateRecord{
		BranchName: "guards-third", PgVersion: 18, StartLSN: "0/0",
	}); err != nil {
		t.Fatalf("record third-project branch: %v", err)
	}
	if err := store.EndpointCreate(ctx, thirdProject, ComputeEndpoint{
		EndpointID: handle, BranchID: FormatBranchID(thirdBranch), Spec: []byte(`{"b":2}`),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("reusing another project's endpoint handle = %v, want ErrConflict", err)
	}
	// And the original is untouched — not reparented, spec not clobbered.
	var ownerBranch, spec string
	if err := pool.QueryRow(ctx,
		`SELECT replace(e.branch_id::text,'-',''), e.spec::text
		   FROM amp_registry.endpoints e WHERE e.name = $1`, handle).Scan(&ownerBranch, &spec); err != nil {
		t.Fatalf("read back the contested endpoint: %v", err)
	}
	if ownerBranch != FormatBranchID(otherBranch) {
		t.Errorf("endpoint reparented to %s, want %s", ownerBranch, FormatBranchID(otherBranch))
	}
	if spec != `{"a": 1}` && spec != `{"a":1}` {
		t.Errorf("endpoint spec clobbered: %s", spec)
	}
}
