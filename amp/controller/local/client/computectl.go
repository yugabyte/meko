package client

import (
	"context"
	"net/http"
	"net/url"
)

// ComputeCtl is the launcher's client for an endpoint's compute_ctl HTTP API.
// Base URL is http://<external_http_address>. Unlike the storage daemons this
// API is JWT-authenticated: the caller passes the already-minted bearer token.
// It ports the /status and /terminate calls in control_plane/src/endpoint.rs.
type ComputeCtl struct {
	t transport
}

// NewComputeCtl builds a compute_ctl client. base is
// http://<external_http_address> (a bare host:port is also accepted); token is
// the bearer JWT sent on every request. A nil hc uses http.DefaultClient.
func NewComputeCtl(base, token string, hc *http.Client) *ComputeCtl {
	return &ComputeCtl{t: newTransport(base, hc, token)}
}

// ComputeStatusResponse is the minimal slice of compute_api::responses::
// ComputeStatusResponse the launcher reads: the compute status and any error.
type ComputeStatusResponse struct {
	Status string  `json:"status"`
	Error  *string `json:"error"`
}

// TerminateResponse is compute_api::responses::TerminateResponse: the LSN at
// termination ("X/Y"), or null if the compute never started.
type TerminateResponse struct {
	LSN *string `json:"lsn"`
}

// Status hits GET /status and returns the compute's current status.
func (c *ComputeCtl) Status(ctx context.Context) (ComputeStatusResponse, error) {
	var resp ComputeStatusResponse
	err := c.t.do(ctx, http.MethodGet, "/status", nil, &resp)
	return resp, err
}

// Terminate requests termination via POST /terminate?mode=<mode> (e.g.
// "immediate") and returns the LSN reached at shutdown.
func (c *ComputeCtl) Terminate(ctx context.Context, mode string) (TerminateResponse, error) {
	path := "/terminate"
	if mode != "" {
		path += "?mode=" + url.QueryEscape(mode)
	}
	var resp TerminateResponse
	err := c.t.do(ctx, http.MethodPost, path, nil, &resp)
	return resp, err
}
