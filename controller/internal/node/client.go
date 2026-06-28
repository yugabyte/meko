// Package node talks to an endpoint's co-located storage nodes (its own
// pageserver / safekeeper) over their internal HTTP APIs. The controller uses it
// to resolve a branch's default fork point from the *ancestor's own* safekeeper,
// which is routing-correct in the co-located topology where every endpoint has
// its own node — unlike the storage controller's per-tenant describe, which
// routes to whichever pageserver attached the tenant last and so misses the
// parent timeline once the tenant has more than one endpoint (#133).
package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client reads LSNs from an endpoint's co-located nodes over plain HTTP. The
// controller reaches those nodes over the same network/DNS the storage
// controller dials (SC_REACHABLE_HOST), so no auth is involved. The zero value
// is not usable — build one with NewClient.
type Client struct {
	http *http.Client
}

// NewClient builds a node client. Each request targets a base URL the caller
// supplies (from deploy.Deployer.EndpointSafekeeperURL), so one client serves
// every endpoint.
func NewClient() *Client {
	return &Client{http: &http.Client{Timeout: 30 * time.Second}}
}

// TimelineFlushLSN reads a branch's durable WAL end — the safekeeper's
// flush_lsn — from the safekeeper serving it
// (GET {skBaseURL}/v1/branch/{branch_id}). This is the
// authoritative "everything committed so far" position, the default branch fork
// point yb_light_local also branches at; unlike a pageserver's last_record_lsn
// (an ingest position) it never lags durable WAL. skBaseURL is the safekeeper's
// HTTP base URL; branchID is 32 lowercase hex digits (the storage-layer
// BranchId).
func (c *Client) TimelineFlushLSN(ctx context.Context, skBaseURL, branchID string) (string, error) {
	u := strings.TrimRight(skBaseURL, "/") +
		"/v1/branch/" + url.PathEscape(branchID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("GET %s: status %d: %s", u, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	// The safekeeper's TimelineStatus carries far more than we need; decode only
	// flush_lsn (the "X/Y" pg_lsn form).
	var status struct {
		FlushLSN string `json:"flush_lsn"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return "", fmt.Errorf("decode safekeeper timeline status: %w", err)
	}
	if status.FlushLSN == "" {
		return "", fmt.Errorf("no flush_lsn for branch %s", branchID)
	}
	return status.FlushLSN, nil
}
