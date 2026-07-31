// Package controller is the amp controller REST app: a small, hand-written typed
// client over the control-plane API (openapi_spec.yaml), the stress
// yugaware_app equivalent. It is deliberately NOT generated — the framework
// consumes ~15 operations and a handful of fields, so a thin net/http client is
// simpler than wiring oapi-codegen and its build step (KISS; the implementation
// plan allows revisiting codegen once the surface grows). The resource ladder is
// User → StoragePool → Project → Branch → Endpoint; create/delete of
// project/branch/endpoint are asynchronous (202 + Task + Location), so this
// package also provides *AndWait helpers that poll GET /tasks/{id} to a terminal
// status. Types carry only the fields the framework reads and tolerate unknown
// fields (encoding/json drops them).
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// maxBody bounds a single response body read so a misbehaving server can't
// balloon memory in the coordinator.
const maxBody = 8 << 20 // 8 MiB

// Task statuses.
const (
	TaskPending   = "pending"
	TaskRunning   = "running"
	TaskSucceeded = "succeeded"
	TaskFailed    = "failed"
)

// Poll backoff defaults for the async-task helpers.
const (
	DefaultPollInitial = 250 * time.Millisecond
	DefaultPollMax     = 2 * time.Second
)

// getTaskRetries/getTaskRetryDelay bound GetTask's transient-error retry: the
// controller is often reached through a kubectl port-forward, which can see a
// brief EOF/reset/refused blip (e.g. while it is being re-established after a
// crash). A GET is idempotent, so retrying it a few times is safe; a bounded
// fixed delay keeps a real (non-transient) failure from stalling long.
const (
	getTaskRetries    = 3
	getTaskRetryDelay = 200 * time.Millisecond
)

// Client is a controller REST client. Base URL and http.Client are injectable.
type Client struct {
	baseURL string // scheme://host[:port], no trailing slash; health lives here
	apiURL  string // baseURL + /api/v1
	hc      *http.Client

	// PollInitial/PollMax bound the async-task poll backoff (settable for tests).
	PollInitial time.Duration
	PollMax     time.Duration
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient injects the http.Client (timeouts, transport, test server).
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

// New builds a Client for the controller rooted at baseURL (e.g.
// "http://127.0.0.1:8080"). The versioned API is served under /api/v1; health
// under the root.
func New(baseURL string, opts ...Option) *Client {
	b := strings.TrimRight(baseURL, "/")
	c := &Client{
		baseURL:     b,
		apiURL:      b + "/api/v1",
		hc:          http.DefaultClient,
		PollInitial: DefaultPollInitial,
		PollMax:     DefaultPollMax,
	}
	for _, o := range opts {
		o(c)
	}
	if c.hc == nil {
		c.hc = http.DefaultClient
	}
	return c
}

// BaseURL is the controller root URL.
func (c *Client) BaseURL() string { return c.baseURL }

// --- resource types (only the fields the framework consumes) ---

// User is an unauthenticated identity.
type User struct {
	ID      string `json:"id"`
	IsAdmin bool   `json:"is_admin"`
}

// CreateUserRequest optionally pins an id (idempotent) and admin flag.
type CreateUserRequest struct {
	ID      string `json:"id,omitempty"`
	IsAdmin bool   `json:"is_admin,omitempty"`
}

// UserList is a page of users.
type UserList struct {
	Users []User `json:"users"`
	Total int    `json:"total"`
	Count int    `json:"count"`
}

// StoragePool is a top-level pool owned by a user.
type StoragePool struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	OwnerUserID string `json:"owner_user_id"`
}

// CreateStoragePoolRequest creates a pool with a unique name.
type CreateStoragePoolRequest struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name"`
	OwnerUserID string `json:"owner_user_id"`
}

// StoragePoolList is a page of pools.
type StoragePoolList struct {
	StoragePools []StoragePool `json:"storage_pools"`
	Total        int           `json:"total"`
	Count        int           `json:"count"`
}

