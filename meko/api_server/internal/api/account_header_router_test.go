package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/api"
)

// Test accounts and environment key. Named because account ids are uuids: inline
// literals push these expectations past the 120-column limit.
const (
	acctA   = "a0000000-0000-0000-0000-000000000001"
	acctB   = "a0000000-0000-0000-0000-000000000002"
	envUUID = "11111111-1111-1111-1111-111111111111"
)

const (
	testAdminSecret = "admin-secret"
	testAPIKeyID    = "b0000000-0000-0000-0000-00000000000a"
)

var headerRowCols = []string{
	"uuid", "id", "account_id", "placement", "region", "state", "k8s_version", "node_count", "provider",
	"datapacks", "max_datapacks", "agent_version", "last_heartbeat_at", "mcp_base_url",
	"cert_fingerprint", "components", "live", "api_key_id", "created_at", "kind", "resources",
	"agent_base_url",
}

// fleetServer stands up the real router in control-plane mode over a mocked system
// DB, so a request exercises the actual middleware chain rather than a handler
// called directly.
func fleetServer(t *testing.T, requireAccountHeader bool) (*httptest.Server, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	router, _, err := api.NewRouter(api.ServerConfig{
		ControlPlaneMode:     true,
		SystemDB:             db,
		AdminAPIKey:          testAdminSecret,
		RequireAccountHeader: requireAccountHeader,
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	srv := httptest.NewServer(router)
	t.Cleanup(func() { srv.Close(); db.Close() })
	return srv, mock
}

func fleetGet(t *testing.T, srv *httptest.Server, path, accountID string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("X-Meko-Admin-Secret", testAdminSecret)
	if accountID != "" {
		req.Header.Set("X-Meko-Account-ID", accountID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Error.Code
}

// With the flag on, the admin secret alone is no longer enough — which is the
// point: the control plane is reached only through a proxy that names an account.
func TestFleetRoutes_RequireAccountHeader_RejectsMissingHeader(t *testing.T) {
	srv, _ := fleetServer(t, true)

	for _, path := range []string{"/api/v1/environments", "/api/v1/entitlements"} {
		resp := fleetGet(t, srv, path, "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s without an account = %d, want 400", path, resp.StatusCode)
			continue
		}
		if code := errorCode(t, resp); code != "VALIDATION_ERROR" {
			t.Errorf("GET %s error code = %q, want VALIDATION_ERROR", path, code)
		}
	}
}

// The header is not merely demanded — the call it admits is still scoped to it.
func TestFleetRoutes_RequireAccountHeader_ServesScopedCall(t *testing.T) {
	srv, mock := fleetServer(t, true)

	mock.ExpectQuery(`FROM meko_system\.environments e WHERE e\.account_id = \$1 ORDER BY e\.live DESC`).
		WithArgs(acctA).
		WillReturnRows(sqlmock.NewRows(headerRowCols).AddRow(
			envUUID, "acme-prod", acctA, "cloud", "us-east-1", "READY", "", 0, "",
			0, 400, "", nil, "", "", "[]", true, testAPIKeyID, time.Now(), "meko", "[]", nil,
		))

	resp := fleetGet(t, srv, "/api/v1/environments", acctA)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the admitted call did not scope: %v", err)
	}
}

// Off by default, so an existing control plane keeps serving the console, the e2e
// and operator curl exactly as before.
func TestFleetRoutes_AccountHeaderOptionalByDefault(t *testing.T) {
	srv, mock := fleetServer(t, false)

	mock.ExpectQuery(`FROM meko_system\.environments e ORDER BY e\.live DESC`).
		WillReturnRows(sqlmock.NewRows(headerRowCols).AddRow(
			envUUID, "acme-prod", acctA, "cloud", "us-east-1", "READY", "", 0, "",
			0, 400, "", nil, "", "", "[]", true, testAPIKeyID, time.Now(), "meko", "[]", nil,
		))

	resp := fleetGet(t, srv, "/api/v1/environments", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the header must stay optional by default", resp.StatusCode)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("an unscoped list must not filter: %v", err)
	}
}

// agentEnroll posts an enroll with the given auth headers ("" = absent) through
// the real router, so the assertion covers the route's middleware chain.
func agentEnroll(t *testing.T, srv *httptest.Server, adminSecret, apiKeyID string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/agent/enroll", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if adminSecret != "" {
		req.Header.Set("X-Meko-Admin-Secret", adminSecret)
	}
	if apiKeyID != "" {
		req.Header.Set("X-Meko-Api-Key-ID", apiKeyID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// The agent surface sits behind the admin secret + the api-key-id header, and
// nothing else: no bearer, no account header. The four cases pin the chain in
// order — no admin secret, no key id, a malformed key id (400 before any lookup),
// then a well-formed call whose key is bound to nothing (the lookup's 401).
func TestAgentRoutes_RequireAdminSecretAndKeyID(t *testing.T) {
	srv, mock := fleetServer(t, true)

	resp := agentEnroll(t, srv, "", testAPIKeyID)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("enroll without the admin secret = %d, want 401", resp.StatusCode)
	}

	resp = agentEnroll(t, srv, testAdminSecret, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("enroll without a key id = %d, want 401", resp.StatusCode)
	}

	resp = agentEnroll(t, srv, testAdminSecret, "not-a-uuid")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("enroll with a malformed key id = %d, want 400", resp.StatusCode)
	}

	// Fully authenticated but the key is bound to nothing: the lookup runs (the
	// query below) and answers 401. Run on a require-account-header control plane
	// on purpose — the account requirement must not reach the agent surface, or an
	// enrolled agent could never report status.
	mock.ExpectQuery(`FROM meko_system\.environment_api_keys WHERE api_key_id = \$1`).
		WithArgs(testAPIKeyID).
		WillReturnRows(sqlmock.NewRows(headerRowCols))
	resp = agentEnroll(t, srv, testAdminSecret, testAPIKeyID)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("enroll with an unbound key = %d, want 401", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "UNAUTHORIZED" {
		t.Errorf("unbound key error code = %q, want UNAUTHORIZED", code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the authenticated enroll never reached the key lookup: %v", err)
	}
}

// account_id is a UUID column, so a garbled header must be refused at the edge rather
// than reaching the query as a 500. Refused, not ignored: treating it as absent would
// serve the whole fleet on a control plane that does not require the header.
func TestFleetRoutes_MalformedAccountHeaderIs400(t *testing.T) {
	for _, requireHeader := range []bool{false, true} {
		srv, _ := fleetServer(t, requireHeader)
		resp := fleetGet(t, srv, "/api/v1/environments", "acme-corp")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("require=%v: malformed account = %d, want 400", requireHeader, resp.StatusCode)
			continue
		}
		if code := errorCode(t, resp); code != "VALIDATION_ERROR" {
			t.Errorf("require=%v: error code = %q, want VALIDATION_ERROR", requireHeader, code)
		}
	}
}
