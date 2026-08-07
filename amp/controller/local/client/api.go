package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
)

// API is the launcher's client for the controller's REST API. Every resource
// the launcher creates — its user and storage pool, its projects, its branches,
// its endpoints — is an ordinary row created through the same routes any other
// client uses, so the integration suite exercises the product's own async task
// path rather than a surface only the launcher touches.
//
// Endpoints work here because the launcher starts its controller with
// `--deploy-driver=local`: the controller keeps the endpoint record and does not
// try to launch a compute for it, which is right, since the launcher spawns
// compute-ctl itself (deploy.LocalDeployer).
//
// Auth is Trust-only: /api/v1 has no auth middleware, matching the launcher's
// local topology.
type API struct {
	t transport
}

// NewAPI builds a product-API client. base is the controller's listen address,
// either a bare host:port or a full http(s) URL. A nil hc uses http.DefaultClient.
func NewAPI(base string, hc *http.Client) *API {
	return &API{t: newTransport(base, hc, "")}
}

// Ready hits GET /readyz: the controller answers once its database is reachable
// and its migrations are applied. It takes no project, so it gates startup
// before the launcher's ladder exists.
func (c *API) Ready(ctx context.Context) error {
	return c.t.do(ctx, http.MethodGet, "/readyz", nil, nil)
}

// EnsureUser creates the user with the given id, or no-ops if it exists.
// POST /api/v1/users is documented idempotent on an explicit id.
func (c *API) EnsureUser(ctx context.Context, id uuid.UUID, isAdmin bool) error {
	body := api.CreateUserRequest{Id: &id, IsAdmin: &isAdmin}
	return c.t.do(ctx, http.MethodPost, "/api/v1/users", body, nil)
}

// EnsureStoragePool creates the pool with the given id and name, or no-ops if it
// exists. POST /api/v1/storage-pools is documented idempotent on an explicit id.
func (c *API) EnsureStoragePool(ctx context.Context, id, ownerUserID uuid.UUID, name string) error {
	body := api.CreateStoragePoolRequest{Id: &id, Name: name, OwnerUserId: ownerUserID}
	return c.t.do(ctx, http.MethodPost, "/api/v1/storage-pools", body, nil)
}

// FindProject returns the pool's project with the given name, or nil if there is
// none. Unlike users and pools, project creation does not take an explicit id
// (it is async and mints its own), so lookup-by-name is how the launcher's
// bootstrap stays idempotent across restarts.
func (c *API) FindProject(ctx context.Context, poolID uuid.UUID, name string) (*api.Project, error) {
	var list api.ProjectList
	path := fmt.Sprintf("/api/v1/storage-pools/%s/projects?limit=100", url.PathEscape(poolID.String()))
	if err := c.t.do(ctx, http.MethodGet, path, nil, &list); err != nil {
		return nil, err
	}
	for i := range list.Projects {
		if list.Projects[i].Name == name {
			return &list.Projects[i], nil
		}
	}
	return nil, nil
}

