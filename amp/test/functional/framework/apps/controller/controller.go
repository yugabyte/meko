// Package controller is the amp controller REST app: a small, hand-written typed
// client over the control-plane API (openapi_spec.yaml), the stress
// yugaware_app equivalent. It is deliberately NOT generated — the framework
// consumes ~15 operations and a handful of fields, so a thin net/http client is
// simpler than wiring oapi-codegen and its build step (KISS; the implementation
// plan allows revisiting codegen once the surface grows). The resource ladder is
// User → StoragePool → Cluster → Branch → Endpoint; create/delete of
// cluster/branch/endpoint are asynchronous (202 + Task + Location), so this
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
	"net"
	"net/http"
	"net/url"
	"os"
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

// Budget for waiting out the controller's state-store 503: a rescheduled
// tserver costs a pod recreate plus a PD detach/attach, rounded up.
const (
	DefaultUnavailableBudget = 90 * time.Second
	DefaultUnavailableDelay  = 2 * time.Second
	unavailableMaxWait       = 10 * time.Second
)

// Matched on the message, not the code: the controller derives the code from
// the status text, so every 503 it sends carries "service_unavailable" —
// including /readyz, whose callers need it to answer immediately.
const storeUnavailableMessage = "state store unavailable"

// Client is a controller REST client. Base URL and http.Client are injectable.
type Client struct {
	baseURL string // scheme://host[:port], no trailing slash; health lives here
	apiURL  string // baseURL + /api/v1
	hc      *http.Client

	// PollInitial/PollMax bound the async-task poll backoff (settable for tests).
	PollInitial time.Duration
	PollMax     time.Duration

	UnavailableBudget time.Duration
	UnavailableDelay  time.Duration
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
		baseURL:           b,
		apiURL:            b + "/api/v1",
		hc:                http.DefaultClient,
		PollInitial:       DefaultPollInitial,
		PollMax:           DefaultPollMax,
		UnavailableBudget: DefaultUnavailableBudget,
		UnavailableDelay:  DefaultUnavailableDelay,
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

// Branch is a PG timeline in a cluster.
type Branch struct {
	ClusterID      string  `json:"cluster_id"`
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

// Cluster is a database in a pool, with its default branch.
type Cluster struct {
	ID            string `json:"id"`
	StoragePoolID string `json:"storage_pool_id"`
	Name          string `json:"name"`
	DefaultBranch Branch `json:"default_branch"`
}

// CreateClusterRequest creates a cluster (and its default branch "main").
type CreateClusterRequest struct {
	Name      string `json:"name"`
	PgVersion int    `json:"pg_version,omitempty"`
	// Extensions opts the default branch "main" into externalized PostgreSQL
	// extensions by catalog name (e.g. "pgvector"): every endpoint on that branch
	// mounts them as read-only ImageVolumes, and a fork inherits the set. Add more
	// to a branch later with AddBranchExtensions. Rejected with 400 on a
	// controller without an extension catalog.
	Extensions []string `json:"extensions,omitempty"`
}

// AddBranchExtensionsRequest enables more externalized extensions on an existing
// branch.
type AddBranchExtensionsRequest struct {
	Extensions []string `json:"extensions"`
}

// Endpoint is the compute bound to a branch.
type Endpoint struct {
	ID             string `json:"id"`
	ClusterID      string `json:"cluster_id"`
	BranchID       string `json:"branch_id"`
	Role           string `json:"role"`
	Address        string `json:"address"`
	TailnetAddress string `json:"tailnet_address"`
	// Resources is the endpoint's current compute reservation, read back from its
	// persisted config. Nil on a controller predating in-place resize, which is
	// how a caller detects the capability without probing the runtime.
	Resources *EndpointResources `json:"resources"`
	// The settings the endpoint was created with, read back verbatim. Nil/empty
	// on a controller predating the endpoint settings API.
	PgSettings         map[string]string `json:"pg_settings"`
	PageserverSettings map[string]any    `json:"pageserver_settings"`
	SafekeeperSettings map[string]any    `json:"safekeeper_settings"`
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
	// Resources is the compute reservation to create the endpoint with. Omitted
	// (nil) leaves the controller's create-time defaults in place.
	Resources *EndpointResources `json:"resources,omitempty"`
	// PgSettings are extra PostgreSQL settings for this endpoint, merged over
	// the branch's pg_settings.
	PgSettings map[string]string `json:"pg_settings,omitempty"`
	// PageserverSettings / SafekeeperSettings override that daemon's toml at pod
	// start. Keys may be dotted paths (`tenant_config.enable_gc`); values must be
	// scalars. Unknown or protected keys are rejected with 400.
	PageserverSettings map[string]any `json:"pageserver_settings,omitempty"`
	SafekeeperSettings map[string]any `json:"safekeeper_settings,omitempty"`
}

// UpdateSettingsRequest is the body of the two settings PATCHes. The controller
// reads an omitted surface as "leave it alone" and `{}` as "clear it".
//
// Pointers for the ability to say `{}`: with `omitempty` an empty map is omitted
// exactly like a nil pointer, so a plain map field could never clear a surface —
// it could only fail to mention one.
type UpdateSettingsRequest struct {
	PgSettings         *map[string]string `json:"pg_settings,omitempty"`
	PageserverSettings *map[string]any    `json:"pageserver_settings,omitempty"`
	SafekeeperSettings *map[string]any    `json:"safekeeper_settings,omitempty"`
	// Branch-only: the write lease (`ttl_ms`, `renew_seconds`).
	WriteTokenSettings *map[string]any `json:"write_token_settings,omitempty"`
}

// EndpointSettingsOutcome is one endpoint's account of what it did with a
// settings change. The distinctions are the point: a setting can be applied in
// place, or cost a restart, or be stored and never take effect at all.
type EndpointSettingsOutcome struct {
	EndpointID string `json:"endpoint_id"`
	// Services bounced onto the new config, and the Postgres settings that left
	// no other option (Postgres fixes them at server start).
	Restarted       []string `json:"restarted"`
	RestartForcedBy []string `json:"restart_forced_by"`
	// Services that took the change while running. For the compute this means
	// Postgres reloaded and kept its connections.
	Reloaded []string `json:"reloaded"`
	// Stored but not in force: compiled-in settings, and names Postgres does not
	// report. Nothing is restarted for these, so this is the only signal that an
	// edit did nothing.
	NotApplied []SettingNotApplied `json:"not_applied"`
	// Why this endpoint could not be told. The settings are stored regardless.
	Unreachable string `json:"unreachable"`
}

// SettingNotApplied is one stored setting that is not in force, and the
// controller's fixed phrase for why.
type SettingNotApplied struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// SettingsUpdateResult is the reply to either settings PATCH: stored, plus what
// the change reached. `Applied` covers the row only — the per-endpoint outcomes
// are best effort on top of a write that already happened.
type SettingsUpdateResult struct {
	Applied   bool                      `json:"applied"`
	Endpoints []EndpointSettingsOutcome `json:"endpoints"`
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
	ClusterID *string `json:"cluster_id"`
	Error     *string `json:"error"`
	// On a terminal task UpdatedAt is the transition into terminal, so
	// [CreatedAt, UpdatedAt] is the window the row spent non-terminal.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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

// --- clusters ---

// CreateCluster plans an async cluster create (202). Returns the Task and the
// Location header value.
func (c *Client) CreateCluster(ctx context.Context, poolID string, req CreateClusterRequest) (Task, string, error) {
	return c.async(ctx, http.MethodPost, c.apiURL+"/storage-pools/"+poolID+"/clusters", req)
}

// ClusterList is a page of clusters.
type ClusterList struct {
	Clusters []Cluster `json:"clusters"`
	Total    int       `json:"total"`
	Count    int       `json:"count"`
}

// ListClusters returns one page of the clusters in a pool. (Beyond the minimal
// enumerated op set, but needed to resolve the id of an asynchronously-created
// cluster by name — the same lookup deploy/smoke-test.sh does.)
func (c *Client) ListClusters(ctx context.Context, poolID string, limit, offset int) (ClusterList, error) {
	var out ClusterList
	_, _, err := c.doJSON(ctx, http.MethodGet, c.apiURL+"/storage-pools/"+poolID+"/clusters"+pageQuery(limit, offset), nil, &out)
	return out, err
}

// GetCluster reads a cluster (with its default branch).
func (c *Client) GetCluster(ctx context.Context, poolID, clusterID string) (Cluster, error) {
	var out Cluster
	_, _, err := c.doJSON(ctx, http.MethodGet, c.apiURL+"/storage-pools/"+poolID+"/clusters/"+clusterID, nil, &out)
	return out, err
}

// DeleteCluster plans an async cascade delete (202).
func (c *Client) DeleteCluster(ctx context.Context, poolID, clusterID string) (Task, string, error) {
	return c.async(ctx, http.MethodDelete, c.apiURL+"/storage-pools/"+poolID+"/clusters/"+clusterID, nil)
}

// --- branches ---

// AddBranchExtensions plans an async branch-extension add (202): it unions the
// names into the branch's set and redeploys every live endpoint on the branch.
func (c *Client) AddBranchExtensions(ctx context.Context, poolID, clusterID, branchID string, req AddBranchExtensionsRequest) (Task, string, error) {
	return c.async(ctx, http.MethodPost, c.apiURL+"/storage-pools/"+poolID+"/clusters/"+clusterID+"/branches/"+branchID+"/extensions", req)
}

// CreateBranch plans an async branch fork (202).
func (c *Client) CreateBranch(ctx context.Context, poolID, clusterID string, req CreateBranchRequest) (Task, string, error) {
	return c.async(ctx, http.MethodPost, c.apiURL+"/storage-pools/"+poolID+"/clusters/"+clusterID+"/branches", req)
}

// DeleteBranch plans an async cascade delete (202).
func (c *Client) DeleteBranch(ctx context.Context, poolID, clusterID, branchID string) (Task, string, error) {
	return c.async(ctx, http.MethodDelete, c.apiURL+"/storage-pools/"+poolID+"/clusters/"+clusterID+"/branches/"+branchID, nil)
}

// BranchList is a page of branches.
type BranchList struct {
	Branches []Branch `json:"branches"`
	Total    int      `json:"total"`
	Count    int      `json:"count"`
}

// ListBranches returns one page of the branches in a cluster (the default branch
// plus its forks). Needed to resolve the id of an asynchronously-created branch by
// name — the same lookup findCluster does for clusters.
func (c *Client) ListBranches(ctx context.Context, poolID, clusterID string, limit, offset int) (BranchList, error) {
	var out BranchList
	_, _, err := c.doJSON(ctx, http.MethodGet, c.apiURL+"/storage-pools/"+poolID+"/clusters/"+clusterID+"/branches"+pageQuery(limit, offset), nil, &out)
	return out, err
}

// ListAllBranches pages through every branch in a cluster.
func (c *Client) ListAllBranches(ctx context.Context, poolID, clusterID string) ([]Branch, error) {
	const page = 100
	var all []Branch
	for offset := 0; ; offset += page {
		list, err := c.ListBranches(ctx, poolID, clusterID, page, offset)
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

func (c *Client) endpointsPath(poolID, clusterID, branchID string) string {
	return c.apiURL + "/storage-pools/" + poolID + "/clusters/" + clusterID + "/branches/" + branchID + "/endpoints"
}

// CreateEndpoint plans an async endpoint deploy (202).
func (c *Client) CreateEndpoint(ctx context.Context, poolID, clusterID, branchID string, req CreateEndpointRequest) (Task, string, error) {
	return c.async(ctx, http.MethodPost, c.endpointsPath(poolID, clusterID, branchID), req)
}

// GetEndpoint reads an endpoint.
func (c *Client) GetEndpoint(ctx context.Context, poolID, clusterID, branchID, endpointID string) (Endpoint, error) {
	var out Endpoint
	_, _, err := c.doJSON(ctx, http.MethodGet, c.endpointsPath(poolID, clusterID, branchID)+"/"+endpointID, nil, &out)
	return out, err
}

// ListEndpoints returns one page of endpoints bound to a branch.
func (c *Client) ListEndpoints(ctx context.Context, poolID, clusterID, branchID string, limit, offset int) (EndpointList, error) {
	var out EndpointList
	_, _, err := c.doJSON(ctx, http.MethodGet, c.endpointsPath(poolID, clusterID, branchID)+pageQuery(limit, offset), nil, &out)
	return out, err
}

// DeleteEndpoint plans an async endpoint retire (202).
func (c *Client) DeleteEndpoint(ctx context.Context, poolID, clusterID, branchID, endpointID string) (Task, string, error) {
	return c.async(ctx, http.MethodDelete, c.endpointsPath(poolID, clusterID, branchID)+"/"+endpointID, nil)
}

// ResizeEndpoint plans an async in-place compute resize (202). First-writer-wins:
// while a resize of the same endpoint is non-terminal, this returns a 409
// *APIError.
func (c *Client) ResizeEndpoint(ctx context.Context, poolID, clusterID, branchID, endpointID string, req ResizeEndpointRequest) (Task, string, error) {
	return c.async(ctx, http.MethodPost, c.endpointsPath(poolID, clusterID, branchID)+"/"+endpointID+"/resize", req)
}

// UpdateBranchSettings replaces the branch-wide settings named in the body
// (sync, 200) and asks every endpoint on the branch to re-read them.
func (c *Client) UpdateBranchSettings(ctx context.Context, poolID, clusterID, branchID string, req UpdateSettingsRequest) (SettingsUpdateResult, error) {
	var out SettingsUpdateResult
	path := c.apiURL + "/storage-pools/" + poolID + "/clusters/" + clusterID + "/branches/" + branchID + "/settings"
	_, _, err := c.doJSON(ctx, http.MethodPatch, path, req, &out)
	return out, err
}

// UpdateEndpointSettings replaces one endpoint's settings (sync, 200) and asks
// that endpoint to re-read them.
func (c *Client) UpdateEndpointSettings(ctx context.Context, poolID, clusterID, branchID, endpointID string, req UpdateSettingsRequest) (SettingsUpdateResult, error) {
	var out SettingsUpdateResult
	path := c.endpointsPath(poolID, clusterID, branchID) + "/" + endpointID + "/settings"
	_, _, err := c.doJSON(ctx, http.MethodPatch, path, req, &out)
	return out, err
}

// PauseEndpoint plans an async compute pause (202): the compute is stopped while
// the endpoint record, its branch, and all of its storage stay put.
func (c *Client) PauseEndpoint(ctx context.Context, poolID, clusterID, branchID, endpointID string) (Task, string, error) {
	return c.async(ctx, http.MethodPost, c.endpointsPath(poolID, clusterID, branchID)+"/"+endpointID+"/pause", nil)
}

// ResumeEndpoint plans an async compute resume (202) of a paused endpoint on the
// same branch.
func (c *Client) ResumeEndpoint(ctx context.Context, poolID, clusterID, branchID, endpointID string) (Task, string, error) {
	return c.async(ctx, http.MethodPost, c.endpointsPath(poolID, clusterID, branchID)+"/"+endpointID+"/resume", nil)
}

// ListAllEndpoints pages through every endpoint bound to a branch.
func (c *Client) ListAllEndpoints(ctx context.Context, poolID, clusterID, branchID string) ([]Endpoint, error) {
	const page = 100
	var all []Endpoint
	for offset := 0; ; offset += page {
		list, err := c.ListEndpoints(ctx, poolID, clusterID, branchID, page, offset)
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
// getTaskRetries. Beyond the state-store 503 doJSON waits out, a decoded API
// error is not retried here — only a dropped, refused, reset or timed-out
// connection.
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

// isTransientConnErr reports whether err looks like a dropped, reset, refused
// or timed-out connection rather than a real API-level failure — the class of
// error a brief port-forward blip or an apiserver-to-kubelet hiccup produces.
func isTransientConnErr(err error) bool {
	if err == nil {
		return false
	}
	// A deadline the caller set is its own budget, not a blip. Checked first
	// because context.DeadlineExceeded reports Timeout() true and would other-
	// wise be read as one.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// The relay transport reports what kubectl printed, so the cause arrives as
	// text with no wrapped error to match on.
	msg := err.Error()
	return strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "i/o timeout")
}

// ListTasksOptions filters listTasks.
type ListTasksOptions struct {
	Limit     int
	Offset    int
	Status    string
	Kind      string
	ClusterID string
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
	if opts.ClusterID != "" {
		q.Set("cluster_id", opts.ClusterID)
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

// CreateClusterAndWait creates a cluster and waits for its task to succeed.
func (c *Client) CreateClusterAndWait(ctx context.Context, poolID string, req CreateClusterRequest) (Task, error) {
	task, loc, err := c.CreateCluster(ctx, poolID, req)
	if err != nil {
		return Task{}, err
	}
	return c.WaitTask(ctx, taskRef(task, loc))
}

// DeleteClusterAndWait deletes a cluster (cascade) and waits for success.
func (c *Client) DeleteClusterAndWait(ctx context.Context, poolID, clusterID string) (Task, error) {
	task, loc, err := c.DeleteCluster(ctx, poolID, clusterID)
	if err != nil {
		return Task{}, err
	}
	return c.WaitTask(ctx, taskRef(task, loc))
}

// AddBranchExtensionsAndWait adds branch extensions and waits for success.
func (c *Client) AddBranchExtensionsAndWait(ctx context.Context, poolID, clusterID, branchID string, req AddBranchExtensionsRequest) (Task, error) {
	task, loc, err := c.AddBranchExtensions(ctx, poolID, clusterID, branchID, req)
	if err != nil {
		return Task{}, err
	}
	return c.WaitTask(ctx, taskRef(task, loc))
}

// CreateBranchAndWait creates a branch and waits for success.
func (c *Client) CreateBranchAndWait(ctx context.Context, poolID, clusterID string, req CreateBranchRequest) (Task, error) {
	task, loc, err := c.CreateBranch(ctx, poolID, clusterID, req)
	if err != nil {
		return Task{}, err
	}
	return c.WaitTask(ctx, taskRef(task, loc))
}

// DeleteBranchAndWait deletes a branch (cascade) and waits for success.
func (c *Client) DeleteBranchAndWait(ctx context.Context, poolID, clusterID, branchID string) (Task, error) {
	task, loc, err := c.DeleteBranch(ctx, poolID, clusterID, branchID)
	if err != nil {
		return Task{}, err
	}
	return c.WaitTask(ctx, taskRef(task, loc))
}

// CreateEndpointAndWait creates an endpoint and waits for success.
func (c *Client) CreateEndpointAndWait(ctx context.Context, poolID, clusterID, branchID string, req CreateEndpointRequest) (Task, error) {
	task, loc, err := c.CreateEndpoint(ctx, poolID, clusterID, branchID, req)
	if err != nil {
		return Task{}, err
	}
	return c.WaitTask(ctx, taskRef(task, loc))
}

// ResizeEndpointAndWait resizes an endpoint and waits for its task to reach a
// terminal status. The Task is returned even when it failed (the caller reads its
// error and result), alongside the failure error.
func (c *Client) ResizeEndpointAndWait(ctx context.Context, poolID, clusterID, branchID, endpointID string, req ResizeEndpointRequest) (Task, error) {
	task, loc, err := c.ResizeEndpoint(ctx, poolID, clusterID, branchID, endpointID, req)
	if err != nil {
		return Task{}, err
	}
	return c.WaitTask(ctx, taskRef(task, loc))
}

// PauseEndpointAndWait pauses an endpoint's compute and waits for its task to
// reach a terminal status. The Task is returned even when it failed.
func (c *Client) PauseEndpointAndWait(ctx context.Context, poolID, clusterID, branchID, endpointID string) (Task, error) {
	task, loc, err := c.PauseEndpoint(ctx, poolID, clusterID, branchID, endpointID)
	if err != nil {
		return Task{}, err
	}
	return c.WaitTask(ctx, taskRef(task, loc))
}

// ResumeEndpointAndWait resumes a paused endpoint and waits for its task to
// reach a terminal status. The Task is returned even when it failed.
func (c *Client) ResumeEndpointAndWait(ctx context.Context, poolID, clusterID, branchID, endpointID string) (Task, error) {
	task, loc, err := c.ResumeEndpoint(ctx, poolID, clusterID, branchID, endpointID)
	if err != nil {
		return Task{}, err
	}
	return c.WaitTask(ctx, taskRef(task, loc))
}

// DeleteEndpointAndWait retires an endpoint and waits for success.
func (c *Client) DeleteEndpointAndWait(ctx context.Context, poolID, clusterID, branchID, endpointID string) (Task, error) {
	task, loc, err := c.DeleteEndpoint(ctx, poolID, clusterID, branchID, endpointID)
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

// doJSON retries while the controller reports its state store unavailable. Safe
// on every method: it sends that answer only for a request it established never
// reached the store, and a 500 when it cannot (db.SafeToRetry).
func (c *Client) doJSON(ctx context.Context, method, url string, body, out any) (int, http.Header, error) {
	var raw []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal %s %s: %w", method, url, err)
		}
		raw = b
	}
	deadline := time.Now().Add(c.UnavailableBudget)
	for {
		status, hdr, err := c.doJSONOnce(ctx, method, url, raw, body != nil, out)
		if !storeUnavailable(err) {
			return status, hdr, err
		}
		wait := c.retryAfter(hdr)
		if time.Now().Add(wait).After(deadline) {
			return status, hdr, err
		}
		select {
		case <-ctx.Done():
			// Keep the 503: without it a caller logs this as the controller hanging.
			return status, hdr, errors.Join(err, ctx.Err())
		case <-time.After(wait):
		}
	}
}

func storeUnavailable(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) &&
		apiErr.StatusCode == http.StatusServiceUnavailable &&
		strings.Contains(apiErr.Message, storeUnavailableMessage)
}

func (c *Client) retryAfter(hdr http.Header) time.Duration {
	fallback := c.UnavailableDelay
	if fallback <= 0 {
		fallback = DefaultUnavailableDelay
	}
	if hdr == nil {
		return fallback
	}
	secs, err := strconv.Atoi(strings.TrimSpace(hdr.Get("Retry-After")))
	if err != nil || secs <= 0 {
		return fallback
	}
	if d := time.Duration(secs) * time.Second; d < unavailableMaxWait {
		return d
	}
	return unavailableMaxWait
}

func (c *Client) doJSONOnce(ctx context.Context, method, url string, raw []byte, hasBody bool, out any) (int, http.Header, error) {
	var rdr io.Reader
	if hasBody {
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, fmt.Errorf("build %s %s: %w", method, url, err)
	}
	if hasBody {
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
