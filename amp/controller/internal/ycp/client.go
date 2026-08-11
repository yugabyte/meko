// Package ycp is the amp controller's client for the YCP fleet control plane
// (meko api_server --control-plane-mode). The controller registers itself as an
// "amp" environment — exchanging a one-time enrollment token for a long-lived,
// environment-scoped status token — and then reports the storage pools,
// databases, branches, and compute endpoints it owns on a periodic heartbeat.
//
// This is the register + report half only. Reconciling YCP-declared desired
// state back into the cluster is a later phase (ac-ycp-reconcile.md), so nothing
// here reads desired state.
package ycp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Kind is the environment kind the amp controller registers and enrolls as. The
// control plane rejects an enrollment whose kind does not match the environment
// it was registered with, so this is what keeps an amp controller from attaching
// to a meko environment.
const Kind = "amp"

// StateReady is the environment state a healthy controller reports. The control
// plane's lifecycle state machine drives the console badge from it.
const StateReady = "READY"

// ClusterFacts are the read-only facts reported on enroll/status. All optional:
// the control plane merges only the non-empty ones.
type ClusterFacts struct {
	Provider   string `json:"provider,omitempty"`
	Region     string `json:"region,omitempty"`
	K8sVersion string `json:"k8s_version,omitempty"`
	NodeCount  int    `json:"node_count,omitempty"`
}

// EnrollRequest is the enrollment-handshake body. Mirrors the control plane's
// service.EnrollRequest; the JSON tags are the contract (the two live in
// separate Go modules and are duplicated rather than imported).
type EnrollRequest struct {
	Name            string       `json:"name"`
	EnrollmentToken string       `json:"enrollment_token"`
	Placement       string       `json:"placement,omitempty"`
	Kind            string       `json:"kind"`
	ClusterFacts    ClusterFacts `json:"cluster_facts"`
}

// EnrollResponse is the identity the control plane hands back. The amp
// controller ignores the stack (that is meko's data plane) and keeps only its
// id and status token.
type EnrollResponse struct {
	EnvironmentID string `json:"environment_id"`
	StatusToken   string `json:"status_token"`
	MCPBaseURL    string `json:"mcp_base_url"`
}

// ResourceStatus is one item of the reported inventory. Mirrors the control
// plane's service.ResourceStatus: kind-agnostic and flat, with Parent linking a
// resource to its owner by id (project→pool, branch→project, endpoint→branch).
type ResourceStatus struct {
	Kind   string            `json:"kind"`
	ID     string            `json:"id"`
	Name   string            `json:"name,omitempty"`
	Parent string            `json:"parent,omitempty"`
	State  string            `json:"state,omitempty"`
	Attrs  map[string]string `json:"attrs,omitempty"`
}

// StatusReport is the heartbeat body. The amp controller reports its state, its
// agent version, and its full resource inventory; it does not report meko
// components, so Components is omitted.
type StatusReport struct {
	EnvironmentState string           `json:"environment_state"`
	AgentVersion     string           `json:"agent_version"`
	ClusterFacts     ClusterFacts     `json:"cluster_facts"`
	Resources        []ResourceStatus `json:"resources"`
}

// Client talks to one YCP control plane. Safe for concurrent use.
type Client struct {
	base string
	http *http.Client
}

// NewClient builds a client against baseURL (a trailing slash is trimmed, so a
// subpath like https://host/ycp works). A nil httpClient gets a 15s-timeout
// default.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), http: httpClient}
}

// ErrUnauthorized is returned when the control plane rejects the status token
// (HTTP 401). The report loop treats it as "the status token is no longer valid"
// and re-enrolls.
var ErrUnauthorized = fmt.Errorf("ycp: unauthorized")

// Enroll performs the enrollment-token handshake and returns the environment
// identity. The caller persists the status token; the enrollment token is not
// needed again.
func (c *Client) Enroll(ctx context.Context, req EnrollRequest) (EnrollResponse, error) {
	var out EnrollResponse
	code, apiErr, err := c.do(ctx, http.MethodPost, "/api/v1/agent/enroll", "", req, &out)
	if err != nil {
		return EnrollResponse{}, err
	}
	if code != http.StatusOK {
		return EnrollResponse{}, httpError("enroll", code, apiErr)
	}
	if out.EnvironmentID == "" || out.StatusToken == "" {
		return EnrollResponse{}, fmt.Errorf("ycp: enroll response missing environment_id/status_token")
	}
	return out, nil
}

// PostStatus reports the inventory heartbeat for envID, authenticated by the
// env-scoped status token. Returns ErrUnauthorized on a 401 so the caller can
// re-enroll.
func (c *Client) PostStatus(ctx context.Context, envID, statusToken string, r StatusReport) error {
	code, apiErr, err := c.do(ctx, http.MethodPost, "/api/v1/agent/status?envId="+url.QueryEscape(envID), statusToken, r, nil)
	if err != nil {
		return err
	}
	switch {
	case code == http.StatusUnauthorized:
		return ErrUnauthorized
	case code >= 300:
		return httpError("status", code, apiErr)
	}
	return nil
}

// httpError formats a non-2xx response, folding in the envelope error message
// when the control plane sent one.
func httpError(op string, code int, apiErr string) error {
	if apiErr != "" {
		return fmt.Errorf("ycp: %s -> HTTP %d: %s", op, code, apiErr)
	}
	return fmt.Errorf("ycp: %s -> HTTP %d", op, code)
}

// do issues one request and decodes the api_server {"data":…,"error":…}
// envelope. It returns the HTTP status code, the envelope's error message (if
// any), and a non-nil error only for transport/marshal/decode failures — HTTP
// status is conveyed through the code so callers can branch on it (e.g. 401).
func (c *Client) do(ctx context.Context, method, path, bearer string, body, out any) (int, string, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, "", fmt.Errorf("ycp: marshal request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return 0, "", fmt.Errorf("ycp: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("ycp: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var env struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	apiErr := ""
	if env.Error != nil {
		apiErr = env.Error.Message
	}
	if resp.StatusCode >= 300 {
		return resp.StatusCode, apiErr, nil
	}
	if out != nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return resp.StatusCode, apiErr, fmt.Errorf("ycp: decode response: %w", err)
		}
	}
	return resp.StatusCode, apiErr, nil
}
