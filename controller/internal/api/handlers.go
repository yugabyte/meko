package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/yugabyte/yb-amp/controller/internal/core"
	"github.com/yugabyte/yb-amp/controller/internal/deploy"
	applog "github.com/yugabyte/yb-amp/controller/internal/log"
	"github.com/yugabyte/yb-amp/controller/internal/node"
	"github.com/yugabyte/yb-amp/controller/internal/storagecontroller"
	"github.com/yugabyte/yb-amp/controller/internal/task"
)

// Provisioning holds the collaborators the create-project / create-endpoint
// workflows use beyond the state store: the storage controller (to record the
// pending-init recipe and, once an endpoint materializes the branch, drop it)
// and the endpoint deployer (to provision the compute). Both are optional —
// with no deployer the endpoint mutation routes refuse with 501, and with no SC
// create-project records the control-plane rows only. SCEndpointURL is the
// controller URL as a *launched endpoint* reaches it (e.g. over the shared
// network); empty means the endpoint runs SC-free. Bootstrap forces initdb of
// the recorded branch on the first endpoint's deploy.
type Provisioning struct {
	SC       *storagecontroller.Client
	Deployer deploy.Deployer
	// Node reads LSNs from an endpoint's co-located safekeeper to resolve a
	// branch's default fork point (#133). Paired with the Deployer (which yields
	// the safekeeper's address); without both, a default fork point can't be
	// resolved and branching requires an explicit fork_point_lsn.
	Node          *node.Client
	EndpointYB    deploy.YBConn
	SCEndpointURL string
	Bootstrap     bool
}

// Controllers implements the generated StrictServerInterface: the controller's
// HTTP behavior, decoupled from the wire format (which the generated code owns)
// and from persistence (which the core.Backend owns). Project, branch, and
// endpoint mutations (create + delete) plan write-ahead tasks and return 202 +
// the Task + a Location header; the worker performs the work — recording the
// control rows plus the storage controller's pending-init recipe and deploying
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

// --- Projects --------------------------------------------------------------
// Create and delete are async: each runs a cheap synchronous pre-check, then
// plans a task (create_project / delete_project) and returns 202 + the Task.
// The project + default branch rows and the storage controller's pending-init
// recipe are written by the create_project worker; the branch is materialized
// later by the first endpoint deployed on it. Delete cascades to the project's
// branches and endpoints in the worker.

// defaultPgVersion is used when a create-project request omits pg_version.
// yb-light is Postgres v18 only.
const defaultPgVersion = 18

func (c *Controllers) CreateProject(ctx context.Context, request CreateProjectRequestObject) (CreateProjectResponseObject, error) {
	if request.Body == nil {
		return CreateProject400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "request body is required"))}, nil
	}
	name := strings.TrimSpace(request.Body.Name)
	if name == "" {
		return CreateProject400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "name is required"))}, nil
	}
	pgVersion := defaultPgVersion
	if request.Body.PgVersion != nil {
		pgVersion = *request.Body.PgVersion
	}
	// yb-light is Postgres v18 only; reject anything else up front rather than
	// failing opaquely when the endpoint image (v18) can't honor it.
	if pgVersion != defaultPgVersion {
		return CreateProject400JSONResponse{BadRequestJSONResponse(apiError("invalid_request",
			fmt.Sprintf("unsupported pg_version %d (only %d is supported)", pgVersion, defaultPgVersion)))}, nil
	}
	// Cheap pre-check: the pool must exist. The task's create_project step would
	// also fail on an absent pool, but a 404 here keeps the synchronous
	// pool-not-found contract rather than surfacing it as a failed task.
	if _, err := c.backend.GetStoragePool(ctx, request.PoolId); err != nil {
		switch {
		case errors.Is(err, core.ErrNotFound):
			return CreateProject404JSONResponse{NotFoundJSONResponse(apiError("not_found", "storage pool not found"))}, nil
		default:
			return nil, fmt.Errorf("get storage pool: %w", err)
		}
	}

	// Plan the work as an async task and return 202 + the Task. The project +
	// default branch rows, the pending-init recipe, and (later) any endpoint are
	// all created by the worker; a duplicate project name now surfaces as a failed
	// task rather than a synchronous 409. WithWriter:false preserves "create
	// project does not deploy an endpoint" — compute is a separate resource.
	t, err := c.tasks.CreateProject(ctx, task.CreateProjectInput{
		PoolID:     request.PoolId,
		Name:       name,
		PgVersion:  pgVersion,
		WithWriter: false,
	})
	if err != nil {
		return nil, fmt.Errorf("create project task: %w", err)
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "create project task accepted",
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String(applog.KeyTaskID, t.ID.String()),
		slog.String("project_name", name))

	return CreateProject202JSONResponse{AcceptedJSONResponse{
		Body:    taskToAPI(t),
		Headers: AcceptedResponseHeaders{Location: "/api/v1/tasks/" + t.ID.String()},
	}}, nil
}

