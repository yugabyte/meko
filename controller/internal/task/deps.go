package task

import (
	"context"
	"encoding/hex"

	"github.com/google/uuid"

	"github.com/yugabyte/yb-amp/controller/internal/deploy"
)

// This file defines the *ports* the step catalog (steps.go) depends on: narrow
// interfaces over the controller's collaborators, so step handlers are unit
// testable against in-memory fakes and the framework stays decoupled from the
// concrete state store, storage controller, and deployer. The real adapters that
// satisfy these ports are assembled by ProductionRegistry (registry.go); the
// id-keyed state operations live on *core.PgBackend (core/provision.go) and the
// recipe operations bridge to the storage-controller client.
//
// Naming: the controller's user-facing ladder is storage pool -> project ->
// branch -> endpoint. A project is the storage layer's tenant and a branch its
// timeline; the controller carries the storage ids verbatim, mapping
// project_id -> tenant_id and branch_id -> timeline_id only at the storage seam
// (the deploy spec and the storage-controller recipe), via hex32.

// StateStore is the set of *idempotent, id-keyed* project/branch operations the
// step catalog drives. Unlike core.Backend's id-generating create methods (which
// the synchronous API path uses), every operation here takes the ids explicitly
// so a step can be re-run after a crash and converge instead of creating a
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

	// SetEndpointAddress records an endpoint's client address (absolute UPDATE,
	// idempotent). A missing endpoint is not an error.
	SetEndpointAddress(ctx context.Context, endpointID uuid.UUID, address string) error

	// DeleteEndpoint removes an endpoint record (idempotent; absent row is a no-op).
	DeleteEndpoint(ctx context.Context, endpointID uuid.UUID) error
}

// BranchRecipe is the pending-init recipe a freshly-declared branch carries until
// an endpoint materializes it (controller/architecture/controller-timeline-create.md).
// It mirrors the storage controller's PendingTimelineInit without coupling the
// task package to that client type; the adapter in ProductionRegistry bridges the
// two. AncestorBranchID is "" for a project's default branch, set (with StartLSN)
// for a branch off another; the ids it bridges are 32 lowercase hex digits.
type BranchRecipe struct {
	BranchName       string
	PgVersion        int
	AncestorBranchID string
	StartLSN         string
}

// RecipeStore records and clears the storage controller's pending-init recipes
// (controller-timeline-create.md). The projectID/branchID are 32 lowercase hex
// digits (the storage-layer TenantId/TimelineId the controller maps to). It may
// be unavailable (no managed storage controller), in which case the handlers
// no-op with a warning — mirroring the synchronous handler.
type RecipeStore interface {
	RecordBranchRecipe(ctx context.Context, projectID, branchID string, r BranchRecipe) error
	// ClearBranchRecipe drops the recipe once the branch is materialized. It is
	// idempotent: a missing recipe is treated as success.
	ClearBranchRecipe(ctx context.Context, projectID, branchID string) error
}

// Deps bundles the collaborators the production step handlers need.
// Recipes and Deployer may be nil when the corresponding service is disabled;
// the handlers that need them degrade or fail accordingly (a create_endpoint
// task is only planned when a deployer is configured — see ProductionRegistry).
type Deps struct {
	State         StateStore
	Recipes       RecipeStore
	Deployer      deploy.Deployer
	EndpointYB    deploy.YBConn
	SCEndpointURL string // controller URL a launched endpoint uses to reach the storage controller
	Bootstrap     bool   // initdb the recorded branch on a (non-fork) endpoint's first deploy
}

// hex32 renders a UUID as the storage layer's 32-lowercase-hex TenantId/TimelineId
// (no dashes): a project id becomes the storage tenant id and a branch id the
// storage timeline id. It matches the controller API layer's identical rendering.
func hex32(id uuid.UUID) string {
	return hex.EncodeToString(id[:])
}
