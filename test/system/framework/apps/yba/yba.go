// Package yba is the YugabyteDB Anywhere (YBA) REST app: a small, hand-written
// typed client over the YBA platform API, the stress framework's yugaware_app
// equivalent narrowed to what the ampqa framework needs to use a universe as a
// TEST TARGET. Like apps/controller it is deliberately not generated — the
// framework consumes a handful of operations and a few dozen fields, so a thin
// net/http client is simpler than wiring codegen (KISS).
//
// Auth is a static API token in the X-AUTH-YW-API-TOKEN header plus a customer
// UUID baked into every path (…/customers/{cUUID}/…). Universe create/delete
// are asynchronous: they return a taskUUID, and *AndWait helpers poll
// GET …/tasks/{tUUID} to a terminal status, mirroring controller.WaitTask.
// Types carry only the fields the framework reads and tolerate unknown fields
// (encoding/json drops them).
//
// Provider bootstrap: GetProviders / FindProvider read; AWS provider creation
// (provider.go) reuses the shared yb-dev-aws-2 keypair and never lets YBA mint
// an AWS key. GCP/Azure creation is not implemented (a clear error). Universe
// creation from a typed spec (provider.go: BuildConfigureRequest) is a single
// call to POST /universes/clusters via CreateUniverseFromSpec, which returns a
// task to poll and then WaitUniverseReady confirms the universe settled healthy.
package yba

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/pg"
)

// maxBody bounds a single response body read so a misbehaving server can't
// balloon memory in the coordinator.
const maxBody = 16 << 20 // 16 MiB (universe details JSON is large)

// Task statuses (YBA's TaskInfo.State values).
const (
	TaskCreated = "Created"
	TaskRunning = "Running"
	TaskSuccess = "Success"
	TaskFailure = "Failure"
	TaskAborted = "Aborted"
)

// Defaults.
const (
	// YBA universe tasks are slow (minutes), so the poll backoff is coarser than
	// controller's sub-second cadence.
	DefaultPollInitial = 3 * time.Second
	DefaultPollMax     = 20 * time.Second

	// DefaultYSQLPort is YugabyteDB's YSQL RPC (libpq) port.
	DefaultYSQLPort = 5433
	// DefaultYSQLUser / DefaultYSQLDatabase are YugabyteDB's built-in YSQL role
	// and database.
	DefaultYSQLUser     = "yugabyte"
	DefaultYSQLDatabase = "yugabyte"

	authHeader = "X-AUTH-YW-API-TOKEN"
	// sessionHeader carries the short-lived session token Login returns; the
	// api_token mint call authenticates with it (there is no durable API token
	// yet at that point — minting one IS the point).
	sessionHeader = "X-AUTH-TOKEN"
)

