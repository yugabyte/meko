package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// This file is the caller-facing surface of the task framework: a Service that
// records tasks (amp-controller.md §8.1). It holds the Store, so a caller depends
// on one Service rather than threading the Store through every call — the Service
// is what anything that initiates a task takes as a dependency. Each CreateX
// method generates all the ids the operation needs up front, describes the
// ordered steps as StepSpecs (kind + marshaled args — pure data, no deps, no
// executable Step), and hands them to Store.Create, which write-ahead-records the
// task and its steps and returns the Task to surface to the user. Building the
// specs performs no effects; the steps only become executable later, when the
// worker rebuilds them via the Registry. The default Postgres major version
// yb-light targets is v18.

// Task kinds — the task.kind values. Each corresponds to a user-visible
// operation whose status the API exposes (GET /tasks/{id}).
const (
	KindCreateProject  = "create_project"
	KindBranch         = "branch"
	KindCreateEndpoint = "create_endpoint"
	KindDeleteEndpoint = "delete_endpoint"
	KindDeleteBranch   = "delete_branch"
	KindDeleteProject  = "delete_project"
)

// Service records tasks against a Store and reads them back. It is the dependency
// API mutation handlers (and anything else that initiates a task) take instead of
// the raw Store; the worker, which claims and runs tasks, uses the Store directly.
type Service struct {
	store Store
}

// NewService returns a task Service backed by store.
func NewService(store Store) *Service {
	return &Service{store: store}
}

// Get returns a task by id (its status, for GET /tasks/{id}), or nil if absent.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Task, error) {
	return s.store.Get(ctx, id)
}