// Branch is a PG timeline in a project.
type Branch struct {
	ProjectID      string  `json:"project_id"`
	BranchID       string  `json:"branch_id"`
	Name           string  `json:"name"`
	PgVersion      int     `json:"pg_version"`
	IsDefault      bool    `json:"is_default"`
	ParentBranchID *string `json:"parent_branch_id"`
	ForkLSN        *string `json:"fork_lsn"`
}

// CreateBranchRequest forks a branch into a new one.
type CreateBranchRequest struct {
	Name           string `json:"name"`
	ParentBranchID string `json:"parent_branch_id,omitempty"`
	ForkPointLSN   string `json:"fork_point_lsn,omitempty"`
}

// Project is a database in a pool, with its default branch.
type Project struct {
	ID            string `json:"id"`
	StoragePoolID string `json:"storage_pool_id"`
	Name          string `json:"name"`
	DefaultBranch Branch `json:"default_branch"`
}

// CreateProjectRequest creates a project (and its default branch "main").
type CreateProjectRequest struct {
	Name      string `json:"name"`
	PgVersion int    `json:"pg_version,omitempty"`
}

// Endpoint is the compute bound to a branch.
type Endpoint struct {
	ID             string `json:"id"`
	ProjectID      string `json:"project_id"`
	BranchID       string `json:"branch_id"`
	Role           string `json:"role"`
	Address        string `json:"address"`
	TailnetAddress string `json:"tailnet_address"`
	// Resources is the endpoint's current compute reservation, read back from its
	// persisted config. Nil on a controller predating in-place resize, which is
	// how a caller detects the capability without probing the runtime.
	Resources *EndpointResources `json:"resources"`
}

// EndpointResources is a compute reservation: cpu + memory as Kubernetes
// quantities, applied as request == limit (Guaranteed QoS).
type EndpointResources struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// ResizeEndpointRequest changes an existing endpoint's compute reservation. An
// omitted dimension keeps its current value.
type ResizeEndpointRequest struct {
	Resources EndpointResources `json:"resources"`
}

// In-place resize outcomes a resize_endpoint task's final step reports. Only a
// deployer error fails the task: pending/infeasible/unsupported are successful
// terminal states in which the new size is persisted and converges on the next
// redeploy.
const (
	ResizeCompleted   = "completed"
	ResizePending     = "pending"
	ResizeInfeasible  = "infeasible"
	ResizeUnsupported = "unsupported"
)

// CreateEndpointRequest creates and deploys an endpoint on a branch.
type CreateEndpointRequest struct {
	Role     string `json:"role,omitempty"`
	DiskType string `json:"disk_type,omitempty"`
}

// EndpointList is a page of endpoints.
type EndpointList struct {
	Endpoints []Endpoint `json:"endpoints"`
	Total     int        `json:"total"`
	Count     int        `json:"count"`
}

// Task is the write-ahead record of a long-running operation.
type Task struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	Status    string  `json:"status"`
	ProjectID *string `json:"project_id"`
	Error     *string `json:"error"`
	// Result is the final step's opaque outcome JSON, absent until a step records
	// one. For a resize_endpoint task it carries in_place_resize + in_place_message.
	Result json.RawMessage `json:"result"`
}

// InPlaceResize decodes the in-place outcome a resize_endpoint task recorded.
// Both values are empty when the task has no result yet or is another kind.
func (t Task) InPlaceResize() (outcome, message string) {
	if len(t.Result) == 0 {
		return "", ""
	}
	var r struct {
		Outcome string `json:"in_place_resize"`
		Message string `json:"in_place_message"`
	}
	if json.Unmarshal(t.Result, &r) != nil {
		return "", ""
	}
	return r.Outcome, r.Message
}

// TaskList is a page of tasks.
type TaskList struct {
	Tasks []Task `json:"tasks"`
	Total int    `json:"total"`
	Count int    `json:"count"`
}

// APIError is a non-2xx response decoded from the Error envelope.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Code != "" || e.Message != "" {
		return fmt.Sprintf("controller %d: %s: %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("controller %d", e.StatusCode)
}

// --- health ---

