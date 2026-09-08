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
// supplies) and create databases (clusters) inside the pool.
//
// User and pool creation are synchronous on the controller; cluster creation is
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
		baseURL: NormalizeAMPBaseURL(baseURL),
		http:    httpClient,
	}
}

// NormalizeAMPBaseURL trims trailing slashes and defaults a missing scheme to
// http:// (the AMP controller speaks plain HTTP today). Without this a bare host
// registered in meko_system.amp_registration (e.g. "100.84.82.87") produces
// `unsupported protocol scheme ""` on the first request. We test for "://"
// rather than url.Parse because a bare "host:port" parses with the host as the
// scheme, which url.Parse would not flag as missing.
//
// The registration handler applies this on the write path too, so the stored
// row (and thus meko_admin's "visit the controller" link, which renders the URL
// verbatim) is normalized; this constructor keeps repairing any row that
// predates that.
func NormalizeAMPBaseURL(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if s == "" || strings.Contains(s, "://") {
		return s
	}
	return "http://" + s
}

/* ---- wire types (subset of the controller's schema) ---- */

// AMPBranch is a Postgres timeline within a cluster (the default branch is
// "main"; forks carry parent_branch_id + fork_lsn).
type AMPBranch struct {
	BranchID  string `json:"branch_id"`
	ClusterID string `json:"cluster_id"`
	Name      string `json:"name"`
	PgVersion int    `json:"pg_version"`
	IsDefault bool   `json:"is_default"`
	ForkLSN   string `json:"fork_lsn,omitempty"`
	CreatedAt string `json:"created_at"`
}

