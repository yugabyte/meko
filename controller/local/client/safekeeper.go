package client

import (
	"context"
	"net/http"
	"net/url"
)

// Safekeeper is the launcher's client for a safekeeper's management HTTP API.
// Base URL is http://<listen_addr>:<http_port>. It ports the subset of
// safekeeper_client::mgmt_api used by control_plane/src/safekeeper.rs.
type Safekeeper struct {
	t transport
}

// NewSafekeeper builds a safekeeper mgmt client. base is
// http://<listen_addr>:<http_port> (a bare host:port is also accepted). A nil
// hc uses http.DefaultClient.
func NewSafekeeper(base string, hc *http.Client) *Safekeeper {
	return &Safekeeper{t: newTransport(base, hc, "")}
}

// SafekeeperTimelineCreateRequest mirrors safekeeper_api::models::
// TimelineCreateRequest. pg_version is a bare number on the wire (PgVersionId
// serializes as u32); the LSNs are "X/Y" strings. system_id, wal_seg_size, and
// commit_lsn are optional and serialize as null when nil.
type SafekeeperTimelineCreateRequest struct {
	BranchID   string  `json:"branch_id"`
	PgVersion  uint32  `json:"pg_version"`
	SystemID   *uint64 `json:"system_id"`
	WalSegSize *uint32 `json:"wal_seg_size"`
	StartLSN   string  `json:"start_lsn"`
	CommitLSN  *string `json:"commit_lsn"`
}

// SafekeeperStatus is the GET /v1/status body (safekeeper_api::models::
// SafekeeperStatus). NodeId serializes as a bare number.
type SafekeeperStatus struct {
	ID uint64 `json:"id"`
}

// SafekeeperTimelineStatus is the minimal slice of safekeeper_api::models::
// TimelineStatus the launcher reads.
type SafekeeperTimelineStatus struct {
	FlushLSN string `json:"flush_lsn"`
}

// Status hits GET /v1/status and returns the safekeeper's node id.
func (s *Safekeeper) Status(ctx context.Context) (SafekeeperStatus, error) {
	var st SafekeeperStatus
	err := s.t.do(ctx, http.MethodGet, "/v1/status", nil, &st)
	return st, err
}

// CreateTimeline creates a timeline via POST /v1/branch.
func (s *Safekeeper) CreateTimeline(ctx context.Context, req SafekeeperTimelineCreateRequest) error {
	return s.t.do(ctx, http.MethodPost, "/v1/branch", req, nil)
}

// TimelineStatus fetches a branch's status via GET /v1/branch/{branchID}.
func (s *Safekeeper) TimelineStatus(ctx context.Context, branchID string) (SafekeeperTimelineStatus, error) {
	var st SafekeeperTimelineStatus
	err := s.t.do(ctx, http.MethodGet, "/v1/branch/"+url.PathEscape(branchID), nil, &st)
	return st, err
}
