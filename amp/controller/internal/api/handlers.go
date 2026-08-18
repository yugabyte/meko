package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/extensions"
	applog "github.com/yugabyte/amp/controller/internal/log"
	"github.com/yugabyte/amp/controller/internal/node"
	"github.com/yugabyte/amp/controller/internal/task"
	"github.com/yugabyte/amp/controller/internal/version"
)

// Provisioning holds the collaborators the create-cluster / create-endpoint
// workflows use beyond the state store: the controller (to record the
// pending-init recipe and, once an endpoint materializes the branch, drop it)
// and the endpoint deployer (to provision the compute). Both are optional —
// with no deployer the endpoint mutation routes refuse with 501, and with no SC
// create-cluster records the control-plane rows only. ControllerURL is the
// controller URL as a *launched endpoint* reaches it (e.g. over the shared
// network); empty means the endpoint runs controller-free.
type Provisioning struct {
	Deployer deploy.Deployer
	// Node reads LSNs from an endpoint's co-located safekeeper to resolve a
	// branch's default fork point (#133). Paired with the Deployer (which yields
	// the safekeeper's address); without both, a default fork point can't be
	// resolved and branching requires an explicit fork_point_lsn.
	Node          *node.Client
	EndpointYB    deploy.YBConn
	ControllerURL string
	// Recipes records a branch's creation record (the task framework's
	// RecipeStore port). Production wires the in-process store; tests inject a fake.
	Recipes task.RecipeStore
	// Extensions is the externalized-extension catalog the create_endpoint step
	// resolves a branch's extension set against. Wired only for the Kubernetes
	// driver (ImageVolumes); nil for Docker / when the feature is off.
	Extensions *extensions.Catalog
}

// Controllers implements the generated StrictServerInterface: the controller's
// HTTP behavior, decoupled from the wire format (which the generated code owns)
// and from persistence (which the core.Backend owns). Cluster, branch, and
// endpoint mutations (create + delete) plan write-ahead tasks and return 202 +
// the Task + a Location header; the worker performs the work — recording the
// control rows plus the controller's pending-init recipe and deploying
// compute (#108, #130, #113).
type Controllers struct {
	backend core.Backend
	tasks   *task.Service
	prov    Provisioning
	logger  *slog.Logger
}

// NewControllers wires the controllers to a backend, the task service (to create
// tasks and read their status), the provisioning collaborators, and the root
// logger.
func NewControllers(backend core.Backend, tasks *task.Service, prov Provisioning, logger *slog.Logger) *Controllers {
	return &Controllers{backend: backend, tasks: tasks, prov: prov, logger: logger}
}

// Compile-time assertion that the controllers satisfy the generated contract.
var _ StrictServerInterface = (*Controllers)(nil)

// --- Meta ------------------------------------------------------------------

// GetVersion returns the controller's build identity — semantic version (from
// the repo-root VERSION file), git commit, and build date — stamped into the
// binary at build time (see internal/version).
func (c *Controllers) GetVersion(_ context.Context, _ GetVersionRequestObject) (GetVersionResponseObject, error) {
	v := version.Get()
	return GetVersion200JSONResponse{Version: v.Version, GitCommit: v.Commit, BuildDate: v.BuildDate}, nil
}

// --- Users -----------------------------------------------------------------

func (c *Controllers) CreateUser(ctx context.Context, request CreateUserRequestObject) (CreateUserResponseObject, error) {
	isAdmin := false
	var explicitID *uuid.UUID
	if request.Body != nil {
		if request.Body.IsAdmin != nil {
			isAdmin = *request.Body.IsAdmin
		}
		explicitID = request.Body.Id
	}
	// An explicit id (e.g. the caller's identity subject) makes creation
	// idempotent; omitting it auto-generates one.
	var u core.User
	var err error
	if explicitID != nil {
		if *explicitID == uuid.Nil {
			return CreateUser400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "user id cannot be the nil UUID"))}, nil
		}
		u, err = c.backend.EnsureUser(ctx, *explicitID, isAdmin)
	} else {
		u, err = c.backend.CreateUser(ctx, isAdmin)
	}
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return CreateUser201JSONResponse(userToAPI(u)), nil
}

func (c *Controllers) GetUser(ctx context.Context, request GetUserRequestObject) (GetUserResponseObject, error) {
	u, err := c.backend.GetUser(ctx, request.UserId)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return GetUser404JSONResponse{NotFoundJSONResponse(apiError("not_found", "user not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get user: %w", err)
	}
	return GetUser200JSONResponse(userToAPI(u)), nil
}

func (c *Controllers) ListUsers(ctx context.Context, request ListUsersRequestObject) (ListUsersResponseObject, error) {
	limit, offset := pageParams(request.Params.Limit, request.Params.Offset)
	users, total, err := c.backend.ListUsers(ctx, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	out := make([]User, 0, len(users))
	for _, u := range users {
		out = append(out, userToAPI(u))
	}
	return ListUsers200JSONResponse(UserList{Users: out, Total: total, Count: len(out)}), nil
}

// --- Storage pools ---------------------------------------------------------

func (c *Controllers) CreateStoragePool(ctx context.Context, request CreateStoragePoolRequestObject) (CreateStoragePoolResponseObject, error) {
	if request.Body == nil {
		return CreateStoragePool400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "request body is required"))}, nil
	}
	name := strings.TrimSpace(request.Body.Name)
	if name == "" {
		return CreateStoragePool400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "name is required"))}, nil
	}
	// Pools are top-level, so the owner is named in the body rather than the path.
	owner := request.Body.OwnerUserId
	if owner == uuid.Nil {
		return CreateStoragePool400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "owner_user_id is required"))}, nil
	}
	// An explicit id (e.g. a Meko datapack UUID) reuses that id for the pool and
	// makes the call idempotent; omitting it auto-generates one.
	var p core.StoragePool
	var err error
	if request.Body.Id != nil {
		if *request.Body.Id == uuid.Nil {
			return CreateStoragePool400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "pool id cannot be the nil UUID"))}, nil
		}
		p, err = c.backend.CreateStoragePoolWithID(ctx, *request.Body.Id, owner, name)
	} else {
		p, err = c.backend.CreateStoragePool(ctx, owner, name)
	}
	switch {
	case errors.Is(err, core.ErrNotFound):
		return CreateStoragePool404JSONResponse{NotFoundJSONResponse(apiError("not_found", "user not found"))}, nil
	case errors.Is(err, core.ErrConflict):
		return CreateStoragePool409JSONResponse{ConflictJSONResponse(apiError("conflict", "a storage pool with that name already exists"))}, nil
	case err != nil:
		return nil, fmt.Errorf("create storage pool: %w", err)
	}
	c.logger.LogAttrs(ctx, slog.LevelInfo, "storage pool created",
		slog.String(applog.KeyUserID, owner.String()),
		slog.String(applog.KeyStoragePoolID, p.ID.String()))
	return CreateStoragePool201JSONResponse(poolToAPI(p)), nil
}

