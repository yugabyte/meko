package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/deploy"
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
// yb-amp targets is v18.

// Task kinds — the task.kind values. Each corresponds to a user-visible
// operation whose status the API exposes (GET /tasks/{id}).
const (
	KindCreateCluster       = "create_cluster"
	KindBranch              = "branch"
	KindCreateEndpoint      = "create_endpoint"
	KindResizeEndpoint      = "resize_endpoint"
	KindDeleteEndpoint      = "delete_endpoint"
	KindDeleteBranch        = "delete_branch"
	KindDeleteCluster       = "delete_cluster"
	KindAddBranchExtensions = "add_branch_extensions"
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

// Result returns the result payload of the task's final step that recorded one
// — the task's user-facing outcome (e.g. a resize's in-place status), as opaque
// JSON. Returns nil when the task has no steps or none recorded a result.
func (s *Service) Result(ctx context.Context, id uuid.UUID) (json.RawMessage, error) {
	steps, err := s.store.Steps(ctx, id)
	if err != nil {
		return nil, err
	}
	for i := len(steps) - 1; i >= 0; i-- {
		if len(steps[i].Result) > 0 {
			return steps[i].Result, nil
		}
	}
	return nil, nil
}

// CreateClusterInput is the request for a create_cluster task: a new cluster (a
// "database") and its default branch under a pool the caller can reach, and
// optionally that branch's first read_write endpoint.
type CreateClusterInput struct {
	PoolID    uuid.UUID `json:"pool_id"`
	Name      string    `json:"name"`
	PgVersion int       `json:"pg_version"`
	// WithWriter also creates and deploys the default branch's first (read_write)
	// endpoint as part of the same task.
	WithWriter bool `json:"with_writer"`
	// CPU and Memory size the WithWriter endpoint (Kubernetes quantities, e.g.
	// "2" / "4Gi"). Empty falls back to the deployer default (2 cores / 4Gi).
	// Ignored when WithWriter is false.
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	// Engine is the named extension bundle to provision as ("postgres", "aidb",
	// "oracle"). The handler validates it and folds its bundle into Extensions;
	// this records what the cluster was created as. Empty means the default.
	Engine string `json:"engine,omitempty"`
	// Extensions is the cluster-wide externalized extension set (catalog names,
	// pre-validated by the handler, including whatever Engine seeds), stored on
	// the default branch so every endpoint on the cluster mounts it. Empty leaves
	// the cluster with catalog defaults only.
	Extensions []string `json:"extensions,omitempty"`
}

// CreateCluster records a create_cluster task: create the cluster + its default
// branch, record the branch's pending-init recipe, then (when WithWriter) create
// and deploy the first write endpoint. The cluster/branch (and endpoint) ids are
// generated here so the whole plan is fixed before execution.
func (s *Service) CreateCluster(ctx context.Context, in CreateClusterInput) (*Task, error) {
	clusterID := uuid.New()
	branchID := uuid.New()

	steps := []StepSpec{
		stepSpec(StepCreateCluster, CreateClusterArgs{
			PoolID:     in.PoolID,
			ClusterID:  clusterID,
			BranchID:   branchID,
			Name:       in.Name,
			PgVersion:  in.PgVersion,
			Engine:     in.Engine,
			Extensions: in.Extensions,
		}),
		stepSpec(StepRecordBranchRecipe, RecordBranchRecipeArgs{
			ClusterID:  clusterID,
			BranchID:   branchID,
			BranchName: defaultBranchName, // always "main", never the cluster name
			PgVersion:  in.PgVersion,
			StartLSN:   "0/0", // a default (Bootstrap) branch starts at the origin
		}),
	}
	if in.WithWriter {
		steps = append(steps, stepSpec(StepCreateEndpoint, CreateEndpointArgs{
			EndpointID: uuid.New(),
			ClusterID:  clusterID,
			BranchID:   branchID,
			Role:       roleReadWrite,
			PgVersion:  in.PgVersion,
			CPU:        in.CPU,
			Memory:     in.Memory,
		}))
	}
	// TargetRef is the new default branch: the UI pre-selects it once the task
	// succeeds (a cluster is created together with its default branch).
	return s.create(ctx, KindCreateCluster, in, clusterID, branchID.String(), steps)
}

// BranchInput is the request for a branch task. The new branch stays in its
// parent's cluster (ClusterID), forked off ParentBranchID at ForkLSN (resolved
// before planning — the storage layer's "X/X" form). PgVersion is the parent's
// (a branch inherits its ancestor's version).
type BranchInput struct {
	PoolID         uuid.UUID `json:"pool_id"`
	ClusterID      uuid.UUID `json:"cluster_id"`
	ParentBranchID uuid.UUID `json:"parent_branch_id"`
	Name           string    `json:"name"`
	ForkLSN        string    `json:"fork_lsn"`
	PgVersion      int       `json:"pg_version"`
	// PgSettings merge over what the fork inherits from its parent.
	PgSettings map[string]string `json:"pg_settings,omitempty"`
	// WithWriter also creates and deploys a write endpoint on the new branch.
	WithWriter bool `json:"with_writer"`
	// MemoryGiBPerVCPU is the control plane's memory-per-core shape, used to size
	// that endpoint. Config belongs to the caller, so it is passed in; 0 = the
	// built-in.
	MemoryGiBPerVCPU int `json:"memory_gib_per_vcpu,omitempty"`
}

// Branch records a branch task: fork the parent into a new branch, record its
// (ancestor-bearing) pending-init recipe, then (when WithWriter) create and
// deploy a write endpoint on the branch — which clones from the ancestor at
// ForkLSN rather than initdb'ing.
func (s *Service) Branch(ctx context.Context, in BranchInput) (*Task, error) {
	branchID := uuid.New()
	parent := in.ParentBranchID

	// ONE step. The branch row, its amp_storage.branch_state seed, and the
	// fork/GC-cutoff interlock all happen inside record_branch_recipe's single
	// transaction, so a fork rejected for being below the ancestor's cutoff leaves
	// nothing behind — splitting them across two steps would commit the row and
	// then fail, orphaning a branch for a fork that never happened.
	//
	// create_branch is registered for task replay but planned by nothing.
	steps := []StepSpec{
		stepSpec(StepRecordBranchRecipe, RecordBranchRecipeArgs{
			ClusterID:        in.ClusterID,
			BranchID:         branchID,
			BranchName:       in.Name,
			PgVersion:        in.PgVersion,
			AncestorBranchID: &parent,
			StartLSN:         in.ForkLSN,
			PgSettings:       in.PgSettings,
		}),
	}
	if in.WithWriter {
		// Sized here rather than left empty. An endpoint that reaches the deployer
		// with no size lands on its fallback, which is the memory-starved pair the
		// floor exists to eliminate — a fork's writer is no more entitled to it
		// than any other endpoint.
		cpu, memory, shapeErr := deploy.ResolveEndpointShape("", "", in.MemoryGiBPerVCPU)
		if shapeErr != "" {
			return nil, fmt.Errorf("branch writer size: %s", shapeErr)
		}
		steps = append(steps, stepSpec(StepCreateEndpoint, CreateEndpointArgs{
			EndpointID: uuid.New(),
			ClusterID:  in.ClusterID,
			BranchID:   branchID,
			Role:       roleReadWrite,
			PgVersion:  in.PgVersion,
			CPU:        cpu,
			Memory:     memory,
		}))
	}
	return s.create(ctx, KindBranch, in, in.ClusterID, branchID.String(), steps)
}

// CreateEndpointInput is the request for a create_endpoint (scale-up) task on an
// existing branch.
type CreateEndpointInput struct {
	ClusterID uuid.UUID `json:"cluster_id"`
	BranchID  uuid.UUID `json:"branch_id"`
	Role      string    `json:"role"`
	PgVersion int       `json:"pg_version"`
	// Name is the caller's handle for the endpoint, unique among its branch's
	// endpoints. Empty means unnamed.
	Name string `json:"name,omitempty"`
	// Extended is opaque per-endpoint compute configuration, stored verbatim.
	Extended json.RawMessage `json:"extended,omitempty"`
	// PgSettings are this endpoint's own extra PostgreSQL settings.
	PgSettings map[string]string `json:"pg_settings,omitempty"`
	// PageserverSettings and SafekeeperSettings are per-endpoint overrides for the
	// two storage daemons in the pod, as flat {key: scalar} maps whose keys may be
	// dotted paths ("tenant_config.enable_gc"). Validated by the API before the
	// task is recorded.
	PageserverSettings map[string]any `json:"pageserver_settings,omitempty"`
	SafekeeperSettings map[string]any `json:"safekeeper_settings,omitempty"`
	// CPU and Memory are the pod's compute reservation (Kubernetes quantities),
	// applied as request == limit for Guaranteed QoS. Empty falls back to the
	// deployer/chart default (2 cores / 4Gi).
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
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
			EndpointID:         endpointID,
			ClusterID:          in.ClusterID,
			BranchID:           in.BranchID,
			Role:               role,
			PgVersion:          in.PgVersion,
			Name:               in.Name,
			CPU:                in.CPU,
			Memory:             in.Memory,
			Extended:           in.Extended,
			PgSettings:         in.PgSettings,
			PageserverSettings: in.PageserverSettings,
			SafekeeperSettings: in.SafekeeperSettings,
		}),
	}
	return s.create(ctx, KindCreateEndpoint, in, in.ClusterID, endpointID.String(), steps)
}

