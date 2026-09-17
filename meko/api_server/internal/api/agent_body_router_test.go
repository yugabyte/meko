package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// CLOUDGA-36984 reproduced on the wire. The sibling test in internal/handlers calls
// the handler directly; this one goes through the real router, so it proves the
// status code a client actually sees, the order the middleware runs in, and that a
// 4xx on this API is the JSON envelope rather than a framework error page.

// agentPost sends an agent request with the two headers YBM stamps. contentType is
// set verbatim, including not at all when empty, because that is the variable under
// test.
func agentPost(t *testing.T, url, contentType, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("X-Meko-Admin-Secret", testAdminSecret)
	req.Header.Set("X-Meko-Api-Key-ID", testAPIKeyID)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// The probe from the ticket: this exact call answered 204 and blanked the
// environment's state.
func TestAgentStatus_NonJSONContentTypeIsRejectedOnTheWire(t *testing.T) {
	srv, mock := fleetServer(t, false)

	resp := agentPost(t, srv.URL+"/api/v1/agent/status", "text/plain", "hello")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("error body content type = %q, want application/json", ct)
	}
	if code := errorCode(t, resp); code != "UNSUPPORTED_MEDIA_TYPE" {
		t.Errorf("error code = %q, want UNSUPPORTED_MEDIA_TYPE", code)
	}
	// The guard runs before the handler, so the environment is never even looked up.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a rejected request reached the database: %v", err)
	}
}

func TestAgentStatus_EmptyBodyIsRejectedOnTheWire(t *testing.T) {
	srv, mock := fleetServer(t, false)

	resp := agentPost(t, srv.URL+"/api/v1/agent/status", "application/json", "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "EMPTY_BODY" {
		t.Errorf("error code = %q, want EMPTY_BODY", code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a rejected request reached the database: %v", err)
	}
}

// Auth first: a probe that cannot authenticate must not learn which routes speak
// JSON.
func TestAgentStatus_AuthRunsBeforeTheBodyGuard(t *testing.T) {
	srv, _ := fleetServer(t, false)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/agent/status",
		strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// The guard skips body-less methods, so the read side of the agent surface is
// untouched by it. A GET sends no content type and must not become a 415.
func TestAgentDesiredState_IsNotRejectedForHavingNoBody(t *testing.T) {
	srv, mock := fleetServer(t, false)
	mock.ExpectQuery(`FROM meko_system\.environments e`).
		WithArgs(testAPIKeyID).
		WillReturnRows(sqlmock.NewRows(headerRowCols).AddRow(
			envUUID, "acme-prod", acctA, "cloud", "us-east-1", "READY", "", 0, "",
			0, 400, "", nil, "", "", "[]", true, testAPIKeyID, time.Now(), "meko", "[]", nil,
		))
	mock.ExpectQuery(`FROM meko_system\.environment_desired_resources`).
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "name", "spec", "generation"}))

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/agent/desired-state", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("X-Meko-Admin-Secret", testAdminSecret)
	req.Header.Set("X-Meko-Api-Key-ID", testAPIKeyID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET desired-state = %d, want 200; the guard must skip body-less methods",
			resp.StatusCode)
	}
}

// CORS preflight carries no body and no content type. It is answered before the
// group middleware, and the method skip keeps that true regardless.
func TestAgentStatus_PreflightIsNotRejected(t *testing.T) {
	srv, _ := fleetServer(t, false)

	req, err := http.NewRequest(http.MethodOptions, srv.URL+"/api/v1/agent/status", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Origin", "https://console.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnsupportedMediaType {
		t.Fatalf("preflight = 415, want it answered by CORS")
	}
	_, _ = io.Copy(io.Discard, resp.Body)
}