func (c *Controllers) ListStoragePools(ctx context.Context, request ListStoragePoolsRequestObject) (ListStoragePoolsResponseObject, error) {
	limit, offset := pageParams(request.Params.Limit, request.Params.Offset)
	pools, total, err := c.backend.ListStoragePools(ctx, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list storage pools: %w", err)
	}
	out := make([]StoragePool, 0, len(pools))
	for _, p := range pools {
		out = append(out, poolToAPI(p))
	}
	return ListStoragePools200JSONResponse(StoragePoolList{StoragePools: out, Total: total, Count: len(out)}), nil
}

func (c *Controllers) GetStoragePool(ctx context.Context, request GetStoragePoolRequestObject) (GetStoragePoolResponseObject, error) {
	p, err := c.backend.GetStoragePool(ctx, request.PoolId)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return GetStoragePool404JSONResponse{NotFoundJSONResponse(apiError("not_found", "storage pool not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get storage pool: %w", err)
	}
	return GetStoragePool200JSONResponse(poolToAPI(p)), nil
}

// --- Clusters --------------------------------------------------------------
// Create and delete are async: each runs a cheap synchronous pre-check, then
// plans a task (create_cluster / delete_cluster) and returns 202 + the Task.
// The cluster + default branch rows and the controller's pending-init
// recipe are written by the create_cluster worker; the branch is materialized
// later by the first endpoint deployed on it. Delete cascades to the cluster's
// branches and endpoints in the worker.

// defaultPgVersion is used when a create-cluster request omits pg_version.
// yb-amp is Postgres v18 only.
const defaultPgVersion = 18

func (c *Controllers) CreateCluster(ctx context.Context, request CreateClusterRequestObject) (CreateClusterResponseObject, error) {
	if request.Body == nil {
		return CreateCluster400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "request body is required"))}, nil
	}
	name := strings.TrimSpace(request.Body.Name)
	if name == "" {
		return CreateCluster400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "name is required"))}, nil
	}
	pgVersion := defaultPgVersion
	if request.Body.PgVersion != nil {
		pgVersion = *request.Body.PgVersion
	}
	// yb-amp is Postgres v18 only; reject anything else up front rather than
	// failing opaquely when the endpoint image (v18) can't honor it.
	if pgVersion != defaultPgVersion {
		return CreateCluster400JSONResponse{BadRequestJSONResponse(apiError("invalid_request",
			fmt.Sprintf("unsupported pg_version %d (only %d is supported)", pgVersion, defaultPgVersion)))}, nil
	}
	// Cheap pre-check: the pool must exist. The task's create_cluster step would
	// also fail on an absent pool, but a 404 here keeps the synchronous
	// pool-not-found contract rather than surfacing it as a failed task.
	if _, err := c.backend.GetStoragePool(ctx, request.PoolId); err != nil {
		switch {
		case errors.Is(err, core.ErrNotFound):
			return CreateCluster404JSONResponse{NotFoundJSONResponse(apiError("not_found", "storage pool not found"))}, nil
		default:
			return nil, fmt.Errorf("get storage pool: %w", err)
		}
	}

	// Optionally provision the default branch's write endpoint in the same task
	// ("create cluster" = the cluster + its writer), sized by resources. When with_writer
	// is false (default), no endpoint is created — compute is a separate resource.
	// resources is only honored alongside with_writer.
	withWriter := request.Body.WithWriter != nil && *request.Body.WithWriter
	var cpu, memory string
	if withWriter {
		var errMsg string
		if cpu, memory, errMsg = parseResourceOverrides(request.Body.Resources); errMsg != "" {
			return CreateCluster400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", errMsg))}, nil
		}
	}

	// Plan the work as an async task and return 202 + the Task. The cluster +
	// default branch rows, the pending-init recipe, and (when with_writer) the
	// write endpoint are all created by the worker; a duplicate cluster name now
	// surfaces as a failed task rather than a synchronous 409.
	t, err := c.tasks.CreateCluster(ctx, task.CreateClusterInput{
		PoolID:     request.PoolId,
		Name:       name,
		PgVersion:  pgVersion,
		WithWriter: withWriter,
		CPU:        cpu,
		Memory:     memory,
	})
	if err != nil {
		return nil, fmt.Errorf("create cluster task: %w", err)
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "create cluster task accepted",
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String(applog.KeyTaskID, t.ID.String()),
		slog.String("cluster_name", name))

	return CreateCluster202JSONResponse{AcceptedJSONResponse{
		Body:    taskToAPI(t),
		Headers: AcceptedResponseHeaders{Location: "/api/v1/tasks/" + t.ID.String()},
	}}, nil
}

// endpointDeployName is the deployer's stable instance handle for an endpoint
// (the container/pod/Helm-release name). It is keyed by the endpoint id so each
// endpoint on a branch (a writer plus read replicas) gets a distinct
// instance. "yb-ep-" + a lowercase-hyphen uuid is a valid DNS-1123 name (42
// chars, under Helm's 53-char limit). CreateEndpoint and DeleteEndpoint must
// agree on it so a retire targets the right instance.
func endpointDeployName(endpointID uuid.UUID) string {
	return "yb-ep-" + endpointID.String()
}

