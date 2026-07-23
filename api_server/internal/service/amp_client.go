package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// AMPClient is a thin HTTP client for the AMP controller REST API (base URL +
// "/api/v1"). Meko uses it to provision AMP resources on a user's behalf:
// ensure the AMP user + shared pool exist (both idempotent, keyed on ids Meko
// supplies) and create databases (projects) inside the pool.
//
// User and pool creation are synchronous on the controller; project creation is
// asynchronous (202 + a task that we poll to completion), mirroring amp_ui's
// resource client.
type AMPClient struct {
	baseURL string
	http    *http.Client
}

// NewAMPClient returns a client for the AMP controller at baseURL
// (e.g. "http://localhost:8080"). "/api/v1" is appended per request. The
// httpClient is owned by the caller and shared across clients so its connection
// pool is reused (creating one per request defeats keep-alive).
func NewAMPClient(baseURL string, httpClient *http.Client) *AMPClient {
	return &AMPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    httpClient,
	}
}

/* ---- wire types (subset of the controller's schema) ---- */

// AMPBranch is a Postgres timeline within a project (the default branch is
// "main"; forks carry parent_branch_id + fork_lsn).
type AMPBranch struct {
	BranchID  string `json:"branch_id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	PgVersion int    `json:"pg_version"`
	IsDefault bool   `json:"is_default"`
	ForkLSN   string `json:"fork_lsn,omitempty"`
	CreatedAt string `json:"created_at"`
}

// AMPProject is an AMP "database": one project with a default branch.
type AMPProject struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	StoragePoolID string    `json:"storage_pool_id"`
	DefaultBranch AMPBranch `json:"default_branch"`
	CreatedAt     string    `json:"created_at"`
}

type ampPool struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	OwnerUserID string `json:"owner_user_id"`
}

type ampProjectList struct {
	Projects []AMPProject `json:"projects"`
}

type ampTask struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	Status    string  `json:"status"`
	ProjectID *string `json:"project_id"`
	TargetRef *string `json:"target_ref"`
	Error     *string `json:"error"`
}

const (
	ampTaskPollInterval = 2 * time.Second
	ampTaskTimeout      = 5 * time.Minute
)

/* ---- calls ---- */

// EnsureUser creates the AMP user with the given id (= the Meko user id) or, if
// it already exists, is a no-op. Idempotent.
func (c *AMPClient) EnsureUser(ctx context.Context, id string) error {
	body := map[string]any{"id": id, "is_admin": false}
	return c.do(ctx, http.MethodPost, "/users", body, nil)
}

// EnsurePool creates the shared pool with the given id (= the datapack id) owned
// by ownerID, or returns the existing pool if that id is already present.
// Idempotent.
func (c *AMPClient) EnsurePool(ctx context.Context, id, ownerID, name string) (ampPool, error) {
	body := map[string]any{"id": id, "name": name, "owner_user_id": ownerID}
	var p ampPool
	err := c.do(ctx, http.MethodPost, "/storage-pools", body, &p)
	return p, err
}

// ListProjects returns the databases (projects) in a pool.
func (c *AMPClient) ListProjects(ctx context.Context, poolID string) ([]AMPProject, error) {
	var list ampProjectList
	err := c.do(ctx, http.MethodGet, "/storage-pools/"+poolID+"/projects?limit=1000", nil, &list)
	return list.Projects, err
}

// CreateProject creates a database in a pool. The controller returns a task; we
// poll it to completion, then read the materialized project back.
func (c *AMPClient) CreateProject(ctx context.Context, poolID, name string, pgVersion *int) (AMPProject, error) {
	body := map[string]any{"name": name}
	if pgVersion != nil {
		body["pg_version"] = *pgVersion
	}
	var task ampTask
	if err := c.do(ctx, http.MethodPost, "/storage-pools/"+poolID+"/projects", body, &task); err != nil {
		return AMPProject{}, err
	}
	done, err := c.pollTask(ctx, task.ID)
	if err != nil {
		return AMPProject{}, err
	}
	if done.ProjectID == nil || *done.ProjectID == "" {
		return AMPProject{}, fmt.Errorf("amp create_project task %s returned no project id", task.ID)
	}
	var p AMPProject
	err = c.do(ctx, http.MethodGet, "/storage-pools/"+poolID+"/projects/"+*done.ProjectID, nil, &p)
	return p, err
}

/* ---- branches ---- */

type ampBranchList struct {
	Branches []AMPBranch `json:"branches"`
}

// CreateBranchInput is the payload for forking a branch.
type CreateBranchInput struct {
	Name         string `json:"name"`
	ParentBranch string `json:"parent_branch_id,omitempty"`
	ForkPointLSN string `json:"fork_point_lsn,omitempty"`
}

// ListBranches returns the branches of a database (project).
func (c *AMPClient) ListBranches(ctx context.Context, poolID, projectID string) ([]AMPBranch, error) {
	var list ampBranchList
	err := c.do(ctx, http.MethodGet, "/storage-pools/"+poolID+"/projects/"+projectID+"/branches?limit=1000", nil, &list)
	return list.Branches, err
}

// CreateBranch forks a branch (task-based: poll, then read back via target_ref).
func (c *AMPClient) CreateBranch(ctx context.Context, poolID, projectID string, in CreateBranchInput) (AMPBranch, error) {
	base := "/storage-pools/" + poolID + "/projects/" + projectID + "/branches"
	var task ampTask
	if err := c.do(ctx, http.MethodPost, base, in, &task); err != nil {
		return AMPBranch{}, err
	}
	done, err := c.pollTask(ctx, task.ID)
	if err != nil {
		return AMPBranch{}, err
	}
	if done.TargetRef == nil || *done.TargetRef == "" {
		return AMPBranch{}, fmt.Errorf("amp branch task %s returned no branch id", task.ID)
	}
	var b AMPBranch
	err = c.do(ctx, http.MethodGet, base+"/"+*done.TargetRef, nil, &b)
	return b, err
}

/* ---- endpoints (compute) ---- */

// AMPEndpoint is a compute instance bound to a branch. address is host:port,
// empty until deployed.
type AMPEndpoint struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	BranchID  string `json:"branch_id"`
	Role      string `json:"role"`
	Address   string `json:"address"`
	// TailnetAddress is AMP's Tailscale (host:port) address, empty unless the
	// endpoint Service is tailnet-exposed. Part of AMP's Endpoint contract.
	TailnetAddress string `json:"tailnet_address"`
	CreatedAt      string `json:"created_at"`
}

type ampEndpointList struct {
	Endpoints []AMPEndpoint `json:"endpoints"`
}

// ListEndpoints returns the compute instances on a branch.
func (c *AMPClient) ListEndpoints(ctx context.Context, poolID, projectID, branchID string) ([]AMPEndpoint, error) {
	var list ampEndpointList
	err := c.do(ctx, http.MethodGet, "/storage-pools/"+poolID+"/projects/"+projectID+"/branches/"+branchID+"/endpoints?limit=1000", nil, &list)
	return list.Endpoints, err
}

// CreateEndpoint provisions compute on a branch (task-based: poll, then read
// back via target_ref).
func (c *AMPClient) CreateEndpoint(ctx context.Context, poolID, projectID, branchID, role, cpu, memory string) (AMPEndpoint, error) {
	base := "/storage-pools/" + poolID + "/projects/" + projectID + "/branches/" + branchID + "/endpoints"
	body := map[string]any{"role": role}
	// AMP nests compute sizing under `resources` as Kubernetes quantities
	// (cpu e.g. "2"/"1500m", memory e.g. "4Gi"). Only send fields we have so
	// AMP applies its own defaults (2 cores / 4Gi) for anything omitted.
	if cpu != "" || memory != "" {
		res := map[string]any{}
		if cpu != "" {
			res["cpu"] = cpu
		}
		if memory != "" {
			res["memory"] = memory
		}
		body["resources"] = res
	}
	var task ampTask
	if err := c.do(ctx, http.MethodPost, base, body, &task); err != nil {
		return AMPEndpoint{}, err
	}
	done, err := c.pollTask(ctx, task.ID)
	if err != nil {
		return AMPEndpoint{}, err
	}
	if done.TargetRef == nil || *done.TargetRef == "" {
		return AMPEndpoint{}, fmt.Errorf("amp create_endpoint task %s returned no endpoint id", task.ID)
	}
	var e AMPEndpoint
	err = c.do(ctx, http.MethodGet, base+"/"+*done.TargetRef, nil, &e)
	return e, err
}

// pollTask polls a task to a terminal state. Transient read errors (network
// blips, controller 5xx) are logged and retried until the overall timeout — a
// single glitch must not abort an in-flight provision. Only a task that reports
// "failed", the deadline, or a cancelled context ends the loop.
func (c *AMPClient) pollTask(ctx context.Context, taskID string) (ampTask, error) {
	deadline := time.Now().Add(ampTaskTimeout)
	var lastErr error
	for {
		var t ampTask
		if err := c.do(ctx, http.MethodGet, "/tasks/"+taskID, nil, &t); err != nil {
			lastErr = err
			slog.Warn("amp poll task: transient error, will retry", "task", taskID, "err", err)
		} else {
			lastErr = nil // a later read succeeded; don't report a stale transient error on timeout
			switch t.Status {
			case "succeeded":
				return t, nil
			case "failed":
				msg := "task failed"
				if t.Error != nil {
					msg = *t.Error
				}
				return ampTask{}, fmt.Errorf("amp task %s (%s): %s", taskID, t.Kind, msg)
			}
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return ampTask{}, fmt.Errorf("amp task %s polling gave up after %s: %w", taskID, ampTaskTimeout, lastErr)
			}
			return ampTask{}, fmt.Errorf("amp task %s did not finish within %s", taskID, ampTaskTimeout)
		}
		select {
		case <-ctx.Done():
			return ampTask{}, ctx.Err()
		case <-time.After(ampTaskPollInterval):
		}
	}
}

// do performs a JSON request against baseURL + "/api/v1" + path, decoding a
// success body into out (when non-nil) and surfacing the controller's nested
// {"error":{"message"}} body on failure.
func (c *AMPClient) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("amp: marshal body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v1"+path, reader)
	if err != nil {
		return fmt.Errorf("amp: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("amp: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("amp: %s %s: %d: %s", method, path, resp.StatusCode, ampErrMessage(data))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("amp: decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

// ampErrMessage pulls a human message out of the controller's error body,
// tolerating its nested {"error":{"message"}} shape (falling back to raw text).
func ampErrMessage(data []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &env) == nil {
		if env.Error.Message != "" {
			return env.Error.Message
		}
		if env.Message != "" {
			return env.Message
		}
	}
	return strings.TrimSpace(string(data))
}
