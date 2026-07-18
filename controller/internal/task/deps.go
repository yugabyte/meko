package task

import (
	"context"
	"encoding/hex"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/deploy"
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
// Naming: the controller's user-facing ladder is storage pool -> project ->
// branch -> endpoint. The storage layer is flat: a branch's branch_id IS the
// storage-layer BranchId (no enclosing tenant), so the controller carries it
// verbatim, rendering it to 32-hex at the storage seam (the pending-init
// recipe) via hex32. A project no longer maps to a storage tenant.

// StateStore is the set of *idempotent, id-keyed* project/branch operations the
// step catalog drives. Unlike core.Backend's id-generating create methods (the
// synchronous read / pre-check path uses the id-generating Get methods and never
// these), every operation here takes the ids explicitly so a step can be re-run
// after a crash and converge instead of creating a
// duplicate or erroring: the ids are assigned write-ahead when the task is
// planned (§8.1) and baked into the step args. *core.PgBackend satisfies this
// structurally.
type StateStore interface {
	// EnsureProject creates a project and its default branch with the given ids,
	// or no-ops if they already exist. A project-name collision within the pool,
	// or a missing pool, is a real error (the step fails the task).
	EnsureProject(ctx context.Context, poolID, projectID, branchID uuid.UUID, name string, pgVersion int) error

	// EnsureBranch creates a branch (a non-default branch) under projectID, forked
	// from parentBranchID at forkLSN, with the given id — or no-ops if it already
	// exists. A name collision within the project, or a missing parent, is a real
	// error. No pool id: a branch is scoped to its project.
	EnsureBranch(ctx context.Context, parentBranchID, projectID, branchID uuid.UUID, name, forkLSN string, pgVersion int) error

	// EnsureEndpointRecord records an endpoint on the (projectID, branchID) branch
	// with the given id, or no-ops if that id already exists. A *second*
	// read_write endpoint (a different id violating the single-writer index) or a
	// missing branch is a real error.
	EnsureEndpointRecord(ctx context.Context, endpointID, projectID, branchID uuid.UUID, role string, config []byte) error

	// SetEndpointAddress records an endpoint's client address and its tailnet
	// address (absolute UPDATE, idempotent). tailnetAddress is "" when the
	// endpoint is not exposed on the tailnet. A missing endpoint is not an error.
	SetEndpointAddress(ctx context.Context, endpointID uuid.UUID, address, tailnetAddress string) error

	// DeleteEndpoint removes an endpoint record (idempotent; absent row is a no-op).
	DeleteEndpoint(ctx context.Context, endpointID uuid.UUID) error

	// EndpointIDsForBranch returns the ids of every endpoint on the
	// (projectID, branchID) branch. It is the cascade-delete enumeration the
	// delete_branch step runs at Execute time (so a concurrently-added endpoint is
	// still retired on replay). An absent branch yields an empty slice, not an
	// error — a re-run mid-cascade converges.
	EndpointIDsForBranch(ctx context.Context, projectID, branchID uuid.UUID) ([]uuid.UUID, error)

	// EndpointIDsForProject returns the ids of every endpoint in the project. It is
	// the cascade-delete enumeration the delete_project step runs at Execute time.
	// An absent project yields an empty slice, not an error.
	EndpointIDsForProject(ctx context.Context, projectID uuid.UUID) ([]uuid.UUID, error)

	// ChildBranchIDs returns the ids of every branch in projectID forked directly
	// from branchID (its in-project children). The delete_branch step calls this
	// before retiring any endpoints: a branch with child forks cannot be deleted
	// (the parent FK is RESTRICT), so the step fails fast and retires nothing,
	// avoiding a half-deleted branch. An absent branch yields an empty slice.
	ChildBranchIDs(ctx context.Context, projectID, branchID uuid.UUID) ([]uuid.UUID, error)

	// DeleteBranch removes the (projectID, branchID) branch record (idempotent;
	// absent row is a no-op). The caller (the delete_branch step) checks for child
	// forks (ChildBranchIDs) and retires the branch's endpoints first; the parent
	// FK is RESTRICT, so a still-referencing child fork makes this a real error
	// (ErrConflict) — a belt-and-suspenders guard behind the step's pre-check.
	DeleteBranch(ctx context.Context, projectID, branchID uuid.UUID) error

	// DeleteProject removes the project and all its branches (idempotent; absent
	// rows are a no-op). The caller (the delete_project step) retires the project's
	// endpoints first; this then drops the branches (nulling their in-project
	// parent pointers first to sidestep the self-FK RESTRICT ordering) and the
	// project row.
	DeleteProject(ctx context.Context, projectID uuid.UUID) error
}

// BranchRecipe is the pending-init recipe a freshly-declared branch carries until
// an endpoint materializes it (controller/architecture/controller-project-create.md).
// It mirrors the controller's PendingTimelineInit without coupling the
// task package to that client type; the adapter in ProductionRegistry bridges the
// two. AncestorBranchID is "" for a project's default branch, set (with StartLSN)
// for a branch off another; the ids it bridges are 32 lowercase hex digits.
type BranchRecipe struct {
	BranchName       string
	PgVersion        int
	AncestorBranchID string
	StartLSN         string
}

// RecipeStore records and clears the controller's pending-init recipes
// (controller-project-create.md). branchID is 32 lowercase hex digits (the
// storage-layer BranchId the controller maps to — the storage layer is flat, so
// the recipe is keyed by branch_id alone). It may be unavailable (no managed
// controller), in which case the steps no-op with a warning — the
// recipe then lives only in the control DB's branch row.
type RecipeStore interface {
	RecordBranchRecipe(ctx context.Context, branchID string, r BranchRecipe) error
	// ClearBranchRecipe drops the recipe once the branch is materialized. It is
	// idempotent: a missing recipe is treated as success.
	ClearBranchRecipe(ctx context.Context, branchID string) error
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
	Bootstrap     bool   // initdb the recorded branch on a (non-fork) endpoint's first deploy
}

// hex32 renders a UUID as the storage layer's 32-lowercase-hex BranchId (no
// dashes): a branch id becomes the storage branch id verbatim (the storage layer
// is flat — no separate tenant). It matches the controller API layer's identical
// rendering.
func hex32(id uuid.UUID) string {
	return hex.EncodeToString(id[:])
}