func (c *Controllers) ListClusters(ctx context.Context, request ListClustersRequestObject) (ListClustersResponseObject, error) {
	limit, offset := pageParams(request.Params.Limit, request.Params.Offset)
	// Scope to usable clusters unless the caller opts in (include_failed=true);
	// the default hides failed-creation orphans so pickers, counts, and clients
	// that operate on each result don't trip over half-provisioned clusters.
	includeFailed := request.Params.IncludeFailed != nil && *request.Params.IncludeFailed
	clusters, total, err := c.backend.ListClusters(ctx, request.PoolId, limit, offset, includeFailed)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return ListClusters404JSONResponse{NotFoundJSONResponse(apiError("not_found", "storage pool not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	out := make([]Cluster, 0, len(clusters))
	for _, p := range clusters {
		out = append(out, clusterToAPI(p))
	}
	return ListClusters200JSONResponse(ClusterList{Clusters: out, Total: total, Count: len(out)}), nil
}

func (c *Controllers) GetCluster(ctx context.Context, request GetClusterRequestObject) (GetClusterResponseObject, error) {
	p, err := c.backend.GetCluster(ctx, request.PoolId, request.ClusterId)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return GetCluster404JSONResponse{NotFoundJSONResponse(apiError("not_found", "cluster not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get cluster: %w", err)
	}
	return GetCluster200JSONResponse(clusterToAPI(p)), nil
}

func (c *Controllers) DeleteCluster(ctx context.Context, request DeleteClusterRequestObject) (DeleteClusterResponseObject, error) {
	// Cheap pre-check: the cluster must exist (404). The cascade delete itself —
	// retiring every endpoint and dropping the branches and the cluster row — runs
	// as the task's idempotent delete_cluster step.
	if _, err := c.backend.GetCluster(ctx, request.PoolId, request.ClusterId); err != nil {
		switch {
		case errors.Is(err, core.ErrNotFound):
			return DeleteCluster404JSONResponse{NotFoundJSONResponse(apiError("not_found", "cluster not found"))}, nil
		default:
			return nil, fmt.Errorf("get cluster: %w", err)
		}
	}

	t, err := c.tasks.DeleteCluster(ctx, task.DeleteClusterInput{
		PoolID:    request.PoolId,
		ClusterID: request.ClusterId,
	})
	if err != nil {
		return nil, fmt.Errorf("delete cluster task: %w", err)
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "delete cluster task accepted",
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String(applog.KeyClusterID, request.ClusterId.String()),
		slog.String(applog.KeyTaskID, t.ID.String()))

	return DeleteCluster202JSONResponse{AcceptedJSONResponse{
		Body:    taskToAPI(t),
		Headers: AcceptedResponseHeaders{Location: "/api/v1/tasks/" + t.ID.String()},
	}}, nil
}

// --- Branches --------------------------------------------------------------

// CreateBranch forks an existing branch (the parent) in the cluster into a new
// branch. The parent defaults to the cluster's default branch; pass
// parent_branch_id to fork off another branch (branch-of-branch). It validates
// and resolves the fork point synchronously (resolveForkLSN), then plans a
// `branch` task (create_branch -> record_branch_recipe with the ancestor set)
// and returns 202 + the Task. The worker records the branch row and the storage
// controller's pending-init recipe — this time with the ancestor and fork LSN
// set — which materializes lazily at the first endpoint deployed on the branch
// (controller-cluster-create.md).
func (c *Controllers) CreateBranch(ctx context.Context, request CreateBranchRequestObject) (CreateBranchResponseObject, error) {
	if request.Body == nil {
		return CreateBranch400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "request body is required"))}, nil
	}
	name := strings.TrimSpace(request.Body.Name)
	if name == "" {
		return CreateBranch400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "name is required"))}, nil
	}

	// Resolve the cluster (404 if absent); it carries the default branch we fall
	// back to when no parent is given.
	cluster, err := c.backend.GetCluster(ctx, request.PoolId, request.ClusterId)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return CreateBranch404JSONResponse{NotFoundJSONResponse(apiError("not_found", "cluster not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get cluster: %w", err)
	}

	// The parent defaults to the cluster's default branch; an explicit
	// parent_branch_id forks off another branch (branch-of-branch).
	parentBranchID := cluster.DefaultBranch.BranchID
	if request.Body.ParentBranchId != nil {
		parentBranchID = *request.Body.ParentBranchId
	}

	// Resolve the parent branch (404 if absent); we need its ids and pg_version
	// for the branch recipe.
	parent, err := c.backend.GetBranch(ctx, request.PoolId, request.ClusterId, parentBranchID)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return CreateBranch404JSONResponse{NotFoundJSONResponse(apiError("not_found", "parent branch not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get parent branch: %w", err)
	}

	// Resolve the fork point before mutating anything, so a bad LSN or an
	// unmaterialized parent 400s without leaving a half-created branch behind.
	forkLSN, errResp := c.resolveForkLSN(ctx, request.PoolId, parent, request.Body.ForkPointLsn)
	if errResp != nil {
		return errResp, nil
	}

	// Plan the work as an async task and return 202 + the Task. The branch row,
	// its (ancestor-bearing) pending-init recipe, and (later) any endpoint are
	// created by the worker; a duplicate branch name now surfaces as a failed task
	// rather than a synchronous 409. The branch inherits the parent's pg_version.
	// WithWriter:false preserves "create branch does not deploy an endpoint".
	t, err := c.tasks.Branch(ctx, task.BranchInput{
		PoolID:         request.PoolId,
		ClusterID:      request.ClusterId,
		ParentBranchID: parentBranchID,
		Name:           name,
		ForkLSN:        forkLSN,
		PgVersion:      parent.PgVersion,
		WithWriter:     false,
		PgSettings:     branchPgSettings(request.Body),
	})
	if err != nil {
		return nil, fmt.Errorf("create branch task: %w", err)
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "create branch task accepted",
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String(applog.KeyClusterID, request.ClusterId.String()),
		slog.String("parent_branch_id", parent.BranchID.String()),
		slog.String(applog.KeyTaskID, t.ID.String()),
		slog.String("branch_name", name),
		slog.String("fork_lsn", forkLSN))

	return CreateBranch202JSONResponse{AcceptedJSONResponse{
		Body:    taskToAPI(t),
		Headers: AcceptedResponseHeaders{Location: "/api/v1/tasks/" + t.ID.String()},
	}}, nil
}

func (c *Controllers) ListBranches(ctx context.Context, request ListBranchesRequestObject) (ListBranchesResponseObject, error) {
	limit, offset := pageParams(request.Params.Limit, request.Params.Offset)
	branches, total, err := c.backend.ListBranches(ctx, request.PoolId, request.ClusterId, limit, offset)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return ListBranches404JSONResponse{NotFoundJSONResponse(apiError("not_found", "cluster not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("list branches: %w", err)
	}
	out := make([]Branch, 0, len(branches))
	for _, b := range branches {
		out = append(out, branchToAPI(b))
	}
	return ListBranches200JSONResponse(BranchList{Branches: out, Total: total, Count: len(out)}), nil
}

func (c *Controllers) GetBranch(ctx context.Context, request GetBranchRequestObject) (GetBranchResponseObject, error) {
	br, err := c.backend.GetBranch(ctx, request.PoolId, request.ClusterId, request.BranchId)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return GetBranch404JSONResponse{NotFoundJSONResponse(apiError("not_found", "branch not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get branch: %w", err)
	}
	return GetBranch200JSONResponse(branchToAPI(br)), nil
}

func (c *Controllers) DeleteBranch(ctx context.Context, request DeleteBranchRequestObject) (DeleteBranchResponseObject, error) {
	// Cheap pre-check: the branch must exist (404). The cascade delete itself —
	// retiring the branch's endpoints and dropping the branch row — runs as the
	// task's idempotent delete_branch step.
	if _, err := c.backend.GetBranch(ctx, request.PoolId, request.ClusterId, request.BranchId); err != nil {
		switch {
		case errors.Is(err, core.ErrNotFound):
			return DeleteBranch404JSONResponse{NotFoundJSONResponse(apiError("not_found", "branch not found"))}, nil
		default:
			return nil, fmt.Errorf("get branch: %w", err)
		}
	}

	// Synchronous child-fork pre-check: a branch with children cannot be deleted
	// (the parent FK is RESTRICT). Reject with a clean 409 rather than accepting a
	// task that is doomed to fail. The step re-checks at Execute time too.
	branches, _, err := c.backend.ListBranches(ctx, request.PoolId, request.ClusterId, maxPageLimit, 0)
	if err != nil {
		return nil, fmt.Errorf("list branches for child-fork check: %w", err)
	}
	for _, b := range branches {
		if b.ParentBranchID != nil && *b.ParentBranchID == request.BranchId {
			return DeleteBranch409JSONResponse{ConflictJSONResponse(apiError("conflict",
				"cannot delete a branch that has child branches"))}, nil
		}
	}

	t, err := c.tasks.DeleteBranch(ctx, task.DeleteBranchInput{
		PoolID:    request.PoolId,
		ClusterID: request.ClusterId,
		BranchID:  request.BranchId,
	})
	if err != nil {
		return nil, fmt.Errorf("delete branch task: %w", err)
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "delete branch task accepted",
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String(applog.KeyClusterID, request.ClusterId.String()),
		slog.String(applog.KeyBranchID, request.BranchId.String()),
		slog.String(applog.KeyTaskID, t.ID.String()))

	return DeleteBranch202JSONResponse{AcceptedJSONResponse{
		Body:    taskToAPI(t),
		Headers: AcceptedResponseHeaders{Location: "/api/v1/tasks/" + t.ID.String()},
	}}, nil
}

// lsnRE matches the storage layer's "X/X" LSN form, e.g. "0/16ABCD0". Each half
// is a 32-bit value, so it is capped at 8 hex digits: a longer group is not a
// valid pg_lsn and would otherwise pass this gate only to fail (500) at the
// `$5::pg_lsn` cast. Branch fork points are validated against it before they
// reach the cast or the controller.
var lsnRE = regexp.MustCompile(`^[0-9a-fA-F]{1,8}/[0-9a-fA-F]{1,8}$`)

// resolveForkLSN determines the LSN a branch forks at, or a 400 response to
// return as-is. An explicit, well-formed fork_point_lsn wins. Otherwise it
// defaults to the parent's durable WAL end — the safekeeper's flush_lsn — read
// from the parent branch's own read_write endpoint, whose co-located safekeeper
// is the WAL authority for the parent timeline. Reading the ancestor's own node
// (rather than the controller's per-tenant describe) is routing-correct
// in the co-located topology, where each endpoint has its own pageserver +
// safekeeper but the SC tracks one attachment per tenant — so describe routes to
// whichever pageserver attached last and misses the parent timeline once the
// tenant has more than one endpoint (#133). flush_lsn also never lags durable
// WAL the way a pageserver's last_record_lsn can (#130). Without a deployer +
// node client there is no safekeeper address to read, so an explicit
// fork_point_lsn is required rather than silently forking at 0/0. That is the
// case for a controller with no deployer at all and for the local driver, whose
// computes sit at addresses the controller cannot derive.
func (c *Controllers) resolveForkLSN(ctx context.Context, poolID uuid.UUID, parent core.Branch, requested *string) (string, CreateBranchResponseObject) {
	if requested != nil {
		if lsn := strings.TrimSpace(*requested); lsn != "" {
			if !lsnRE.MatchString(lsn) {
				return "", CreateBranch400JSONResponse{BadRequestJSONResponse(apiError("invalid_request",
					"fork_point_lsn must be an LSN of the form X/X (hex), e.g. 0/16ABCD0"))}
			}
			return lsn, nil
		}
	}
	if c.prov.Deployer == nil || c.prov.Node == nil {
		return "", CreateBranch400JSONResponse{BadRequestJSONResponse(apiError("invalid_request",
			"fork_point_lsn is required: this controller cannot reach the parent's safekeeper to resolve one"))}
	}
	rw, errResp := c.parentWriterEndpoint(ctx, poolID, parent)
	if errResp != nil {
		return "", errResp
	}
	skURL := c.prov.Deployer.EndpointSafekeeperURL(endpointDeployName(rw.ID))
	lsn, err := c.prov.Node.TimelineFlushLSN(ctx, skURL, parent.BranchID.String())
	if err != nil {
		// The parent's safekeeper is unreachable, does not serve the timeline, or
		// reports no flush_lsn — there is nothing to branch from. Steer the caller
		// rather than branching at a wrong point.
		c.logger.LogAttrs(ctx, slog.LevelWarn, "could not resolve default fork point from parent safekeeper",
			slog.String(applog.KeyBranchID, parent.BranchID.String()), slog.String("error", err.Error()))
		return "", CreateBranch400JSONResponse{BadRequestJSONResponse(apiError("invalid_request",
			"cannot determine a fork point for the parent branch (deploy an endpoint on it first, or pass fork_point_lsn)"))}
	}
	return lsn, nil
}

// parentWriterEndpoint returns the parent branch's read_write endpoint, whose
// co-located safekeeper holds the parent timeline's durable WAL. It returns a
// 400 response (to return as-is) when the parent has no read_write endpoint —
// not yet materialized, so there is nothing to branch from — or when the lookup
// fails, steering the caller to deploy one or pass an explicit fork_point_lsn.
func (c *Controllers) parentWriterEndpoint(ctx context.Context, poolID uuid.UUID, parent core.Branch) (core.Endpoint, CreateBranchResponseObject) {
	steer := CreateBranch400JSONResponse{BadRequestJSONResponse(apiError("invalid_request",
		"cannot determine a fork point for the parent branch (deploy an endpoint on it first, or pass fork_point_lsn)"))}
	// The single-writer rule caps a branch at one read_write endpoint; a generous
	// page covers any read_only replicas alongside it without a role filter on the
	// backend.
	eps, _, err := c.backend.ListEndpoints(ctx, poolID, parent.ClusterID, parent.BranchID, 100, 0)
	if err != nil {
		c.logger.LogAttrs(ctx, slog.LevelWarn, "could not list parent branch endpoints for fork point",
			slog.String(applog.KeyBranchID, parent.BranchID.String()), slog.String("error", err.Error()))
		return core.Endpoint{}, steer
	}
	for _, ep := range eps {
		if ep.Role == core.RoleReadWrite {
			return ep, nil
		}
	}
	return core.Endpoint{}, steer
}

// --- Endpoints (deployed by a task, #108/#113) -----------------------------

// defaultEndpointCPU / defaultEndpointMemory back a create-endpoint request that
// omits a resource reservation. They mirror the deployer's and the chart's own
// defaults; the handler resolves the concrete value up front so the task's params
// carry it (and a reclaim/replay deploys the same size). request == limit at the
// chart gives the pod Guaranteed QoS.
const (
	defaultEndpointCPU    = "2"
	defaultEndpointMemory = "4Gi"
)

// Validation for caller-supplied Kubernetes resource quantities, so a malformed
// value is rejected synchronously with 400 rather than accepted, planned as a
// task, and only failing later in the worker's helm run.
var (
	// CPU: an integer/decimal number of cores, optionally with the milli suffix
	// (e.g. "2", "1.5", "500m").
	cpuQuantityRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?m?$`)
	// Memory: a number with an optional binary (Ki/Mi/Gi/Ti/Pi/Ei) or decimal-SI
	// (k/M/G/T/P/E) suffix, the milli suffix, or a decimal exponent
	// (e.g. "4Gi", "512Mi", "1500M", "1e9").
	memoryQuantityRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?((Ki|Mi|Gi|Ti|Pi|Ei)|[kKMGTPE]|m|[eE][0-9]+)?$`)
)

// parseResourceOverrides validates an optional resources object and returns the
// trimmed cpu/memory overrides ("" when unset — the caller applies its default).
// errMsg is non-empty on an invalid quantity; callers wrap it in their own
// operation's 400 response type (the generated response types differ per route).
func parseResourceOverrides(r *EndpointResources) (cpu, memory, errMsg string) {
	if r == nil {
		return "", "", ""
	}
	if r.Cpu != nil {
		if v := strings.TrimSpace(*r.Cpu); v != "" {
			if !cpuQuantityRE.MatchString(v) {
				return "", "", `resources.cpu must be a valid Kubernetes CPU quantity (e.g. "2", "500m")`
			}
			cpu = v
		}
	}
	if r.Memory != nil {
		if v := strings.TrimSpace(*r.Memory); v != "" {
			if !memoryQuantityRE.MatchString(v) {
				return "", "", `resources.memory must be a valid Kubernetes memory quantity (e.g. "4Gi", "512Mi")`
			}
			memory = v
		}
	}
	return cpu, memory, ""
}

func (c *Controllers) CreateEndpoint(ctx context.Context, request CreateEndpointRequestObject) (CreateEndpointResponseObject, error) {
	if c.prov.Deployer == nil {
		return nil, echo.NewHTTPError(http.StatusNotImplemented, "endpoint deployment is not enabled on this controller")
	}
	role := core.RoleReadWrite
	if request.Body != nil && request.Body.Role != nil {
		role = string(*request.Body.Role)
	}
	if !core.ValidRole(role) {
		return CreateEndpoint400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "role must be read_write or read_only"))}, nil
	}

	// Resolve the pod's CPU + memory reservation, defaulting to 2 cores / 4Gi.
	// The values are Kubernetes resource quantities passed through to the chart,
	// which renders each as both a request and a limit (Guaranteed QoS). An empty
	// (whitespace-only) override falls back to the default rather than emitting an
	// invalid empty quantity.
	cpu, memory := defaultEndpointCPU, defaultEndpointMemory
	if request.Body != nil {
		oCPU, oMem, errMsg := parseResourceOverrides(request.Body.Resources)
		if errMsg != "" {
			return CreateEndpoint400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", errMsg))}, nil
		}
		if oCPU != "" {
			cpu = oCPU
		}
		if oMem != "" {
			memory = oMem
		}
	}

	// Disk type defaults to network (the configured storage class). A local
	// instance SSD is not yet supported and is rejected up front rather than
	// silently provisioning the default.
	if request.Body != nil && request.Body.DiskType != nil {
		switch *request.Body.DiskType {
		case Network:
		case Local:
			return CreateEndpoint400JSONResponse{BadRequestJSONResponse(apiError("unsupported_disk_type", "local-disk-backed endpoints are not yet supported"))}, nil
		default:
			return CreateEndpoint400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "disk_type must be network or local"))}, nil
		}
	}

	// Externalized extensions to enable on this endpoint. Each opt-in is
	// validated against the controller's catalog (an unknown name is a 400, not
	// a silent drop) and, in the step, persisted onto the branch's durable set so
	// forks inherit it. Requires the catalog (Kubernetes deployer); rejected when
	// it is unavailable (Docker driver / feature off), so a caller gets a clear
	// error rather than a silently ignored request.
	var exts []string
	if request.Body != nil && request.Body.Extensions != nil {
		if c.prov.Extensions == nil {
			return CreateEndpoint400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "externalized extensions are not supported on this controller"))}, nil
		}
		for _, raw := range *request.Body.Extensions {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			if !c.prov.Extensions.Has(name) {
				return CreateEndpoint400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", fmt.Sprintf("unknown extension %q", name)))}, nil
			}
			exts = append(exts, name)
		}
	}

	br, err := c.backend.GetBranch(ctx, request.PoolId, request.ClusterId, request.BranchId)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return CreateEndpoint404JSONResponse{NotFoundJSONResponse(apiError("not_found", "branch not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get branch: %w", err)
	}

	// A name already taken on the branch answers 409 here rather than surfacing
	// as a failed task, so the caller learns immediately that it has to pick
	// another. This is a courtesy check, not the guard: endpoints_name_per_branch
	// is what actually enforces it, and it is what catches the race between this
	// read and the step's insert.
	var name string
	if request.Body != nil && request.Body.Name != nil {
		name = strings.TrimSpace(*request.Body.Name)
	}
	if name != "" {
		taken, err := c.endpointNameTaken(ctx, request.PoolId, request.ClusterId, request.BranchId, name)
		if err != nil {
			return nil, err
		}
		if taken {
			return CreateEndpoint409JSONResponse{ConflictJSONResponse(apiError("conflict",
				fmt.Sprintf("an endpoint named %q already exists on this branch", name)))}, nil
		}
	}

	// This endpoint's own settings. They merge over its branch's at deploy time
	// (the step resolves the effective set), so only these are stored on the row.
	var pgSettings map[string]string
	if request.Body != nil && request.Body.PgSettings != nil {
		pgSettings = *request.Body.PgSettings
	}

	// The two storage daemons' overrides. See validateDaemonSettings for why a bad
	// key has to be caught here rather than at the pod.
	var pageserverSettings, safekeeperSettings map[string]any
	if request.Body != nil && request.Body.PageserverSettings != nil {
		pageserverSettings = *request.Body.PageserverSettings
	}
	if request.Body != nil && request.Body.SafekeeperSettings != nil {
		safekeeperSettings = *request.Body.SafekeeperSettings
	}
	if err := validateDaemonSettings(daemonPageserver, pageserverSettings); err != nil {
		return CreateEndpoint400JSONResponse{BadRequestJSONResponse(
			apiError("invalid_argument", err.Error()))}, nil
	}
	if err := validateDaemonSettings(daemonSafekeeper, safekeeperSettings); err != nil {
		return CreateEndpoint400JSONResponse{BadRequestJSONResponse(
			apiError("invalid_argument", err.Error()))}, nil
	}

	// Opaque passthrough: stored verbatim under config.extended and handed to the
	// deployer. The control plane does not look inside.
	var extended json.RawMessage
	if request.Body != nil && request.Body.ExtendedConfig != nil {
		raw, err := json.Marshal(*request.Body.ExtendedConfig)
		if err != nil {
			return nil, fmt.Errorf("encode extended_config: %w", err)
		}
		extended = raw
	}

	// Plan the work as an async task and return 202 + the Task. The endpoint row,
	// its deploy, and the address record are all created by the worker's
	// create_endpoint step; the single-writer rule (a duplicate read_write
	// endpoint) now surfaces as a failed task rather than a synchronous 409.
	t, err := c.tasks.CreateEndpoint(ctx, task.CreateEndpointInput{
		ClusterID:          br.ClusterID,
		BranchID:           br.BranchID,
		Role:               role,
		PgVersion:          br.PgVersion,
		Name:               name,
		CPU:                cpu,
		Memory:             memory,
		Extensions:         exts,
		Extended:           extended,
		PgSettings:         pgSettings,
		PageserverSettings: pageserverSettings,
		SafekeeperSettings: safekeeperSettings,
	})
	if err != nil {
		return nil, fmt.Errorf("create endpoint task: %w", err)
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "create endpoint task accepted",
		slog.String(applog.KeyClusterID, br.ClusterID.String()),
		slog.String(applog.KeyBranchID, request.BranchId.String()),
		slog.String(applog.KeyTaskID, t.ID.String()),
		slog.String("role", role),
		slog.String("cpu", cpu),
		slog.String("memory", memory))

	return CreateEndpoint202JSONResponse{AcceptedJSONResponse{
		Body:    taskToAPI(t),
		Headers: AcceptedResponseHeaders{Location: "/api/v1/tasks/" + t.ID.String()},
	}}, nil
}

