package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/yugabyte/yb-light/amp/internal/core"
	"github.com/yugabyte/yb-light/amp/internal/deploy"
	applog "github.com/yugabyte/yb-light/amp/internal/log"
	"github.com/yugabyte/yb-light/amp/internal/storagecontroller"
	"github.com/yugabyte/yb-light/amp/internal/task"
)

// Provisioning holds the collaborators the create-timeline / create-endpoint
// workflows use beyond the state store: the storage controller (to record the
// pending-init recipe and, once an endpoint materializes the timeline, drop it)
// and the endpoint deployer (to provision the compute). Both are optional —
// with no deployer the endpoint mutation routes refuse with 501, and with no SC
// create-timeline records the control-plane rows only. SCEndpointURL is the
// controller URL as a *launched endpoint* reaches it (e.g. over the shared
// network); empty means the endpoint runs SC-free. Bootstrap forces initdb of
// the recorded timeline on the first endpoint's deploy.
type Provisioning struct {
	SC            *storagecontroller.Client
	Deployer      deploy.Deployer
	EndpointYB    deploy.YBConn
	SCEndpointURL string
	Bootstrap     bool
}

// Controllers implements the generated StrictServerInterface: the controller's
// HTTP behavior, decoupled from the wire format (which the generated code owns)
// and from persistence (which the core.Backend owns). Timeline create records
// the tenant + main timeline and the storage controller's pending-init recipe;
// the timeline is materialized later by the first endpoint deployed on it (#108).
// Timeline delete/branch route through the task framework eventually (§6) and
// are logged-only for now — issue #92: "the 3 timeline calls should just log for
// now, no task framework".
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

// --- Timelines -------------------------------------------------------------
// Create records a tenant + its main timeline plus the storage controller's
// pending-init recipe; the timeline is materialized later by the first endpoint
// deployed on it. Delete and branch remain logged-only until the task framework
// lands (#92).

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
	tl, err := c.backend.CreateTimeline(ctx, request.PoolId, name, pgVersion)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return CreateTimeline404JSONResponse{NotFoundJSONResponse(apiError("not_found", "storage pool not found"))}, nil
	case errors.Is(err, core.ErrConflict):
		return CreateTimeline409JSONResponse{ConflictJSONResponse(apiError("conflict", "a timeline with that name already exists in the pool"))}, nil
	case err != nil:
		return nil, fmt.Errorf("create timeline: %w", err)
	}

	// Record the pending-init recipe with the storage controller. The timeline is
	// declared here, not yet materialized: the first endpoint deployed for it
	// attaches a pageserver and bootstraps it (amp-timeline-create.md). The recipe
	// lives in both the control DB (the pg_cluster row) and the storage controller.
	// Storage-layer ids are 32-hex (no dashes); the control plane stores the same
	// bytes as a SQL uuid. Synchronous for now — #113 moves this onto the
	// write-ahead task framework. We do NOT eagerly create the tenant or deploy an
	// endpoint here: compute is a separate resource (POST .../endpoints).
	if c.prov.SC != nil {
		if err := c.prov.SC.RecordPendingTimelineInit(ctx,
			hex32(tl.TenantID), hex32(tl.TimelineID),
			storagecontroller.PendingTimelineInit{
				BranchName:         tl.Name,
				PgVersion:          pgVersion,
				AncestorTimelineID: nil, // root (Bootstrap) timeline
				StartLSN:           "0/0",
			}); err != nil {
			return nil, fmt.Errorf("storage controller record pending timeline init: %w", err)
		}
	} else {
		c.logger.LogAttrs(ctx, slog.LevelWarn, "storage controller not available; recipe recorded in the control DB only")
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "timeline created",
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String(applog.KeyTenantID, tl.TenantID.String()),
		slog.String(applog.KeyTimelineID, tl.TimelineID.String()),
		slog.String("timeline_name", tl.Name),
		slog.Bool("recipe_recorded", c.prov.SC != nil))

	return CreateTimeline201JSONResponse(timelineToAPI(tl)), nil
}

// hex32 renders a UUID as the storage layer's 32-lowercase-hex TenantId/TimelineId.
func hex32(id uuid.UUID) string {
	return hex.EncodeToString(id[:])
}

// endpointDeployName is the deployer's stable instance handle for an endpoint
// (the container/pod/Helm-release name). It is keyed by the endpoint id so each
// endpoint on a timeline (a writer plus read replicas) gets a distinct
// instance. "yb-ep-" + a lowercase-hyphen uuid is a valid DNS-1123 name (42
// chars, under Helm's 53-char limit). CreateEndpoint and DeleteEndpoint must
// agree on it so a retire targets the right instance.
func endpointDeployName(endpointID uuid.UUID) string {
	return "yb-ep-" + endpointID.String()
}

