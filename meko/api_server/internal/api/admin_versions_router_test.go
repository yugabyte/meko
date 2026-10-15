package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/api"
)

var adminVersionRoutes = []string{
	"GET /api/v1/admin/environments/:id/desired/controller",
	"PUT /api/v1/admin/environments/:id/desired/controller",
	"GET /api/v1/admin/environments/:id/desired/fleet-version",
	"PUT /api/v1/admin/environments/:id/desired/fleet-version",
}

func registeredRoutes(t *testing.T, cfg api.ServerConfig) map[string]bool {
	t.Helper()
	router, _, err := api.NewRouter(cfg)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	routes := map[string]bool{}
	for _, r := range router.Routes() {
		routes[r.Method+" "+r.Path] = true
	}
	return routes
}

// The admin group exists in every mode, but its environment routes need the fleet
// registry, which only control-plane mode builds.
func TestAdminVersionRoutes_MountOnlyInControlPlaneMode(t *testing.T) {
	cp := registeredRoutes(t, api.ServerConfig{ControlPlaneMode: true})
	dp := registeredRoutes(t, api.ServerConfig{})
	based := registeredRoutes(t, api.ServerConfig{ControlPlaneMode: true, BasePath: "/ycp"})
	for _, route := range adminVersionRoutes {
		if !cp[route] {
			t.Errorf("%s is not mounted in control-plane mode", route)
		}
		if dp[route] {
			t.Errorf("%s is mounted without control-plane mode", route)
		}
		method, path, _ := strings.Cut(route, " ")
		if !based[method+" /ycp"+path] || based[route] {
			t.Errorf("%s did not move under the base path", route)
		}
	}
}

func adminVersionRequest(
	t *testing.T, srv *httptest.Server, method, path, secret, accountID, body string,
) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Meko-Admin-Secret", secret)
	}
	if accountID != "" {
		req.Header.Set("X-Meko-Account-ID", accountID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

const controllerPath = "/api/v1/admin/environments/acme-prod/desired/controller"

func TestAdminVersionRoutes_RequireTheAdminSecret(t *testing.T) {
	srv, mock := fleetServer(t, false)
	for _, secret := range []string{"", "wrong"} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			resp := adminVersionRequest(t, srv, method, controllerPath, secret, "", `{"version":"2.1.0"}`)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s with secret %q = %d, want 401", method, secret, resp.StatusCode)
			}
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("an unauthenticated call reached the database: %v", err)
	}
}

// --require-account-header gates the fleet routes, not these: an operator moving an
// environment's software holds the admin secret and may have no account to name.
func TestAdminVersionRoutes_IgnoreRequireAccountHeader(t *testing.T) {
	srv, mock := fleetServer(t, true)
	mock.ExpectQuery(`FROM meko_system\.environments e WHERE e\.id = \$1 LIMIT 2`).
		WithArgs("acme-prod").
		WillReturnRows(sqlmock.NewRows(headerRowCols).AddRow(
			envUUID, "acme-prod", acctA, "cloud", "us-east-1", "READY", "", 0, "",
			0, 400, "", nil, "", "", "[]", true, testAPIKeyID, time.Now(), "amp", "[]", nil, "BYOC", false,
		))
	mock.ExpectQuery(`INSERT INTO meko_system\.environment_desired_resources .* RETURNING generation`).
		WithArgs(envUUID, "controller", "controller", []byte(`{"version":"2.1.0"}`)).
		WillReturnRows(sqlmock.NewRows([]string{"generation"}).AddRow(int64(0)))

	resp := adminVersionRequest(t, srv, http.MethodPut, controllerPath, testAdminSecret, "", `{"version":"2.1.0"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// An account, when sent, resolves an id two accounts share, so it reaches the lookup.
func TestAdminVersionRoutes_AccountHeaderScopesTheLookup(t *testing.T) {
	srv, mock := fleetServer(t, false)
	mock.ExpectQuery(`FROM meko_system\.environments e WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctB, "acme-prod").
		WillReturnRows(sqlmock.NewRows(headerRowCols))

	resp := adminVersionRequest(t, srv, http.MethodGet, controllerPath, testAdminSecret, acctB, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an account that owns no such environment", resp.StatusCode)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestAdminVersionRoutes_MalformedAccountHeaderIs400(t *testing.T) {
	srv, mock := fleetServer(t, false)
	resp := adminVersionRequest(t, srv, http.MethodPut, controllerPath, testAdminSecret, "acme-corp",
		`{"version":"2.1.0"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "VALIDATION_ERROR" {
		t.Errorf("error code = %q, want VALIDATION_ERROR", code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Every public route that writes desired state, through the real middleware: a body
// naming an admin-only kind is a 400 before the database is touched.
func TestPublicDesiredWrites_RefuseAdminOnlyKinds(t *testing.T) {
	const pool = "aaaaaaaa-1111-2222-3333-444444444444"
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/environments/acme-prod/desired",
			`{"resources":[{"kind":"controller","name":"controller","spec":{"version":"2.1.0"}}]}`},
		{http.MethodPost, "/api/v1/environments",
			`{"name":"acme-prod","kind":"amp","api_key_id":"` + testAPIKeyID + `",` +
				`"desired":[{"kind":"fleet-version","name":"fleet","spec":{"version":"2.1.0"}}]}`},
		{http.MethodPatch, "/api/v1/environments/resources",
			`{"storage_pool":"` + pool + `","regions":[{"region":"us-east-1","resources":[` +
				`{"kind":"controller","name":"` + pool + `","delete":true}]}]}`},
	} {
		srv, mock := fleetServer(t, false)
		resp := adminVersionRequest(t, srv, tc.method, tc.path, testAdminSecret, "", tc.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s %s = %d, want 400", tc.method, tc.path, resp.StatusCode)
		} else if code := errorCode(t, resp); code != "VALIDATION_ERROR" {
			t.Errorf("%s %s error code = %q, want VALIDATION_ERROR", tc.method, tc.path, code)
		}
		resp.Body.Close()
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("%s %s reached the database: %v", tc.method, tc.path, err)
		}
	}
}
