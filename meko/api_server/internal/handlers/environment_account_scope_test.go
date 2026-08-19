package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

var scopeRowCols = []string{
	"uuid", "id", "account_id", "placement", "region", "state", "k8s_version", "node_count", "provider",
	"datapacks", "max_datapacks", "agent_version", "last_heartbeat_at", "mcp_base_url",
	"cert_fingerprint", "components", "live", "enroll_token_hash", "enroll_token_expires_at",
	"status_token_hash", "created_at", "kind", "resources",
}

func scopeRow(id, accountID string) *sqlmock.Rows {
	return sqlmock.NewRows(scopeRowCols).AddRow(
		"11111111-1111-1111-1111-111111111111", id, accountID, "cloud", "us-east-1", "READY", "", 0, "",
		0, 400, "", nil, "", "", "[]", true, "", nil, "", time.Now(), "meko", "[]",
	)
}

// newScopeHandler builds the fleet handler over a mocked DB.
func newScopeHandler(t *testing.T) (*EnvironmentHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	svc := service.NewEnvironmentService(repository.NewEnvironmentRepo(db), service.BootstrapDefaults{})
	return NewEnvironmentHandler(svc), mock, func() { db.Close() }
}

// fleetRequest runs one fleet request through the handler. accountID, when
// non-empty, is sent in the header YBM stamps.
func fleetRequest(
	t *testing.T, h echo.HandlerFunc, method, target, accountID, body, paramID string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if accountID != "" {
		req.Header.Set(auth.AccountIDHeader, accountID)
	}
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	if paramID != "" {
		c.SetParamNames("id")
		c.SetParamValues(paramID)
	}
	if err := h(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	return rec
}

// The header is the whole point of the feature: it has to reach the query as a
// filter, spelled exactly as YBM sends it.
func TestList_FiltersByAccountHeader(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	mock.ExpectQuery(`FROM meko_system\.environments WHERE account_id = \$1 ORDER BY live DESC`).
		WithArgs("acct-1").
		WillReturnRows(scopeRow("acme-prod", "acct-1"))

	rec := fleetRequest(t, h.List, http.MethodGet, "/api/v1/environments", "acct-1", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the account header did not reach the query: %v", err)
	}
}

// A fleet-admin caller that sends no account keeps seeing the whole fleet, which
// is what the console, the ycp e2e and the local scripts rely on.
func TestList_UnscopedWithoutAccountHeader(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	// ORDER BY must follow the table directly — no WHERE clause.
	mock.ExpectQuery(`FROM meko_system\.environments ORDER BY live DESC`).
		WillReturnRows(scopeRow("acme-prod", "acct-1"))

	rec := fleetRequest(t, h.List, http.MethodGet, "/api/v1/environments", "", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("an unscoped list must not filter: %v", err)
	}
}

// Reading across accounts is a 404, not a 403: an account cannot use the error to
// learn that an environment it does not own exists.
func TestGet_OtherAccountsEnvironmentIs404(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	// Scoped in SQL: acct-2's query simply matches nothing, so acct-1's row is
	// never read, let alone compared.
	mock.ExpectQuery(`WHERE account_id = \$1 AND id = \$2`).
		WithArgs("acct-2", "acme-prod").
		WillReturnRows(sqlmock.NewRows(scopeRowCols))

	rec := fleetRequest(t, h.Get, http.MethodGet, "/api/v1/environments/acme-prod", "acct-2", "", "acme-prod")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// The registered environment is stamped with the header's account, not the body's:
// the body is caller-supplied and the header is not.
func TestRegister_StampsAccountFromHeader(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	mock.ExpectQuery(`INSERT INTO meko_system\.environments`).
		WillReturnRows(sqlmock.NewRows([]string{"uuid"}).AddRow("11111111-1111-1111-1111-111111111111"))

	rec := fleetRequest(t, h.Register, http.MethodPost, "/api/v1/environments",
		"acct-1", `{"name":"acme-prod","account_id":"spoofed"}`, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			Environment struct {
				AccountID string `json:"account_id"`
			} `json:"environment"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Data.Environment.AccountID != "acct-1" {
		t.Errorf("account = %q, want acct-1 from the header", body.Data.Environment.AccountID)
	}
}
