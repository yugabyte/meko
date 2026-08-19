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

const testAdminSecret = "admin-secret"

var headerRowCols = []string{
	"uuid", "id", "account_id", "placement", "region", "state", "k8s_version", "node_count", "provider",
	"datapacks", "max_datapacks", "agent_version", "last_heartbeat_at", "mcp_base_url",
	"cert_fingerprint", "components", "live", "enroll_token_hash", "enroll_token_expires_at",
	"status_token_hash", "created_at", "kind", "resources",
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

	mock.ExpectQuery(`FROM meko_system\.environments WHERE account_id = \$1 ORDER BY live DESC`).
		WithArgs("acct-1").
		WillReturnRows(sqlmock.NewRows(headerRowCols).AddRow(
			"11111111-1111-1111-1111-111111111111", "acme-prod", "acct-1", "cloud", "us-east-1", "READY", "", 0, "",
			0, 400, "", nil, "", "", "[]", true, "", nil, "", time.Now(), "meko", "[]",
		))

	resp := fleetGet(t, srv, "/api/v1/environments", "acct-1")
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

	mock.ExpectQuery(`FROM meko_system\.environments ORDER BY live DESC`).
		WillReturnRows(sqlmock.NewRows(headerRowCols).AddRow(
			"11111111-1111-1111-1111-111111111111", "acme-prod", "acct-1", "cloud", "us-east-1", "READY", "", 0, "",
			0, 400, "", nil, "", "", "[]", true, "", nil, "", time.Now(), "meko", "[]",
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

// The agent surface authenticates with its own enrollment and status tokens and
// has no account to send, so the requirement must not reach it — an enrolled
// agent would otherwise stop being able to report status.
func TestAgentRoutes_UnaffectedByAccountHeaderRequirement(t *testing.T) {
	srv, _ := fleetServer(t, true)

	resp, err := http.Post(srv.URL+"/api/v1/agent/enroll", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	// The handler's own validation answers (no enrollment_token), which is only
	// reachable if the account-header middleware is not in this route's chain.
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.Contains(body.Error.Message, "X-Meko-Account-ID") {
		t.Errorf("agent enroll was gated on the account header: %q", body.Error.Message)
	}
}
