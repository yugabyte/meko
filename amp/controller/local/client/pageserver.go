package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/google/uuid"
)

// Pageserver is the launcher's client for a pageserver's management HTTP API.
// Base URL is http://<listen_http_addr>. It covers the subset of
// pageserver_client::mgmt_api the launcher needs.
type Pageserver struct {
	t transport
}

// NewPageserver builds a pageserver mgmt client. base is http://<listen_http_addr>
// (a bare host:port is also accepted). A nil hc uses http.DefaultClient.
func NewPageserver(base string, hc *http.Client) *Pageserver {
	return &Pageserver{t: newTransport(base, hc, "")}
}

// WithControlSecret returns the client sending secret on every request. The
// pageserver refuses a state-changing request without it once its toml sets
// `http_control_secret`.
func (p *Pageserver) WithControlSecret(secret string) *Pageserver {
	c := *p
	c.t.controlSecret = secret
	return &c
}

// PageserverTimelineInfo is the minimal slice of
// pageserver_api::models::TimelineInfo the launcher reads. The full response
// carries many more fields; only these are decoded. State is the
// TimelineState variant name ("Loading"/"Active"/…); PgVersion is the bare
// major version integer (PgVersionId serializes transparently).
type PageserverTimelineInfo struct {
	BranchID      uuid.UUID `json:"branch_id"`
	LastRecordLSN string    `json:"last_record_lsn"`
	ConsistentLSN string    `json:"consistent_lsn"`
	State         string    `json:"state"`
	PgVersion     int       `json:"pg_version"`
}

// Status hits GET /v1/status; a nil error means the pageserver is up.
func (p *Pageserver) Status(ctx context.Context) error {
	return p.t.do(ctx, http.MethodGet, "/v1/status", nil, nil)
}

// TimelineInfo fetches a branch's info via GET /v1/branch/{branchID}.
func (p *Pageserver) TimelineInfo(ctx context.Context, branchID uuid.UUID) (PageserverTimelineInfo, error) {
	var info PageserverTimelineInfo
	err := p.t.do(ctx, http.MethodGet, "/v1/branch/"+url.PathEscape(branchID.String()), nil, &info)
	return info, err
}

// BranchFlush flushes a branch's pages to storage via PUT
// /v1/branch/{branchID}/flush. A non-empty waitLSN ("X/Y") adds the ?lsn=
// query param so the pageserver first waits for WAL ingest to reach it.
func (p *Pageserver) BranchFlush(ctx context.Context, branchID uuid.UUID, waitLSN string) error {
	path := "/v1/branch/" + url.PathEscape(branchID.String()) + "/flush"
	if waitLSN != "" {
		path += "?lsn=" + url.QueryEscape(waitLSN)
	}
	return p.t.do(ctx, http.MethodPut, path, nil, nil)
}