// hex32 renders a UUID as the storage layer's 32-lowercase-hex BranchId (the
// flat storage identity; the product branch_id is the storage branch_id verbatim).
func hex32(id uuid.UUID) string {
	return hex.EncodeToString(id[:])
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

func (c *Controllers) ListProjects(ctx context.Context, request ListProjectsRequestObject) (ListProjectsResponseObject, error) {
	limit, offset := pageParams(request.Params.Limit, request.Params.Offset)
	projects, total, err := c.backend.ListProjects(ctx, request.PoolId, limit, offset)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return ListProjects404JSONResponse{NotFoundJSONResponse(apiError("not_found", "storage pool not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("list projects: %w", err)
	}
	out := make([]Project, 0, len(projects))
	for _, p := range projects {
		out = append(out, projectToAPI(p))
	}
	return ListProjects200JSONResponse(ProjectList{Projects: out, Total: total, Count: len(out)}), nil
}

func (c *Controllers) GetProject(ctx context.Context, request GetProjectRequestObject) (GetProjectResponseObject, error) {
	p, err := c.backend.GetProject(ctx, request.PoolId, request.ProjectId)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return GetProject404JSONResponse{NotFoundJSONResponse(apiError("not_found", "project not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get project: %w", err)
	}
	return GetProject200JSONResponse(projectToAPI(p)), nil
}

func (c *Controllers) DeleteProject(ctx context.Context, request DeleteProjectRequestObject) (DeleteProjectResponseObject, error) {
	// Cheap pre-check: the project must exist (404). The cascade delete itself —
	// retiring every endpoint and dropping the branches and the project row — runs
	// as the task's idempotent delete_project step.
	if _, err := c.backend.GetProject(ctx, request.PoolId, request.ProjectId); err != nil {
		switch {
		case errors.Is(err, core.ErrNotFound):
			return DeleteProject404JSONResponse{NotFoundJSONResponse(apiError("not_found", "project not found"))}, nil
		default:
			return nil, fmt.Errorf("get project: %w", err)
		}
	}

	t, err := c.tasks.DeleteProject(ctx, task.DeleteProjectInput{
		PoolID:    request.PoolId,
		ProjectID: request.ProjectId,
	})
	if err != nil {
		return nil, fmt.Errorf("delete project task: %w", err)
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "delete project task accepted",
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String(applog.KeyProjectID, request.ProjectId.String()),
		slog.String(applog.KeyTaskID, t.ID.String()))

	return DeleteProject202JSONResponse{AcceptedJSONResponse{
		Body:    taskToAPI(t),
		Headers: AcceptedResponseHeaders{Location: "/api/v1/tasks/" + t.ID.String()},
	}}, nil
}

// --- Branches --------------------------------------------------------------

// CreateBranch forks an existing branch (the parent) in the project into a new
// branch. The parent defaults to the project's default branch; pass
// parent_branch_id to fork off another branch (branch-of-branch). It validates
// and resolves the fork point synchronously (resolveForkLSN), then plans a
// `branch` task (create_branch -> record_branch_recipe with the ancestor set)
// and returns 202 + the Task. The worker records the branch row and the storage
// controller's pending-init recipe — this time with the ancestor and fork LSN
// set — which materializes lazily at the first endpoint deployed on the branch
// (controller-project-create.md).
func (c *Controllers) CreateBranch(ctx context.Context, request CreateBranchRequestObject) (CreateBranchResponseObject, error) {
	if request.Body == nil {
		return CreateBranch400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "request body is required"))}, nil
	}
	name := strings.TrimSpace(request.Body.Name)
	if name == "" {
		return CreateBranch400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "name is required"))}, nil
	}

	// Resolve the project (404 if absent); it carries the default branch we fall
	// back to when no parent is given.
	project, err := c.backend.GetProject(ctx, request.PoolId, request.ProjectId)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return CreateBranch404JSONResponse{NotFoundJSONResponse(apiError("not_found", "project not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get project: %w", err)
	}

	// The parent defaults to the project's default branch; an explicit
	// parent_branch_id forks off another branch (branch-of-branch).
	parentBranchID := project.DefaultBranch.BranchID
	if request.Body.ParentBranchId != nil {
		parentBranchID = *request.Body.ParentBranchId
	}

	// Resolve the parent branch (404 if absent); we need its ids and pg_version
	// for the branch recipe.
	parent, err := c.backend.GetBranch(ctx, request.PoolId, request.ProjectId, parentBranchID)
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
		ProjectID:      request.ProjectId,
		ParentBranchID: parentBranchID,
		Name:           name,
		ForkLSN:        forkLSN,
		PgVersion:      parent.PgVersion,
		WithWriter:     false,
	})
	if err != nil {
		return nil, fmt.Errorf("create branch task: %w", err)
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "create branch task accepted",
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String(applog.KeyProjectID, request.ProjectId.String()),
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
	branches, total, err := c.backend.ListBranches(ctx, request.PoolId, request.ProjectId, limit, offset)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return ListBranches404JSONResponse{NotFoundJSONResponse(apiError("not_found", "project not found"))}, nil
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
	br, err := c.backend.GetBranch(ctx, request.PoolId, request.ProjectId, request.BranchId)
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
	if _, err := c.backend.GetBranch(ctx, request.PoolId, request.ProjectId, request.BranchId); err != nil {
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
	branches, _, err := c.backend.ListBranches(ctx, request.PoolId, request.ProjectId, maxPageLimit, 0)
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
		ProjectID: request.ProjectId,
		BranchID:  request.BranchId,
	})
	if err != nil {
		return nil, fmt.Errorf("delete branch task: %w", err)
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "delete branch task accepted",
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String(applog.KeyProjectID, request.ProjectId.String()),
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
// reach the cast or the storage controller.
var lsnRE = regexp.MustCompile(`^[0-9a-fA-F]{1,8}/[0-9a-fA-F]{1,8}$`)

// resolveForkLSN determines the LSN a branch forks at, or a 400 response to
// return as-is. An explicit, well-formed fork_point_lsn wins. Otherwise it
// defaults to the parent's durable WAL end — the safekeeper's flush_lsn — read
// from the parent branch's own read_write endpoint, whose co-located safekeeper
// is the WAL authority for the parent timeline. Reading the ancestor's own node
// (rather than the storage controller's per-tenant describe) is routing-correct
// in the co-located topology, where each endpoint has its own pageserver +
// safekeeper but the SC tracks one attachment per tenant — so describe routes to
// whichever pageserver attached last and misses the parent timeline once the
// tenant has more than one endpoint (#133). flush_lsn also never lags durable
// WAL the way a pageserver's last_record_lsn can (#130). Without a deployer +
// node client there is no safekeeper address to read, so an explicit
// fork_point_lsn is required rather than silently forking at 0/0.
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
			"fork_point_lsn is required when endpoint deployment is not configured"))}
	}
	rw, errResp := c.parentWriterEndpoint(ctx, poolID, parent)
	if errResp != nil {
		return "", errResp
	}
	skURL := c.prov.Deployer.EndpointSafekeeperURL(endpointDeployName(rw.ID))
	lsn, err := c.prov.Node.TimelineFlushLSN(ctx, skURL, hex32(parent.BranchID))
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
	eps, _, err := c.backend.ListEndpoints(ctx, poolID, parent.ProjectID, parent.BranchID, 100, 0)
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

	br, err := c.backend.GetBranch(ctx, request.PoolId, request.ProjectId, request.BranchId)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return CreateEndpoint404JSONResponse{NotFoundJSONResponse(apiError("not_found", "branch not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get branch: %w", err)
	}

	// Plan the work as an async task and return 202 + the Task. The endpoint row,
	// its deploy, and the address record are all created by the worker's
	// create_endpoint step; the single-writer rule (a duplicate read_write
	// endpoint) now surfaces as a failed task rather than a synchronous 409. A fork
	// carries its ancestry so the step clones from the ancestor at the fork LSN
	// rather than initdb'ing a fresh branch.
	t, err := c.tasks.CreateEndpoint(ctx, task.CreateEndpointInput{
		ProjectID:        br.ProjectID,
		BranchID:         br.BranchID,
		Role:             role,
		PgVersion:        br.PgVersion,
		AncestorBranchID: br.ParentBranchID,
		AncestorLSN:      br.ForkLSN,
	})
	if err != nil {
		return nil, fmt.Errorf("create endpoint task: %w", err)
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "create endpoint task accepted",
		slog.String(applog.KeyProjectID, br.ProjectID.String()),
		slog.String(applog.KeyBranchID, request.BranchId.String()),
		slog.String(applog.KeyTaskID, t.ID.String()),
		slog.String("role", role))

	return CreateEndpoint202JSONResponse{AcceptedJSONResponse{
		Body:    taskToAPI(t),
		Headers: AcceptedResponseHeaders{Location: "/api/v1/tasks/" + t.ID.String()},
	}}, nil
}

