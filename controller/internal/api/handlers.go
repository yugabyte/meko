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
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/yugabyte/yb-amp/controller/internal/core"
	"github.com/yugabyte/yb-amp/controller/internal/deploy"
	applog "github.com/yugabyte/yb-amp/controller/internal/log"
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
	SC            *storagecontroller.Client
	Deployer      deploy.Deployer
	EndpointYB    deploy.YBConn
	SCEndpointURL string
	Bootstrap     bool
}

// Controllers implements the generated StrictServerInterface: the controller's
// HTTP behavior, decoupled from the wire format (which the generated code owns)
// and from persistence (which the core.Backend owns). Project create and branch
// record the control rows plus the storage controller's pending-init recipe; the
// branch is materialized later by the first endpoint deployed on it (#108, #130).
// Project and branch delete still route through the task framework eventually
// (§6) and are logged-only for now (#92).
type Controllers struct {
	backend core.Backend
	tasks   task.Store
	prov    Provisioning
	logger  *slog.Logger
}

// NewControllers wires the controllers to a backend, the task store (for task
// status reads), the provisioning collaborators, and the root logger.
func NewControllers(backend core.Backend, tasks task.Store, prov Provisioning, logger *slog.Logger) *Controllers {
	return &Controllers{backend: backend, tasks: tasks, prov: prov, logger: logger}
}

// Compile-time assertion that the controllers satisfy the generated contract.
var _ StrictServerInterface = (*Controllers)(nil)

// --- Users -----------------------------------------------------------------

func (c *Controllers) CreateUser(ctx context.Context, request CreateUserRequestObject) (CreateUserResponseObject, error) {
	isAdmin := false
	if request.Body != nil && request.Body.IsAdmin != nil {
		isAdmin = *request.Body.IsAdmin
	}
	u, err := c.backend.CreateUser(ctx, isAdmin)
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
	p, err := c.backend.CreateStoragePool(ctx, owner, name)
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
// Create records a project + its default branch plus the storage controller's
// pending-init recipe; the branch is materialized later by the first endpoint
// deployed on it. Delete remains logged-only until the task framework lands (#92).

// defaultPgVersion is used when a create-project request omits pg_version.
// yb-light is Postgres v17 only.
const defaultPgVersion = 17

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
	// yb-light is Postgres v17 only; reject anything else up front rather than
	// failing opaquely when the endpoint image (v17) can't honor it.
	if pgVersion != defaultPgVersion {
		return CreateProject400JSONResponse{BadRequestJSONResponse(apiError("invalid_request",
			fmt.Sprintf("unsupported pg_version %d (only %d is supported)", pgVersion, defaultPgVersion)))}, nil
	}
	p, err := c.backend.CreateProject(ctx, request.PoolId, name, pgVersion)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return CreateProject404JSONResponse{NotFoundJSONResponse(apiError("not_found", "storage pool not found"))}, nil
	case errors.Is(err, core.ErrConflict):
		return CreateProject409JSONResponse{ConflictJSONResponse(apiError("conflict", "a project with that name already exists in the pool"))}, nil
	case err != nil:
		return nil, fmt.Errorf("create project: %w", err)
	}

	// Record the pending-init recipe for the project's default branch with the
	// storage controller. The branch is declared here, not yet materialized: the
	// first endpoint deployed for it attaches a pageserver and bootstraps it
	// (controller-project-create.md). The recipe lives in both the control DB (the
	// branch row) and the storage controller. Storage-layer ids are 32-hex (no
	// dashes); the control plane stores the same bytes as a SQL uuid. Synchronous
	// for now — #113 moves this onto the write-ahead task framework. We do NOT
	// eagerly create the tenant or deploy an endpoint here: compute is a separate
	// resource (POST .../endpoints).
	def := p.DefaultBranch
	if c.prov.SC != nil {
		if err := c.prov.SC.RecordPendingTimelineInit(ctx,
			hex32(def.ProjectID), hex32(def.BranchID),
			storagecontroller.PendingTimelineInit{
				BranchName:         def.Name,
				PgVersion:          pgVersion,
				AncestorTimelineID: nil, // default (Bootstrap) branch
				StartLSN:           "0/0",
			}); err != nil {
			return nil, fmt.Errorf("storage controller record pending timeline init: %w", err)
		}
	} else {
		c.logger.LogAttrs(ctx, slog.LevelWarn, "storage controller not available; recipe recorded in the control DB only")
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "project created",
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String(applog.KeyProjectID, p.ID.String()),
		slog.String(applog.KeyBranchID, def.BranchID.String()),
		slog.String("project_name", p.Name),
		slog.Bool("recipe_recorded", c.prov.SC != nil))

	return CreateProject201JSONResponse(projectToAPI(p)), nil
}

