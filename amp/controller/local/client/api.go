package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
)

// API is the launcher's client for the controller's *product* REST API
// (/api/v1), as opposed to Controller which drives the storage-layer control
// surface (/control/v1).
//
// The launcher needs this for exactly one thing: standing up its own resource
// ladder — a user, a storage pool, and a project — so the branches and endpoints
// it creates are ordinary rows in the control plane's resource model rather than
// a parallel registry. Everything after that (declaring branches, recording
// endpoints) stays on /control/v1, because the launcher runs local processes and
// cannot use the product endpoint API: with no deployer configured that returns
// 501.
//
// Auth is Trust-only (--dev): /api/v1 has no auth middleware, matching the
// launcher's local topology.
type API struct {
	t transport
}

// NewAPI builds a product-API client. base is the controller's listen address,
// either a bare host:port or a full http(s) URL. A nil hc uses http.DefaultClient.
func NewAPI(base string, hc *http.Client) *API {
	return &API{t: newTransport(base, hc, "")}
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
