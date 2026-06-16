package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/yugabyte/yb-light/amp/internal/core"
	applog "github.com/yugabyte/yb-light/amp/internal/log"
	"github.com/yugabyte/yb-light/amp/internal/task"
)

// Controllers implements the generated StrictServerInterface: the controller's
// HTTP behavior, decoupled from the wire format (which the generated code owns)
// and from persistence (which the core.Backend owns). Mutations that the design
// routes through the task framework (timelines, §6) are logged-only for now —
// issue #92: "the 3 timeline calls should just log for now, no task framework".
type Controllers struct {
	backend core.Backend
	tasks   task.Store
	logger  *slog.Logger
}

// NewControllers wires the controllers to a backend, the task store (for task
// status reads), and the root logger.
func NewControllers(backend core.Backend, tasks task.Store, logger *slog.Logger) *Controllers {
	return &Controllers{backend: backend, tasks: tasks, logger: logger}
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

// --- Timelines (logged-only until the task framework lands, #92) -----------

func (c *Controllers) CreateTimeline(ctx context.Context, request CreateTimelineRequestObject) (CreateTimelineResponseObject, error) {
	if request.Body == nil {
		return CreateTimeline400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "request body is required"))}, nil
	}
	name := strings.TrimSpace(request.Body.Name)
	if name == "" {
		return CreateTimeline400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "name is required"))}, nil
	}
	attrs := []slog.Attr{
		slog.String(applog.KeyUserID, request.UserId.String()),
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String("timeline_name", name),
	}
	if request.Body.PgVersion != nil {
		attrs = append(attrs, slog.Int("pg_version", *request.Body.PgVersion))
	}
	c.logger.LogAttrs(ctx, slog.LevelInfo, "create timeline requested (logged only; task framework pending)", attrs...)
	return CreateTimeline202JSONResponse(loggedAck("create timeline")), nil
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