// ResizeEndpointInput is the request for a resize_endpoint task: change an
// existing endpoint's compute reservation. CPU/Memory are Kubernetes quantities
// applied as request == limit (Guaranteed QoS preserved).
type ResizeEndpointInput struct {
	ClusterID  uuid.UUID `json:"cluster_id"`
	EndpointID uuid.UUID `json:"endpoint_id"`
	CPU        string    `json:"cpu"`
	Memory     string    `json:"memory"`
}

// ResizeEndpoint records a resize_endpoint task: a single resize_endpoint step
// that persists the new size to the endpoint's config (durable across redeploys)
// and resizes the running pod in place.
func (s *Service) ResizeEndpoint(ctx context.Context, in ResizeEndpointInput) (*Task, error) {
	steps := []StepSpec{
		stepSpec(StepResizeEndpoint, ResizeEndpointArgs{
			EndpointID: in.EndpointID,
			CPU:        in.CPU,
			Memory:     in.Memory,
		}),
	}
	return s.create(ctx, KindResizeEndpoint, in, in.ClusterID, in.EndpointID.String(), steps)
}

// DeleteEndpointInput is the request for a delete_endpoint (scale-down) task.
type DeleteEndpointInput struct {
	ClusterID  uuid.UUID `json:"cluster_id"`
	EndpointID uuid.UUID `json:"endpoint_id"`
}

