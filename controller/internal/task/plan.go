package task

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// This file defines the task kinds and their write-ahead *plans* (amp-controller.md
// §8.1): the ordered list of steps a task will run, with all ids assigned and
// baked into the step args up front, before any step executes. A plan is pure
// data — building one mutates nothing — so the same builders are used by tests
// today and by the API mutation handlers once those are moved onto the framework
// (#113). The default Postgres major version yb-light targets is v17.

// Task kinds — the task.kind values. Each corresponds to a user-visible
// operation whose status the API exposes (GET /tasks/{id}).
const (
	KindCreateProject  = "create_project"
	KindBranch         = "branch"
	KindCreateEndpoint = "create_endpoint"
	KindDeleteEndpoint = "delete_endpoint"
)

// Plan is a built, ready-to-persist task: its kind, the resource it targets (for
// the task API's status monitoring), the originating request (params, echoed
// back via the task API), and its ordered steps. Hand it to Store.PlanFrom to
// write-ahead-record it. ProjectID/TargetRef are nullable: a task that creates
// its own target may not have one when planned.
type Plan struct {
	Kind      string
	ProjectID *uuid.UUID
	TargetRef *string
	Params    json.RawMessage
	Steps     []StepPlan
}

// CreateProjectInput is the request for a create_project task: a new project (a
// "database") and its default branch under a pool the caller can reach, and
// optionally that branch's first read_write endpoint.
type CreateProjectInput struct {
	PoolID    uuid.UUID `json:"pool_id"`
	Name      string    `json:"name"`
	PgVersion int       `json:"pg_version"`
	// WithWriter also creates and deploys the default branch's first (read_write)
	// endpoint as part of the same task.
	WithWriter bool `json:"with_writer"`
}

// BuildCreateProject plans a create_project task: create the project + its
// default branch, record the branch's pending-init recipe, then (when WithWriter)
// create and deploy the first write endpoint. The project/branch (and endpoint)
// ids are generated here so the whole plan is fixed before execution.
func BuildCreateProject(in CreateProjectInput) (Plan, error) {
	projectID := uuid.New()
	branchID := uuid.New()

	steps := []StepPlan{
		mustStep(StepCreateProject, CreateProjectArgs{
			PoolID:    in.PoolID,
			ProjectID: projectID,
			BranchID:  branchID,
			Name:      in.Name,
			PgVersion: in.PgVersion,
		}),
		mustStep(StepRecordBranchRecipe, RecordBranchRecipeArgs{
			ProjectID:  projectID,
			BranchID:   branchID,
			BranchName: defaultBranchName, // the default branch is always "main", not the project name
			PgVersion:  in.PgVersion,
			StartLSN:   "0/0", // a default (Bootstrap) branch starts at the origin
		}),
	}
	if in.WithWriter {
		steps = append(steps, mustStep(StepCreateEndpoint, CreateEndpointArgs{
			EndpointID: uuid.New(),
			ProjectID:  projectID,
			BranchID:   branchID,
			Role:       roleReadWrite,
			PgVersion:  in.PgVersion,
		}))
	}
	return plan(KindCreateProject, in, projectID, steps)
}

// BranchInput is the request for a branch task. The new branch stays in its
// parent's project (ProjectID), forked off ParentBranchID at ForkLSN (resolved
// before planning — the storage layer's "X/X" form). PgVersion is the parent's
// (a branch inherits its ancestor's version).
type BranchInput struct {
	PoolID         uuid.UUID `json:"pool_id"`
	ProjectID      uuid.UUID `json:"project_id"`
	ParentBranchID uuid.UUID `json:"parent_branch_id"`
	Name           string    `json:"name"`
	ForkLSN        string    `json:"fork_lsn"`
	PgVersion      int       `json:"pg_version"`
	// WithWriter also creates and deploys a write endpoint on the new branch.
	WithWriter bool `json:"with_writer"`
}

// BuildBranch plans a branch task: fork the parent into a new branch, record its
// (ancestor-bearing) pending-init recipe, then (when WithWriter) create and
// deploy a write endpoint on the branch — which clones from the ancestor at
// ForkLSN rather than initdb'ing.
func BuildBranch(in BranchInput) (Plan, error) {
	branchID := uuid.New()
	parent := in.ParentBranchID

	steps := []StepPlan{
		mustStep(StepCreateBranch, CreateBranchArgs{
			ParentBranchID: parent,
			ProjectID:      in.ProjectID,
			BranchID:       branchID,
			Name:           in.Name,
			ForkLSN:        in.ForkLSN,
			PgVersion:      in.PgVersion,
		}),
		mustStep(StepRecordBranchRecipe, RecordBranchRecipeArgs{
			ProjectID:        in.ProjectID,
			BranchID:         branchID,
			BranchName:       in.Name,
			PgVersion:        in.PgVersion,
			AncestorBranchID: &parent,
			StartLSN:         in.ForkLSN,
		}),
	}
	if in.WithWriter {
		steps = append(steps, mustStep(StepCreateEndpoint, CreateEndpointArgs{
			EndpointID:       uuid.New(),
			ProjectID:        in.ProjectID,
			BranchID:         branchID,
			Role:             roleReadWrite,
			PgVersion:        in.PgVersion,
			AncestorBranchID: &parent,
			AncestorLSN:      in.ForkLSN,
		}))
	}
	return plan(KindBranch, in, in.ProjectID, steps)
}

