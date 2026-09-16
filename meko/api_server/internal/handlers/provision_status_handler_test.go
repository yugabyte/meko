package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

const (
	testDatapackID   = "5fdf044d-0296-409b-a8d5-f44ea0266b40"
	testMCPServerURL = "https://mcp.example"
	testDatapackName = "e2e"
	testDBConnStr    = "postgres://host/db"
)

// newProvisionStatusRequest wires one request through
// DatapackProvisionStatusHandler with the datapack already in context, the way
// RequireDatapackAccess leaves it. authHeader is sent verbatim when non-empty.
func newProvisionStatusRequest(t *testing.T, authHeader string) (*httptest.ResponseRecorder, sqlmock.Sqlmock) {
	t.Helper()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectQuery("SELECT datapack_id, datapack_name").
		WithArgs(testDatapackID).
		WillReturnRows(sqlmock.NewRows([]string{
			"datapack_id", "datapack_name",
			"datapack_ysql_connection_string",
			"datapack_ysql_username", "datapack_ysql_password",
		}).AddRow(testDatapackID, testDatapackName, testDBConnStr, "user", "pw"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/datapacks/"+testDatapackID+"/provision-status", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()

	c := echo.New().NewContext(req, rec)
	c.Set(auth.ContextKeyDatapack, &models.Datapack{ID: testDatapackID, Name: testDatapackName})

	repo := repository.NewDatapackRepo(db, testMCPServerURL, repository.DeployEnv(""))
	if err := DatapackProvisionStatusHandler(repo)(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	return rec, mock
}

type provisionDetails struct {
	DatapackID   string `json:"datapack_id"`
	MCPServerURL string `json:"mcp_server_url"`
	DatapackName string `json:"datapack_name"`
	DBConnStr    string `json:"db_conn_str"`
	MCPAPIKey    string `json:"mcp_api_key"`
}

// decodeProvisionDetails pulls the whole `data` payload out of the envelope.
// An absent or null `data` decodes to the zero value, so callers assert on a
// populated field rather than on mcp_api_key alone -- an empty envelope would
// otherwise be indistinguishable from a legitimately empty token.
func decodeProvisionDetails(t *testing.T, rec *httptest.ResponseRecorder) provisionDetails {
	t.Helper()
	var body struct {
		Data provisionDetails `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return body.Data
}

// assertServedDetails checks the response really carries the row the repository
// returned, so a handler that short-circuited to an empty 200 cannot pass.
func assertServedDetails(t *testing.T, got provisionDetails, mock sqlmock.Sqlmock) {
	t.Helper()
	if got.DatapackID != testDatapackID {
		t.Errorf("datapack_id = %q, want %q", got.DatapackID, testDatapackID)
	}
	if got.MCPServerURL != testMCPServerURL {
		t.Errorf("mcp_server_url = %q, want %q", got.MCPServerURL, testMCPServerURL)
	}
	if got.DatapackName != testDatapackName {
		t.Errorf("datapack_name = %q, want %q", got.DatapackName, testDatapackName)
	}
	// redactProvisionDetails strips the connection fields from every response;
	// asserting that here keeps this test honest about which fields a 200
	// actually carries.
	if got.DBConnStr != "" {
		t.Errorf("db_conn_str = %q, want redacted", got.DBConnStr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("provision-details query not run as expected: %v", err)
	}
}

// With auth disabled DevAuth supplies the account and there is no bearer token
// to echo. The route is still authorized (RequireDatapackAccess ran), so it must
// serve the details with mcp_api_key empty. A 401 here logs the SPA out
// entirely, because its interceptor reads any 401 as an expired session — which
// stranded BYOC users on a login page whose endpoints 404.
func TestProvisionStatusWithoutBearerTokenServesDetails(t *testing.T) {
	rec, mock := newProvisionStatusRequest(t, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	got := decodeProvisionDetails(t, rec)
	assertServedDetails(t, got, mock)
	if got.MCPAPIKey != "" {
		t.Errorf("mcp_api_key = %q, want empty when the caller sent no token", got.MCPAPIKey)
	}
}

func TestProvisionStatusEchoesBearerToken(t *testing.T) {
	rec, mock := newProvisionStatusRequest(t, "Bearer tok-123")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	got := decodeProvisionDetails(t, rec)
	assertServedDetails(t, got, mock)
	if got.MCPAPIKey != "tok-123" {
		t.Errorf("mcp_api_key = %q, want the caller's token echoed back", got.MCPAPIKey)
	}
}
