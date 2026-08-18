// Package ycpclient is the reconcile agent's client for the YCP control plane
// (meko api_server --control-plane-mode): enroll for an env-scoped status token,
// pull desired state, and post status heartbeats. It speaks the same
// /api/v1/agent/* protocol and {"data","error"} envelope as the meko-agent, and
// implements the reconcile engine's Source and Reporter so the engine can drive
// its loop directly.
package ycpclient

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

	"meko/ycp/internal/api"
)

// ErrUnauthorized is returned when the control plane rejects the status token
// (HTTP 401), so the caller can re-enroll.
var ErrUnauthorized = fmt.Errorf("ycp: unauthorized")

// Client talks to one YCP control plane. Set Token after enrolling (or from
// persisted state) to authenticate desired-state/status calls.
type Client struct {
	base  string
	http  *http.Client
	token string
}

// New builds a client against baseURL (trailing slash trimmed, so a subpath like
// https://host/ycp works). A nil httpClient gets a 15s-timeout default.
func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), http: httpClient}
}

// SetToken sets the env-scoped status token used for desired-state and status.
func (c *Client) SetToken(t string) { c.token = t }

// Enroll performs the enrollment-token handshake for an amp environment and
// returns the environment id + status token. The caller persists the token; the
// enrollment token is not needed again until it expires.
func (c *Client) Enroll(ctx context.Context, req api.EnrollRequest) (api.EnrollResponse, error) {
	var out api.EnrollResponse
	code, err := c.do(ctx, http.MethodPost, "/api/v1/agent/enroll", req, &out)
	if err != nil {
		return api.EnrollResponse{}, err
	}
	if code != http.StatusOK {
		return api.EnrollResponse{}, fmt.Errorf("ycp: enroll -> HTTP %d", code)
	}
	if out.EnvironmentID == "" || out.StatusToken == "" {
		return api.EnrollResponse{}, fmt.Errorf("ycp: enroll response missing environment_id/status_token")
	}
	c.token = out.StatusToken
	return out, nil
}

// DesiredFor fetches desired state for envID. Returns ErrUnauthorized on a 401.
func (c *Client) DesiredFor(ctx context.Context, envID string) (api.DesiredState, error) {
	var ds api.DesiredState
	code, err := c.do(ctx, http.MethodGet, "/api/v1/agent/desired-state?envId="+url.QueryEscape(envID), nil, &ds)
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

// PostStatusFor posts a status heartbeat for envID. Returns ErrUnauthorized on
// a 401.
func (c *Client) PostStatusFor(ctx context.Context, envID string, r api.StatusReport) error {
	code, err := c.do(ctx, http.MethodPost, "/api/v1/agent/status?envId="+url.QueryEscape(envID), r, nil)
	if err != nil {
		return err
	}
	if code == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if code >= 300 {
		return fmt.Errorf("ycp: status -> HTTP %d", code)
	}
	return nil
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
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
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
