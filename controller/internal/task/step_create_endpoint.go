package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/yugabyte/yb-amp/controller/internal/deploy"
	applog "github.com/yugabyte/yb-amp/controller/internal/log"
)

// StepCreateEndpoint is the task_step.kind for the create_endpoint step.
const StepCreateEndpoint = "create_endpoint"

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

// createEndpointStep records the endpoint, provisions the compute, records the
// resulting address, and clears the branch's pending-init recipe (the endpoint
// materializes the branch). All four effects are idempotent, so the step
// converges when replayed: the record insert is id-keyed ON CONFLICT DO NOTHING,
// the deploy is keyed by the endpoint's stable name (deployer idempotency), the
// address write is an absolute UPDATE, and the recipe clear tolerates an
// already-absent recipe. Its deploy collaborators are bound at construction
// (deployer is required); only the embedded args are serialized.
type createEndpointStep struct {
	CreateEndpointArgs
	state     StateStore
	recipes   RecipeStore
	deployer  deploy.Deployer
	yb        deploy.YBConn
	scURL     string
	bootstrap bool
}

// NewCreateEndpointStep binds create_endpoint args + its deps into an executable step.
func NewCreateEndpointStep(args CreateEndpointArgs, deps Deps) Step {
	return &createEndpointStep{
		CreateEndpointArgs: args,
		state:              deps.State,
		recipes:            deps.Recipes,
		deployer:           deps.Deployer,
		yb:                 deps.EndpointYB,
		scURL:              deps.SCEndpointURL,
		bootstrap:          deps.Bootstrap,
	}
}

func buildCreateEndpointStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args CreateEndpointArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode create_endpoint args: %w", err)
	}
	return NewCreateEndpointStep(args, deps), nil
}

func (*createEndpointStep) Kind() string { return StepCreateEndpoint }

func (s *createEndpointStep) Execute(ctx context.Context) StepResult {
	if s.deployer == nil {
		return Fail(fmt.Errorf("create_endpoint step requires a deployer, but none is configured"))
	}
	logger := applog.FromContext(ctx).With(applog.KeyEndpointID, s.EndpointID.String())

	// Record the endpoint row first: it claims the single-writer slot (a second
	// writer is rejected here) and gives the deploy a stable id, before any
	// compute is spent. config is the spec stored for (re)creation.
	config, err := endpointConfig(s.CreateEndpointArgs)
	if err != nil {
		return Fail(err)
	}
	if err := s.state.EnsureEndpointRecord(ctx, s.EndpointID, s.ProjectID, s.BranchID, s.Role, config); err != nil {
		return Fail(fmt.Errorf("ensure endpoint record: %w", err))
	}

	// The deploy spec speaks the storage layer's flat branch id: a branch's id is
	// the storage branch id verbatim, rendered via hex32 (no enclosing tenant).
	spec := deploy.EndpointSpec{
		Name:                 EndpointDeployName(s.EndpointID),
		BranchID:             hex32(s.BranchID),
		PgVersion:            s.PgVersion,
		YB:                   s.yb,
		StorageControllerURL: s.scURL,
		// A fork clones from its ancestor; only a default branch initdb's.
		Bootstrap: s.bootstrap && s.AncestorBranchID == nil,
	}
	if s.AncestorBranchID != nil {
		spec.AncestorBranchID = hex32(*s.AncestorBranchID)
		spec.AncestorLSN = s.AncestorLSN
		spec.Bootstrap = false
	}

	deployed, err := s.deployer.CreateEndpoint(ctx, spec)
	if err != nil {
		return Fail(fmt.Errorf("deploy endpoint: %w", err))
	}
	if err := s.state.SetEndpointAddress(ctx, s.EndpointID, deployed.Address); err != nil {
		return Fail(fmt.Errorf("record endpoint address: %w", err))
	}

	// The endpoint materialized the branch, so drop the pending-init recipe.
	// Best-effort and idempotent: a lingering recipe is harmless (nothing reads
	// it), so a clear failure does not fail the task.
	if s.recipes != nil {
		if err := s.recipes.ClearBranchRecipe(ctx, hex32(s.BranchID)); err != nil {
			logger.Warn("failed to clear branch recipe (continuing)",
				applog.KeyProjectID, s.ProjectID.String(), applog.KeyBranchID, s.BranchID.String(), "error", err.Error())
		}
	}

	logger.Info("endpoint created and deployed", "address", deployed.Address)
	return Ok(mustResult(map[string]string{"endpoint_id": s.EndpointID.String(), "address": deployed.Address}))
}

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
