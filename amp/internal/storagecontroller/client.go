package storagecontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Client talks to the managed storage controller's internal HTTP API. The amp
// controller is its only caller (§11); this is not part of the public API
// surface. The zero value is not usable — build one with NewClient.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a client for a storage controller listening at addr
// (host:port). It speaks plain HTTP: the managed controller runs co-located
// with the amp controller (§11), so a wildcard or empty bind host resolves to
// loopback.
func NewClient(addr string) *Client {
	if host, port, err := net.SplitHostPort(addr); err == nil {
		switch host {
		case "", "0.0.0.0", "::", "[::]":
			addr = net.JoinHostPort("127.0.0.1", port)
		}
	}
	return &Client{
		baseURL: "http://" + addr,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Ready reports whether the storage controller is up and serving (GET /ready).
// It doubles as the managed subprocess's readiness probe (see Spec).
func (c *Client) Ready(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/ready", nil, nil)
}

// CreateTimeline registers a new timeline under tenant
// (POST /v1/tenant/{tenant}/timeline). The request and response bodies are
// passed through as raw JSON so this client stays decoupled from the upstream
// pageserver timeline types; the task step catalog (§8) owns their shape.
func (c *Client) CreateTimeline(ctx context.Context, tenantID string, req json.RawMessage) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.do(ctx, http.MethodPost, "/v1/tenant/"+url.PathEscape(tenantID)+"/timeline", req, &out)
	return out, err
}

// DeleteTimeline removes a timeline
// (DELETE /v1/tenant/{tenant}/timeline/{timeline}).
func (c *Client) DeleteTimeline(ctx context.Context, tenantID, timelineID string) error {
	return c.do(ctx, http.MethodDelete,
		"/v1/tenant/"+url.PathEscape(tenantID)+"/timeline/"+url.PathEscape(timelineID), nil, nil)
}

// GetTimeline describes a timeline
// (GET /control/v1/tenant/{tenant}/timeline/{timeline}).
func (c *Client) GetTimeline(ctx context.Context, tenantID, timelineID string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.do(ctx, http.MethodGet,
		"/control/v1/tenant/"+url.PathEscape(tenantID)+"/timeline/"+url.PathEscape(timelineID), nil, &out)
	return out, err
}

// do issues one request. body, if non-nil, is JSON-encoded; out, if non-nil,
// receives the decoded JSON response. A non-2xx status is returned as an error
// carrying a snippet of the response body.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(snippet))
	}
	if out != nil {
		// The caller asked for a body, so an empty response (io.EOF) is an
		// error, not a silently-empty result.
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
