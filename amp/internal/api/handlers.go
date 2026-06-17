package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/yugabyte/yb-light/amp/internal/core"
	"github.com/yugabyte/yb-light/amp/internal/deploy"
	applog "github.com/yugabyte/yb-light/amp/internal/log"
	"github.com/yugabyte/yb-light/amp/internal/storagecontroller"
	"github.com/yugabyte/yb-light/amp/internal/task"
)

// Provisioning holds the collaborators the create-timeline workflow uses beyond
// the state store: the storage controller (to register the tenant) and the
// endpoint deployer (to provision the compute). Both are optional — with neither,
// create-timeline records the tenant + timeline only. SCEndpointURL is the
// controller URL as a *launched endpoint* reaches it (e.g. over the shared
// network); empty means the endpoint runs SC-free.
type Provisioning struct {
	SC            *storagecontroller.Client
	Deployer      deploy.Deployer
	EndpointYB    deploy.YBConn
	SCEndpointURL string
}

// Controllers implements the generated StrictServerInterface: the controller's
// HTTP behavior, decoupled from the wire format (which the generated code owns)
// and from persistence (which the core.Backend owns). Mutations that the design
// routes through the task framework (timelines, §6) are logged-only for now —
// issue #92: "the 3 timeline calls should just log for now, no task framework".
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

// --- Storage pools ---------------------------------------------------------

func (c *Controllers) CreateStoragePool(ctx context.Context, request CreateStoragePoolRequestObject) (CreateStoragePoolResponseObject, error) {
	if request.Body == nil {
		return CreateStoragePool400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "request body is required"))}, nil
	}
	name := strings.TrimSpace(request.Body.Name)
	if name == "" {
		return CreateStoragePool400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "name is required"))}, nil
	}
	p, err := c.backend.CreateStoragePool(ctx, request.UserId, name)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return CreateStoragePool404JSONResponse{NotFoundJSONResponse(apiError("not_found", "user not found"))}, nil
	case errors.Is(err, core.ErrConflict):
		return CreateStoragePool409JSONResponse{ConflictJSONResponse(apiError("conflict", "a storage pool with that name already exists"))}, nil
	case err != nil:
		return nil, fmt.Errorf("create storage pool: %w", err)
	}
	c.logger.LogAttrs(ctx, slog.LevelInfo, "storage pool created",
		slog.String(applog.KeyUserID, request.UserId.String()),
		slog.String(applog.KeyStoragePoolID, p.ID.String()))
	return CreateStoragePool201JSONResponse(poolToAPI(p)), nil
}