// AMPCluster is an AMP "database": one cluster with a default branch.
type AMPCluster struct {
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

type ampClusterList struct {
	Clusters []AMPCluster `json:"clusters"`
}

type ampTask struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	Status    string  `json:"status"`
	ClusterID *string `json:"cluster_id"`
	TargetRef *string `json:"target_ref"`
	Error     *string `json:"error"`
	// Result is the task's final step result as opaque JSON. For a
	// resize_endpoint task it carries the in-place outcome
	// ({"in_place_resize":..., "in_place_message":...}); nil otherwise.
	Result json.RawMessage `json:"result,omitempty"`
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

// ListClusters returns the databases (clusters) in a pool.
func (c *AMPClient) ListClusters(ctx context.Context, poolID string) ([]AMPCluster, error) {
	var list ampClusterList
	err := c.do(ctx, http.MethodGet, "/storage-pools/"+poolID+"/clusters?limit=1000", nil, &list)
	return list.Clusters, err
}

// GetCluster reads a single database (cluster) by id.
func (c *AMPClient) GetCluster(ctx context.Context, poolID, clusterID string) (AMPCluster, error) {
	var p AMPCluster
	err := c.do(ctx, http.MethodGet, "/storage-pools/"+poolID+"/clusters/"+clusterID, nil, &p)
	return p, err
}

// DeleteCluster deletes a database, cascading to its branches and endpoints.
// The controller returns a task; we poll it to completion (a delete that is
// rejected synchronously — e.g. a 409 conflict — surfaces as an error here).
func (c *AMPClient) DeleteCluster(ctx context.Context, poolID, clusterID string) error {
	var task ampTask
	if err := c.do(ctx, http.MethodDelete, "/storage-pools/"+poolID+"/clusters/"+clusterID, nil, &task); err != nil {
		return err
	}
	_, err := c.pollTask(ctx, task.ID)
	return err
}

// DeleteClusterAsync issues the delete but does NOT poll the resulting task.
// Used for bulk teardown (datapack deletion), where blocking on each cascade to
// finish would make deleting a datapack take minutes. Synchronous rejections
// (404 / 409 / 5xx) still surface here; the cascade itself runs on the controller.
func (c *AMPClient) DeleteClusterAsync(ctx context.Context, poolID, clusterID string) error {
	var task ampTask
	return c.do(ctx, http.MethodDelete, "/storage-pools/"+poolID+"/clusters/"+clusterID, nil, &task)
}

// createCluster POSTs a create_cluster request, polls the task to completion,
// and reads the materialized cluster back. Shared by the cluster-only and
// with_writer variants — they differ only in the request body.
func (c *AMPClient) createCluster(ctx context.Context, poolID string, body map[string]any) (AMPCluster, error) {
	var task ampTask
	if err := c.do(ctx, http.MethodPost, "/storage-pools/"+poolID+"/clusters", body, &task); err != nil {
		return AMPCluster{}, err
	}
	done, err := c.pollTask(ctx, task.ID)
	if err != nil {
		return AMPCluster{}, err
	}
	if done.ClusterID == nil || *done.ClusterID == "" {
		return AMPCluster{}, fmt.Errorf("amp create_cluster task %s returned no cluster id", task.ID)
	}
	var p AMPCluster
	err = c.do(ctx, http.MethodGet, "/storage-pools/"+poolID+"/clusters/"+*done.ClusterID, nil, &p)
	return p, err
}

// ampDefaultEngine is the controller's default cluster engine (a plain Postgres
// with no engine-bundled extensions). We omit `engine` from the request when it
// is empty or this default so the common path (e.g. the MCP tool) sends the same
// body it always has — the controller applies the same default either way.
const ampDefaultEngine = "postgres"

// applyClusterEngineExtensions sets the optional `engine` and `extensions`
// fields on a create_cluster body. Both are opt-in: engine is sent only for a
// non-default engine, extensions only when at least one non-blank name is
// picked. Extensions compose with the engine's own bundle on the controller, so
// they are independent (e.g. pgvector on a plain postgres cluster).
func applyClusterEngineExtensions(body map[string]any, engine string, extensions []string) {
	if e := strings.TrimSpace(engine); e != "" && e != ampDefaultEngine {
		body["engine"] = e
	}
	clean := make([]string, 0, len(extensions))
	for _, x := range extensions {
		if x = strings.TrimSpace(x); x != "" {
			clean = append(clean, x)
		}
	}
	if len(clean) > 0 {
		body["extensions"] = clean
	}
}

// CreateCluster creates a database in a pool (no compute), polling the task to
// completion and reading the materialized cluster back. engine/extensions are
// optional (see applyClusterEngineExtensions).
func (c *AMPClient) CreateCluster(ctx context.Context, poolID, name string, pgVersion *int, engine string, extensions []string) (AMPCluster, error) {
	body := map[string]any{"name": name}
	if pgVersion != nil {
		body["pg_version"] = *pgVersion
	}
	applyClusterEngineExtensions(body, engine, extensions)
	return c.createCluster(ctx, poolID, body)
}

// CreateClusterWithWriter creates a database AND its `main`-branch read_write
// endpoint in ONE background task (with_writer). Like CreateCluster it polls the
// task to completion and returns the materialized cluster — the initial cluster +
// writer provisions in well under a minute, so we wait (consistent with every
// other Meko→controller call) rather than leave the caller guessing. cpu/memory
// size the writer; omit either to take AMP's default (2 cores / 4Gi).
func (c *AMPClient) CreateClusterWithWriter(ctx context.Context, poolID, name string, pgVersion *int, cpu, memory, engine string, extensions []string) (AMPCluster, error) {
	body := map[string]any{"name": name, "with_writer": true}
	if pgVersion != nil {
		body["pg_version"] = *pgVersion
	}
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
	applyClusterEngineExtensions(body, engine, extensions)
	return c.createCluster(ctx, poolID, body)
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

// ListBranches returns the branches of a database (cluster).
func (c *AMPClient) ListBranches(ctx context.Context, poolID, clusterID string) ([]AMPBranch, error) {
	var list ampBranchList
	err := c.do(ctx, http.MethodGet, "/storage-pools/"+poolID+"/clusters/"+clusterID+"/branches?limit=1000", nil, &list)
	return list.Branches, err
}

// GetBranch reads a single branch by id.
func (c *AMPClient) GetBranch(ctx context.Context, poolID, clusterID, branchID string) (AMPBranch, error) {
	var b AMPBranch
	err := c.do(ctx, http.MethodGet, "/storage-pools/"+poolID+"/clusters/"+clusterID+"/branches/"+branchID, nil, &b)
	return b, err
}

// DeleteBranch deletes a branch, cascading to its endpoints (task-based: poll to
// completion). A branch with child branches is rejected by the controller
// (409), surfaced as an error here.
func (c *AMPClient) DeleteBranch(ctx context.Context, poolID, clusterID, branchID string) error {
	var task ampTask
	if err := c.do(ctx, http.MethodDelete, "/storage-pools/"+poolID+"/clusters/"+clusterID+"/branches/"+branchID, nil, &task); err != nil {
		return err
	}
	_, err := c.pollTask(ctx, task.ID)
	return err
}

// CreateBranch forks a branch (task-based: poll, then read back via target_ref).
func (c *AMPClient) CreateBranch(ctx context.Context, poolID, clusterID string, in CreateBranchInput) (AMPBranch, error) {
	base := "/storage-pools/" + poolID + "/clusters/" + clusterID + "/branches"
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

// AMPResources is the compute sizing (Kubernetes quantities) reserved for an
// endpoint's pod: cpu e.g. "2"/"1500m", memory e.g. "4Gi".
type AMPResources struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// AMPEndpoint is a compute instance bound to a branch. address is host:port,
// empty until deployed.
type AMPEndpoint struct {
	ID        string `json:"id"`
	ClusterID string `json:"cluster_id"`
	BranchID  string `json:"branch_id"`
	Role      string `json:"role"`
	Address   string `json:"address"`
	// TailnetAddress is AMP's Tailscale (host:port) address, empty unless the
	// endpoint Service is tailnet-exposed. Part of AMP's Endpoint contract.
	TailnetAddress string `json:"tailnet_address"`
	// Resources is the endpoint's current compute reservation, surfaced so the
	// UI can show cpu/memory and pre-fill a resize. Nil if the controller omits
	// it.
	Resources *AMPResources `json:"resources,omitempty"`
	CreatedAt string        `json:"created_at"`
}

type ampEndpointList struct {
	Endpoints []AMPEndpoint `json:"endpoints"`
}

// ListEndpoints returns the compute instances on a branch.
func (c *AMPClient) ListEndpoints(ctx context.Context, poolID, clusterID, branchID string) ([]AMPEndpoint, error) {
	var list ampEndpointList
	err := c.do(ctx, http.MethodGet, "/storage-pools/"+poolID+"/clusters/"+clusterID+"/branches/"+branchID+"/endpoints?limit=1000", nil, &list)
	return list.Endpoints, err
}

// GetEndpoint reads a single compute instance (endpoint) by id.
func (c *AMPClient) GetEndpoint(ctx context.Context, poolID, clusterID, branchID, endpointID string) (AMPEndpoint, error) {
	var e AMPEndpoint
	err := c.do(ctx, http.MethodGet, "/storage-pools/"+poolID+"/clusters/"+clusterID+"/branches/"+branchID+"/endpoints/"+endpointID, nil, &e)
	return e, err
}

// DeleteEndpoint retires a compute instance (task-based: poll to completion).
func (c *AMPClient) DeleteEndpoint(ctx context.Context, poolID, clusterID, branchID, endpointID string) error {
	var task ampTask
	if err := c.do(ctx, http.MethodDelete, "/storage-pools/"+poolID+"/clusters/"+clusterID+"/branches/"+branchID+"/endpoints/"+endpointID, nil, &task); err != nil {
		return err
	}
	_, err := c.pollTask(ctx, task.ID)
	return err
}

// CreateEndpoint provisions compute on a branch (task-based: poll, then read
// back via target_ref).
func (c *AMPClient) CreateEndpoint(ctx context.Context, poolID, clusterID, branchID, role, cpu, memory string) (AMPEndpoint, error) {
	base := "/storage-pools/" + poolID + "/clusters/" + clusterID + "/branches/" + branchID + "/endpoints"
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

// ResizeResult is the outcome of an in-place resize: the endpoint at its new
// size, plus whether the live pod resize applied. Outcome is one of
// "completed" | "pending" | "infeasible" | "unsupported" (defaults to
// "completed" when the controller reports none); Message is the controller's
// human note, if any.
type ResizeResult struct {
	Endpoint AMPEndpoint `json:"endpoint"`
	Outcome  string      `json:"outcome"`
	Message  string      `json:"message,omitempty"`
}

// ResizeEndpoint changes a compute instance's cpu/memory in place. The
// controller records a resize_endpoint task (resize the running pod, or fall
// back to a rollout); we poll it to completion, read the in-place outcome from
// the task result, then read the endpoint back at its new size. Send only the
// fields we have so AMP leaves the rest unchanged.
func (c *AMPClient) ResizeEndpoint(ctx context.Context, poolID, clusterID, branchID, endpointID, cpu, memory string) (ResizeResult, error) {
	base := "/storage-pools/" + poolID + "/clusters/" + clusterID + "/branches/" + branchID + "/endpoints/" + endpointID
	res := map[string]any{}
	if cpu != "" {
		res["cpu"] = cpu
	}
	if memory != "" {
		res["memory"] = memory
	}
	var task ampTask
	if err := c.do(ctx, http.MethodPost, base+"/resize", map[string]any{"resources": res}, &task); err != nil {
		return ResizeResult{}, err
	}
	done, err := c.pollTask(ctx, task.ID)
	if err != nil {
		return ResizeResult{}, err
	}
	outcome, message := "completed", ""
	if len(done.Result) > 0 {
		var r struct {
			InPlaceResize  string `json:"in_place_resize"`
			InPlaceMessage string `json:"in_place_message"`
		}
		if json.Unmarshal(done.Result, &r) == nil {
			if r.InPlaceResize != "" {
				outcome = r.InPlaceResize
			}
			message = r.InPlaceMessage
		}
	}
	var e AMPEndpoint
	if err := c.do(ctx, http.MethodGet, base, nil, &e); err != nil {
		return ResizeResult{}, err
	}
	return ResizeResult{Endpoint: e, Outcome: outcome, Message: message}, nil
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
