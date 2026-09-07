// Package ycpclient is the reconcile agent's client for the YCP control plane:
// enroll as the first check-in, pull desired state, and post status heartbeats.
// Every call authenticates with the environment's YBM API key, sent as a bearer;
// the base URL is YBM's agent pass-through
// (https://<ybm>/api/public/v1/accounts/<id>/ycp) or, in dev, the control plane's
// own /api/v1 (see SetDevHeaders). It speaks the same /agent/* protocol and
// {"data","error"} envelope as the meko-agent, and implements the reconcile
// engine's Source and Reporter so the engine can drive its loop directly.
package ycpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"meko/ycp/internal/api"
)

// ErrUnauthorized is returned when the control plane rejects the API key (HTTP
// 401): the key was revoked. Not retryable with the credentials the agent holds --
// the caller logs loudly and keeps ticking, so a re-issued key (a redeploy with a
// fresh Secret) recovers.
var ErrUnauthorized = fmt.Errorf("ycp: unauthorized")

// Client talks to one YCP control plane.
type Client struct {
	base   string
	http   *http.Client
	apiKey string
	// Dev mode: sent instead of the bearer, for an agent pointed straight at a
	// standalone control plane (no YBM to validate a key). See SetDevHeaders.
	devAdminSecret string
	devAPIKeyID    string
}

// New builds a client against baseURL (trailing slash trimmed), authenticating
// every call with apiKey as a bearer. baseURL carries the full API base -- agent
// routes are appended as /agent/*. A nil httpClient gets a 15s-timeout default.
func New(baseURL, apiKey string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), apiKey: apiKey, http: httpClient}
}

// SetDevHeaders switches the client to dev mode: instead of the bearer it sends
// the control plane's admin secret plus a chosen key id -- the headers YBM would
// have stamped after validating a real key. This is what lets an agent run
// against a standalone control plane, where no YBM exists to mint or validate
// keys (the e2e, the local scripts, an operator's dev loop).
func (c *Client) SetDevHeaders(adminSecret, apiKeyID string) {
	c.devAdminSecret, c.devAPIKeyID = adminSecret, apiKeyID
}

// Enroll is the agent's first check-in: it binds the live agent to the
// environment its API key was registered with and returns the environment id and
// the stack to run. Called at every startup -- it is idempotent, so there is
// nothing to persist between restarts.
func (c *Client) Enroll(ctx context.Context, req api.EnrollRequest) (api.EnrollResponse, error) {
	var out api.EnrollResponse
	code, err := c.do(ctx, http.MethodPost, "/agent/enroll", req, &out)
	if err != nil {
		return api.EnrollResponse{}, err
	}
	if code == http.StatusUnauthorized {
		return api.EnrollResponse{}, ErrUnauthorized
	}
	if code != http.StatusOK {
		return api.EnrollResponse{}, fmt.Errorf("ycp: enroll -> HTTP %d", code)
	}
	if out.EnvironmentID == "" {
		return api.EnrollResponse{}, fmt.Errorf("ycp: enroll response missing environment_id")
	}
	return out, nil
}

// Desired fetches the environment's desired state. Returns ErrUnauthorized on a 401.
func (c *Client) Desired(ctx context.Context) (api.DesiredState, error) {
	var ds api.DesiredState
	code, err := c.do(ctx, http.MethodGet, "/agent/desired-state", nil, &ds)
	if err != nil {
		return api.DesiredState{}, err
	}
	if code == http.StatusUnauthorized {
		return api.DesiredState{}, ErrUnauthorized
	}
	if code >= 300 {
		return api.DesiredState{}, fmt.Errorf("ycp: desired-state -> HTTP %d", code)
	}
	return ds, nil
}

// PostStatus posts a status heartbeat and reports whether the task delta it carried
// was stored.
//
// tasksAccepted is false only when the control plane took the report but could not
// store the tasks -- a 200 whose body says so. The caller must then hold that delta
// rather than retiring it, because a delta is delivered once and a dropped
// transition cannot be recovered from the feed (see ampbackend.TaskCollector).
//
// It defaults to TRUE for anything without that body: a 204 (the common path, and
// what a control plane predating task reporting always answers). Defaulting the
// other way would wedge an agent against such a control plane -- it would re-send
// the same delta forever, never advancing its watermark, for tasks nothing is going
// to store or display.
func (c *Client) PostStatus(ctx context.Context, r api.StatusReport) (tasksAccepted bool, err error) {
	var body struct {
		TasksRecorded *bool `json:"tasks_recorded"`
	}
	code, err := c.do(ctx, http.MethodPost, "/agent/status", r, &body)
	if err != nil {
		return false, err
	}
	if code == http.StatusUnauthorized {
		return false, ErrUnauthorized
	}
	if code >= 300 {
		return false, fmt.Errorf("ycp: status -> HTTP %d", code)
	}
	if body.TasksRecorded != nil {
		return *body.TasksRecorded, nil
	}
	return true, nil
}

// do issues one request and decodes the {"data","error"} envelope. It returns
// the HTTP status code so callers can branch on 401.
func (c *Client) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("ycp: marshal request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return 0, fmt.Errorf("ycp: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.devAdminSecret != "" && c.devAPIKeyID != "" {
		req.Header.Set("X-Meko-Admin-Secret", c.devAdminSecret)
		req.Header.Set("X-Meko-Api-Key-ID", c.devAPIKeyID)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("ycp: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return resp.StatusCode, nil
	}
	if out != nil && len(raw) > 0 {
		if err := decodeBody(raw, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// decodeBody unwraps the api_server {"data","error"} envelope, identified by the
// "data" key alone ("error" is omitempty and dropped on success). A flat payload
// with no "data" key is decoded as-is.
func decodeBody(raw []byte, out any) error {
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil {
		return json.Unmarshal(raw, out)
	}
	data, ok := probe["data"]
	if !ok {
		return json.Unmarshal(raw, out)
	}
	if errRaw, ok := probe["error"]; ok {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(errRaw, &e) == nil && e.Message != "" {
			return fmt.Errorf("%s: %s", e.Code, e.Message)
		}
	}
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	return json.Unmarshal(data, out)
}