// DeleteEndpoint records a delete_endpoint task: a single delete_endpoint step.
func (s *Service) DeleteEndpoint(ctx context.Context, in DeleteEndpointInput) (*Task, error) {
	steps := []StepSpec{
		stepSpec(StepDeleteEndpoint, DeleteEndpointArgs{EndpointID: in.EndpointID}),
	}
	return s.create(ctx, KindDeleteEndpoint, in, in.ClusterID, in.EndpointID.String(), steps)
}

// DeleteBranchInput is the request for a delete_branch (cascade) task.
type DeleteBranchInput struct {
	PoolID    uuid.UUID `json:"pool_id"`
	ClusterID uuid.UUID `json:"cluster_id"`
	BranchID  uuid.UUID `json:"branch_id"`
}

// DeleteBranch records a delete_branch task: a single, self-contained
// delete_branch step that enumerates and retires the branch's endpoints at
// Execute time, then drops the branch. The worker has no dynamic sub-step
// expansion, so the cascade is one idempotent step rather than a planned
// per-endpoint fan-out.
func (s *Service) DeleteBranch(ctx context.Context, in DeleteBranchInput) (*Task, error) {
	steps := []StepSpec{
		stepSpec(StepDeleteBranch, DeleteBranchArgs{ClusterID: in.ClusterID, BranchID: in.BranchID}),
	}
	return s.create(ctx, KindDeleteBranch, in, in.ClusterID, in.BranchID.String(), steps)
}