// hex32 renders a UUID as the storage layer's 32-lowercase-hex TenantId/TimelineId.
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
	c.logger.LogAttrs(ctx, slog.LevelInfo, "delete project requested (logged only; task framework pending)",
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String(applog.KeyProjectID, request.ProjectId.String()))
	return DeleteProject202JSONResponse(loggedAck("delete project")), nil
}

// --- Branches --------------------------------------------------------------

// CreateBranch forks an existing branch (the parent) in the project into a new
// branch. The parent defaults to the project's default branch; pass
// parent_branch_id to fork off another branch (branch-of-branch). Like
// create-project it records a recipe — the control-plane row plus the storage
// controller's pending-init, this time with the ancestor and fork LSN set — and
// materializes lazily at the first endpoint deployed on the branch
// (controller-project-create.md). #113 moves the recipe writes onto the
// write-ahead task framework.
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
	forkLSN, errResp := c.resolveForkLSN(ctx, parent, request.Body.ForkPointLsn)
	if errResp != nil {
		return errResp, nil
	}

	br, err := c.backend.CreateBranch(ctx, request.PoolId, request.ProjectId, parentBranchID, name, forkLSN)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return CreateBranch404JSONResponse{NotFoundJSONResponse(apiError("not_found", "parent branch not found"))}, nil
	case errors.Is(err, core.ErrConflict):
		return CreateBranch409JSONResponse{ConflictJSONResponse(apiError("conflict", "a branch with that name already exists in the project"))}, nil
	case err != nil:
		return nil, fmt.Errorf("create branch: %w", err)
	}

	// Record the branch recipe with the storage controller: same project, the
	// ancestor (the parent) and the fork LSN set. Synchronous for now (#113).
	if c.prov.SC != nil {
		ancestor := hex32(parent.BranchID)
		if err := c.prov.SC.RecordPendingTimelineInit(ctx,
			hex32(br.ProjectID), hex32(br.BranchID),
			storagecontroller.PendingTimelineInit{
				BranchName:         br.Name,
				PgVersion:          parent.PgVersion, // a branch inherits the ancestor's version
				AncestorTimelineID: &ancestor,
				StartLSN:           forkLSN,
			}); err != nil {
			return nil, fmt.Errorf("storage controller record pending timeline init: %w", err)
		}
	} else {
		c.logger.LogAttrs(ctx, slog.LevelWarn, "storage controller not available; branch recipe recorded in the control DB only")
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "branch created",
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String(applog.KeyProjectID, br.ProjectID.String()),
		slog.String("parent_branch_id", parent.BranchID.String()),
		slog.String(applog.KeyBranchID, br.BranchID.String()),
		slog.String("branch_name", br.Name),
		slog.String("fork_lsn", forkLSN),
		slog.Bool("recipe_recorded", c.prov.SC != nil))

	return CreateBranch201JSONResponse(branchToAPI(br)), nil
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
	c.logger.LogAttrs(ctx, slog.LevelInfo, "delete branch requested (logged only; task framework pending)",
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String(applog.KeyProjectID, request.ProjectId.String()),
		slog.String(applog.KeyBranchID, request.BranchId.String()))
	return DeleteBranch202JSONResponse(loggedAck("delete branch")), nil
}

// lsnRE matches the storage layer's "X/X" LSN form, e.g. "0/16ABCD0". Each half
// is a 32-bit value, so it is capped at 8 hex digits: a longer group is not a
// valid pg_lsn and would otherwise pass this gate only to fail (500) at the
// `$5::pg_lsn` cast. Branch fork points are validated against it before they
// reach the cast or the storage controller.
var lsnRE = regexp.MustCompile(`^[0-9a-fA-F]{1,8}/[0-9a-fA-F]{1,8}$`)