// Healthz reports liveness (GET /healthz).
// VersionInfo is GET /api/v1/version — the controller's own build identity
// (amp hybrid CalVer; "dev" on un-stamped local builds).
type VersionInfo struct {
	Version   string `json:"version"`
	GitCommit string `json:"git_commit"`
	BuildDate string `json:"build_date"`
}

// Version fetches the controller's build identity. Controllers predating the
// version endpoint return an error (callers treat that as "unknown").
func (c *Client) Version(ctx context.Context) (VersionInfo, error) {
	var out VersionInfo
	_, _, err := c.doJSON(ctx, http.MethodGet, c.apiURL+"/version", nil, &out)
	return out, err
}

func (c *Client) Healthz(ctx context.Context) error { return c.health(ctx, "/healthz") }

// Readyz reports readiness (GET /readyz).
func (c *Client) Readyz(ctx context.Context) error { return c.health(ctx, "/readyz") }

func (c *Client) health(ctx context.Context, path string) error {
	code, _, err := c.doJSON(ctx, http.MethodGet, c.baseURL+path, nil, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("%s returned %d", path, code)
	}
	return nil
}

// --- users ---

// CreateUser creates a user (sync 201). Pinning an existing id is a no-op.
func (c *Client) CreateUser(ctx context.Context, req CreateUserRequest) (User, error) {
	var out User
	_, _, err := c.doJSON(ctx, http.MethodPost, c.apiURL+"/users", req, &out)
	return out, err
}

// ListUsers returns one page of users.
func (c *Client) ListUsers(ctx context.Context, limit, offset int) (UserList, error) {
	var out UserList
	_, _, err := c.doJSON(ctx, http.MethodGet, c.apiURL+"/users"+pageQuery(limit, offset), nil, &out)
	return out, err
}

// --- storage pools ---

// CreateStoragePool creates a pool (sync 201).
func (c *Client) CreateStoragePool(ctx context.Context, req CreateStoragePoolRequest) (StoragePool, error) {
	var out StoragePool
	_, _, err := c.doJSON(ctx, http.MethodPost, c.apiURL+"/storage-pools", req, &out)
	return out, err
}

// ListStoragePools returns one page of pools.
func (c *Client) ListStoragePools(ctx context.Context, limit, offset int) (StoragePoolList, error) {
	var out StoragePoolList
	_, _, err := c.doJSON(ctx, http.MethodGet, c.apiURL+"/storage-pools"+pageQuery(limit, offset), nil, &out)
	return out, err
}

// --- projects ---

// CreateProject plans an async project create (202). Returns the Task and the
// Location header value.
func (c *Client) CreateProject(ctx context.Context, poolID string, req CreateProjectRequest) (Task, string, error) {
	return c.async(ctx, http.MethodPost, c.apiURL+"/storage-pools/"+poolID+"/projects", req)
}

// ProjectList is a page of projects.
type ProjectList struct {
	Projects []Project `json:"projects"`
	Total    int       `json:"total"`
	Count    int       `json:"count"`
}

// ListProjects returns one page of the projects in a pool. (Beyond the minimal
// enumerated op set, but needed to resolve the id of an asynchronously-created
// project by name — the same lookup deploy/smoke-test.sh does.)
func (c *Client) ListProjects(ctx context.Context, poolID string, limit, offset int) (ProjectList, error) {
	var out ProjectList
	_, _, err := c.doJSON(ctx, http.MethodGet, c.apiURL+"/storage-pools/"+poolID+"/projects"+pageQuery(limit, offset), nil, &out)
	return out, err
}

// GetProject reads a project (with its default branch).
func (c *Client) GetProject(ctx context.Context, poolID, projectID string) (Project, error) {
	var out Project
	_, _, err := c.doJSON(ctx, http.MethodGet, c.apiURL+"/storage-pools/"+poolID+"/projects/"+projectID, nil, &out)
	return out, err
}

// DeleteProject plans an async cascade delete (202).
func (c *Client) DeleteProject(ctx context.Context, poolID, projectID string) (Task, string, error) {
	return c.async(ctx, http.MethodDelete, c.apiURL+"/storage-pools/"+poolID+"/projects/"+projectID, nil)
}

// --- branches ---

