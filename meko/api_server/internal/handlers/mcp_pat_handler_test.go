package handlers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// patTestCols matches the column ordering the repo SELECT/RETURNING
// statements use after the scopes column was added in 000004.
var patTestCols = []string{
	"id", "user_id", "name", "token_prefix",
	"created_at", "last_used_at", "expires_at", "revoked_at", "scopes",
}

// patScopesLit renders a Postgres TEXT[] literal usable as a sqlmock
// driver.Value for the scopes column.
func patScopesLit(scopes ...string) string {
	if len(scopes) == 0 {
		return "{}"
	}
	out := "{"
	for i, s := range scopes {
		if i > 0 {
			out += ","
		}
		out += `"` + s + `"`
	}
	out += "}"
	return out
}

// patTestEnv wires a real handler against a sqlmock-backed service so
// the handler tests can exercise both happy paths and error mappings
// without needing a live database.
type patTestEnv struct {
	e       *echo.Echo
	handler *handlers.MCPPATHandler
	mock    sqlmock.Sqlmock
	close   func()
}

func newPATTestEnv(t *testing.T) *patTestEnv {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	patRepo := repository.NewMCPPATRepo(db)
	auditRepo := repository.NewMCPPATAuditRepo(db)
	svc := service.NewMCPPATService(patRepo, auditRepo)
	h := handlers.NewMCPPATHandler(svc)

	e := echo.New()
	return &patTestEnv{
		e:       e,
		handler: h,
		mock:    mock,
		close:   func() { _ = db.Close() },
	}
}

// withAccount installs a synthetic account on the request context so
// the handler's auth.GetAccount lookup succeeds.
//
//nolint:unparam // userID is the canonical test fixture; helper kept for explicit per-test override.
func (env *patTestEnv) withAccount(c echo.Context, userID string) {
	c.Set(auth.ContextKeyAccount, &models.Account{ID: userID})
}

func TestMCPPATHandler_Create_HappyPath(t *testing.T) {
	env := newPATTestEnv(t)
	defer env.close()

	now := time.Now().UTC()
	expires := now.AddDate(0, 0, 90)
	env.mock.ExpectQuery(`SELECT COUNT\(\*\)`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	env.mock.ExpectQuery(`SELECT 1\s+FROM meko_system\.mcp_personal_access_tokens`).
		WithArgs("user-1", "cursor").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}))
	env.mock.ExpectQuery(`INSERT INTO meko_system\.mcp_personal_access_tokens`).
		WithArgs("user-1", "cursor", sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(patTestCols).AddRow(
			"pat-1", "user-1", "cursor", "mko_tkn_abcdef00",
			now, nil, expires, nil, patScopesLit("*"),
		))
	env.mock.ExpectQuery(`INSERT INTO meko_system\.mcp_pat_audit_log`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("audit-1"))

	body, _ := json.Marshal(models.CreateMCPTokenRequest{Name: "cursor"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/meko-tokens", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := env.e.NewContext(req, rec)
	env.withAccount(c, "user-1")

	if err := env.handler.Create(c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	var resp handlers.APIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected data map, got %T", resp.Data)
	}
	tok, _ := data["token"].(string)
	if !strings.HasPrefix(tok, auth.PATScheme) {
		t.Errorf("expected token to start with %s, got %q", auth.PATScheme, tok)
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATHandler_Create_ValidationError(t *testing.T) {
	env := newPATTestEnv(t)
	defer env.close()

	body, _ := json.Marshal(models.CreateMCPTokenRequest{Name: ""})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/meko-tokens", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := env.e.NewContext(req, rec)
	env.withAccount(c, "user-1")

	if err := env.handler.Create(c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}

	var resp handlers.APIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != "VALIDATION_ERROR" {
		t.Errorf("expected VALIDATION_ERROR, got %+v", resp.Error)
	}
}

func TestMCPPATHandler_Create_CapReachedReturns429(t *testing.T) {
	env := newPATTestEnv(t)
	defer env.close()

	env.mock.ExpectQuery(`SELECT COUNT\(\*\)`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(service.MaxActivePATsPerUser))

	body, _ := json.Marshal(models.CreateMCPTokenRequest{Name: "cursor"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/meko-tokens", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := env.e.NewContext(req, rec)
	env.withAccount(c, "user-1")

	if err := env.handler.Create(c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}

	var resp handlers.APIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != "TOKEN_CAP_REACHED" {
		t.Errorf("expected TOKEN_CAP_REACHED, got %+v", resp.Error)
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATHandler_Create_Unauthorized(t *testing.T) {
	env := newPATTestEnv(t)
	defer env.close()

	body, _ := json.Marshal(models.CreateMCPTokenRequest{Name: "cursor"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/meko-tokens", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := env.e.NewContext(req, rec)

	if err := env.handler.Create(c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestMCPPATHandler_List_HappyPath(t *testing.T) {
	env := newPATTestEnv(t)
	defer env.close()

	now := time.Now().UTC()
	expires := now.AddDate(0, 0, 30)
	env.mock.ExpectQuery(`FROM meko_system\.mcp_personal_access_tokens\s+WHERE user_id = \$1`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows(patTestCols).AddRow(
			"pat-1", "user-1", "cursor", "mko_tkn_abcdef00",
			now, nil, expires, nil, patScopesLit("*"),
		))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/meko-tokens", nil)
	rec := httptest.NewRecorder()
	c := env.e.NewContext(req, rec)
	env.withAccount(c, "user-1")

	if err := env.handler.List(c); err != nil {
		t.Fatalf("List: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp handlers.APIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	items, ok := resp.Data.([]any)
	if !ok {
		t.Fatalf("expected []any data, got %T", resp.Data)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 item, got %d", len(items))
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATHandler_Revoke_HappyPath(t *testing.T) {
	env := newPATTestEnv(t)
	defer env.close()

	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	env.mock.ExpectQuery(`WHERE user_id = \$1 AND id = \$2`).
		WithArgs("user-1", "pat-1").
		WillReturnRows(sqlmock.NewRows(patTestCols).AddRow(
			"pat-1", "user-1", "cursor", "mko_tkn_abcdef00",
			now, nil, expires, nil, patScopesLit("*"),
		))
	env.mock.ExpectExec(`UPDATE meko_system\.mcp_personal_access_tokens\s+SET revoked_at`).
		WithArgs("user-1", "pat-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	env.mock.ExpectQuery(`INSERT INTO meko_system\.mcp_pat_audit_log`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("audit-1"))

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/me/meko-tokens/pat-1", nil)
	rec := httptest.NewRecorder()
	c := env.e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("pat-1")
	env.withAccount(c, "user-1")

	if err := env.handler.Revoke(c); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATHandler_Revoke_NotFoundReturns404(t *testing.T) {
	env := newPATTestEnv(t)
	defer env.close()

	env.mock.ExpectQuery(`WHERE user_id = \$1 AND id = \$2`).
		WithArgs("user-1", "pat-ghost").
		WillReturnRows(sqlmock.NewRows(patTestCols))

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/me/meko-tokens/pat-ghost", nil)
	rec := httptest.NewRecorder()
	c := env.e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("pat-ghost")
	env.withAccount(c, "user-1")

	if err := env.handler.Revoke(c); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATHandler_Revoke_MissingIDReturns400(t *testing.T) {
	env := newPATTestEnv(t)
	defer env.close()

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/me/meko-tokens/", nil)
	rec := httptest.NewRecorder()
	c := env.e.NewContext(req, rec)
	env.withAccount(c, "user-1")

	if err := env.handler.Revoke(c); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}
