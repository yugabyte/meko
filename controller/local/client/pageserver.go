package client

import (
	"context"
	"net/http"
	"net/url"
)

// Pageserver is the launcher's client for a pageserver's management HTTP API.
// Base URL is http://<listen_http_addr>. It ports the subset of
// pageserver_client::mgmt_api used by control_plane/src/pageserver.rs.
type Pageserver struct {
	t transport
}

// NewPageserver builds a pageserver mgmt client. base is http://<listen_http_addr>
// (a bare host:port is also accepted). A nil hc uses http.DefaultClient.
func NewPageserver(base string, hc *http.Client) *Pageserver {
	return &Pageserver{t: newTransport(base, hc, "")}
}

// PageserverTimelineInfo is the minimal slice of
// pageserver_api::models::TimelineInfo the launcher reads. The full response
// carries many more fields; only these are decoded. State is the
// TimelineState variant name ("Loading"/"Active"/…); PgVersion is the bare
// major version integer (PgVersionId serializes transparently).
type PageserverTimelineInfo struct {
	BranchID          string `json:"branch_id"`
	LastRecordLSN     string `json:"last_record_lsn"`
	DiskConsistentLSN string `json:"disk_consistent_lsn"`
	State             string `json:"state"`
	PgVersion         int    `json:"pg_version"`
}

// Status hits GET /v1/status; a nil error means the pageserver is up.
func (p *Pageserver) Status(ctx context.Context) error {
	return p.t.do(ctx, http.MethodGet, "/v1/status", nil, nil)
}

// TimelineInfo fetches a branch's info via GET /v1/branch/{branchID}.
func (p *Pageserver) TimelineInfo(ctx context.Context, branchID string) (PageserverTimelineInfo, error) {
	var info PageserverTimelineInfo
	err := p.t.do(ctx, http.MethodGet, "/v1/branch/"+url.PathEscape(branchID), nil, &info)
	return info, err
}

// BranchFlush flushes a branch's pages to storage via PUT
// /v1/branch/{branchID}/flush. A non-empty waitLSN ("X/Y") adds the ?lsn=
// query param so the pageserver first waits for WAL ingest to reach it.
func (p *Pageserver) BranchFlush(ctx context.Context, branchID, waitLSN string) error {
	path := "/v1/branch/" + url.PathEscape(branchID) + "/flush"
	if waitLSN != "" {
		path += "?lsn=" + url.QueryEscape(waitLSN)
	}
	return p.t.do(ctx, http.MethodPut, path, nil, nil)
}