// CreateEndpointInput is the request for a create_endpoint (scale-up) task on an
// existing branch. When the target branch is a fork, AncestorBranchID /
// AncestorLSN carry its fork point so the endpoint clones from the ancestor.
type CreateEndpointInput struct {
	ProjectID        uuid.UUID  `json:"project_id"`
	BranchID         uuid.UUID  `json:"branch_id"`
	Role             string     `json:"role"`
	PgVersion        int        `json:"pg_version"`
	AncestorBranchID *uuid.UUID `json:"ancestor_branch_id,omitempty"`
	AncestorLSN      string     `json:"ancestor_lsn,omitempty"`
}

// BuildCreateEndpoint plans a create_endpoint task: a single create_endpoint step
// on an existing branch. Role defaults to read_write when unset.
func BuildCreateEndpoint(in CreateEndpointInput) (Plan, error) {
	role := in.Role
	if role == "" {
		role = roleReadWrite
	}
	steps := []StepPlan{
		mustStep(StepCreateEndpoint, CreateEndpointArgs{
			EndpointID:       uuid.New(),
			ProjectID:        in.ProjectID,
			BranchID:         in.BranchID,
			Role:             role,
			PgVersion:        in.PgVersion,
			AncestorBranchID: in.AncestorBranchID,
			AncestorLSN:      in.AncestorLSN,
		}),
	}
	return plan(KindCreateEndpoint, in, in.ProjectID, steps)
}

// DeleteEndpointInput is the request for a delete_endpoint (scale-down) task.
type DeleteEndpointInput struct {
	ProjectID  uuid.UUID `json:"project_id"`
	EndpointID uuid.UUID `json:"endpoint_id"`
}

// BuildDeleteEndpoint plans a delete_endpoint task: a single delete_endpoint step.
func BuildDeleteEndpoint(in DeleteEndpointInput) (Plan, error) {
	steps := []StepPlan{
		mustStep(StepDeleteEndpoint, DeleteEndpointArgs{EndpointID: in.EndpointID}),
	}
	return plan(KindDeleteEndpoint, in, in.ProjectID, steps)
}

// EndpointDeployName is the deployer's stable instance handle for an endpoint
// (the container/pod/Helm-release name), keyed by the endpoint id. It matches
// the controller API layer's identical helper so the task and synchronous paths
// name the same instance. "yb-ep-" + a lowercase-hyphen uuid is a valid DNS-1123
// name (42 chars, under Helm's 53-char limit).
func EndpointDeployName(endpointID uuid.UUID) string {
	return "yb-ep-" + endpointID.String()
}

// roleReadWrite mirrors core.RoleReadWrite without importing core (the task
// package stays independent of the domain layer). The value is the endpoint_role
// enum literal the schema and core agree on.
const roleReadWrite = "read_write"

// defaultBranchName mirrors core.DefaultBranchName without importing core: a
// project's default branch is always named "main", independent of the project
// name. The create_project step (EnsureProject) names the branch the same way.
const defaultBranchName = "main"

// plan finishes a Plan: it marshals the originating request as params and stamps
// the task's target project (project_id, exposed by the task API for monitoring).
// A nil project is left unstamped. projectID is taken by value and the address of
// a copy is stored, so callers can pass a field directly.
func plan(kind string, params any, projectID uuid.UUID, steps []StepPlan) (Plan, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return Plan{}, fmt.Errorf("marshal %s params: %w", kind, err)
	}
	p := Plan{Kind: kind, Params: raw, Steps: steps}
	if projectID != uuid.Nil {
		pid := projectID
		p.ProjectID = &pid
	}
	return p, nil
}

// mustStep builds a StepPlan, marshaling typed args to JSON. The args are
// fixed-shape structs of ids and scalars, so marshaling cannot fail; an empty
// args object is a safe degradation a step handler will reject on decode.
func mustStep(kind string, args any) StepPlan {
	raw, err := json.Marshal(args)
	if err != nil {
		raw = json.RawMessage(`{}`)
	}
	return StepPlan{Kind: kind, Args: raw}
}
