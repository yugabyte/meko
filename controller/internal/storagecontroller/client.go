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

// Client talks to the managed storage controller's internal HTTP API. The controller
// is its only caller (§11); this is not part of the public API
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

// PendingTimelineInit is the recipe for a branch that has been declared but
// not yet materialized: the storage controller stores it until an endpoint's
// pageserver attaches and bootstraps the branch (controller/architecture/
// controller-project-create.md). It mirrors the storage controller's
// PendingTimelineInitRequest serde shape (libs/pageserver_api controller_api).
// AncestorBranchID is null for a root (Bootstrap) branch; set, with start_lsn,
// for a branch off another. branch_name is a non-unique human-readable label.
// start_lsn is the controller's Lsn string form ("hi/lo" uppercase hex, e.g.
// "0/0"); pg_version is the bare major version (18).
type PendingTimelineInit struct {
	BranchName       string  `json:"branch_name"`
	PgVersion        int     `json:"pg_version"`
	AncestorBranchID *string `json:"ancestor_branch_id"`
	StartLSN         string  `json:"start_lsn"`
}

// ListedPendingTimelineInit is one entry returned by the list-all endpoint: a
// PendingTimelineInit together with its branch_id. It mirrors the storage
// controller's PendingTimelineInit response struct (which carries the id, unlike
// the request).
type ListedPendingTimelineInit struct {
	BranchID         string  `json:"branch_id"`
	BranchName       string  `json:"branch_name"`
	PgVersion        int     `json:"pg_version"`
	AncestorBranchID *string `json:"ancestor_branch_id"`
	StartLSN         string  `json:"start_lsn"`
}

// RecordPendingTimelineInit records the recipe for a not-yet-materialized branch
// (POST /control/v1/branch/{branch_id}/pending_init). branchID is 32 lowercase
// hex digits (the storage-layer BranchId). Idempotent on the branch_id key.
func (c *Client) RecordPendingTimelineInit(ctx context.Context, branchID string, req PendingTimelineInit) error {
	return c.do(ctx, http.MethodPost,
		"/control/v1/branch/"+url.PathEscape(branchID)+"/pending_init",
		req, nil)
}

// DeletePendingTimelineInit drops the recipe once the branch is materialized
// (DELETE /control/v1/branch/{branch_id}/pending_init). It is idempotent: a
// missing recipe (404) — e.g. a branch an earlier endpoint already materialized —
// is treated as success.
func (c *Client) DeletePendingTimelineInit(ctx context.Context, branchID string) error {
	err := c.do(ctx, http.MethodDelete,
		"/control/v1/branch/"+url.PathEscape(branchID)+"/pending_init",
		nil, nil)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		return nil
	}
	return err
}

// ListPendingTimelineInits returns every pending-init recipe the storage
// controller is holding (GET /control/v1/pending_timeline_inits). The endpoint
// lists all branches (there is no per-id lookup in the flat model), so callers
// filter the result by branch_name client-side.
func (c *Client) ListPendingTimelineInits(ctx context.Context) ([]ListedPendingTimelineInit, error) {
	var out []ListedPendingTimelineInit
	if err := c.do(ctx, http.MethodGet, "/control/v1/pending_timeline_inits", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
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