func (c *Controllers) GetEndpoint(ctx context.Context, request GetEndpointRequestObject) (GetEndpointResponseObject, error) {
	ep, err := c.backend.GetEndpoint(ctx, request.PoolId, request.ClusterId, request.BranchId, request.EndpointId)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return GetEndpoint404JSONResponse{NotFoundJSONResponse(apiError("not_found", "endpoint not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get endpoint: %w", err)
	}
	return GetEndpoint200JSONResponse(endpointToAPI(ep)), nil
}

func (c *Controllers) ListEndpoints(ctx context.Context, request ListEndpointsRequestObject) (ListEndpointsResponseObject, error) {
	limit, offset := pageParams(request.Params.Limit, request.Params.Offset)
	endpoints, total, err := c.backend.ListEndpoints(ctx, request.PoolId, request.ClusterId, request.BranchId, limit, offset)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return ListEndpoints404JSONResponse{NotFoundJSONResponse(apiError("not_found", "branch not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("list endpoints: %w", err)
	}
	out := make([]Endpoint, 0, len(endpoints))
	for _, e := range endpoints {
		out = append(out, endpointToAPI(e))
	}
	return ListEndpoints200JSONResponse(EndpointList{Endpoints: out, Total: total, Count: len(out)}), nil
}

func (c *Controllers) DeleteEndpoint(ctx context.Context, request DeleteEndpointRequestObject) (DeleteEndpointResponseObject, error) {
	if c.prov.Deployer == nil {
		return nil, echo.NewHTTPError(http.StatusNotImplemented, "endpoint deployment is not enabled on this controller")
	}
	if _, err := c.backend.GetEndpoint(ctx, request.PoolId, request.ClusterId, request.BranchId, request.EndpointId); err != nil {
		switch {
		case errors.Is(err, core.ErrNotFound):
			return DeleteEndpoint404JSONResponse{NotFoundJSONResponse(apiError("not_found", "endpoint not found"))}, nil
		default:
			return nil, fmt.Errorf("get endpoint: %w", err)
		}
	}

	// Plan the work as an async task and return 202 + the Task. Retiring the
	// running instance and dropping the record run as the worker's idempotent
	// delete_endpoint step.
	t, err := c.tasks.DeleteEndpoint(ctx, task.DeleteEndpointInput{
		ClusterID:  request.ClusterId,
		EndpointID: request.EndpointId,
	})
	if err != nil {
		return nil, fmt.Errorf("delete endpoint task: %w", err)
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "delete endpoint task accepted",
		slog.String(applog.KeyEndpointID, request.EndpointId.String()),
		slog.String(applog.KeyTaskID, t.ID.String()))

	return DeleteEndpoint202JSONResponse{AcceptedJSONResponse{
		Body:    taskToAPI(t),
		Headers: AcceptedResponseHeaders{Location: "/api/v1/tasks/" + t.ID.String()},
	}}, nil
}

// endpointNameTaken reports whether the branch already has an endpoint by this
// name. A generous page covers any realistic per-branch endpoint count.
func (c *Controllers) endpointNameTaken(ctx context.Context, poolID, clusterID, branchID uuid.UUID, name string) (bool, error) {
	eps, _, err := c.backend.ListEndpoints(ctx, poolID, clusterID, branchID, 1000, 0)
	if err != nil {
		return false, fmt.Errorf("list endpoints: %w", err)
	}
	for _, ep := range eps {
		if ep.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (c *Controllers) ResizeEndpoint(ctx context.Context, request ResizeEndpointRequestObject) (ResizeEndpointResponseObject, error) {
	if c.prov.Deployer == nil {
		return nil, echo.NewHTTPError(http.StatusNotImplemented, "endpoint deployment is not enabled on this controller")
	}
	if request.Body == nil {
		return ResizeEndpoint400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "a resources body is required"))}, nil
	}

	// The GetEndpoint doubles as the 404 check and the source of the endpoint's
	// current reservation, so an unspecified dimension keeps its current value
	// (a resize that sets only cpu leaves memory unchanged, and vice versa).
	ep, err := c.backend.GetEndpoint(ctx, request.PoolId, request.ClusterId, request.BranchId, request.EndpointId)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return ResizeEndpoint404JSONResponse{NotFoundJSONResponse(apiError("not_found", "endpoint not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get endpoint: %w", err)
	}
	cpu := ep.CPU
	if cpu == "" {
		cpu = defaultEndpointCPU
	}
	memory := ep.Memory
	if memory == "" {
		memory = defaultEndpointMemory
	}
	if request.Body.Resources.Cpu != nil {
		if v := strings.TrimSpace(*request.Body.Resources.Cpu); v != "" {
			if !cpuQuantityRE.MatchString(v) {
				return ResizeEndpoint400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", `resources.cpu must be a valid Kubernetes CPU quantity (e.g. "2", "500m")`))}, nil
			}
			cpu = v
		}
	}
	if request.Body.Resources.Memory != nil {
		if v := strings.TrimSpace(*request.Body.Resources.Memory); v != "" {
			if !memoryQuantityRE.MatchString(v) {
				return ResizeEndpoint400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", `resources.memory must be a valid Kubernetes memory quantity (e.g. "4Gi", "512Mi")`))}, nil
			}
			memory = v
		}
	}

	// Plan the work as an async task and return 202 + the Task. The worker's
	// resize_endpoint step persists the new size, updates the workload's desired
	// resources, and resizes the running pod in place; its result reports the
	// in-place outcome.
	t, err := c.tasks.ResizeEndpoint(ctx, task.ResizeEndpointInput{
		ClusterID:  request.ClusterId,
		EndpointID: request.EndpointId,
		CPU:        cpu,
		Memory:     memory,
	})
	switch {
	case errors.Is(err, task.ErrResizeInProgress):
		// First-writer-wins: another resize of this endpoint is already in flight.
		return ResizeEndpoint409JSONResponse{ConflictJSONResponse(apiError("conflict", "a resize is already in progress for this endpoint"))}, nil
	case err != nil:
		return nil, fmt.Errorf("resize endpoint task: %w", err)
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "resize endpoint task accepted",
		slog.String(applog.KeyEndpointID, request.EndpointId.String()),
		slog.String(applog.KeyTaskID, t.ID.String()),
		slog.String("cpu", cpu),
		slog.String("memory", memory))

	return ResizeEndpoint202JSONResponse{AcceptedJSONResponse{
		Body:    taskToAPI(t),
		Headers: AcceptedResponseHeaders{Location: "/api/v1/tasks/" + t.ID.String()},
	}}, nil
}

// --- Tasks -----------------------------------------------------------------

func (c *Controllers) GetTask(ctx context.Context, request GetTaskRequestObject) (GetTaskResponseObject, error) {
	t, err := c.tasks.Get(ctx, request.TaskId)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	if t == nil {
		return GetTask404JSONResponse{NotFoundJSONResponse(apiError("not_found", "task not found"))}, nil
	}
	out := taskToAPI(t)
	// Surface the final step's result (e.g. a resize's in-place outcome) on the
	// poll target so clients can report it. Best-effort: a result-read failure
	// doesn't fail the task fetch.
	if res, err := c.tasks.Result(ctx, t.ID); err == nil && len(res) > 0 {
		var v any
		if json.Unmarshal(res, &v) == nil {
			out.Result = &v
		}
	}
	return GetTask200JSONResponse(out), nil
}

func (c *Controllers) ListTasks(ctx context.Context, request ListTasksRequestObject) (ListTasksResponseObject, error) {
	limit, offset := pageParams(request.Params.Limit, request.Params.Offset)

	// Zero-value filter fields are unfiltered: an empty Status/Kind matches any,
	// and a nil ClusterID matches any cluster. The server has no wire-level enum
	// validator middleware, so an unrecognized status must be rejected here —
	// otherwise it reaches the Postgres ::task_status cast and surfaces as a 500.
	f := task.TaskFilter{}
	if request.Params.Status != nil {
		switch task.Status(*request.Params.Status) {
		case task.StatusPending, task.StatusRunning, task.StatusSucceeded, task.StatusFailed:
			f.Status = task.Status(*request.Params.Status)
		default:
			return ListTasks400JSONResponse{BadRequestJSONResponse(apiError("invalid_argument",
				"status must be one of pending, running, succeeded, failed"))}, nil
		}
	}
	if request.Params.Kind != nil {
		f.Kind = *request.Params.Kind
	}
	if request.Params.ClusterId != nil {
		f.ClusterID = request.Params.ClusterId
	}

	tasks, total, err := c.tasks.List(ctx, f, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	out := make([]Task, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, taskToAPI(t))
	}
	return ListTasks200JSONResponse(TaskList{Tasks: out, Total: total, Count: len(out)}), nil
}

// --- mapping helpers -------------------------------------------------------

func userToAPI(u core.User) User {
	return User{Id: u.ID, IsAdmin: u.IsAdmin, CreatedAt: u.CreatedAt}
}

func poolToAPI(p core.StoragePool) StoragePool {
	return StoragePool{Id: p.ID, Name: p.Name, OwnerUserId: p.OwnerUserID, CreatedAt: p.CreatedAt}
}

func clusterToAPI(p core.Cluster) Cluster {
	return Cluster{
		Id:            p.ID,
		StoragePoolId: p.StoragePoolID,
		Name:          p.Name,
		CreatedAt:     p.CreatedAt,
		DefaultBranch: branchToAPI(p.DefaultBranch),
	}
}

// branchPgSettings pulls the caller's settings off a create-branch body. They
// merge over what the branch inherits from its ancestor.
func branchPgSettings(body *CreateBranchRequest) map[string]string {
	if body == nil || body.PgSettings == nil {
		return nil
	}
	return *body.PgSettings
}

func branchToAPI(b core.Branch) Branch {
	out := Branch{
		ClusterId:      b.ClusterID,
		BranchId:       b.BranchID,
		Name:           b.Name,
		PgVersion:      b.PgVersion,
		IsDefault:      b.IsDefault,
		ParentBranchId: b.ParentBranchID, // nil for a cluster's default branch
		CreatedAt:      b.CreatedAt,
	}
	if b.ForkLSN != "" {
		out.ForkLsn = &b.ForkLSN
	}
	if len(b.PgSettings) > 0 {
		out.PgSettings = &b.PgSettings
	}
	if len(b.Extensions) > 0 {
		out.Extensions = &b.Extensions
	}
	return out
}

func endpointToAPI(e core.Endpoint) Endpoint {
	// Report the *effective* reservation: an endpoint created before resize
	// existed has no cpu/memory in its config (stored as ""), but the pod still
	// runs at the create-time defaults — so surface those rather than empty
	// strings, which would misrepresent the pod's real size.
	cpu, memory := e.CPU, e.Memory
	if cpu == "" {
		cpu = defaultEndpointCPU
	}
	if memory == "" {
		memory = defaultEndpointMemory
	}
	out := Endpoint{
		Id:             e.ID,
		ClusterId:      e.ClusterID,
		BranchId:       e.BranchID,
		Role:           EndpointRole(e.Role),
		Name:           e.Name,
		Address:        e.Address,
		TailnetAddress: e.TailnetAddress,
		Resources:      &EndpointResources{Cpu: &cpu, Memory: &memory},
		CreatedAt:      e.CreatedAt,
	}
	if len(e.PgSettings) > 0 {
		out.PgSettings = &e.PgSettings
	}
	if len(e.PageserverSettings) > 0 {
		ps := DaemonSettings(e.PageserverSettings)
		out.PageserverSettings = &ps
	}
	if len(e.SafekeeperSettings) > 0 {
		sk := DaemonSettings(e.SafekeeperSettings)
		out.SafekeeperSettings = &sk
	}
	// Echoed verbatim, including its internal shape: whoever set it is the only
	// party that knows what it means.
	if len(e.Extended) > 0 {
		var ext map[string]any
		if json.Unmarshal(e.Extended, &ext) == nil {
			out.ExtendedConfig = &ext
		}
	}
	return out
}

// Pagination bounds for the list endpoints, matching the spec's limit parameter.
const (
	defaultPageLimit = 100
	maxPageLimit     = 1000
)

// pageParams resolves the spec's optional limit/offset query parameters into
// backend arguments: limit defaults to 100 and is clamped to 1..1000; offset
// defaults to 0 and is clamped to >= 0. Out-of-range values are clamped rather
// than rejected, so a caller never has a list call 400 on pagination alone.
func pageParams(limit *Limit, offset *Offset) (int, int) {
	l := defaultPageLimit
	if limit != nil {
		l = int(*limit)
	}
	l = max(1, min(l, maxPageLimit))

	o := 0
	if offset != nil {
		o = int(*offset)
	}
	o = max(0, o)
	return l, o
}

func taskToAPI(t *task.Task) Task {
	out := Task{
		Id:        t.ID,
		Kind:      t.Kind,
		Status:    TaskStatus(t.Status),
		ClusterId: t.ClusterID,
		TargetRef: t.TargetRef,
		Error:     t.Error,
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
	if len(t.Params) > 0 {
		var v any
		if err := json.Unmarshal(t.Params, &v); err == nil {
			out.Params = &v
		}
	}
	return out
}