func (c *Controllers) ListTimelines(ctx context.Context, request ListTimelinesRequestObject) (ListTimelinesResponseObject, error) {
	limit, offset := pageParams(request.Params.Limit, request.Params.Offset)
	timelines, total, err := c.backend.ListTimelines(ctx, request.PoolId, limit, offset)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return ListTimelines404JSONResponse{NotFoundJSONResponse(apiError("not_found", "storage pool not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("list timelines: %w", err)
	}
	out := make([]Timeline, 0, len(timelines))
	for _, t := range timelines {
		out = append(out, timelineToAPI(t))
	}
	return ListTimelines200JSONResponse(TimelineList{Timelines: out, Total: total, Count: len(out)}), nil
}

func (c *Controllers) DeleteTimeline(ctx context.Context, request DeleteTimelineRequestObject) (DeleteTimelineResponseObject, error) {
	c.logger.LogAttrs(ctx, slog.LevelInfo, "delete timeline requested (logged only; task framework pending)",
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
		slog.String(applog.KeyStoragePoolID, request.PoolId.String()),
		slog.String(applog.KeyTimelineID, request.TimelineId.String()), // the parent
		slog.String("branch_name", name))
	return BranchTimeline202JSONResponse(loggedAck("branch timeline")), nil
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

	tl, err := c.backend.GetTimeline(ctx, request.PoolId, request.TimelineId)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return CreateEndpoint404JSONResponse{NotFoundJSONResponse(apiError("not_found", "timeline not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get timeline: %w", err)
	}

	// config records the spec used to (re)create the endpoint (the endpoint's
	// own id is the row id, so it is not part of the recreate spec).
	config, err := json.Marshal(map[string]any{
		"tenant_id":   tl.TenantID,
		"timeline_id": tl.TimelineID,
		"role":        role,
		"pg_version":  tl.PgVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("encode endpoint config: %w", err)
	}

	// Record the row first: it enforces the single-writer rule (409) before we
	// spend a deploy, and gives the deploy a stable endpoint id. The address is
	// filled in after the deploy reports ready.
	ep, err := c.backend.CreateEndpoint(ctx, tl.TenantID, tl.TimelineID, role, config)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return CreateEndpoint404JSONResponse{NotFoundJSONResponse(apiError("not_found", "timeline not found"))}, nil
	case errors.Is(err, core.ErrConflict):
		return CreateEndpoint409JSONResponse{ConflictJSONResponse(apiError("conflict", "a read_write endpoint already exists for this timeline"))}, nil
	case err != nil:
		return nil, fmt.Errorf("create endpoint: %w", err)
	}

	// Deploy synchronously. The deployer launches the endpoint, which (in SC mode,
	// with bootstrap) materializes the timeline and registers/attaches to the
	// controller, then returns the address clients connect to. On failure, remove
	// the row we just committed so it doesn't linger with nothing deployed — a
	// background context so cleanup runs even if the request ctx was cancelled.
	// The instance name is keyed by the endpoint id (unique per endpoint), since a
	// timeline can carry more than one endpoint (a writer plus read replicas).
	deployed, err := c.prov.Deployer.CreateEndpoint(ctx, deploy.EndpointSpec{
		Name:                 endpointDeployName(ep.ID),
		TenantID:             hex32(tl.TenantID),
		TimelineID:           hex32(tl.TimelineID),
		PgVersion:            tl.PgVersion,
		YB:                   c.prov.EndpointYB,
		StorageControllerURL: c.prov.SCEndpointURL,
		Bootstrap:            c.prov.Bootstrap,
	})
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

	// The endpoint has materialized the timeline (its supervisor ran initdb and
	// registered it with the storage controller), so drop the pending-init recipe.
	// On recCtx so a cancelled request still consumes it. Best-effort and
	// idempotent: a no-op for a timeline an earlier endpoint already materialized,
	// and a lingering recipe is harmless (nothing reads it).
	if c.prov.SC != nil {
		if err := c.prov.SC.DeletePendingTimelineInit(recCtx, hex32(tl.TenantID), hex32(tl.TimelineID)); err != nil {
			c.logger.LogAttrs(recCtx, slog.LevelWarn, "failed to delete pending timeline init (continuing)",
				slog.String(applog.KeyEndpointID, ep.ID.String()), slog.String("error", err.Error()))
		}
	}

	c.logger.LogAttrs(recCtx, slog.LevelInfo, "endpoint created and deployed",
		slog.String(applog.KeyTenantID, tl.TenantID.String()),
		slog.String(applog.KeyTimelineID, request.TimelineId.String()),
		slog.String(applog.KeyEndpointID, ep.ID.String()),
		slog.String("address", deployed.Address))
	return CreateEndpoint201JSONResponse(endpointToAPI(ep)), nil
}

func (c *Controllers) GetEndpoint(ctx context.Context, request GetEndpointRequestObject) (GetEndpointResponseObject, error) {
	ep, err := c.backend.GetEndpoint(ctx, request.PoolId, request.TimelineId, request.EndpointId)
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
	endpoints, total, err := c.backend.ListEndpoints(ctx, request.PoolId, request.TimelineId, limit, offset)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return ListEndpoints404JSONResponse{NotFoundJSONResponse(apiError("not_found", "timeline not found"))}, nil
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
	ep, err := c.backend.GetEndpoint(ctx, request.PoolId, request.TimelineId, request.EndpointId)
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

func endpointToAPI(e core.Endpoint) Endpoint {
	return Endpoint{
		Id:         e.ID,
		TenantId:   e.TenantID,
		TimelineId: e.TimelineID,
		Role:       EndpointRole(e.Role),
		Address:    e.Address,
		CreatedAt:  e.CreatedAt,
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
