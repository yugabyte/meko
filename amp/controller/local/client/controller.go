package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/registry"
)

// Controller is the launcher's client for the controller's storage-layer
// control API (the routes registry.Register mounts). It is the Go port of the
// reqwest `Controller::dispatch` client in control_plane/src/controller.rs.
//
// Every route is project-scoped: the branch and endpoint rows it addresses are
// the control plane's own, shared with the product API, and those belong to a
// project. The launcher provisions one at startup (see launch.EnsureLadder) and
// binds it into this client, so callers pass branch ids and endpoint handles just
// as they did when this drove a separate flat registry.
//
// The base URL is the controller's listen addr (host:port). In the Rust launcher
// the configured URL carries a `/upcall` prefix that dispatch strips; here the
// caller passes the already-stripped base. Auth is Trust-only (--dev): there is
// no JWT, matching the launcher's local topology.
type Controller struct {
	t         transport
	projectID uuid.UUID
}

// NewController builds a controller client bound to projectID. base is the
// controller's listen address, either a bare host:port or a full http(s) URL. A
// nil hc uses http.DefaultClient.
func NewController(base string, projectID uuid.UUID, hc *http.Client) *Controller {
	return &Controller{t: newTransport(base, hc, ""), projectID: projectID}
}

// Ready hits GET /ready, the readiness probe used to gate startup. It is the one
// route that is not project-scoped, so it works before the ladder exists.
func (c *Controller) Ready(ctx context.Context) error {
	return c.t.do(ctx, http.MethodGet, "/ready", nil, nil)
}

// base is the project-scoped route prefix every other call hangs off.
func (c *Controller) base() string {
	return "/control/v1/projects/" + url.PathEscape(c.projectID.String())
}

// RecordBranch declares a branch and seeds its storage-layer state via POST
// /control/v1/projects/{projectID}/branch/{branchID}/record.
func (c *Controller) RecordBranch(ctx context.Context, branchID string, req registry.BranchCreateRecord) error {
	return c.t.do(ctx, http.MethodPost, c.base()+"/branch/"+url.PathEscape(branchID)+"/record", req, nil)
}

// ListBranches returns every live branch in the project via GET
// /control/v1/projects/{projectID}/branches.
func (c *Controller) ListBranches(ctx context.Context) ([]registry.BranchRecord, error) {
	var out []registry.BranchRecord
	err := c.t.do(ctx, http.MethodGet, c.base()+"/branches", nil, &out)
	return out, err
}

// SetBranchName renames a branch via PUT
// /control/v1/projects/{projectID}/branch/{branchID}/branch_name.
func (c *Controller) SetBranchName(ctx context.Context, branchID string, req registry.SetBranchNameRequest) error {
	return c.t.do(ctx, http.MethodPut, c.base()+"/branch/"+url.PathEscape(branchID)+"/branch_name", req, nil)
}

// ListBranchNames returns all branch-id -> name mappings via GET
// /control/v1/projects/{projectID}/timeline_branch_names.
func (c *Controller) ListBranchNames(ctx context.Context) ([]registry.BranchNameEntry, error) {
	var out []registry.BranchNameEntry
	err := c.t.do(ctx, http.MethodGet, c.base()+"/timeline_branch_names", nil, &out)
	return out, err
}

// EndpointCreate persists a compute-endpoint record via POST
// /control/v1/projects/{projectID}/endpoint.
func (c *Controller) EndpointCreate(ctx context.Context, req registry.ComputeEndpoint) error {
	return c.t.do(ctx, http.MethodPost, c.base()+"/endpoint", req, nil)
}

// EndpointList returns all persisted compute-endpoint records via GET
// /control/v1/projects/{projectID}/endpoint.
func (c *Controller) EndpointList(ctx context.Context) ([]registry.ComputeEndpoint, error) {
	var out []registry.ComputeEndpoint
	err := c.t.do(ctx, http.MethodGet, c.base()+"/endpoint", nil, &out)
	return out, err
}

// EndpointDelete removes a persisted compute-endpoint record via DELETE
// /control/v1/projects/{projectID}/endpoint/{endpointID}.
func (c *Controller) EndpointDelete(ctx context.Context, endpointID string) error {
	return c.t.do(ctx, http.MethodDelete, c.base()+"/endpoint/"+url.PathEscape(endpointID), nil, nil)
}

// BranchDrop tombstones a branch (PUT
// /control/v1/projects/{projectID}/branch/{id}/drop). Idempotent.
func (c *Controller) BranchDrop(ctx context.Context, branchID string) error {
	return c.t.do(ctx, http.MethodPut, c.base()+"/branch/"+url.PathEscape(branchID)+"/drop", nil, nil)
}
