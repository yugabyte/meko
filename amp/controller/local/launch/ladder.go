package launch

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/local/client"
	"github.com/yugabyte/amp/controller/local/config"
)

// The launcher's own resource ladder: a user and a storage pool, nothing more.
//
// Clusters are NOT bootstrapped. A cluster is one independent lineage, so it is
// something a caller creates on purpose (`cluster create`) rather than ambient
// state the launcher carries — which is also how the product works. Everything
// below a cluster is addressed by id, so there is no "current cluster" to track.
//
// The user and pool ids are fixed rather than generated. Both create endpoints
// are idempotent on an explicit id, so a fixed one converges even against a
// database that outlives the config dir, and there is exactly one launcher
// identity per machine.
const (
	// LauncherUserID owns everything the launcher creates.
	LauncherUserID = "a11c0000-0000-0000-0000-0000000000d1"
	// LauncherPoolID is the launcher's storage pool.
	LauncherPoolID = "a11c0000-0000-0000-0000-0000000000d2"
	// LauncherPoolName names the pool for a human reading the admin console.
	LauncherPoolName = "yb-amp-local"
)

// branchTaskTimeout bounds how long a branch create/delete task is waited on.
// Branch tasks do no I/O beyond a handful of row writes — nothing in them waits
// on a pod — so this only has to cover worker pickup plus the transaction.
const branchTaskTimeout = 60 * time.Second

// endpointTaskTimeout bounds how long an endpoint create/delete task is waited
// on. With the local driver the step launches nothing — the launcher spawns its
// own compute — so like a branch task this only has to cover worker pickup plus
// a couple of row writes.
const endpointTaskTimeout = 60 * time.Second

// clusterCreateTimeout bounds how long EnsureLadder waits for the create-cluster
// task. It is a couple of task-worker poll intervals plus the two inserts the
// task actually does; nothing in it waits on a pod.
const clusterCreateTimeout = 60 * time.Second

// EnsureLadder makes sure the launcher's user and storage pool exist. Idempotent.
// Returns whether env was mutated, so the caller can persist — nothing to persist
// today, since both ids are constants.
//
// It must run after the controller answers /readyz and before anything creates
// a cluster. Returns whether env was mutated, so the caller can persist.
func EnsureLadder(ctx context.Context, env *config.LocalEnv) (bool, error) {
	base, err := controllerBase(env)
	if err != nil {
		return false, err
	}
	apiClient := client.NewAPI(base, httpClient())

	userID, err := uuid.Parse(LauncherUserID)
	if err != nil {
		return false, fmt.Errorf("parse launcher user id: %w", err)
	}
	poolID, err := uuid.Parse(LauncherPoolID)
	if err != nil {
		return false, fmt.Errorf("parse launcher pool id: %w", err)
	}

	if err := apiClient.EnsureUser(ctx, userID, true); err != nil {
		return false, fmt.Errorf("ensure launcher user: %w", err)
	}
	if err := apiClient.EnsureStoragePool(ctx, poolID, userID, LauncherPoolName); err != nil {
		return false, fmt.Errorf("ensure launcher storage pool: %w", err)
	}

	return false, nil
}

// awaitTask polls a task to a terminal status, failing on `failed` and on
// timeout. The controller's mutations are async (202 + task), so this is how a
// synchronous CLI waits for one.
func awaitTask(ctx context.Context, api *client.API, taskID uuid.UUID, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		task, err := api.GetTask(ctx, taskID)
		if err != nil {
			return fmt.Errorf("poll task %s: %w", taskID, err)
		}
		switch task.Status {
		case "succeeded":
			return nil
		case "failed":
			msg := "no error recorded"
			if task.Error != nil {
				msg = *task.Error
			}
			return fmt.Errorf("task %s failed: %s", taskID, msg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("task %s did not finish within %s (status %s)", taskID, timeout, task.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// persistLadder writes env back to disk after EnsureLadder mutated it, reporting
// a failure to stderr rather than aborting startup: the ids are re-resolvable on
// the next run, so a config write failure is not fatal to a cluster that is
// already up.
func persistLadder(env *config.LocalEnv) {
	if err := env.PersistConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not persist the launcher cluster id: %v\n", err)
	}
}

// branchAPI bundles the product-API client with the launcher's pool and cluster
// ids — everything a branch call needs. Branches go through /api/v1 (#515), so
// this is the seam every branch command starts from.
type branchAPI struct {
	api    *client.API
	poolID uuid.UUID
}

func newBranchAPI(env *config.LocalEnv) (*branchAPI, error) {
	base, err := controllerBase(env)
	if err != nil {
		return nil, err
	}
	poolID, err := uuid.Parse(LauncherPoolID)
	if err != nil {
		return nil, fmt.Errorf("parse launcher pool id: %w", err)
	}
	return &branchAPI{api: client.NewAPI(base, httpClient()), poolID: poolID}, nil
}

// allBranches returns every live branch across the launcher's clusters.
//
// The launcher owns more than one cluster: a ROOT branch is a cluster create,
// because a cluster is exactly one independent lineage (its default branch is
// that lineage's root, with its own initdb). So resolution spans clusters — a
// branch id is not scoped to any one cluster.
func (b *branchAPI) allBranches(ctx context.Context) ([]api.Branch, error) {
	clusters, err := b.api.ListClusters(ctx, b.poolID)
	if err != nil {
		return nil, err
	}
	var out []api.Branch
	for _, p := range clusters {
		branches, err := b.api.ListBranches(ctx, b.poolID, p.Id)
		if err != nil {
			return nil, err
		}
		out = append(out, branches...)
	}
	return out, nil
}

// resolveBranch returns the live branch with the given id, from any
// of the launcher's clusters.
func (b *branchAPI) resolveBranch(ctx context.Context, id config.BranchID) (*api.Branch, error) {
	branches, err := b.allBranches(ctx)
	if err != nil {
		return nil, err
	}
	for i := range branches {
		if branches[i].BranchId == id {
			return &branches[i], nil
		}
	}
	return nil, nil
}

// resolveBranchInCluster returns the live branch with the given name in one
// cluster, which is how a just-created fork's id is read back. Names are unique
// among a cluster's live branches, so within a cluster a name is unambiguous.
func (b *branchAPI) resolveBranchInCluster(ctx context.Context, clusterID uuid.UUID, name string) (*api.Branch, error) {
	branches, err := b.api.ListBranches(ctx, b.poolID, clusterID)
	if err != nil {
		return nil, err
	}
	for i := range branches {
		if branches[i].Name == name {
			return &branches[i], nil
		}
	}
	return nil, nil
}

// parseBranchID parses a branch id at the launcher's edge — a CLI flag — into the
// uuid everything inside carries. Lenient: `uuid.Parse` also takes the flat-hex
// spelling ids minted before the id was a uuid were printed in.
func parseBranchID(raw string) (config.BranchID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("malformed branch id %q: %w", raw, err)
	}
	return id, nil
}