// List returns a page of tasks matching f, ordered newest-first, plus the total
// matching count (for GET /tasks).
func (s *Service) List(ctx context.Context, f TaskFilter, limit, offset int) ([]*Task, int, error) {
	return s.store.List(ctx, f, limit, offset)
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

// CreateProject records a create_project task: create the project + its default
// branch, record the branch's pending-init recipe, then (when WithWriter) create
// and deploy the first write endpoint. The project/branch (and endpoint) ids are
// generated here so the whole plan is fixed before execution.
func (s *Service) CreateProject(ctx context.Context, in CreateProjectInput) (*Task, error) {
	projectID := uuid.New()
	branchID := uuid.New()

	steps := []StepSpec{
		stepSpec(StepCreateProject, CreateProjectArgs{
			PoolID:    in.PoolID,
			ProjectID: projectID,
			BranchID:  branchID,
			Name:      in.Name,
			PgVersion: in.PgVersion,
		}),
		stepSpec(StepRecordBranchRecipe, RecordBranchRecipeArgs{
			ProjectID:  projectID,
			BranchID:   branchID,
			BranchName: defaultBranchName, // the default branch is always "main", not the project name
			PgVersion:  in.PgVersion,
			StartLSN:   "0/0", // a default (Bootstrap) branch starts at the origin
		}),
	}
	if in.WithWriter {
		steps = append(steps, stepSpec(StepCreateEndpoint, CreateEndpointArgs{
			EndpointID: uuid.New(),
			ProjectID:  projectID,
			BranchID:   branchID,
			Role:       roleReadWrite,
			PgVersion:  in.PgVersion,
		}))
	}
	// TargetRef is the new default branch: the UI pre-selects it once the task
	// succeeds (a project is created together with its default branch).
	return s.create(ctx, KindCreateProject, in, projectID, branchID.String(), steps)
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

// Branch records a branch task: fork the parent into a new branch, record its
// (ancestor-bearing) pending-init recipe, then (when WithWriter) create and
// deploy a write endpoint on the branch — which clones from the ancestor at
// ForkLSN rather than initdb'ing.
func (s *Service) Branch(ctx context.Context, in BranchInput) (*Task, error) {
	branchID := uuid.New()
	parent := in.ParentBranchID

	steps := []StepSpec{
		stepSpec(StepCreateBranch, CreateBranchArgs{
			ParentBranchID: parent,
			ProjectID:      in.ProjectID,
			BranchID:       branchID,
			Name:           in.Name,
			ForkLSN:        in.ForkLSN,
			PgVersion:      in.PgVersion,
		}),
		stepSpec(StepRecordBranchRecipe, RecordBranchRecipeArgs{
			ProjectID:        in.ProjectID,
			BranchID:         branchID,
			BranchName:       in.Name,
			PgVersion:        in.PgVersion,
			AncestorBranchID: &parent,
			StartLSN:         in.ForkLSN,
		}),
	}
	if in.WithWriter {
		steps = append(steps, stepSpec(StepCreateEndpoint, CreateEndpointArgs{
			EndpointID:       uuid.New(),
			ProjectID:        in.ProjectID,
			BranchID:         branchID,
			Role:             roleReadWrite,
			PgVersion:        in.PgVersion,
			AncestorBranchID: &parent,
			AncestorLSN:      in.ForkLSN,
		}))
	}
	return s.create(ctx, KindBranch, in, in.ProjectID, branchID.String(), steps)
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

// CreateEndpoint records a create_endpoint task: a single create_endpoint step on
// an existing branch. Role defaults to read_write when unset.
func (s *Service) CreateEndpoint(ctx context.Context, in CreateEndpointInput) (*Task, error) {
	role := in.Role
	if role == "" {
		role = roleReadWrite
	}
	endpointID := uuid.New()
	steps := []StepSpec{
		stepSpec(StepCreateEndpoint, CreateEndpointArgs{
			EndpointID:       endpointID,
			ProjectID:        in.ProjectID,
			BranchID:         in.BranchID,
			Role:             role,
			PgVersion:        in.PgVersion,
			AncestorBranchID: in.AncestorBranchID,
			AncestorLSN:      in.AncestorLSN,
		}),
	}
	return s.create(ctx, KindCreateEndpoint, in, in.ProjectID, endpointID.String(), steps)
}

// DeleteEndpointInput is the request for a delete_endpoint (scale-down) task.
type DeleteEndpointInput struct {
	ProjectID  uuid.UUID `json:"project_id"`
	EndpointID uuid.UUID `json:"endpoint_id"`
}

// DeleteEndpoint records a delete_endpoint task: a single delete_endpoint step.
func (s *Service) DeleteEndpoint(ctx context.Context, in DeleteEndpointInput) (*Task, error) {
	steps := []StepSpec{
		stepSpec(StepDeleteEndpoint, DeleteEndpointArgs{EndpointID: in.EndpointID}),
	}
	return s.create(ctx, KindDeleteEndpoint, in, in.ProjectID, in.EndpointID.String(), steps)
}

// DeleteBranchInput is the request for a delete_branch (cascade) task.
type DeleteBranchInput struct {
	PoolID    uuid.UUID `json:"pool_id"`
	ProjectID uuid.UUID `json:"project_id"`
	BranchID  uuid.UUID `json:"branch_id"`
}

// DeleteBranch records a delete_branch task: a single, self-contained
// delete_branch step that enumerates and retires the branch's endpoints at
// Execute time, then drops the branch. The worker has no dynamic sub-step
// expansion, so the cascade is one idempotent step rather than a planned
// per-endpoint fan-out.
func (s *Service) DeleteBranch(ctx context.Context, in DeleteBranchInput) (*Task, error) {
	steps := []StepSpec{
		stepSpec(StepDeleteBranch, DeleteBranchArgs{ProjectID: in.ProjectID, BranchID: in.BranchID}),
	}
	return s.create(ctx, KindDeleteBranch, in, in.ProjectID, in.BranchID.String(), steps)
}

// DeleteProjectInput is the request for a delete_project (cascade) task.
type DeleteProjectInput struct {
	PoolID    uuid.UUID `json:"pool_id"`
	ProjectID uuid.UUID `json:"project_id"`
}

// DeleteProject records a delete_project task: a single, self-contained
// delete_project step that enumerates and retires all the project's endpoints at
// Execute time, then drops the project (and all its branches). One idempotent
// step, for the same reason as DeleteBranch.
func (s *Service) DeleteProject(ctx context.Context, in DeleteProjectInput) (*Task, error) {
	steps := []StepSpec{
		stepSpec(StepDeleteProject, DeleteProjectArgs{ProjectID: in.ProjectID}),
	}
	// DeleteProject leaves TargetRef empty: project_id already carries the id.
	return s.create(ctx, KindDeleteProject, in, in.ProjectID, "", steps)
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

// create marshals the originating request as params, stamps the task's target
// project (project_id, exposed by the task API for monitoring; a nil project is
// left unstamped) and the resource the task acts on (targetRef — the branch or
// endpoint id, so the UI/e2e harness can pre-select it once the task succeeds; an
// empty targetRef is left unstamped), and persists the task + its steps via the
// Store. projectID is taken by value and the address of a copy is stored, so
// callers can pass a field directly.
func (s *Service) create(ctx context.Context, kind string, params any, projectID uuid.UUID, targetRef string, steps []StepSpec) (*Task, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal %s params: %w", kind, err)
	}
	spec := TaskSpec{Kind: kind, Params: raw, Steps: steps}
	if projectID != uuid.Nil {
		pid := projectID
		spec.ProjectID = &pid
	}
	if targetRef != "" {
		ref := targetRef
		spec.TargetRef = &ref
	}
	return s.store.Create(ctx, spec)
}

// stepSpec builds a StepSpec, marshaling typed args to JSON. The args are
// fixed-shape structs of ids and scalars, so marshaling cannot fail; an empty
// args object is a safe degradation the step's Builder will reject on decode.
func stepSpec(kind string, args any) StepSpec {
	raw, err := json.Marshal(args)
	if err != nil {
		raw = json.RawMessage(`{}`)
	}
	return StepSpec{Kind: kind, Args: raw}
}
