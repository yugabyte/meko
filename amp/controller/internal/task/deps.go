package task

import (
	"context"
	"encoding/hex"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/extensions"
)

// This file defines the *ports* the step catalog (the step_*.go files) depends
// on: narrow interfaces over the controller's collaborators, so steps are unit
// testable against in-memory fakes and the framework stays decoupled from the
// concrete state store, controller, and deployer. The real adapters that
// satisfy these ports are bundled into Deps and carried by the Registry, which
// binds them into each step at construction (registry.go) — so a built step keeps
// only the specific ports it uses and Execute needs only a context. The id-keyed
// state operations live on *core.PgBackend (core/provision.go) and the recipe
// operations bridge to the in-process registry store.
//
// Naming: the controller's user-facing ladder is storage pool -> cluster ->
// branch -> endpoint. The storage layer is flat: a branch's branch_id IS the
// storage-layer BranchId (no enclosing tenant), so the controller carries it
// verbatim, rendering it to 32-hex at the storage seam (the pending-init
// recipe) via hex32. A cluster no longer maps to a storage tenant.

// StateStore is the set of *idempotent, id-keyed* cluster/branch operations the
// step catalog drives. Unlike core.Backend's id-generating create methods (the
// synchronous read / pre-check path uses the id-generating Get methods and never
// these), every operation here takes the ids explicitly so a step can be re-run
// after a crash and converge instead of creating a
// duplicate or erroring: the ids are assigned write-ahead when the task is
// planned (§8.1) and baked into the step args. *core.PgBackend satisfies this
// structurally.
type StateStore interface {
	// EnsureCluster creates a cluster and its default branch with the given ids,
	// or no-ops if they already exist. A cluster-name collision within the pool,
	// or a missing pool, is a real error (the step fails the task).
	EnsureCluster(ctx context.Context, poolID, clusterID, branchID uuid.UUID, name string, pgVersion int) error

	// EnsureBranch creates a branch (a non-default branch) under clusterID, forked
	// from parentBranchID at forkLSN, with the given id — or no-ops if it already
	// exists. A name collision within the cluster, or a missing parent, is a real
	// error. No pool id: a branch is scoped to its cluster.
	EnsureBranch(ctx context.Context, parentBranchID, clusterID, branchID uuid.UUID, name, forkLSN string, pgVersion int) error

	// EnsureEndpointRecord records an endpoint on the (clusterID, branchID) branch
	// with the given id, or no-ops if that id already exists. A *second*
	// read_write endpoint (a different id violating the single-writer index) or a
	// missing branch is a real error.
	//
	// The four trailing JSON documents are positional and same-typed, so check the
	// order when editing a call. Not a struct because the type would have to live
	// in `task` or `core`, and `core`'s in-package test imports back the other
	// way, so that edge would be an import cycle.
	EnsureEndpointRecord(ctx context.Context, endpointID, clusterID, branchID uuid.UUID, role, name string,
		config, pgSettings, pageserverSettings, safekeeperSettings []byte) error

	// BranchExtensions returns the branch's opted-in externalized extension names
	// (the durable set the create_endpoint step unions with the catalog defaults
	// to resolve the endpoint's ImageVolume mounts). An absent branch yields an
	// empty slice, not an error.
	BranchExtensions(ctx context.Context, clusterID, branchID uuid.UUID) ([]string, error)

	// BranchPgSettings returns the branch's baseline PostgreSQL settings (nil for
	// none / an absent branch). An endpoint's own settings merge over these.
	BranchPgSettings(ctx context.Context, clusterID, branchID uuid.UUID) (map[string]string, error)

	// AddBranchExtensions adds names to the branch's durable extension set (the
	// createEndpoint opt-in). It is an absolute union — extensions becomes
	// distinct(existing ∪ names) — so a task replay is idempotent. An absent
	// branch is a no-op (mirrors BranchExtensions); empty names is a no-op.
	AddBranchExtensions(ctx context.Context, clusterID, branchID uuid.UUID, names []string) error

	// SetEndpointAddress records an endpoint's client address and its tailnet
	// address (absolute UPDATE, idempotent). tailnetAddress is "" when the
	// endpoint is not exposed on the tailnet. A missing endpoint is not an error.
	SetEndpointAddress(ctx context.Context, endpointID uuid.UUID, address, tailnetAddress string) error

	// DeleteEndpoint removes an endpoint record (idempotent; absent row is a no-op).
	DeleteEndpoint(ctx context.Context, endpointID uuid.UUID) error

	// UpdateEndpointResources merges the new cpu/memory into the endpoint's stored
	// config (the (re)create spec), so a controller-driven redeploy keeps the new
	// size. Idempotent; an absent endpoint is a no-op. The resize step's DB-side
	// write, paired with the deployer's live pod resize.
	UpdateEndpointResources(ctx context.Context, endpointID uuid.UUID, cpu, memory string) error

	// EndpointIDsForBranch returns the ids of every endpoint on the
	// (clusterID, branchID) branch. It is the cascade-delete enumeration the
	// delete_branch step runs at Execute time (so a concurrently-added endpoint is
	// still retired on replay). An absent branch yields an empty slice, not an
	// error — a re-run mid-cascade converges.
	EndpointIDsForBranch(ctx context.Context, clusterID, branchID uuid.UUID) ([]uuid.UUID, error)

	// EndpointIDsForCluster returns the ids of every endpoint in the cluster. It is
	// the cascade-delete enumeration the delete_cluster step runs at Execute time.
	// An absent cluster yields an empty slice, not an error.
	EndpointIDsForCluster(ctx context.Context, clusterID uuid.UUID) ([]uuid.UUID, error)

	// ChildBranchIDs returns the ids of every branch in clusterID forked directly
	// from branchID (its in-cluster children). The delete_branch step calls this
	// before retiring any endpoints: a branch with child forks cannot be deleted
	// (the parent FK is RESTRICT), so the step fails fast and retires nothing,
	// avoiding a half-deleted branch. An absent branch yields an empty slice.
	ChildBranchIDs(ctx context.Context, clusterID, branchID uuid.UUID) ([]uuid.UUID, error)

	// DeleteBranch TOMBSTONES the branch: `dropped_at` is stamped and the row stays,
	// because a branch id is never reused. Idempotent — an absent or already
	// tombstoned row is a no-op. A live child fork or a surviving endpoint is
	// ErrConflict, backstopping the delete_branch step's own pre-check.
	//
	// Do NOT also stamp `reap_after`: internal/branchgc owns that deadline.
	DeleteBranch(ctx context.Context, clusterID, branchID uuid.UUID) error

	// DeleteCluster tombstones the cluster and every branch under it, and revokes
	// its grants (idempotent; absent rows are a no-op). It refuses while any of the
	// cluster's branches still has an endpoint; delete_cluster retires them first.
	// Branches are tombstoned exactly as DeleteBranch does — `dropped_at` only.
	DeleteCluster(ctx context.Context, clusterID uuid.UUID) error
}

