package ycp

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
)

// page slices all[offset:offset+limit] and reports the untruncated total, the
// contract listAll pages against.
func page[T any](all []T, limit, offset int) ([]T, int, error) {
	total := len(all)
	if offset >= total {
		return nil, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return all[offset:end], total, nil
}

type fakeInv struct {
	pools     []core.StoragePool
	clusters  map[uuid.UUID][]core.Cluster
	branches  map[uuid.UUID][]core.Branch
	endpoints map[uuid.UUID][]core.Endpoint
}

func (f fakeInv) ListStoragePools(_ context.Context, limit, offset int) ([]core.StoragePool, int, error) {
	return page(f.pools, limit, offset)
}
func (f fakeInv) ListClusters(_ context.Context, poolID uuid.UUID, limit, offset int, _ bool) ([]core.Cluster, int, error) {
	return page(f.clusters[poolID], limit, offset)
}
func (f fakeInv) ListBranches(_ context.Context, _, clusterID uuid.UUID, limit, offset int) ([]core.Branch, int, error) {
	return page(f.branches[clusterID], limit, offset)
}
func (f fakeInv) ListEndpoints(_ context.Context, _, _, branchID uuid.UUID, limit, offset int) ([]core.Endpoint, int, error) {
	return page(f.endpoints[branchID], limit, offset)
}

// BuildInventory flattens the pool→project→branch→endpoint tree, linking each
// resource to its owner by id.
func TestBuildInventory_WalksTheTree(t *testing.T) {
	poolID := uuid.New()
	clusterID := uuid.New()
	mainID := uuid.New()
	devID := uuid.New()
	epID := uuid.New()

	inv := fakeInv{
		pools:    []core.StoragePool{{ID: poolID, Name: "default"}},
		clusters: map[uuid.UUID][]core.Cluster{poolID: {{ID: clusterID, Name: "app"}}},
		branches: map[uuid.UUID][]core.Branch{clusterID: {
			{ClusterID: clusterID, BranchID: mainID, Name: "main", PgVersion: 15, IsDefault: true},
			{ClusterID: clusterID, BranchID: devID, Name: "dev", PgVersion: 15, ForkLSN: "0/16B3760"},
		}},
		endpoints: map[uuid.UUID][]core.Endpoint{mainID: {
			{ID: epID, ClusterID: clusterID, BranchID: mainID, Role: core.RoleReadWrite, Name: "writer", Address: "host:5433"},
		}},
	}

	res, err := BuildInventory(context.Background(), inv)
	if err != nil {
		t.Fatalf("BuildInventory: %v", err)
	}

	byID := map[string]ResourceStatus{}
	for _, r := range res {
		byID[r.ID] = r
	}
	if len(res) != 5 {
		t.Fatalf("got %d resources, want 5 (pool, project, 2 branches, endpoint): %+v", len(res), res)
	}
	if r := byID[poolID.String()]; r.Kind != ResKindStoragePool || r.Parent != "" {
		t.Errorf("pool = %+v", r)
	}
	if r := byID[clusterID.String()]; r.Kind != ResKindProject || r.Parent != poolID.String() {
		t.Errorf("cluster parent = %+v, want pool", r)
	}
	if r := byID[devID.String()]; r.Kind != ResKindBranch || r.Parent != clusterID.String() || r.Attrs["fork_lsn"] != "0/16B3760" {
		t.Errorf("dev branch = %+v", r)
	}
	if r := byID[mainID.String()]; r.Attrs["is_default"] != "true" {
		t.Errorf("main branch is_default = %q, want true", r.Attrs["is_default"])
	}
	if r := byID[epID.String()]; r.Kind != ResKindEndpoint || r.Parent != mainID.String() || r.State != "ready" || r.Attrs["role"] != core.RoleReadWrite {
		t.Errorf("endpoint = %+v", r)
	}
}

// A controller that owns nothing reports an explicit empty set (not nil), so the
// control plane clears any stale inventory rather than leaving it untouched.
func TestBuildInventory_EmptyIsNonNil(t *testing.T) {
	res, err := BuildInventory(context.Background(), fakeInv{})
	if err != nil {
		t.Fatalf("BuildInventory: %v", err)
	}
	if res == nil {
		t.Fatal("empty inventory returned nil, want a non-nil empty slice")
	}
	if len(res) != 0 {
		t.Fatalf("empty inventory returned %d items", len(res))
	}
}
