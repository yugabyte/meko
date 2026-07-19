package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/yugabyte/amp/controller/internal/registry"
)

// Controller is the launcher's client for the controller's storage-layer
// control API (the routes registry.Register mounts). It is the Go port of the
// reqwest `Controller::dispatch` client in control_plane/src/controller.rs.
//
// The base URL is the controller's listen addr (host:port). In the Rust
// launcher the configured URL carries a `/upcall` prefix that dispatch strips;
// here the caller passes the already-stripped base. Auth is Trust-only
// (--dev): there is no JWT, matching the launcher's local topology.
type Controller struct {
	t transport
}

// NewController builds a controller client. base is the controller's listen
// address, either a bare host:port or a full http(s) URL. A nil hc uses
// http.DefaultClient.
func NewController(base string, hc *http.Client) *Controller {
	return &Controller{t: newTransport(base, hc, "")}
}

// Ready hits GET /ready, the readiness probe used to gate startup.
func (c *Controller) Ready(ctx context.Context) error {
	return c.t.do(ctx, http.MethodGet, "/ready", nil, nil)
}

// AttachHook bumps (or clears) a branch's generation via POST
// /debug/v1/attach-hook.
func (c *Controller) AttachHook(ctx context.Context, req registry.AttachHookRequest) (registry.AttachHookResponse, error) {
	var resp registry.AttachHookResponse
	err := c.t.do(ctx, http.MethodPost, "/debug/v1/attach-hook", req, &resp)
	return resp, err
}

// NodeRegister self-registers a pageserver via POST /control/v1/node.
func (c *Controller) NodeRegister(ctx context.Context, req registry.NodeRegisterRequest) error {
	return c.t.do(ctx, http.MethodPost, "/control/v1/node", req, nil)
}

// NodeList returns the registered nodes via GET /control/v1/node.
func (c *Controller) NodeList(ctx context.Context) ([]registry.NodeDescribeResponse, error) {
	var out []registry.NodeDescribeResponse
	err := c.t.do(ctx, http.MethodGet, "/control/v1/node", nil, &out)
	return out, err
}

// BranchRegister records a launcher-created branch via POST
// /control/v1/branch/{branchID}/register.
func (c *Controller) BranchRegister(ctx context.Context, branchID string, req registry.BranchRegisterRequest) error {
	return c.t.do(ctx, http.MethodPost, "/control/v1/branch/"+url.PathEscape(branchID)+"/register", req, nil)
}

// RecordPendingInit records a pending timeline-init recipe via POST
// /control/v1/branch/{branchID}/pending_init.
func (c *Controller) RecordPendingInit(ctx context.Context, branchID string, req registry.PendingBranchInitRequest) error {
	return c.t.do(ctx, http.MethodPost, "/control/v1/branch/"+url.PathEscape(branchID)+"/pending_init", req, nil)
}

// ListPendingInits returns all pending timeline inits via GET
// /control/v1/pending_timeline_inits.
func (c *Controller) ListPendingInits(ctx context.Context) ([]registry.PendingBranchInit, error) {
	var out []registry.PendingBranchInit
	err := c.t.do(ctx, http.MethodGet, "/control/v1/pending_timeline_inits", nil, &out)
	return out, err
}

// DeletePendingInit discards a materialized init recipe via DELETE
// /control/v1/branch/{branchID}/pending_init.
func (c *Controller) DeletePendingInit(ctx context.Context, branchID string) error {
	return c.t.do(ctx, http.MethodDelete, "/control/v1/branch/"+url.PathEscape(branchID)+"/pending_init", nil, nil)
}

// SetBranchName sets (or clears) a branch's name alias via PUT
// /control/v1/branch/{branchID}/branch_name.
func (c *Controller) SetBranchName(ctx context.Context, branchID string, req registry.SetBranchNameRequest) error {
	return c.t.do(ctx, http.MethodPut, "/control/v1/branch/"+url.PathEscape(branchID)+"/branch_name", req, nil)
}

// ListBranchNames returns all branch-id -> alias mappings via GET
// /control/v1/timeline_branch_names.
func (c *Controller) ListBranchNames(ctx context.Context) ([]registry.BranchNameEntry, error) {
	var out []registry.BranchNameEntry
	err := c.t.do(ctx, http.MethodGet, "/control/v1/timeline_branch_names", nil, &out)
	return out, err
}

// EndpointCreate persists a compute-endpoint record via POST
// /control/v1/endpoint.
func (c *Controller) EndpointCreate(ctx context.Context, req registry.ComputeEndpoint) error {
	return c.t.do(ctx, http.MethodPost, "/control/v1/endpoint", req, nil)
}

// EndpointList returns all persisted compute-endpoint records via GET
// /control/v1/endpoint.
func (c *Controller) EndpointList(ctx context.Context) ([]registry.ComputeEndpoint, error) {
	var out []registry.ComputeEndpoint
	err := c.t.do(ctx, http.MethodGet, "/control/v1/endpoint", nil, &out)
	return out, err
}

// EndpointDelete removes a persisted compute-endpoint record via DELETE
// /control/v1/endpoint/{endpointID}.
func (c *Controller) EndpointDelete(ctx context.Context, endpointID string) error {
	return c.t.do(ctx, http.MethodDelete, "/control/v1/endpoint/"+url.PathEscape(endpointID), nil, nil)
}

// BranchDrop marks a branch dropped (PUT /control/v1/branch/{id}/drop). Idempotent.
func (c *Controller) BranchDrop(ctx context.Context, branchID string) error {
	return c.t.do(ctx, http.MethodPut, "/control/v1/branch/"+url.PathEscape(branchID)+"/drop", nil, nil)
}