func (c *Controllers) GetEndpoint(ctx context.Context, request GetEndpointRequestObject) (GetEndpointResponseObject, error) {
	ep, err := c.backend.GetEndpoint(ctx, request.PoolId, request.ProjectId, request.BranchId, request.EndpointId)
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
	endpoints, total, err := c.backend.ListEndpoints(ctx, request.PoolId, request.ProjectId, request.BranchId, limit, offset)
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
	if _, err := c.backend.GetEndpoint(ctx, request.PoolId, request.ProjectId, request.BranchId, request.EndpointId); err != nil {
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
		ProjectID:  request.ProjectId,
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

// --- Tasks -----------------------------------------------------------------

func (c *Controllers) GetTask(ctx context.Context, request GetTaskRequestObject) (GetTaskResponseObject, error) {
	t, err := c.tasks.Get(ctx, request.TaskId)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	if t == nil {
		return GetTask404JSONResponse{NotFoundJSONResponse(apiError("not_found", "task not found"))}, nil
	}
	return GetTask200JSONResponse(taskToAPI(t)), nil
}

func (c *Controllers) ListTasks(ctx context.Context, request ListTasksRequestObject) (ListTasksResponseObject, error) {
	limit, offset := pageParams(request.Params.Limit, request.Params.Offset)

	// Zero-value filter fields are unfiltered: an empty Status/Kind matches any,
	// and a nil ProjectID matches any project. The server has no wire-level enum
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
	if request.Params.ProjectId != nil {
		f.ProjectID = request.Params.ProjectId
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

func projectToAPI(p core.Project) Project {
	return Project{
		Id:            p.ID,
		StoragePoolId: p.StoragePoolID,
		Name:          p.Name,
		CreatedAt:     p.CreatedAt,
		DefaultBranch: branchToAPI(p.DefaultBranch),
	}
}

func branchToAPI(b core.Branch) Branch {
	out := Branch{
		ProjectId:      b.ProjectID,
		BranchId:       b.BranchID,
		Name:           b.Name,
		PgVersion:      b.PgVersion,
		IsDefault:      b.IsDefault,
		ParentBranchId: b.ParentBranchID, // nil for a project's default branch
		CreatedAt:      b.CreatedAt,
	}
	if b.ForkLSN != "" {
		out.ForkLsn = &b.ForkLSN
	}
	return out
}

func endpointToAPI(e core.Endpoint) Endpoint {
	return Endpoint{
		Id:        e.ID,
		ProjectId: e.ProjectID,
		BranchId:  e.BranchID,
		Role:      EndpointRole(e.Role),
		Address:   e.Address,
		CreatedAt: e.CreatedAt,
	}
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
		ProjectId: t.ProjectID,
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