// CreateBranch plans an async branch fork (202).
func (c *Client) CreateBranch(ctx context.Context, poolID, projectID string, req CreateBranchRequest) (Task, string, error) {
	return c.async(ctx, http.MethodPost, c.apiURL+"/storage-pools/"+poolID+"/projects/"+projectID+"/branches", req)
}

// DeleteBranch plans an async cascade delete (202).
func (c *Client) DeleteBranch(ctx context.Context, poolID, projectID, branchID string) (Task, string, error) {
	return c.async(ctx, http.MethodDelete, c.apiURL+"/storage-pools/"+poolID+"/projects/"+projectID+"/branches/"+branchID, nil)
}

// BranchList is a page of branches.
type BranchList struct {
	Branches []Branch `json:"branches"`
	Total    int      `json:"total"`
	Count    int      `json:"count"`
}

// ListBranches returns one page of the branches in a project (the default branch
// plus its forks). Needed to resolve the id of an asynchronously-created branch by
// name — the same lookup findProject does for projects.
func (c *Client) ListBranches(ctx context.Context, poolID, projectID string, limit, offset int) (BranchList, error) {
	var out BranchList
	_, _, err := c.doJSON(ctx, http.MethodGet, c.apiURL+"/storage-pools/"+poolID+"/projects/"+projectID+"/branches"+pageQuery(limit, offset), nil, &out)
	return out, err
}

// ListAllBranches pages through every branch in a project.
func (c *Client) ListAllBranches(ctx context.Context, poolID, projectID string) ([]Branch, error) {
	const page = 100
	var all []Branch
	for offset := 0; ; offset += page {
		list, err := c.ListBranches(ctx, poolID, projectID, page, offset)
		if err != nil {
			return nil, err
		}
		all = append(all, list.Branches...)
		if list.Count < page || len(all) >= list.Total {
			return all, nil
		}
	}
}

// --- endpoints ---

func (c *Client) endpointsPath(poolID, projectID, branchID string) string {
	return c.apiURL + "/storage-pools/" + poolID + "/projects/" + projectID + "/branches/" + branchID + "/endpoints"
}

// CreateEndpoint plans an async endpoint deploy (202).
func (c *Client) CreateEndpoint(ctx context.Context, poolID, projectID, branchID string, req CreateEndpointRequest) (Task, string, error) {
	return c.async(ctx, http.MethodPost, c.endpointsPath(poolID, projectID, branchID), req)
}

// GetEndpoint reads an endpoint.
func (c *Client) GetEndpoint(ctx context.Context, poolID, projectID, branchID, endpointID string) (Endpoint, error) {
	var out Endpoint
	_, _, err := c.doJSON(ctx, http.MethodGet, c.endpointsPath(poolID, projectID, branchID)+"/"+endpointID, nil, &out)
	return out, err
}

// ListEndpoints returns one page of endpoints bound to a branch.
func (c *Client) ListEndpoints(ctx context.Context, poolID, projectID, branchID string, limit, offset int) (EndpointList, error) {
	var out EndpointList
	_, _, err := c.doJSON(ctx, http.MethodGet, c.endpointsPath(poolID, projectID, branchID)+pageQuery(limit, offset), nil, &out)
	return out, err
}

// DeleteEndpoint plans an async endpoint retire (202).
func (c *Client) DeleteEndpoint(ctx context.Context, poolID, projectID, branchID, endpointID string) (Task, string, error) {
	return c.async(ctx, http.MethodDelete, c.endpointsPath(poolID, projectID, branchID)+"/"+endpointID, nil)
}

// ResizeEndpoint plans an async in-place compute resize (202). First-writer-wins:
// while a resize of the same endpoint is non-terminal, this returns a 409
// *APIError.
func (c *Client) ResizeEndpoint(ctx context.Context, poolID, projectID, branchID, endpointID string, req ResizeEndpointRequest) (Task, string, error) {
	return c.async(ctx, http.MethodPost, c.endpointsPath(poolID, projectID, branchID)+"/"+endpointID+"/resize", req)
}