// BranchRecipe is the pending-init recipe a freshly-declared branch carries until
// an endpoint materializes it (controller/architecture/controller-cluster-create.md).
// It keeps the task package decoupled from the storage-layer store's types; the
// adapter in cmd/controller bridges the two. AncestorBranchID is "" for a
// cluster's default branch, set (with StartLSN) for a branch off another; the ids
// it bridges are 32 lowercase hex digits.
type BranchRecipe struct {
	BranchName       string
	PgVersion        int
	AncestorBranchID string
	StartLSN         string
	// PgSettings merge over the ancestor's, the caller's winning.
	PgSettings map[string]string
}

// RecipeStore seeds the storage-layer state a declared branch needs before any
// pageserver can attach it (controller-cluster-create.md).
//
// clusterID scopes the branch to its cluster: there is one branch table, shared
// with the product API, so the write has to say which cluster's branch it means.
// branchID is that branch's 32-lowercase-hex BranchId — the flat rendering the
// storage layer addresses.
//
// It may be unavailable (no store wired), in which case the step no-ops with a
// warning; the branch then exists in the control plane but has no
// amp_storage.branch_state row, so nothing can materialize it.
type RecipeStore interface {
	RecordBranchRecipe(ctx context.Context, clusterID uuid.UUID, branchID string, r BranchRecipe) error
}

// Deps bundles the collaborators the production steps need. The Registry holds
// one Deps and binds it into each step it builds (registry.go); a step then keeps
// only the specific ports it uses. Recipes and Deployer may be nil when the
// corresponding service is disabled; the steps that need them degrade or fail
// accordingly (a create_endpoint task is only planned when a deployer is
// configured — see plan.go / the API wiring, #113).
type Deps struct {
	State         StateStore
	Recipes       RecipeStore
	Deployer      deploy.Deployer
	EndpointYB    deploy.YBConn
	ControllerURL string // controller URL a launched endpoint uses to reach the controller
	// Extensions is the externalized-extension catalog the create_endpoint step
	// resolves a branch's extension set against (names -> pinned images). Nil
	// disables externalized extensions (the endpoint ships only image-baked ones),
	// e.g. under the Docker driver or when the feature is off.
	Extensions *extensions.Catalog
}

// hex32 renders a branch uuid as the flat 32-lowercase-hex spelling the daemons
// and the endpoint spec carry (no dashes). Every `branch_id` column holds the uuid
// itself, so this is a wire rendering, not a second identity — and the id needs no
// scoping either way, the storage layer being flat (no enclosing tenant). It
// matches the controller API layer's identical rendering.
func hex32(id uuid.UUID) string {
	return hex.EncodeToString(id[:])
}