// CreateProject starts a create-project task and returns it. The project (and its
// default branch) exist once the task succeeds — poll with GetTask.
//
// A project is one independent lineage, so this is also how the launcher creates
// a *root* branch: the project carries the caller's chosen name and its default
// branch is always "main".
func (c *API) CreateProject(ctx context.Context, poolID uuid.UUID, name string, pgVersion int) (*api.Task, error) {
	body := api.CreateProjectRequest{Name: name, PgVersion: &pgVersion}
	var t api.Task
	path := fmt.Sprintf("/api/v1/storage-pools/%s/projects", url.PathEscape(poolID.String()))
	if err := c.t.do(ctx, http.MethodPost, path, body, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// GetTask fetches a task by id, for polling an async mutation to completion.
func (c *API) GetTask(ctx context.Context, id uuid.UUID) (*api.Task, error) {
	var t api.Task
	path := "/api/v1/tasks/" + url.PathEscape(id.String())
	if err := c.t.do(ctx, http.MethodGet, path, nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// GetProject fetches a project (with its default branch) by id.
func (c *API) GetProject(ctx context.Context, poolID, projectID uuid.UUID) (*api.Project, error) {
	var p api.Project
	path := fmt.Sprintf("/api/v1/storage-pools/%s/projects/%s",
		url.PathEscape(poolID.String()), url.PathEscape(projectID.String()))
	if err := c.t.do(ctx, http.MethodGet, path, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// --- branches ---------------------------------------------------------------
//
// The launcher drives branches through the product API rather than the
// storage-layer control surface: they are the same rows either way, and going
// through /api/v1 means the integration suite exercises the async task path the
// product actually uses instead of a surface only the launcher touches (#515).

// CreateBranch starts a branch task and returns it. The controller assigns the
// branch id; read it back from the branch list once the task succeeds. forkLSN
// must be supplied because resolving one from the parent's safekeeper needs a
// deployer, which the launcher has none of. Poll the returned task with GetTask.
func (c *API) CreateBranch(ctx context.Context, poolID, projectID uuid.UUID, name string,
	parent *uuid.UUID, forkLSN string,
) (*api.Task, error) {
	body := api.CreateBranchRequest{Name: name, ParentBranchId: parent}
	if forkLSN != "" {
		body.ForkPointLsn = &forkLSN
	}
	var t api.Task
	path := fmt.Sprintf("/api/v1/storage-pools/%s/projects/%s/branches",
		url.PathEscape(poolID.String()), url.PathEscape(projectID.String()))
	if err := c.t.do(ctx, http.MethodPost, path, body, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// ListProjects returns the pool's live projects. The launcher has several once
// root branches start creating them (a project is one lineage), so name and id
// resolution search across all of them.
func (c *API) ListProjects(ctx context.Context, poolID uuid.UUID) ([]api.Project, error) {
	var list api.ProjectList
	path := fmt.Sprintf("/api/v1/storage-pools/%s/projects?limit=1000", url.PathEscape(poolID.String()))
	if err := c.t.do(ctx, http.MethodGet, path, nil, &list); err != nil {
		return nil, err
	}
	return list.Projects, nil
}

// ListBranches returns the project's live branches. This is also the name -> id
// resolution the launcher needs: branch names are unique among a project's live
// branches, so no separate name registry is required.
func (c *API) ListBranches(ctx context.Context, poolID, projectID uuid.UUID) ([]api.Branch, error) {
	var list api.BranchList
	path := fmt.Sprintf("/api/v1/storage-pools/%s/projects/%s/branches?limit=1000",
		url.PathEscape(poolID.String()), url.PathEscape(projectID.String()))
	if err := c.t.do(ctx, http.MethodGet, path, nil, &list); err != nil {
		return nil, err
	}
	return list.Branches, nil
}

// DeleteBranch starts a delete-branch task and returns it. Poll with GetTask.
func (c *API) DeleteBranch(ctx context.Context, poolID, projectID, branchID uuid.UUID) (*api.Task, error) {
	var t api.Task
	path := fmt.Sprintf("/api/v1/storage-pools/%s/projects/%s/branches/%s",
		url.PathEscape(poolID.String()), url.PathEscape(projectID.String()),
		url.PathEscape(branchID.String()))
	if err := c.t.do(ctx, http.MethodDelete, path, nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// --- endpoints ---------------------------------------------------------------

// EndpointCreate is what the launcher supplies when creating an endpoint. Role
// is always read_write (it creates only primaries) and the pg version comes from
// the branch, so neither is here.
type EndpointCreate struct {
	// Name is the handle the CLI and the harness address the endpoint by. It is
	// the row's name, unique among the branch's endpoints.
	Name string
	// Extended is the launcher's local run config (the ports it binds), stored
	// verbatim as the endpoint's extended_config.
	Extended json.RawMessage
	// PgSettings are the caller's --pg-setting overrides for this endpoint.
	PgSettings map[string]string
}

// CreateEndpoint starts a create-endpoint task and returns it. Poll it with
// GetTask; the endpoint is then found by name.
//
// The branch's single-writer slot is claimed by the task's step, so a second
// primary on the same branch surfaces as a failed task, not a rejected POST. A
// name already taken on the branch IS rejected synchronously, with a 409.
func (c *API) CreateEndpoint(ctx context.Context, poolID, projectID, branchID uuid.UUID, in EndpointCreate) (*api.Task, error) {
	body := api.CreateEndpointRequest{}
	if in.Name != "" {
		body.Name = &in.Name
	}
	if len(in.Extended) > 0 {
		var ext map[string]any
		if err := json.Unmarshal(in.Extended, &ext); err != nil {
			return nil, fmt.Errorf("encode extended_config: %w", err)
		}
		body.ExtendedConfig = &ext
	}
	if len(in.PgSettings) > 0 {
		body.PgSettings = &in.PgSettings
	}
	var t api.Task
	path := fmt.Sprintf("/api/v1/storage-pools/%s/projects/%s/branches/%s/endpoints",
		url.PathEscape(poolID.String()), url.PathEscape(projectID.String()),
		url.PathEscape(branchID.String()))
	if err := c.t.do(ctx, http.MethodPost, path, body, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// GetBranch returns one branch, which is how the launcher reads its
// pg_settings baseline before building a compute spec.
func (c *API) GetBranch(ctx context.Context, poolID, projectID, branchID uuid.UUID) (*api.Branch, error) {
	var b api.Branch
	path := fmt.Sprintf("/api/v1/storage-pools/%s/projects/%s/branches/%s",
		url.PathEscape(poolID.String()), url.PathEscape(projectID.String()),
		url.PathEscape(branchID.String()))
	if err := c.t.do(ctx, http.MethodGet, path, nil, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// DeleteEndpoint starts a delete-endpoint task and returns it. Poll with GetTask.
// With the local driver the step retires nothing and just drops the record —
// the launcher has already stopped the compute by the time it calls this.
func (c *API) DeleteEndpoint(ctx context.Context, poolID, projectID, branchID, endpointID uuid.UUID) (*api.Task, error) {
	var t api.Task
	path := fmt.Sprintf("/api/v1/storage-pools/%s/projects/%s/branches/%s/endpoints/%s",
		url.PathEscape(poolID.String()), url.PathEscape(projectID.String()),
		url.PathEscape(branchID.String()), url.PathEscape(endpointID.String()))
	if err := c.t.do(ctx, http.MethodDelete, path, nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// ListEndpoints returns the branch's endpoints.
func (c *API) ListEndpoints(ctx context.Context, poolID, projectID, branchID uuid.UUID) ([]api.Endpoint, error) {
	var list api.EndpointList
	path := fmt.Sprintf("/api/v1/storage-pools/%s/projects/%s/branches/%s/endpoints?limit=1000",
		url.PathEscape(poolID.String()), url.PathEscape(projectID.String()),
		url.PathEscape(branchID.String()))
	if err := c.t.do(ctx, http.MethodGet, path, nil, &list); err != nil {
		return nil, err
	}
	return list.Endpoints, nil
}