// Client is a YBA REST client. Base URL, customer UUID, API token, and the
// http.Client are all injectable (the http.Client carries TLS policy — YBA is
// commonly self-signed, so callers inject a client with the right transport).
type Client struct {
	baseURL      string // scheme://host[:port], no trailing slash
	apiURL       string // baseURL + /api/v1
	customerUUID string
	apiToken     string
	authToken    string // session token from Login (bootstrap only); mints the durable apiToken
	hc           *http.Client
	logf         func(format string, args ...any)

	// PollInitial/PollMax bound the async-task poll backoff (settable for tests).
	PollInitial time.Duration
	PollMax     time.Duration
	// BootstrapRetry is the poll cadence Bootstrap waits between register→login→
	// mint attempts while the pod is still coming up (settable for tests).
	BootstrapRetry time.Duration
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient injects the http.Client (timeouts, TLS transport, test server).
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

// WithPoll overrides the async-task poll backoff bounds.
func WithPoll(initial, max time.Duration) Option {
	return func(c *Client) { c.PollInitial, c.PollMax = initial, max }
}

// WithLogf injects the log sink (default log.Printf). Pass a masked sink
// (core.Masker.Logf) so a resolved secret never survives into a log line — this
// client never logs credential material itself, but a masked sink is defence in
// depth.
func WithLogf(logf func(format string, args ...any)) Option {
	return func(c *Client) { c.logf = logf }
}

// New builds a Client for the YBA rooted at baseURL (e.g.
// "https://yba.example.com") scoped to customerUUID, authenticating with
// apiToken.
func New(baseURL, customerUUID, apiToken string, opts ...Option) *Client {
	b := strings.TrimRight(baseURL, "/")
	c := &Client{
		baseURL:        b,
		apiURL:         b + "/api/v1",
		customerUUID:   customerUUID,
		apiToken:       apiToken,
		hc:             http.DefaultClient,
		logf:           log.Printf,
		PollInitial:    DefaultPollInitial,
		PollMax:        DefaultPollMax,
		BootstrapRetry: DefaultBootstrapRetry,
	}
	for _, o := range opts {
		o(c)
	}
	if c.hc == nil {
		c.hc = http.DefaultClient
	}
	return c
}

func (c *Client) log(format string, args ...any) {
	if c.logf != nil {
		c.logf(format, args...)
	}
}

// BaseURL is the YBA root URL.
func (c *Client) BaseURL() string { return c.baseURL }

// CustomerUUID is the customer scope of this client.
func (c *Client) CustomerUUID() string { return c.customerUUID }

// --- resource types (only the fields the framework consumes) ---

// Universe is a YBA universe with the node/cluster details the framework reads.
type Universe struct {
	UniverseUUID    string          `json:"universeUUID"`
	Name            string          `json:"name"`
	UniverseDetails UniverseDetails `json:"universeDetails"`
}

// UniverseDetails carries the node set and clusters of a universe.
type UniverseDetails struct {
	NodeDetailsSet   []NodeDetails `json:"nodeDetailsSet"`
	Clusters         []Cluster     `json:"clusters"`
	UpdateSucceeded  bool          `json:"updateSucceeded"`
	UpdateInProgress bool          `json:"updateInProgress"`
}

// IsReady reports whether the universe finished its create/edit successfully: no
// update in flight and the last update succeeded. A freshly-submitted create has
// updateInProgress=true until the task's UniverseUpdateSucceeded subtask runs.
func (u Universe) IsReady() bool {
	return u.UniverseDetails.UpdateSucceeded && !u.UniverseDetails.UpdateInProgress
}

// NodeDetails is one universe node. YSQLServerRpcPort is the libpq port; it
// defaults to 5433 when absent.
type NodeDetails struct {
	NodeName          string    `json:"nodeName"`
	CloudInfo         CloudInfo `json:"cloudInfo"`
	State             string    `json:"state"`
	IsTserver         bool      `json:"isTserver"`
	IsMaster          bool      `json:"isMaster"`
	YSQLServerRpcPort int       `json:"ysqlServerRpcPort"`
}

// CloudInfo is a node's addressing.
type CloudInfo struct {
	PrivateIP string `json:"private_ip"`
	PublicIP  string `json:"public_ip"`
	Region    string `json:"region"`
	Zone      string `json:"az"`
}

// Cluster is a universe cluster; only userIntent (for the software version) is
// consumed.
type Cluster struct {
	UserIntent UserIntent `json:"userIntent"`
}

// UserIntent carries the software version off a cluster.
type UserIntent struct {
	YBSoftwareVersion string `json:"ybSoftwareVersion"`
	UniverseName      string `json:"universeName"`
}

// Version returns the universe's YugabyteDB software version from the first
// cluster that declares one, or "".
func (u Universe) Version() string {
	for _, cl := range u.UniverseDetails.Clusters {
		if cl.UserIntent.YBSoftwareVersion != "" {
			return cl.UserIntent.YBSoftwareVersion
		}
	}
	return ""
}

// TaskRef is the async-operation reference returned by create/delete.
type TaskRef struct {
	TaskUUID     string `json:"taskUUID"`
	ResourceUUID string `json:"resourceUUID"`
}

// TaskStatus is one task's progress (GET …/tasks/{tUUID}).
type TaskStatus struct {
	Status  string  `json:"status"`
	Percent float64 `json:"percent"`
	Title   string  `json:"title"`
}

// Provider is a YBA cloud provider (read-only in this app).
type Provider struct {
	UUID string `json:"uuid"`
	Code string `json:"code"`
	Name string `json:"name"`
}

// HealthCheck is one node's health-check payload. The framework treats it as
// opaque beyond the node identity, so only NodeName is typed; the rest is kept
// as raw JSON.
type HealthCheck struct {
	NodeName string          `json:"node_name"`
	NodeData json.RawMessage `json:"node_data"`
}

// APIError is a non-2xx response decoded from YBA's error envelope.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("yba %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("yba %d", e.StatusCode)
}

