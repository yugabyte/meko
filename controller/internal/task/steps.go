package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/yugabyte/yb-amp/controller/internal/deploy"
	applog "github.com/yugabyte/yb-amp/controller/internal/log"
)

// This file is the step catalog (amp-controller.md §8): the re-usable, idempotent
// steps every task is composed of. Each handler reads its typed args from the
// step's planned JSON, performs its effect through a narrow port (deps.go), and
// returns a JSON result. Every Run must converge when re-run after a crash or
// partial execution — the worker replays the first non-succeeded step on reclaim
// (§8.1, §8.2), so a step that already half-applied must finish cleanly the
// second time rather than double-apply or error.

// Step kinds — the catalog. These are the task_step.kind values; the worker
// looks a step's kind up in the Registry to find its Handler.
const (
	StepCreateProject      = "create_project"
	StepCreateBranch       = "create_branch"
	StepRecordBranchRecipe = "record_branch_recipe"
	StepCreateEndpoint     = "create_endpoint"
	StepDeleteEndpoint     = "delete_endpoint"
)

// --- create_project ---------------------------------------------------------

// CreateProjectArgs are the inputs to the create_project step: the project + its
// default branch ids (assigned write-ahead at plan time) and the pool they live
// under.
type CreateProjectArgs struct {
	PoolID    uuid.UUID `json:"pool_id"`
	ProjectID uuid.UUID `json:"project_id"`
	BranchID  uuid.UUID `json:"branch_id"`
	Name      string    `json:"name"`
	PgVersion int       `json:"pg_version"`
}

// CreateProjectHandler creates a project and its default branch.
type CreateProjectHandler struct{ State StateStore }

func (CreateProjectHandler) Kind() string { return StepCreateProject }

func (h CreateProjectHandler) Run(ctx context.Context, in StepInput) (json.RawMessage, error) {
	var args CreateProjectArgs
	if err := json.Unmarshal(in.Step.Args, &args); err != nil {
		return nil, fmt.Errorf("decode args: %w", err)
	}
	if err := h.State.EnsureProject(ctx, args.PoolID, args.ProjectID, args.BranchID, args.Name, args.PgVersion); err != nil {
		return nil, fmt.Errorf("ensure project+default branch: %w", err)
	}
	return mustResult(map[string]string{"project_id": args.ProjectID.String(), "branch_id": args.BranchID.String()}), nil
}

// --- create_branch ----------------------------------------------------------

// CreateBranchArgs are the inputs to the create_branch step. The branch stays in
// its parent's project; ForkLSN is the storage layer's "X/X" pg_lsn form,
// resolved at plan time.
type CreateBranchArgs struct {
	ParentBranchID uuid.UUID `json:"parent_branch_id"`
	ProjectID      uuid.UUID `json:"project_id"`
	BranchID       uuid.UUID `json:"branch_id"`
	Name           string    `json:"name"`
	ForkLSN        string    `json:"fork_lsn"`
	PgVersion      int       `json:"pg_version"`
}

// CreateBranchHandler forks an existing branch into a new branch.
type CreateBranchHandler struct{ State StateStore }

func (CreateBranchHandler) Kind() string { return StepCreateBranch }

func (h CreateBranchHandler) Run(ctx context.Context, in StepInput) (json.RawMessage, error) {
	var args CreateBranchArgs
	if err := json.Unmarshal(in.Step.Args, &args); err != nil {
		return nil, fmt.Errorf("decode args: %w", err)
	}
	if err := h.State.EnsureBranch(ctx, args.ParentBranchID, args.ProjectID, args.BranchID, args.Name, args.ForkLSN, args.PgVersion); err != nil {
		return nil, fmt.Errorf("ensure branch: %w", err)
	}
	return mustResult(map[string]string{"project_id": args.ProjectID.String(), "branch_id": args.BranchID.String()}), nil
}

// --- record_branch_recipe ---------------------------------------------------

// RecordBranchRecipeArgs are the inputs to the record_branch_recipe step: the
// recipe the storage controller holds until an endpoint materializes the branch.
// AncestorBranchID is nil for a project's default branch, set for a branch off
// another (with StartLSN = the fork point).
type RecordBranchRecipeArgs struct {
	ProjectID        uuid.UUID  `json:"project_id"`
	BranchID         uuid.UUID  `json:"branch_id"`
	BranchName       string     `json:"branch_name"`
	PgVersion        int        `json:"pg_version"`
	AncestorBranchID *uuid.UUID `json:"ancestor_branch_id,omitempty"`
	StartLSN         string     `json:"start_lsn"`
}

