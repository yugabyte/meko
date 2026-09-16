package task

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/extensions"
)

// resolveWithCatalog resolves the way a step does: a real catalog, so the names
// map to the pinned images production would mount.
func resolveWithCatalog(ctx context.Context, state StateStore, branchID uuid.UUID) ([]deploy.Extension, error) {
	return resolveBranchExtensions(ctx, extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil), state, branchID)
}

// The mounts are the branch's set resolved through the catalog — pinned images,
// preload flags, unknown names dropped rather than wedging the deploy.
func TestResolveBranchExtensionsResolvesThroughTheCatalog(t *testing.T) {
	state := newFakeStateStore()
	clusterID, branchID := uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pg_cron", "totally-unknown-ext", "age")

	exts, err := resolveWithCatalog(t.Context(), state, branchID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []deploy.Extension{
		{Name: "pg_cron", Image: "quay.io/yugabyte/amp-ext-pg_cron:1.6.7", Preload: true},
		{Name: "age", Image: "quay.io/yugabyte/amp-ext-age:1.8.0"},
	}
	if !reflect.DeepEqual(exts, want) {
		t.Errorf("resolve = %+v, want %+v", exts, want)
	}
	if got := extensionNames(exts); !reflect.DeepEqual(got, []string{"pg_cron", "age"}) {
		t.Errorf("extensionNames = %v, want [pg_cron age]", got)
	}
}

// The branch id is the whole address: it is a primary key, so the set resolves
// from it alone — no cluster id is passed, and the backends ignore the one they
// are handed.
func TestResolveBranchExtensionsAddressesTheBranchByItsID(t *testing.T) {
	state := &recordingExtStore{fakeStateStore: newFakeStateStore()}
	clusterID, branchID := uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pgvector")

	exts, err := resolveWithCatalog(t.Context(), state, branchID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if state.gotBranch != branchID {
		t.Errorf("resolved extensions for branch %v, want %v", state.gotBranch, branchID)
	}
	if got := extensionNames(exts); !reflect.DeepEqual(got, []string{"pgvector"}) {
		t.Errorf("resolve = %v, want [pgvector] — the branch's set, resolved with no cluster id", got)
	}
}

// No catalog is not an error: the feature is off (Docker driver / no catalog
// configured) and the endpoint ships only its image-baked extensions.
func TestResolveBranchExtensionsWithoutCatalogMountsNothing(t *testing.T) {
	exts, err := resolveBranchExtensions(t.Context(), nil, newFakeStateStore(), uuid.New())
	if err != nil {
		t.Fatalf("resolve with no catalog: %v", err)
	}
	if exts != nil {
		t.Errorf("resolve = %+v, want nil", exts)
	}
}

// An empty resolve yields nil, not an empty slice: the recreate config omits the
// key entirely rather than recording an empty set.
func TestResolveBranchExtensionsEmptyResolveYieldsNil(t *testing.T) {
	// A catalog with no defaults and a branch that opted into nothing.
	exts, err := resolveWithCatalog(t.Context(), newFakeStateStore(), uuid.New())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if exts != nil {
		t.Errorf("resolve = %+v, want nil", exts)
	}
	if got := extensionNames(exts); got != nil {
		t.Errorf("extensionNames = %v, want nil", got)
	}
}

// A failed read is the caller's failure, not a silently unmounted endpoint.
func TestResolveBranchExtensionsPropagatesReadFailure(t *testing.T) {
	state := &recordingExtStore{fakeStateStore: newFakeStateStore(), err: errors.New("boom")}

	if _, err := resolveWithCatalog(t.Context(), state, uuid.New()); err == nil {
		t.Fatal("resolve succeeded with an unreadable branch set, want a failure")
	}
}

// recordingExtStore is a fakeStateStore that counts, records and can fail the
// branch extension reads a step drives.
type recordingExtStore struct {
	*fakeStateStore
	gotBranch uuid.UUID
	calls     int
	// err fails every read past the first okReads (zero: fail from the first).
	err     error
	okReads int
}

func (r *recordingExtStore) BranchExtensions(ctx context.Context, clusterID, branchID uuid.UUID) ([]string, error) {
	r.gotBranch = branchID
	r.calls++
	if r.err != nil && r.calls > r.okReads {
		return nil, r.err
	}
	return r.fakeStateStore.BranchExtensions(ctx, clusterID, branchID)
}