// --- universes ---

// ListUniverses returns every universe for the customer.
func (c *Client) ListUniverses(ctx context.Context) ([]Universe, error) {
	var out []Universe
	_, err := c.doJSON(ctx, http.MethodGet, c.custURL("/universes"), nil, &out)
	return out, err
}

// GetUniverse resolves a universe by UUID or by name. It lists universes and
// matches ref against UniverseUUID first, then Name — one code path that works
// for either reference form and tolerates YBA's UUID formatting.
func (c *Client) GetUniverse(ctx context.Context, ref string) (Universe, error) {
	list, err := c.ListUniverses(ctx)
	if err != nil {
		return Universe{}, err
	}
	for _, u := range list {
		if u.UniverseUUID == ref {
			return u, nil
		}
	}
	for _, u := range list {
		if u.Name == ref {
			return u, nil
		}
	}
	return Universe{}, fmt.Errorf("universe %q not found among %d universes", ref, len(list))
}

// CreateUniverse plans an async universe create. spec is the raw universe
// configuration payload (the same shape YBA's UI/CLI posts); the framework
// stays agnostic of its full structure. Returns the task reference.
func (c *Client) CreateUniverse(ctx context.Context, spec map[string]any) (TaskRef, error) {
	var out TaskRef
	_, err := c.doJSON(ctx, http.MethodPost, c.custURL("/universes"), spec, &out)
	return out, err
}

// DeleteUniverse plans an async universe delete. Returns the task reference.
func (c *Client) DeleteUniverse(ctx context.Context, universeID string) (TaskRef, error) {
	var out TaskRef
	_, err := c.doJSON(ctx, http.MethodDelete, c.custURL("/universes/"+universeID), nil, &out)
	return out, err
}