// RecordBranchRecipeHandler records the storage controller's pending-init recipe.
// With no storage controller (Recipes nil) it no-ops with a warning, matching the
// synchronous handler — the recipe then lives only in the control DB's branch row.
type RecordBranchRecipeHandler struct{ Recipes RecipeStore }

func (RecordBranchRecipeHandler) Kind() string { return StepRecordBranchRecipe }

func (h RecordBranchRecipeHandler) Run(ctx context.Context, in StepInput) (json.RawMessage, error) {
	var args RecordBranchRecipeArgs
	if err := json.Unmarshal(in.Step.Args, &args); err != nil {
		return nil, fmt.Errorf("decode args: %w", err)
	}
	if h.Recipes == nil {
		applog.FromContext(ctx).Warn("storage controller not available; branch recipe recorded in the control DB only",
			applog.KeyProjectID, args.ProjectID.String(), applog.KeyBranchID, args.BranchID.String())
		return mustResult(map[string]bool{"recorded": false}), nil
	}
	ancestor := ""
	if args.AncestorBranchID != nil {
		ancestor = hex32(*args.AncestorBranchID)
	}
	if err := h.Recipes.RecordBranchRecipe(ctx, hex32(args.BranchID), BranchRecipe{
		BranchName:       args.BranchName,
		PgVersion:        args.PgVersion,
		AncestorBranchID: ancestor,
		StartLSN:         args.StartLSN,
	}); err != nil {
		return nil, fmt.Errorf("record branch recipe: %w", err)
	}
	return mustResult(map[string]bool{"recorded": true}), nil
}

// --- create_endpoint --------------------------------------------------------

// CreateEndpointArgs are the inputs to the create_endpoint step — the shared step
// that create-project, branch, and scale-up all use (§8). It records the
// endpoint, deploys the compute, records the address, and clears the branch
// recipe. AncestorBranchID/AncestorLSN are set when the target branch is a fork,
// so the endpoint clones from its ancestor instead of initdb'ing.
type CreateEndpointArgs struct {
	EndpointID       uuid.UUID  `json:"endpoint_id"`
	ProjectID        uuid.UUID  `json:"project_id"`
	BranchID         uuid.UUID  `json:"branch_id"`
	Role             string     `json:"role"`
	PgVersion        int        `json:"pg_version"`
	AncestorBranchID *uuid.UUID `json:"ancestor_branch_id,omitempty"`
	AncestorLSN      string     `json:"ancestor_lsn,omitempty"`
}

// CreateEndpointHandler records the endpoint, provisions the compute, records the
// resulting address, and clears the branch's pending-init recipe (the endpoint
// materializes the branch). All four effects are idempotent, so the step
// converges when replayed: the record insert is id-keyed ON CONFLICT DO NOTHING,
// the deploy is keyed by the endpoint's stable name (deployer idempotency), the
// address write is an absolute UPDATE, and the recipe clear tolerates an
// already-absent recipe.
type CreateEndpointHandler struct {
	State     StateStore
	Recipes   RecipeStore
	Deployer  deploy.Deployer
	YB        deploy.YBConn
	SCURL     string
	Bootstrap bool
}

func (CreateEndpointHandler) Kind() string { return StepCreateEndpoint }