// resolveForkLSN determines the LSN a branch forks at, or a 400 response to
// return as-is. An explicit, well-formed fork_point_lsn wins. Otherwise it
// defaults to the parent's latest LSN, read from the storage controller. Without
// a storage controller there is no way to resolve a default (and no recipe to
// materialize the branch), so an explicit fork_point_lsn is required rather than
// silently forking at 0/0.
//
// NOTE: the default is the pageserver's last_record_lsn, the ingested position,
// which can lag the safekeeper's durable flush_lsn that yb_light_local branches
// at; the controller does not expose flush_lsn, so an exact point needs
// fork_point_lsn (#130).
func (c *Controllers) resolveForkLSN(ctx context.Context, parent core.Branch, requested *string) (string, CreateBranchResponseObject) {
	if requested != nil {
		if lsn := strings.TrimSpace(*requested); lsn != "" {
			if !lsnRE.MatchString(lsn) {
				return "", CreateBranch400JSONResponse{BadRequestJSONResponse(apiError("invalid_request",
					"fork_point_lsn must be an LSN of the form X/X (hex), e.g. 0/16ABCD0"))}
			}
			return lsn, nil
		}
	}
	if c.prov.SC == nil {
		return "", CreateBranch400JSONResponse{BadRequestJSONResponse(apiError("invalid_request",
			"fork_point_lsn is required when no storage controller is configured"))}
	}
	lsn, err := c.prov.SC.TimelineLatestLSN(ctx, hex32(parent.ProjectID), hex32(parent.BranchID))
	if err != nil {
		// No usable LSN: the parent has not been materialized (no endpoint ever
		// deployed on it) or the controller is unreachable — there is nothing to
		// branch from. Steer the caller rather than branching at a wrong point.
		c.logger.LogAttrs(ctx, slog.LevelWarn, "could not resolve default fork point",
			slog.String(applog.KeyBranchID, parent.BranchID.String()), slog.String("error", err.Error()))
		return "", CreateBranch400JSONResponse{BadRequestJSONResponse(apiError("invalid_request",
			"cannot determine a fork point for the parent branch (deploy an endpoint on it first, or pass fork_point_lsn)"))}
	}
	return lsn, nil
}

