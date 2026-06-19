package storagecontroller

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

// CreateTenant registers a tenant with the storage controller (POST /v1/tenant).
// Placement is "Detached" so it succeeds before any pageserver has registered —
// the endpoint's attach-hook (at endpoint start) attaches the tenant to that
// endpoint's pageserver and assigns the generation. tenantID is 32 lowercase hex
// digits (no dashes), the storage-layer TenantId. Idempotent on the controller.
func (c *Client) CreateTenant(ctx context.Context, tenantID string) error {
	body := map[string]any{
		"new_tenant_id":    tenantID,
		"placement_policy": "Detached",
	}
	var out json.RawMessage // response is {"shards":[...]}; accept and discard.
	return c.do(ctx, http.MethodPost, "/v1/tenant", body, &out)
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

// TimelineLatestLSN returns the timeline's latest LSN — the pageserver's
// last_record_lsn — from the describe endpoint
// (GET /control/v1/tenant/{tenant}/timeline/{timeline}). It is the default
// branch fork point. The describe response nests LSNs per shard
// ({"shards":[{"last_record_lsn":"X/Y", ...}], ...}); yb-light timelines are
// single-shard (each gets its own co-located pageserver + safekeeper), so the
// one shard's tip is the latest LSN. ids are 32 lowercase hex digits.
func (c *Client) TimelineLatestLSN(ctx context.Context, tenantID, timelineID string) (string, error) {
	var resp struct {
		Shards []struct {
			LastRecordLSN string `json:"last_record_lsn"`
		} `json:"shards"`
	}
	if err := c.do(ctx, http.MethodGet,
		"/control/v1/tenant/"+url.PathEscape(tenantID)+"/timeline/"+url.PathEscape(timelineID), nil, &resp); err != nil {
		return "", err
	}
	if len(resp.Shards) == 0 || resp.Shards[0].LastRecordLSN == "" {
		return "", fmt.Errorf("no last_record_lsn for timeline %s", timelineID)
	}
	return resp.Shards[0].LastRecordLSN, nil
}

// PendingTimelineInit is the recipe for a timeline that has been declared but
// not yet materialized: the storage controller stores it until an endpoint's
// pageserver attaches and bootstraps the timeline (amp/architecture/
// amp-timeline-create.md). ancestor_timeline_id is null for a root (Bootstrap)
// timeline; set, with start_lsn, for a branch. start_lsn is the controller's
// Lsn string form ("hi/lo" uppercase hex, e.g. "0/0"); pg_version is the bare
// major version (17).
type PendingTimelineInit struct {
	BranchName         string  `json:"branch_name"`
	PgVersion          int     `json:"pg_version"`
	AncestorTimelineID *string `json:"ancestor_timeline_id"`
	StartLSN           string  `json:"start_lsn"`
}

// RecordPendingTimelineInit records the recipe for a not-yet-materialized
// timeline (POST /control/v1/tenant/{tenant}/timeline/{timeline}/pending_init).
// ids are 32 lowercase hex digits (the storage-layer TenantId/TimelineId).
func (c *Client) RecordPendingTimelineInit(ctx context.Context, tenantID, timelineID string, req PendingTimelineInit) error {
	return c.do(ctx, http.MethodPost,
		"/control/v1/tenant/"+url.PathEscape(tenantID)+"/timeline/"+url.PathEscape(timelineID)+"/pending_init",
		req, nil)
}

// DeletePendingTimelineInit drops the recipe once the timeline is materialized
// (DELETE .../pending_init). It is idempotent: a missing recipe (404) — e.g. a
// timeline an earlier endpoint already materialized — is treated as success.
func (c *Client) DeletePendingTimelineInit(ctx context.Context, tenantID, timelineID string) error {
	err := c.do(ctx, http.MethodDelete,
		"/control/v1/tenant/"+url.PathEscape(tenantID)+"/timeline/"+url.PathEscape(timelineID)+"/pending_init",
		nil, nil)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		return nil
	}
	return err
}

// statusError is returned by do for a non-2xx response, so callers can react to
// a specific status (e.g. tolerate 404 on an idempotent delete).
type statusError struct {
	method, path string
	code         int
	body         string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s %s: status %d: %s", e.method, e.path, e.code, e.body)
}

// do issues one request. body, if non-nil, is JSON-encoded; out, if non-nil,
// receives the decoded JSON response. A non-2xx status is returned as a
// *statusError carrying a snippet of the response body.
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
		return &statusError{method: method, path: path, code: resp.StatusCode, body: string(bytes.TrimSpace(snippet))}
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