// DeleteClusterInput is the request for a delete_cluster (cascade) task.
type DeleteClusterInput struct {
	PoolID    uuid.UUID `json:"pool_id"`
	ClusterID uuid.UUID `json:"cluster_id"`
}

// DeleteCluster records a delete_cluster task: a single, self-contained
// delete_cluster step that enumerates and retires all the cluster's endpoints at
// Execute time, then drops the cluster (and all its branches). One idempotent
// step, for the same reason as DeleteBranch.
func (s *Service) DeleteCluster(ctx context.Context, in DeleteClusterInput) (*Task, error) {
	steps := []StepSpec{
		stepSpec(StepDeleteCluster, DeleteClusterArgs{ClusterID: in.ClusterID}),
	}
	// DeleteCluster leaves TargetRef empty: cluster_id already carries the id.
	return s.create(ctx, KindDeleteCluster, in, in.ClusterID, "", steps)
}

// AddBranchExtensionsInput is the request for an add_branch_extensions task:
// enable extensions on one branch. Names are catalog names, pre-validated by the
// handler.
type AddBranchExtensionsInput struct {
	PoolID     uuid.UUID `json:"pool_id"`
	ClusterID  uuid.UUID `json:"cluster_id"`
	BranchID   uuid.UUID `json:"branch_id"`
	Extensions []string  `json:"extensions"`
}

// AddBranchExtensions records an add_branch_extensions task: a single,
// self-contained step that unions the names into the branch's durable set and
// re-renders every endpoint on the branch so the new ImageVolume mounts take
// effect. One idempotent step (absolute union + deploy-per-endpoint), for the
// same reason as DeleteBranch.
func (s *Service) AddBranchExtensions(ctx context.Context, in AddBranchExtensionsInput) (*Task, error) {
	steps := []StepSpec{
		stepSpec(StepAddBranchExtensions, AddBranchExtensionsArgs{
			ClusterID:  in.ClusterID,
			BranchID:   in.BranchID,
			Extensions: in.Extensions,
		}),
	}
	return s.create(ctx, KindAddBranchExtensions, in, in.BranchID, "", steps)
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
// cluster's default branch is always named "main", independent of the cluster
// name and not settable by a caller. The create_cluster step (EnsureCluster)
// names the branch the same way.
const defaultBranchName = "main"

// create marshals the originating request as params, stamps the task's target
// cluster (cluster_id, exposed by the task API for monitoring; a nil cluster is
// left unstamped) and the resource the task acts on (targetRef — the branch or
// endpoint id, so the UI/integration harness can pre-select it once the task succeeds; an
// empty targetRef is left unstamped), and persists the task + its steps via the
// Store. clusterID is taken by value and the address of a copy is stored, so
// callers can pass a field directly.
func (s *Service) create(ctx context.Context, kind string, params any, clusterID uuid.UUID, targetRef string, steps []StepSpec) (*Task, error) {
	return s.createWithID(ctx, uuid.Nil, kind, params, clusterID, targetRef, steps)
}

// createWithID is the stable-id form of create, used by planners whose step args
// must refer to the task before Store.Create is called (the migration
// launchers). A zero taskID defers id assignment to the store; a fixed one is
// honored and replays converge on the original plan.
func (s *Service) createWithID(ctx context.Context, taskID uuid.UUID, kind string, params any, clusterID uuid.UUID, targetRef string, steps []StepSpec) (*Task, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal %s params: %w", kind, err)
	}
	spec := TaskSpec{ID: taskID, Kind: kind, Params: raw, Steps: steps}
	if clusterID != uuid.Nil {
		pid := clusterID
		spec.ClusterID = &pid
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