func (h CreateEndpointHandler) Run(ctx context.Context, in StepInput) (json.RawMessage, error) {
	if h.Deployer == nil {
		return nil, fmt.Errorf("create_endpoint step requires a deployer, but none is configured")
	}
	var args CreateEndpointArgs
	if err := json.Unmarshal(in.Step.Args, &args); err != nil {
		return nil, fmt.Errorf("decode args: %w", err)
	}
	logger := applog.FromContext(ctx).With(applog.KeyEndpointID, args.EndpointID.String())

	// Record the endpoint row first: it claims the single-writer slot (a second
	// writer is rejected here) and gives the deploy a stable id, before any
	// compute is spent. config is the spec stored for (re)creation.
	config, err := endpointConfig(args)
	if err != nil {
		return nil, err
	}
	if err := h.State.EnsureEndpointRecord(ctx, args.EndpointID, args.ProjectID, args.BranchID, args.Role, config); err != nil {
		return nil, fmt.Errorf("ensure endpoint record: %w", err)
	}

	// The deploy spec speaks the storage layer's tenant/timeline ids: a project is
	// the storage tenant and a branch its timeline, mapped here via hex32.
	spec := deploy.EndpointSpec{
		Name:                 EndpointDeployName(args.EndpointID),
		TenantID:             hex32(args.ProjectID),
		TimelineID:           hex32(args.BranchID),
		PgVersion:            args.PgVersion,
		YB:                   h.YB,
		StorageControllerURL: h.SCURL,
		// A fork clones from its ancestor; only a default branch initdb's.
		Bootstrap: h.Bootstrap && args.AncestorBranchID == nil,
	}
	if args.AncestorBranchID != nil {
		spec.AncestorTimelineID = hex32(*args.AncestorBranchID)
		spec.AncestorLSN = args.AncestorLSN
		spec.Bootstrap = false
	}

	deployed, err := h.Deployer.CreateEndpoint(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("deploy endpoint: %w", err)
	}
	if err := h.State.SetEndpointAddress(ctx, args.EndpointID, deployed.Address); err != nil {
		return nil, fmt.Errorf("record endpoint address: %w", err)
	}

	// The endpoint materialized the branch, so drop the pending-init recipe.
	// Best-effort and idempotent: a lingering recipe is harmless (nothing reads
	// it), so a clear failure does not fail the task.
	if h.Recipes != nil {
		if err := h.Recipes.ClearBranchRecipe(ctx, hex32(args.BranchID)); err != nil {
			logger.Warn("failed to clear branch recipe (continuing)",
				applog.KeyProjectID, args.ProjectID.String(), applog.KeyBranchID, args.BranchID.String(), "error", err.Error())
		}
	}

	logger.Info("endpoint created and deployed", "address", deployed.Address)
	return mustResult(map[string]string{"endpoint_id": args.EndpointID.String(), "address": deployed.Address}), nil
}

// --- delete_endpoint --------------------------------------------------------

// DeleteEndpointArgs are the inputs to the delete_endpoint step.
type DeleteEndpointArgs struct {
	EndpointID uuid.UUID `json:"endpoint_id"`
}

// DeleteEndpointHandler retires the running instance, then deletes the record.
// Both effects are idempotent (a missing instance / row is a no-op), so a
// replay after a crash between them converges.
type DeleteEndpointHandler struct {
	State    StateStore
	Deployer deploy.Deployer
}

func (DeleteEndpointHandler) Kind() string { return StepDeleteEndpoint }

func (h DeleteEndpointHandler) Run(ctx context.Context, in StepInput) (json.RawMessage, error) {
	if h.Deployer == nil {
		return nil, fmt.Errorf("delete_endpoint step requires a deployer, but none is configured")
	}
	var args DeleteEndpointArgs
	if err := json.Unmarshal(in.Step.Args, &args); err != nil {
		return nil, fmt.Errorf("decode args: %w", err)
	}
	if err := h.Deployer.DeleteEndpoint(ctx, EndpointDeployName(args.EndpointID)); err != nil {
		return nil, fmt.Errorf("retire endpoint instance: %w", err)
	}
	if err := h.State.DeleteEndpoint(ctx, args.EndpointID); err != nil {
		return nil, fmt.Errorf("delete endpoint record: %w", err)
	}
	return mustResult(map[string]string{"endpoint_id": args.EndpointID.String()}), nil
}

// --- helpers ----------------------------------------------------------------

// endpointConfig is the spec stored on the endpoint row for (re)creation. The
// endpoint's own id is the row id, so it is not part of the recreate spec. This
// matches the JSON the synchronous create-endpoint handler stores.
func endpointConfig(args CreateEndpointArgs) ([]byte, error) {
	b, err := json.Marshal(map[string]any{
		"project_id": args.ProjectID,
		"branch_id":  args.BranchID,
		"role":       args.Role,
		"pg_version": args.PgVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("encode endpoint config: %w", err)
	}
	return b, nil
}

// mustResult marshals a step result. The values are small, fixed-shape maps, so
// marshaling cannot fail; a result is informational (read back via the task API)
// and never load-bearing, so a marshal error degrades to an empty result rather
// than failing an otherwise-successful step.
func mustResult(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
