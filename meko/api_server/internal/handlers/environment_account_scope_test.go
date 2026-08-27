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
	"github.com/lib/pq"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// Test accounts and environment key. Named because account ids are uuids: inline
// literals push these expectations past the 120-column limit.
const (
	acctA   = "a0000000-0000-0000-0000-000000000001"
	acctB   = "a0000000-0000-0000-0000-000000000002"
	envUUID = "11111111-1111-1111-1111-111111111111"
	// keyA/keyB are api key ids: keyA is what scopeRow stores, keyB a replacement.
	keyA = "b0000000-0000-0000-0000-00000000000a"
	keyB = "b0000000-0000-0000-0000-00000000000b"
)

var scopeRowCols = []string{
	"uuid", "id", "account_id", "placement", "region", "state", "k8s_version", "node_count", "provider",
	"datapacks", "max_datapacks", "agent_version", "last_heartbeat_at", "mcp_base_url",
	"cert_fingerprint", "components", "live", "api_key_id", "created_at", "kind", "resources",
}

func scopeRow(id, accountID string) *sqlmock.Rows {
	return sqlmock.NewRows(scopeRowCols).AddRow(
		envUUID, id, accountID, "cloud", "us-east-1", "READY", "", 0, "",
		0, 400, "", nil, "", "", "[]", true, keyA, time.Now(), "meko", "[]",
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
		WithArgs(acctA).
		WillReturnRows(scopeRow("acme-prod", acctA))

	rec := fleetRequest(t, h.List, http.MethodGet, "/api/v1/environments", acctA, "", "")
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
		WillReturnRows(scopeRow("acme-prod", acctA))

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
		WithArgs(acctB, "acme-prod").
		WillReturnRows(sqlmock.NewRows(scopeRowCols))

	rec := fleetRequest(t, h.Get, http.MethodGet, "/api/v1/environments/acme-prod", acctB, "", "acme-prod")
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
		WillReturnRows(sqlmock.NewRows([]string{"uuid"}).AddRow(envUUID))

	rec := fleetRequest(t, h.Register, http.MethodPost, "/api/v1/environments",
		acctA, `{"name":"acme-prod","account_id":"`+acctB+`","api_key_id":"`+keyA+`"}`, "")
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
	if body.Data.Environment.AccountID != acctA {
		t.Errorf("account = %q, want acct-1 from the header", body.Data.Environment.AccountID)
	}
}

// A registration without an api_key_id is refused: the key binding is the only
// credential an agent can ever attach with.
func TestRegister_RequiresAPIKeyID(t *testing.T) {
	h, _, done := newScopeHandler(t)
	defer done()

	rec := fleetRequest(t, h.Register, http.MethodPost, "/api/v1/environments",
		acctA, `{"name":"acme-prod"}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "VALIDATION_ERROR") {
		t.Errorf("error code missing from %s", rec.Body.String())
	}
}

// Re-registering an existing name is a 409 with a legible code, not a reset of
// the existing row.
func TestRegister_ExistingNameIs409(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	mock.ExpectQuery(`INSERT INTO meko_system\.environments`).
		WillReturnError(&pq.Error{
			Code:       "23505",
			Constraint: "environments_account_id_uniq",
			Message:    `duplicate key value violates unique constraint "environments_account_id_uniq"`,
		})

	rec := fleetRequest(t, h.Register, http.MethodPost, "/api/v1/environments",
		acctA, `{"name":"acme-prod","api_key_id":"`+keyB+`"}`, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ENVIRONMENT_EXISTS") {
		t.Errorf("error code missing from %s", rec.Body.String())
	}
}

// One key drives one environment, so registering a key another environment already
// holds is a 409 with a legible code — not a 500 carrying raw driver text.
func TestRegister_KeyBoundElsewhereIs409(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	mock.ExpectQuery(`INSERT INTO meko_system\.environments`).
		WillReturnError(&pq.Error{
			Code:       "23505",
			Constraint: "environments_api_key_id_uniq",
			Message:    `duplicate key value violates unique constraint "environments_api_key_id_uniq"`,
		})

	rec := fleetRequest(t, h.Register, http.MethodPost, "/api/v1/environments",
		acctA, `{"name":"acme-prod","api_key_id":"`+keyA+`"}`, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "API_KEY_BOUND") {
		t.Errorf("error code missing from %s", rec.Body.String())
	}
}

// A PATCH deregister is the same operation as DELETE reached another way, so it
// has to hand back the binding too — otherwise YBM cannot tell which key to revoke.
func TestAction_DeregisterReturnsAPIKeyID(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	mock.ExpectQuery(`WHERE account_id = \$1 AND id = \$2`).
		WithArgs(acctA, "acme-prod").
		WillReturnRows(scopeRow("acme-prod", acctA))
	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := fleetRequest(t, h.Action, http.MethodPatch, "/api/v1/environments/acme-prod",
		acctA, `{"action":"deregister"}`, "acme-prod")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			APIKeyID string `json:"api_key_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Data.APIKeyID != keyA {
		t.Errorf("api_key_id = %q, want %q", body.Data.APIKeyID, keyA)
	}
}

// The other lifecycle actions leave the binding in place, so they keep their bare
// 202 rather than implying there is a key to revoke.
func TestAction_SuspendStaysNoContent(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	mock.ExpectQuery(`WHERE account_id = \$1 AND id = \$2`).
		WithArgs(acctA, "acme-prod").
		WillReturnRows(scopeRow("acme-prod", acctA))
	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := fleetRequest(t, h.Action, http.MethodPatch, "/api/v1/environments/acme-prod",
		acctA, `{"action":"suspend"}`, "acme-prod")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
}

// Deregistration hands back the environment's key binding: nothing authenticates
// with it any more, so YBM revokes it.
func TestDeregister_ReturnsAPIKeyID(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	mock.ExpectQuery(`WHERE account_id = \$1 AND id = \$2`).
		WithArgs(acctA, "acme-prod").
		WillReturnRows(scopeRow("acme-prod", acctA))
	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := fleetRequest(t, h.Deregister, http.MethodDelete, "/api/v1/environments/acme-prod",
		acctA, "", "acme-prod")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			APIKeyID string `json:"api_key_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Data.APIKeyID != keyA {
		t.Errorf("api_key_id = %q, want the deleted environment's binding %q", body.Data.APIKeyID, keyA)
	}
}