// SetRuntimeConfig sets a runtime-config key at a scope (PUT text/plain, the raw
// value — RuntimeConfController.setKey rejects application/json). scopeUUID is a
// provider / universe / customer / global scope UUID.
func (c *Client) SetRuntimeConfig(ctx context.Context, scopeUUID, key, value string) error {
	url := c.custURL("/runtime_config/" + scopeUUID + "/key/" + key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(value))
	if err != nil {
		return fmt.Errorf("build set runtime config %s: %w", key, err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set(authHeader, c.apiToken)
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("set runtime config %s: %w", key, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseAPIError(resp.StatusCode, data)
	}
	return nil
}

// SetYbcEnabledForProvider toggles ybc.provider.enabled on a provider. Disabling
// it makes YBA skip YBC install and the WaitForYbcServer subtask for universes on
// that provider (UniverseCRUDHandler gates create-time YBC on this key). The
// foundation workload runs via amp compute and needs no YBC.
func (c *Client) SetYbcEnabledForProvider(ctx context.Context, providerUUID string, enabled bool) error {
	return c.SetRuntimeConfig(ctx, providerUUID, "ybc.provider.enabled", strconv.FormatBool(enabled))
}

// ForceDeleteUniverse destroys a universe with isForceDelete=true so the cloud
// nodes are terminated even for a FAILED or locked universe — a plain delete
// no-ops or errors on those, stranding the instances. (UniverseController.destroy
// isForceDelete/isDeleteBackups/isDeleteAssociatedCerts.)
func (c *Client) ForceDeleteUniverse(ctx context.Context, universeID string) (TaskRef, error) {
	var out TaskRef
	url := c.custURL("/universes/" + universeID + "?isForceDelete=true&isDeleteBackups=false&isDeleteAssociatedCerts=false")
	_, err := c.doJSON(ctx, http.MethodDelete, url, nil, &out)
	return out, err
}

// ForceDeleteUniverseAndWait force-deletes a universe and waits for the destroy
// task, so node termination is confirmed before the caller proceeds.
func (c *Client) ForceDeleteUniverseAndWait(ctx context.Context, universeID string) (TaskRef, error) {
	ref, err := c.ForceDeleteUniverse(ctx, universeID)
	if err != nil {
		return TaskRef{}, err
	}
	_, err = c.WaitTask(ctx, ref.TaskUUID)
	return ref, err
}

// UniverseHealth triggers and returns the universe's health-check results (one
// entry per node). An empty slice means the check reported nothing.
func (c *Client) UniverseHealth(ctx context.Context, universeID string) ([]HealthCheck, error) {
	var out []HealthCheck
	_, err := c.doJSON(ctx, http.MethodGet, c.custURL("/universes/"+universeID+"/health_check"), nil, &out)
	return out, err
}

// --- tasks ---

// GetTask reads one task's progress.
func (c *Client) GetTask(ctx context.Context, taskUUID string) (TaskStatus, error) {
	var out TaskStatus
	_, err := c.doJSON(ctx, http.MethodGet, c.custURL("/tasks/"+taskUUID), nil, &out)
	return out, err
}

// WaitTask polls GET …/tasks/{tUUID} with capped exponential backoff under ctx
// until the task reaches a terminal status. A failed or aborted task returns an
// error carrying the task title.
func (c *Client) WaitTask(ctx context.Context, taskUUID string) (TaskStatus, error) {
	if taskUUID == "" {
		return TaskStatus{}, fmt.Errorf("wait task: empty task uuid")
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
		task, err := c.GetTask(ctx, taskUUID)
		if err != nil {
			return TaskStatus{}, err
		}
		switch task.Status {
		case TaskSuccess:
			return task, nil
		case TaskFailure, TaskAborted:
			msg := fmt.Sprintf("task %s (%s) ended %s", taskUUID, task.Title, task.Status)
			if subs := c.failedSubtaskErrors(ctx, taskUUID); len(subs) > 0 {
				msg += ": " + strings.Join(subs, "; ")
			}
			return task, fmt.Errorf("%s", msg)
		}
		select {
		case <-ctx.Done():
			return TaskStatus{}, fmt.Errorf("waiting for task %s: %w", taskUUID, ctx.Err())
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// failedSubtaskErrors fetches the errorString of each failed subtask of a task
// (GET …/tasks/{uuid}/failed) so a task Failure carries YBA's real reason — an
// EC2 provisioning error, a preflight rejection — instead of just the task
// title. Best-effort: an unreachable endpoint (e.g. a cancelled ctx) yields nil
// and the caller keeps the task-title error.
func (c *Client) failedSubtaskErrors(ctx context.Context, taskUUID string) []string {
	var out struct {
		FailedSubTasks []struct {
			ErrorString      string `json:"errorString"`
			SubTaskGroupType string `json:"subTaskGroupType"`
		} `json:"failedSubTasks"`
	}
	if _, err := c.doJSON(ctx, http.MethodGet, c.custURL("/tasks/"+taskUUID+"/failed"), nil, &out); err != nil {
		return nil
	}
	var msgs []string
	for _, st := range out.FailedSubTasks {
		if s := strings.TrimSpace(st.ErrorString); s != "" {
			if gt := strings.TrimSpace(st.SubTaskGroupType); gt != "" {
				s = gt + ": " + s
			}
			msgs = append(msgs, s)
		}
	}
	return msgs
}

// CreateUniverseAndWait creates a universe and waits for its task to succeed.
func (c *Client) CreateUniverseAndWait(ctx context.Context, spec map[string]any) (TaskRef, error) {
	ref, err := c.CreateUniverse(ctx, spec)
	if err != nil {
		return TaskRef{}, err
	}
	_, err = c.WaitTask(ctx, ref.TaskUUID)
	return ref, err
}

// DeleteUniverseAndWait deletes a universe and waits for its task to succeed.
func (c *Client) DeleteUniverseAndWait(ctx context.Context, universeID string) (TaskRef, error) {
	ref, err := c.DeleteUniverse(ctx, universeID)
	if err != nil {
		return TaskRef{}, err
	}
	_, err = c.WaitTask(ctx, ref.TaskUUID)
	return ref, err
}

// --- providers ---

// GetProviders lists the customer's cloud providers. Provider CREATION lives in
// provider.go.
func (c *Client) GetProviders(ctx context.Context) ([]Provider, error) {
	var out []Provider
	_, err := c.doJSON(ctx, http.MethodGet, c.custURL("/providers"), nil, &out)
	return out, err
}

// FindProvider returns the provider matching name (exact), or an error.
func (c *Client) FindProvider(ctx context.Context, name string) (Provider, error) {
	ps, err := c.GetProviders(ctx)
	if err != nil {
		return Provider{}, err
	}
	for _, p := range ps {
		if p.Name == name {
			return p, nil
		}
	}
	return Provider{}, fmt.Errorf("provider %q not found", name)
}

// --- connection extraction ---

// YSQLConnInfo extracts a YSQL connection target from a universe: the ConnInfo
// points at the first live tserver (private IP, its ysqlServerRpcPort or 5433),
// and the returned slice lists every live tserver as host:port. A universe with
// no live tserver is an error.
func YSQLConnInfo(u Universe) (pg.ConnInfo, []string, error) {
	var ci pg.ConnInfo
	var addrs []string
	for _, n := range u.UniverseDetails.NodeDetailsSet {
		if !n.IsTserver {
			continue
		}
		host := n.CloudInfo.PrivateIP
		if host == "" {
			host = n.CloudInfo.PublicIP
		}
		if host == "" {
			continue
		}
		if !isLive(n.State) {
			continue
		}
		port := n.YSQLServerRpcPort
		if port == 0 {
			port = DefaultYSQLPort
		}
		addrs = append(addrs, fmt.Sprintf("%s:%d", host, port))
		if ci.Host == "" {
			ci = pg.ConnInfo{
				Host:     host,
				Port:     port,
				User:     DefaultYSQLUser,
				Database: DefaultYSQLDatabase,
			}
		}
	}
	if ci.Host == "" {
		return pg.ConnInfo{}, nil, fmt.Errorf("universe %q has no live tserver with an address", u.Name)
	}
	return ci, addrs, nil
}

// isLive reports whether a node state counts as serving. YBA uses "Live" for a
// healthy node; an empty state (some payloads omit it) is treated as live so a
// sparse fixture still yields a target.
func isLive(state string) bool {
	return state == "" || strings.EqualFold(state, "Live")
}

// --- transport helpers ---

func (c *Client) custURL(suffix string) string {
	return c.apiURL + "/customers/" + c.customerUUID + suffix
}

// doJSON issues a request authenticated with the static API token
// (X-AUTH-YW-API-TOKEN) — the normal path for every customer-scoped operation.
func (c *Client) doJSON(ctx context.Context, method, url string, body, out any) (int, error) {
	return c.do(ctx, method, url, map[string]string{authHeader: c.apiToken}, body, out)
}

// do is the shared transport: it marshals body (if any), sets the given auth
// headers, executes under ctx, bounds the response read, and decodes a 2xx JSON
// body into out (non-2xx → *APIError). headers lets bootstrap use the session
// header (X-AUTH-TOKEN) instead of the API-token header.
func (c *Client) do(ctx context.Context, method, url string, headers map[string]string, body, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("marshal %s %s: %w", method, url, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, fmt.Errorf("build %s %s: %w", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read %s %s: %w", method, url, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out != nil && len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return resp.StatusCode, fmt.Errorf("decode %s %s: %w", method, url, err)
			}
		}
		return resp.StatusCode, nil
	}
	return resp.StatusCode, parseAPIError(resp.StatusCode, data)
}

// parseAPIError decodes YBA's error shape. YBA returns either {"error": "msg"}
// or {"error": {...}} or {"success":false,"error":...}; it also sometimes
// returns a bare string. This tolerates all of them.
func parseAPIError(status int, data []byte) *APIError {
	e := &APIError{StatusCode: status}
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &env) == nil && len(env.Error) > 0 {
		var s string
		if json.Unmarshal(env.Error, &s) == nil {
			e.Message = s
		} else {
			e.Message = strings.TrimSpace(string(env.Error))
		}
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(data))
	}
	return e
}
