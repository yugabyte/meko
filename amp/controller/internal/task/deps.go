package task

import (
	"context"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/extensions"
	"github.com/yugabyte/amp/controller/internal/voyager/migration"
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
// INVARIANT, in two halves, because the worker re-runs a whole STEP on a
// serialization abort (worker.go) and that is bigger than the call which aborted:
//
//   - the aborting operation must be ATOMIC — a failed call leaves nothing behind.
//     Today's two hold to it by committing their record and their interlock in one
//     transaction (registry.RecordBranch, core.EnsureEndpointRecord).
//   - everything the step did BEFORE that call is re-executed too, so it must
//     converge — the Step contract in task.go, not a new demand. An individually
//     atomic operation still double-applies if an earlier effect does not.
//
// Naming: the controller's user-facing ladder is storage pool -> cluster ->
// branch -> endpoint. The storage layer is flat: a branch's branch_id IS the
// storage-layer BranchId (no enclosing tenant), so the controller carries it
// verbatim as a uuid, in one spelling end to end. A cluster no longer maps to a storage
// tenant.

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
	// or no-ops if they already exist. extensions is the create-time externalized
	// extension set, written onto the default branch row; nil/empty means none. A
	// fork later inherits its parent's set, and every endpoint resolves against
	// its own branch's row. A cluster-name collision within the pool, or a missing
	// pool, is a real error (the step fails the task).
	EnsureCluster(ctx context.Context, poolID, clusterID, branchID uuid.UUID, name string, pgVersion int, engine string, extensions []string) error

	// EnsureBranch creates a branch (a non-default branch) under clusterID, forked
	// from parentBranchID at forkLSN, with the given id — or no-ops if it already
	// exists. The fork inherits the parent branch's externalized-extension set
	// (copied onto its own row), so every endpoint on it mounts the same set. A
	// name collision within the cluster, or a missing parent, is a real error. No
	// pool id: a branch is scoped to its cluster.
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
	// (the set the create_endpoint step unions with the catalog defaults to
	// resolve that endpoint's ImageVolume mounts). It lives on the branch's own
	// row — seeded on the default branch by EnsureCluster, inherited by a fork,
	// and extended per branch by AddBranchExtensions. An absent branch yields an
	// empty slice, not an error.
	BranchExtensions(ctx context.Context, clusterID, branchID uuid.UUID) ([]string, error)

	// BranchPgSettings returns the branch's baseline PostgreSQL settings (nil for
	// none / an absent branch). An endpoint's own settings merge over these.
	BranchPgSettings(ctx context.Context, clusterID, branchID uuid.UUID) (map[string]string, error)

	// AddBranchExtensions adds names to the (clusterID, branchID) branch's durable
	// extension set (the add_branch_extensions opt-in): adding is per branch. It is
	// an absolute union — the row becomes distinct(existing ∪ names) — so a task
	// replay is idempotent. An absent branch is a no-op; empty names is a no-op.
	AddBranchExtensions(ctx context.Context, clusterID, branchID uuid.UUID, names []string) error

	// BranchEndpointDeploys returns the redeploy record for every live endpoint on
	// the (clusterID, branchID) branch (the add_branch_extensions step's
	// Execute-time enumeration, so a concurrently-added endpoint is still remounted
	// on replay). An absent branch yields an empty slice, not an error.
	BranchEndpointDeploys(ctx context.Context, clusterID, branchID uuid.UUID) ([]core.EndpointDeploy, error)

	// AllEndpointDeploys returns the renderable desired state of every live
	// endpoint in the deployment — the fleet roll's Execute-time enumeration
	// (plan_endpoint_upgrades), which has no branch or cluster to scope by. One
	// read for a roll of N endpoints rather than N, and unbounded: the caller
	// wants the whole fleet and holds it in memory.
	AllEndpointDeploys(ctx context.Context) ([]core.EndpointDeploy, error)

	// EndpointDeploysByIDs returns the renderable desired state of the named
	// endpoints, in one read. Unlike the fleet enumeration it keeps an
	// address-less endpoint, so naming a broken one still attempts it; ids with no
	// live row are absent from the result.
	EndpointDeploysByIDs(ctx context.Context, ids []uuid.UUID) ([]core.EndpointDeploy, error)

	// EndpointDeploy returns one endpoint's renderable desired state by id, for a
	// step that redeploys a single endpoint and holds nothing but its id.
	// core.ErrNotFound when the endpoint is gone or its branch is tombstoned — a
	// redeploy has nothing to converge to.
	EndpointDeploy(ctx context.Context, endpointID uuid.UUID) (core.EndpointDeploy, error)

	// UpdateEndpointExtensions records the resolved extension names into the
	// endpoint's stored config blob (audit / recreate spec), an absolute jsonb
	// merge like UpdateEndpointResources. Idempotent; an absent endpoint is a
	// no-op.
	UpdateEndpointExtensions(ctx context.Context, endpointID uuid.UUID, names []string) error

	// SetEndpointAddress records an endpoint's client address and its tailnet
	// address (absolute UPDATE, idempotent). tailnetAddress is "" when the
	// endpoint is not exposed on the tailnet. A missing endpoint is not an error.
	SetEndpointAddress(ctx context.Context, endpointID uuid.UUID, address, tailnetAddress string) error

	// SetEndpointVersion records the version an endpoint is RUNNING, written after
	// a deploy reports back (absolute UPDATE, idempotent; a missing endpoint is not
	// an error). BranchEndpointDeploys reads it back, so it is what every later
	// controller-driven redeploy renders.
	SetEndpointVersion(ctx context.Context, endpointID uuid.UUID, version string) error

	// SetEndpointTargetVersion records the version an endpoint SHOULD run, written
	// before an upgrade deploys. Differing from the running version is an upgrade
	// that has not converged.
	SetEndpointTargetVersion(ctx context.Context, endpointID uuid.UUID, version string) error

	// DeleteEndpoint removes an endpoint record (idempotent; absent row is a no-op).
	DeleteEndpoint(ctx context.Context, endpointID uuid.UUID) error

	// UpdateEndpointResources merges the new cpu/memory into the endpoint's stored
	// config (the (re)create spec), so a controller-driven redeploy keeps the new
	// size. Idempotent; an absent endpoint is a no-op. The resize step's DB-side
	// write, paired with the deployer's live pod resize.
	UpdateEndpointResources(ctx context.Context, endpointID uuid.UUID, cpu, memory string) error

	// SetEndpointYCPGeneration records the YCP desired-state generation fully
	// applied to an endpoint. The worker calls it after every step of the task
	// carrying it has succeeded, so the stored value means the work landed rather
	// than that the controller returned 202 — which is what lets the reconcile agent
	// re-issue an update whose task failed instead of latching it as applied
	// (meko#985). MONOTONIC: a task carrying an older generation that completes after
	// a newer one leaves the newer value standing. Absent row is a no-op.
	SetEndpointYCPGeneration(ctx context.Context, endpointID uuid.UUID, gen int64) error

	// SetBranchYCPGeneration is SetEndpointYCPGeneration for a branch. A tombstoned
	// branch is skipped — nothing will reconcile it again.
	SetBranchYCPGeneration(ctx context.Context, branchID uuid.UUID, gen int64) error

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
// adapter in cmd/controller bridges the two. AncestorBranchID is nil for a
// cluster's default branch, set (with StartLSN) for a branch off another.
type BranchRecipe struct {
	BranchName       string
	PgVersion        int
	AncestorBranchID *uuid.UUID
	StartLSN         string
	// PgSettings merge over the ancestor's, the caller's winning.
	PgSettings map[string]string
}

// RecipeStore seeds the storage-layer state a declared branch needs before any
// pageserver can attach it (controller-cluster-create.md).
//
// clusterID scopes the branch to its cluster: there is one branch table, shared
// with the product API, so the write has to say which cluster's branch it means.
// branchID is that branch's BranchId — the uuid every `branch_id` column holds,
// in the one spelling both sides use.
//
// It may be unavailable (no store wired), in which case the step no-ops with a
// warning; the branch then exists in the control plane but has no
// amp_storage.branch_state row, so nothing can materialize it.
type RecipeStore interface {
	RecordBranchRecipe(ctx context.Context, clusterID, branchID uuid.UUID, r BranchRecipe) error
}

// VoyagerMigrationEndpointLookup resolves the deployed client address
// (host:port) of the endpoint a create-migration-target plan allocated, so the
// attach step can record it as the migration's target. It is intentionally
// narrower than the controller's core backend, keeping the migration steps off
// the domain layer.
type VoyagerMigrationEndpointLookup interface {
	EndpointAddress(ctx context.Context, poolID, projectID, branchID, endpointID uuid.UUID) (string, error)
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

	// VoyagerMigrations, VoyagerMigrationEndpoints, and VoyagerRunner are the
	// Voyager-migration ports the launcher/target steps use. Each is nil when the
	// migration feature is disabled; no migration task is planned without them, so
	// a step built without one fails cleanly rather than running half-wired.
	VoyagerMigrations         migration.Store
	VoyagerMigrationEndpoints VoyagerMigrationEndpointLookup
	VoyagerRunner             deploy.VoyagerJobRunner
	// Extensions is the externalized-extension catalog the create_endpoint step
	// resolves a branch's extension set against (names -> pinned images). Nil
	// disables externalized extensions (the endpoint ships only image-baked ones),
	// e.g. under the Docker driver or when the feature is off.
	Extensions *extensions.Catalog

	// Subtasks runs the children a fan-out parent owns (subtask_runner.go). Unlike
	// every other field it is not filled in by the caller building this struct: a
	// runner needs the Worker, the Worker needs the Registry, and the Registry
	// carries this Deps — so it is written into the registry's copy after the
	// worker exists, by WithSubtasks (worker.go). Nil for a worker built without
	// that option, and for anything that builds a step outside a worker; a parent
	// step fails at Execute rather than at build, as the other optional ports do.
	Subtasks Subtasks
}
