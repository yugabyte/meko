package ycp

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
)

// Resource kinds reported to YCP. They mirror the amp resource ladder (storage
// pool → project/"database" → branch → compute endpoint); the console renders
// the tree from the Parent links.
const (
	ResKindStoragePool = "storage_pool"
	ResKindProject     = "project"
	ResKindBranch      = "branch"
	ResKindEndpoint    = "endpoint"
)

// inventoryPageSize is how many rows each List call pulls. The inventory is a
// full walk, so this only bounds per-query memory, not the total reported.
const inventoryPageSize = 200

// Inventory is the read surface BuildInventory walks. *core.PgBackend (and the
// in-memory core.MemBackend used in tests) satisfy it, so the builder needs no
// database of its own.
type Inventory interface {
	ListStoragePools(ctx context.Context, limit, offset int) ([]core.StoragePool, int, error)
	ListClusters(ctx context.Context, poolID uuid.UUID, limit, offset int, includeFailed bool) ([]core.Cluster, int, error)
	ListBranches(ctx context.Context, poolID, clusterID uuid.UUID, limit, offset int) ([]core.Branch, int, error)
	ListEndpoints(ctx context.Context, poolID, clusterID, branchID uuid.UUID, limit, offset int) ([]core.Endpoint, int, error)
}

// BuildInventory walks every storage pool, its projects, their branches, and the
// endpoints on each branch, flattening them into the ResourceStatus list the
// status heartbeat carries. It is a snapshot: what the controller owns right now.
//
// Failed/orphaned projects are excluded (includeFailed=false) — the inventory is
// what the environment actually runs, not what it tried to.
func BuildInventory(ctx context.Context, inv Inventory) ([]ResourceStatus, error) {
	// Non-nil so a controller that owns nothing reports an explicit empty set
	// (which the control plane stores, clearing any stale inventory) rather than
	// a nil the server would leave untouched.
	out := []ResourceStatus{}

	pools, err := listAll(func(limit, offset int) ([]core.StoragePool, int, error) {
		return inv.ListStoragePools(ctx, limit, offset)
	})
	if err != nil {
		return nil, fmt.Errorf("list storage pools: %w", err)
	}
	for _, p := range pools {
		out = append(out, ResourceStatus{Kind: ResKindStoragePool, ID: p.ID.String(), Name: p.Name})

		clusters, err := listAll(func(limit, offset int) ([]core.Cluster, int, error) {
			return inv.ListClusters(ctx, p.ID, limit, offset, false)
		})
		if err != nil {
			return nil, fmt.Errorf("list clusters of pool %s: %w", p.ID, err)
		}
		for _, cl := range clusters {
			out = append(out, ResourceStatus{
				Kind: ResKindProject, ID: cl.ID.String(), Name: cl.Name, Parent: p.ID.String(),
			})

			branches, err := listAll(func(limit, offset int) ([]core.Branch, int, error) {
				return inv.ListBranches(ctx, p.ID, cl.ID, limit, offset)
			})
			if err != nil {
				return nil, fmt.Errorf("list branches of cluster %s: %w", cl.ID, err)
			}
			for _, b := range branches {
				out = append(out, branchResource(b, cl.ID))

				endpoints, err := listAll(func(limit, offset int) ([]core.Endpoint, int, error) {
					return inv.ListEndpoints(ctx, p.ID, cl.ID, b.BranchID, limit, offset)
				})
				if err != nil {
					return nil, fmt.Errorf("list endpoints of branch %s: %w", b.BranchID, err)
				}
				for _, e := range endpoints {
					out = append(out, endpointResource(e, b.BranchID))
				}
			}
		}
	}
	return out, nil
}

func branchResource(b core.Branch, projectID uuid.UUID) ResourceStatus {
	attrs := map[string]string{
		"pg_version": strconv.Itoa(b.PgVersion),
		"is_default": strconv.FormatBool(b.IsDefault),
	}
	if b.ForkLSN != "" {
		attrs["fork_lsn"] = b.ForkLSN
	}
	return ResourceStatus{
		Kind: ResKindBranch, ID: b.BranchID.String(), Name: b.Name,
		Parent: projectID.String(), Attrs: attrs,
	}
}

func endpointResource(e core.Endpoint, branchID uuid.UUID) ResourceStatus {
	attrs := map[string]string{"role": e.Role}
	if e.Address != "" {
		attrs["address"] = e.Address
	}
	// An endpoint with no address yet is still deploying; one with an address is
	// serving. This drives the console badge without the controller having to
	// model a separate endpoint state.
	state := "provisioning"
	if e.Address != "" {
		state = "ready"
	}
	return ResourceStatus{
		Kind: ResKindEndpoint, ID: e.ID.String(), Name: e.Name,
		Parent: branchID.String(), State: state, Attrs: attrs,
	}
}

// listAll pages through a List(limit, offset)->(items, total, err) function until
// it has every item, so the inventory reflects the whole fleet regardless of how
// many resources a pool/project/branch holds.
func listAll[T any](page func(limit, offset int) ([]T, int, error)) ([]T, error) {
	var all []T
	for offset := 0; ; offset += inventoryPageSize {
		items, total, err := page(inventoryPageSize, offset)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(all) >= total || len(items) == 0 {
			return all, nil
		}
	}
}