// ListAllEndpoints pages through every endpoint bound to a branch.
func (c *Client) ListAllEndpoints(ctx context.Context, poolID, projectID, branchID string) ([]Endpoint, error) {
	const page = 100
	var all []Endpoint
	for offset := 0; ; offset += page {
		list, err := c.ListEndpoints(ctx, poolID, projectID, branchID, page, offset)
		if err != nil {
			return nil, err
		}
		all = append(all, list.Endpoints...)
		if list.Count < page || len(all) >= list.Total {
			return all, nil
		}
	}
}

// --- tasks ---

// GetTask reads one task's status, retrying up to getTaskRetries times (small
// fixed backoff) on a transient connection error before surfacing it — see
// getTaskRetries. A real API error (4xx/5xx, decoded from the body) is never
// retried, only a dropped/refused/reset connection.
func (c *Client) GetTask(ctx context.Context, taskID string) (Task, error) {
	var out Task
	var err error
	for attempt := 0; ; attempt++ {
		out = Task{}
		_, _, err = c.doJSON(ctx, http.MethodGet, c.apiURL+"/tasks/"+taskID, nil, &out)
		if err == nil || attempt >= getTaskRetries || !isTransientConnErr(err) {
			return out, err
		}
		select {
		case <-ctx.Done():
			return Task{}, ctx.Err()
		case <-time.After(getTaskRetryDelay):
		}
	}
}

// isTransientConnErr reports whether err looks like a dropped, reset, or
// refused connection rather than a real API-level failure — the class of
// error a brief port-forward blip produces.
func isTransientConnErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset")
}

// ListTasksOptions filters listTasks.
type ListTasksOptions struct {
	Limit     int
	Offset    int
	Status    string
	Kind      string
	ProjectID string
}

// ListTasks returns one page of tasks, most recent first.
func (c *Client) ListTasks(ctx context.Context, opts ListTasksOptions) (TaskList, error) {
	q := url.Values{}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	if opts.Kind != "" {
		q.Set("kind", opts.Kind)
	}
	if opts.ProjectID != "" {
		q.Set("project_id", opts.ProjectID)
	}
	u := c.apiURL + "/tasks"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	var out TaskList
	_, _, err := c.doJSON(ctx, http.MethodGet, u, nil, &out)
	return out, err
}

// --- async task discipline ---