func (c *Controllers) ListStoragePools(ctx context.Context, request ListStoragePoolsRequestObject) (ListStoragePoolsResponseObject, error) {
	pools, err := c.backend.ListStoragePools(ctx, request.UserId)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return ListStoragePools404JSONResponse{NotFoundJSONResponse(apiError("not_found", "user not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("list storage pools: %w", err)
	}
	out := make([]StoragePool, 0, len(pools))
	for _, p := range pools {
		out = append(out, poolToAPI(p))
	}
	return ListStoragePools200JSONResponse(StoragePoolList{StoragePools: out}), nil
}

func (c *Controllers) GetStoragePool(ctx context.Context, request GetStoragePoolRequestObject) (GetStoragePoolResponseObject, error) {
	p, err := c.backend.GetStoragePool(ctx, request.UserId, request.PoolId)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return GetStoragePool404JSONResponse{NotFoundJSONResponse(apiError("not_found", "storage pool not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get storage pool: %w", err)
	}
	return GetStoragePool200JSONResponse(poolToAPI(p)), nil
}

// --- Timelines -------------------------------------------------------------
// Create provisions a tenant + its main timeline. Delete and branch remain
// logged-only until the task framework lands (#92).

// defaultPgVersion is used when a create-timeline request omits pg_version.
// yb-light is Postgres v17 only.
const defaultPgVersion = 17

func (c *Controllers) CreateTimeline(ctx context.Context, request CreateTimelineRequestObject) (CreateTimelineResponseObject, error) {
	if request.Body == nil {
		return CreateTimeline400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "request body is required"))}, nil
	}
	name := strings.TrimSpace(request.Body.Name)
	if name == "" {
		return CreateTimeline400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "name is required"))}, nil
	}
	pgVersion := defaultPgVersion
	if request.Body.PgVersion != nil {
		pgVersion = *request.Body.PgVersion
	}
	// yb-light is Postgres v17 only; reject anything else up front rather than
	// failing opaquely when the endpoint image (v17) can't honor it.
	if pgVersion != defaultPgVersion {
		return CreateTimeline400JSONResponse{BadRequestJSONResponse(apiError("invalid_request",
			fmt.Sprintf("unsupported pg_version %d (only %d is supported)", pgVersion, defaultPgVersion)))}, nil
	}
	tl, err := c.backend.CreateTimeline(ctx, request.UserId, request.PoolId, name, pgVersion)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return CreateTimeline404JSONResponse{NotFoundJSONResponse(apiError("not_found", "storage pool not found"))}, nil
	case errors.Is(err, core.ErrConflict):
		return CreateTimeline409JSONResponse{ConflictJSONResponse(apiError("conflict", "a timeline with that name already exists in the pool"))}, nil
	case err != nil:
		return nil, fmt.Errorf("create timeline: %w", err)
	}
	tenantHex := hex32(tl.TenantID)
	timelineHex := hex32(tl.TimelineID)
	c.logger.LogAttrs(ctx, slog.LevelInfo, "timeline created",
		slog.String(applog.KeyUserID, request.UserId.String()),
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String(applog.KeyTenantID, tl.TenantID.String()),
		slog.String(applog.KeyTimelineID, tl.TimelineID.String()),
		slog.String("timeline_name", tl.Name))

	// TODO(#106): this workflow is synchronous and non-atomic across subsystems —
	// CreateTimeline has already committed, so a failure in the storage-controller
	// or endpoint steps below orphans the timeline record (a same-name retry then
	// 409s). Move onto the write-ahead task framework (durable/resumable), or add a
	// compensation block that deletes the committed tenant/timeline on failure.

	// Register the tenant with the storage controller (the controller is the
	// tenant/node registry + generation authority; the endpoint attaches to it).
	if c.prov.SC != nil {
		if err := c.prov.SC.CreateTenant(ctx, tenantHex); err != nil {
			return nil, fmt.Errorf("register tenant %s with storage controller: %w", tenantHex, err)
		}
		c.logger.LogAttrs(ctx, slog.LevelInfo, "tenant registered with storage controller",
			slog.String(applog.KeyTenantID, tl.TenantID.String()))
	}

	out := timelineToAPI(tl)

	// Provision the compute endpoint for the timeline (the data plane), and
	// record it. The deployer launches the endpoint, which materializes the
	// timeline (bootstrap) and — in SC mode — registers + attaches to the
	// controller. The returned address is what a client connects to.
	if c.prov.Deployer != nil {
		ep, err := c.prov.Deployer.CreateEndpoint(ctx, deploy.EndpointSpec{
			Name:                 "yb-ep-" + timelineHex,
			TenantID:             tenantHex,
			TimelineID:           timelineHex,
			PgVersion:            pgVersion,
			YB:                   c.prov.EndpointYB,
			StorageControllerURL: c.prov.SCEndpointURL,
			Bootstrap:            true,
		})
		if err != nil {
			return nil, fmt.Errorf("create endpoint: %w", err)
		}
		rec, err := c.backend.CreateEndpoint(ctx, tl.TenantID, tl.TimelineID, "read_write", ep.Address)
		if err != nil {
			// The container is up but unrecorded; tear it down so it (and its
			// published port) don't leak with nothing pointing at it. Background
			// context so cleanup runs even if the request ctx was cancelled.
			if derr := c.prov.Deployer.DeleteEndpoint(context.Background(), ep.ID); derr != nil {
				c.logger.LogAttrs(ctx, slog.LevelError, "failed to remove endpoint after record failure",
					slog.String(applog.KeyEndpointID, ep.ID), slog.String("error", derr.Error()))
			}
			return nil, fmt.Errorf("record endpoint: %w", err)
		}
		out.Endpoint = &Endpoint{Id: rec.ID, Role: ReadWrite, Address: rec.Address}
		c.logger.LogAttrs(ctx, slog.LevelInfo, "endpoint created",
			slog.String(applog.KeyTenantID, tl.TenantID.String()),
			slog.String(applog.KeyTimelineID, tl.TimelineID.String()),
			slog.String(applog.KeyEndpointID, rec.ID.String()),
			slog.String("address", rec.Address))
	}
	return CreateTimeline201JSONResponse(out), nil
}

// hex32 renders a UUID as the storage layer's 32-lowercase-hex TenantId/TimelineId.
func hex32(id uuid.UUID) string {
	return hex.EncodeToString(id[:])
}

func (c *Controllers) DeleteTimeline(ctx context.Context, request DeleteTimelineRequestObject) (DeleteTimelineResponseObject, error) {
	c.logger.LogAttrs(ctx, slog.LevelInfo, "delete timeline requested (logged only; task framework pending)",
		slog.String(applog.KeyUserID, request.UserId.String()),
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String(applog.KeyTimelineID, request.TimelineId.String()))
	return DeleteTimeline202JSONResponse(loggedAck("delete timeline")), nil
}

func (c *Controllers) BranchTimeline(ctx context.Context, request BranchTimelineRequestObject) (BranchTimelineResponseObject, error) {
	if request.Body == nil {
		return BranchTimeline400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "request body is required"))}, nil
	}
	name := strings.TrimSpace(request.Body.Name)
	if name == "" {
		return BranchTimeline400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "name is required"))}, nil
	}
	c.logger.LogAttrs(ctx, slog.LevelInfo, "branch timeline requested (logged only; task framework pending)",
		slog.String(applog.KeyUserID, request.UserId.String()),
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String(applog.KeyTimelineID, request.TimelineId.String()), // the parent
		slog.String("branch_name", name))
	return BranchTimeline202JSONResponse(loggedAck("branch timeline")), nil
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

func timelineToAPI(t core.Timeline) Timeline {
	return Timeline{
		TenantId:   t.TenantID,
		TimelineId: t.TimelineID,
		Name:       t.Name,
		PgVersion:  t.PgVersion,
		IsMain:     t.IsMain,
		CreatedAt:  t.CreatedAt,
	}
}

func taskToAPI(t *task.Task) Task {
	out := Task{
		Id:        t.ID,
		Kind:      t.Kind,
		Status:    TaskStatus(t.Status),
		TenantId:  t.TenantID,
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
