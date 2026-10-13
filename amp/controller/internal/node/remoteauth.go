package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// RemoteAuthState is what an endpoint's supervisor reports having done with the
// cloud_admin password on its current boot (GET /v1/remote-auth).
type RemoteAuthState string

const (
	// RemoteAuthPending: remote_auth has not decided yet.
	RemoteAuthPending RemoteAuthState = "pending"
	// RemoteAuthApplied: this boot set the password, and the catalog had none.
	RemoteAuthApplied RemoteAuthState = "applied"
	// RemoteAuthAlreadySet: the catalog already had a password, so the
	// endpoint's own was not applied.
	RemoteAuthAlreadySet RemoteAuthState = "already_set"
	// RemoteAuthUnset: no password was supplied and the catalog has none, so
	// remote auth fails until one is set.
	RemoteAuthUnset RemoteAuthState = "unset"
	// RemoteAuthFailed: the supervisor gave up setting it.
	RemoteAuthFailed RemoteAuthState = "failed"
	// RemoteAuthUnsupported is never sent by a supervisor: it is a 404, from an
	// endpoint image older than the route.
	RemoteAuthUnsupported RemoteAuthState = "unsupported"
)

// RemoteAuthStatus is the supervisor's answer. LSN is set only with
// RemoteAuthApplied, and not always then: it is the WAL flush position read
// after the ALTER ROLE committed, so a fork at it carries the password.
type RemoteAuthStatus struct {
	State RemoteAuthState
	LSN   string
}

// remoteAuthTimeout bounds one request. The supervisor answers from memory, so
// a slow answer is a pod that cannot be reached, and the caller polls again. A
// variable so a test need not wait it out.
var remoteAuthTimeout = 10 * time.Second

// RemoteAuth asks the endpoint supervisor at supervisorBaseURL
// (deploy.Deployer.EndpointSupervisorURL) what it did with the cloud_admin
// password. A 404 is RemoteAuthUnsupported with no error; any other non-200, an
// unreachable supervisor or a reply with no state is an error. The state is
// returned as sent, so a caller must tolerate one it does not know.
func (c *Client) RemoteAuth(ctx context.Context, supervisorBaseURL string) (RemoteAuthStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, remoteAuthTimeout)
	defer cancel()
	u := strings.TrimRight(supervisorBaseURL, "/") + "/v1/remote-auth"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return RemoteAuthStatus{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return RemoteAuthStatus{}, fmt.Errorf("GET %s: %w: %w", u, ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return RemoteAuthStatus{State: RemoteAuthUnsupported}, nil
	default:
		return RemoteAuthStatus{}, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	var reply struct {
		State RemoteAuthState `json:"state"`
		LSN   *string         `json:"lsn"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return RemoteAuthStatus{}, fmt.Errorf("decode the remote-auth reply: %w", err)
	}
	if reply.State == "" {
		return RemoteAuthStatus{}, errors.New("remote-auth reply carries no state")
	}
	status := RemoteAuthStatus{State: reply.State}
	if reply.LSN != nil {
		status.LSN = *reply.LSN
	}
	return status, nil
}