// WaitTask polls GET /tasks/{id} with capped exponential backoff under ctx until
// the task reaches a terminal status. A failed task returns an error carrying
// the task's recorded error.
func (c *Client) WaitTask(ctx context.Context, taskID string) (Task, error) {
	if taskID == "" {
		return Task{}, fmt.Errorf("wait task: empty task id")
	}
	backoff := c.PollInitial
	if backoff <= 0 {
		backoff = DefaultPollInitial
	}
	maxBackoff := c.PollMax
	if maxBackoff <= 0 {
		maxBackoff = DefaultPollMax
	}
	for {
		task, err := c.GetTask(ctx, taskID)
		if err != nil {
			return Task{}, err
		}
		switch task.Status {
		case TaskSucceeded:
			return task, nil
		case TaskFailed:
			msg := ""
			if task.Error != nil {
				msg = *task.Error
			}
			return task, fmt.Errorf("task %s (%s) failed: %s", task.ID, task.Kind, msg)
		}
		select {
		case <-ctx.Done():
			return Task{}, fmt.Errorf("waiting for task %s: %w", taskID, ctx.Err())
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// CreateProjectAndWait creates a project and waits for its task to succeed.
func (c *Client) CreateProjectAndWait(ctx context.Context, poolID string, req CreateProjectRequest) (Task, error) {
	task, loc, err := c.CreateProject(ctx, poolID, req)
	if err != nil {
		return Task{}, err
	}
	return c.WaitTask(ctx, taskRef(task, loc))
}

// DeleteProjectAndWait deletes a project (cascade) and waits for success.
func (c *Client) DeleteProjectAndWait(ctx context.Context, poolID, projectID string) (Task, error) {
	task, loc, err := c.DeleteProject(ctx, poolID, projectID)
	if err != nil {
		return Task{}, err
	}
	return c.WaitTask(ctx, taskRef(task, loc))
}

// CreateBranchAndWait creates a branch and waits for success.
func (c *Client) CreateBranchAndWait(ctx context.Context, poolID, projectID string, req CreateBranchRequest) (Task, error) {
	task, loc, err := c.CreateBranch(ctx, poolID, projectID, req)
	if err != nil {
		return Task{}, err
	}
	return c.WaitTask(ctx, taskRef(task, loc))
}

// DeleteBranchAndWait deletes a branch (cascade) and waits for success.
func (c *Client) DeleteBranchAndWait(ctx context.Context, poolID, projectID, branchID string) (Task, error) {
	task, loc, err := c.DeleteBranch(ctx, poolID, projectID, branchID)
	if err != nil {
		return Task{}, err
	}
	return c.WaitTask(ctx, taskRef(task, loc))
}

// CreateEndpointAndWait creates an endpoint and waits for success.
func (c *Client) CreateEndpointAndWait(ctx context.Context, poolID, projectID, branchID string, req CreateEndpointRequest) (Task, error) {
	task, loc, err := c.CreateEndpoint(ctx, poolID, projectID, branchID, req)
	if err != nil {
		return Task{}, err
	}
	return c.WaitTask(ctx, taskRef(task, loc))
}

// ResizeEndpointAndWait resizes an endpoint and waits for its task to reach a
// terminal status. The Task is returned even when it failed (the caller reads its
// error and result), alongside the failure error.
func (c *Client) ResizeEndpointAndWait(ctx context.Context, poolID, projectID, branchID, endpointID string, req ResizeEndpointRequest) (Task, error) {
	task, loc, err := c.ResizeEndpoint(ctx, poolID, projectID, branchID, endpointID, req)
	if err != nil {
		return Task{}, err
	}
	return c.WaitTask(ctx, taskRef(task, loc))
}

// DeleteEndpointAndWait retires an endpoint and waits for success.
func (c *Client) DeleteEndpointAndWait(ctx context.Context, poolID, projectID, branchID, endpointID string) (Task, error) {
	task, loc, err := c.DeleteEndpoint(ctx, poolID, projectID, branchID, endpointID)
	if err != nil {
		return Task{}, err
	}
	return c.WaitTask(ctx, taskRef(task, loc))
}

// --- transport helpers ---

// async performs a 202 mutation and returns the Task plus the Location header.
func (c *Client) async(ctx context.Context, method, url string, body any) (Task, string, error) {
	var task Task
	_, hdr, err := c.doJSON(ctx, method, url, body, &task)
	if err != nil {
		return Task{}, "", err
	}
	loc := ""
	if hdr != nil {
		loc = hdr.Get("Location")
	}
	return task, loc, nil
}

func (c *Client) doJSON(ctx context.Context, method, url string, body, out any) (int, http.Header, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal %s %s: %w", method, url, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, fmt.Errorf("build %s %s: %w", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, resp.Header, fmt.Errorf("read %s %s: %w", method, url, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out != nil && len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return resp.StatusCode, resp.Header, fmt.Errorf("decode %s %s: %w", method, url, err)
			}
		}
		return resp.StatusCode, resp.Header, nil
	}
	return resp.StatusCode, resp.Header, parseAPIError(resp.StatusCode, data)
}

// parseAPIError decodes the { "error": { code, message } } envelope, tolerating
// a body that isn't the envelope.
func parseAPIError(status int, data []byte) *APIError {
	e := &APIError{StatusCode: status}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &env) == nil {
		e.Code = env.Error.Code
		e.Message = env.Error.Message
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(data))
	}
	return e
}

// taskRef prefers the id embedded in a Location header (/api/v1/tasks/{id}),
// falling back to the task body's id.
func taskRef(task Task, location string) string {
	if location != "" {
		if i := strings.LastIndexByte(strings.TrimRight(location, "/"), '/'); i >= 0 {
			if id := strings.TrimRight(location, "/")[i+1:]; id != "" {
				return id
			}
		}
	}
	return task.ID
}

func pageQuery(limit, offset int) string {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	if enc := q.Encode(); enc != "" {
		return "?" + enc
	}
	return ""
}