// --- Endpoints (synchronously deployed, #108) ------------------------------

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

	br, err := c.backend.GetBranch(ctx, request.PoolId, request.ProjectId, request.BranchId)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return CreateEndpoint404JSONResponse{NotFoundJSONResponse(apiError("not_found", "branch not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get branch: %w", err)
	}

	// config records the spec used to (re)create the endpoint (the endpoint's
	// own id is the row id, so it is not part of the recreate spec).
	config, err := json.Marshal(map[string]any{
		"project_id": br.ProjectID,
		"branch_id":  br.BranchID,
		"role":       role,
		"pg_version": br.PgVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("encode endpoint config: %w", err)
	}

	// Record the row first: it enforces the single-writer rule (409) before we
	// spend a deploy, and gives the deploy a stable endpoint id. The address is
	// filled in after the deploy reports ready.
	ep, err := c.backend.CreateEndpoint(ctx, br.ProjectID, br.BranchID, role, config)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return CreateEndpoint404JSONResponse{NotFoundJSONResponse(apiError("not_found", "branch not found"))}, nil
	case errors.Is(err, core.ErrConflict):
		return CreateEndpoint409JSONResponse{ConflictJSONResponse(apiError("conflict", "a read_write endpoint already exists for this branch"))}, nil
	case err != nil:
		return nil, fmt.Errorf("create endpoint: %w", err)
	}

	// Deploy synchronously. The deployer launches the endpoint, which (in SC mode,
	// with bootstrap) materializes the branch and registers/attaches to the
	// controller, then returns the address clients connect to. On failure, remove
	// the row we just committed so it doesn't linger with nothing deployed — a
	// background context so cleanup runs even if the request ctx was cancelled.
	// The instance name is keyed by the endpoint id (unique per endpoint), since a
	// branch can carry more than one endpoint (a writer plus read replicas). The
	// deploy spec speaks the storage layer's tenant/timeline ids, which are the
	// project/branch ids verbatim.
	spec := deploy.EndpointSpec{
		Name:                 endpointDeployName(ep.ID),
		TenantID:             hex32(br.ProjectID),
		TimelineID:           hex32(br.BranchID),
		PgVersion:            br.PgVersion,
		YB:                   c.prov.EndpointYB,
		StorageControllerURL: c.prov.SCEndpointURL,
		Bootstrap:            c.prov.Bootstrap,
	}
	// A branch must clone from its ancestor at the fork LSN, not initdb a fresh
	// timeline — otherwise it comes up empty. The deployer relays the ancestor to
	// the endpoint, whose pageserver writes a Branch init recipe (lineage reads
	// serve the pre-fork pages from shared storage). Bootstrap is forced off.
	if br.ParentBranchID != nil {
		spec.AncestorTimelineID = hex32(*br.ParentBranchID)
		spec.AncestorLSN = br.ForkLSN
		spec.Bootstrap = false
	}
	deployed, err := c.prov.Deployer.CreateEndpoint(ctx, spec)
	if err != nil {
		if derr := c.backend.DeleteEndpoint(context.Background(), ep.ID); derr != nil {
			c.logger.LogAttrs(ctx, slog.LevelError, "failed to remove endpoint after deploy failure",
				slog.String(applog.KeyEndpointID, ep.ID.String()), slog.String("error", derr.Error()))
		}
		return nil, fmt.Errorf("deploy endpoint: %w", err)
	}
	// The deploy succeeded — the endpoint is live, so finish recording it on a
	// context a cancelled request can't abort. Otherwise a client disconnect
	// between the deploy and the address write would orphan the running endpoint
	// (the deploy-failure branch above compensates; this step must too). If the
	// address genuinely can't be recorded, retire the just-deployed endpoint so a
	// retry isn't blocked by a half-written row.
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := c.backend.SetEndpointAddress(recCtx, ep.ID, deployed.Address); err != nil {
		if derr := c.prov.Deployer.DeleteEndpoint(recCtx, endpointDeployName(ep.ID)); derr != nil {
			c.logger.LogAttrs(recCtx, slog.LevelError, "failed to retire endpoint after address-record failure",
				slog.String(applog.KeyEndpointID, ep.ID.String()), slog.String("error", derr.Error()))
		}
		if derr := c.backend.DeleteEndpoint(recCtx, ep.ID); derr != nil {
			c.logger.LogAttrs(recCtx, slog.LevelError, "failed to remove endpoint row after address-record failure",
				slog.String(applog.KeyEndpointID, ep.ID.String()), slog.String("error", derr.Error()))
		}
		return nil, fmt.Errorf("record endpoint address: %w", err)
	}
	ep.Address = deployed.Address

	// The endpoint has materialized the branch (its supervisor ran initdb and
	// registered it with the storage controller), so drop the pending-init recipe.
	// On recCtx so a cancelled request still consumes it. Best-effort and
	// idempotent: a no-op for a branch an earlier endpoint already materialized,
	// and a lingering recipe is harmless (nothing reads it).
	if c.prov.SC != nil {
		if err := c.prov.SC.DeletePendingTimelineInit(recCtx, hex32(br.ProjectID), hex32(br.BranchID)); err != nil {
			c.logger.LogAttrs(recCtx, slog.LevelWarn, "failed to delete pending timeline init (continuing)",
				slog.String(applog.KeyEndpointID, ep.ID.String()), slog.String("error", err.Error()))
		}
	}

	c.logger.LogAttrs(recCtx, slog.LevelInfo, "endpoint created and deployed",
		slog.String(applog.KeyProjectID, br.ProjectID.String()),
		slog.String(applog.KeyBranchID, request.BranchId.String()),
		slog.String(applog.KeyEndpointID, ep.ID.String()),
		slog.String("address", deployed.Address))
	return CreateEndpoint201JSONResponse(endpointToAPI(ep)), nil
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
	ep, err := c.backend.GetEndpoint(ctx, request.PoolId, request.ProjectId, request.BranchId, request.EndpointId)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return DeleteEndpoint404JSONResponse{NotFoundJSONResponse(apiError("not_found", "endpoint not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get endpoint: %w", err)
	}
	// Retire synchronously: remove the running instance, then drop the record.
	// Both are idempotent, so a retried request converges. The deployer keys the
	// instance by the endpoint name (= the spec.Name CreateEndpoint launched).
	if err := c.prov.Deployer.DeleteEndpoint(ctx, endpointDeployName(ep.ID)); err != nil {
		return nil, fmt.Errorf("retire endpoint: %w", err)
	}
	if err := c.backend.DeleteEndpoint(ctx, ep.ID); err != nil {
		return nil, fmt.Errorf("delete endpoint: %w", err)
	}
	c.logger.LogAttrs(ctx, slog.LevelInfo, "endpoint retired",
		slog.String(applog.KeyEndpointID, ep.ID.String()))
	return DeleteEndpoint204Response{}, nil
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

// --- mapping helpers -------------------------------------------------------

func loggedAck(what string) OperationAck {
	return OperationAck{
		Acknowledged: true,
		Detail:       what + " logged; execution is pending the task framework",
	}
}

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
