package handlers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

type internalEnv struct {
	e       *echo.Echo
	handler *handlers.MCPPATInternalHandler
	mock    sqlmock.Sqlmock
	close   func()
}

func newInternalEnv(t *testing.T) *internalEnv {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	patRepo := repository.NewMCPPATRepo(db)
	auditRepo := repository.NewMCPPATAuditRepo(db)
	svc := service.NewMCPPATService(patRepo, auditRepo)
	h := handlers.NewMCPPATInternalHandler(svc)
	return &internalEnv{
		e:       echo.New(),
		handler: h,
		mock:    mock,
		close: func() {
			if cErr := db.Close(); cErr != nil {
				t.Logf("db.Close: %v", cErr)
			}
		},
	}
}

var validateRowCols = []string{
	"id", "user_id", "name", "token_prefix",
	"created_at", "last_used_at", "expires_at", "revoked_at", "scopes",
}

func TestMCPPATInternalHandler_Validate_BadJSON(t *testing.T) {
	env := newInternalEnv(t)
	defer env.close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/meko-tokens/validate",
		bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := env.e.NewContext(req, rec)

	if err := env.handler.Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestMCPPATInternalHandler_Validate_EmptyToken(t *testing.T) {
	env := newInternalEnv(t)
	defer env.close()

	body, _ := json.Marshal(models.ValidatePATRequest{Token: ""})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/meko-tokens/validate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := env.e.NewContext(req, rec)

	if err := env.handler.Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestMCPPATInternalHandler_Validate_UnknownToken(t *testing.T) {
	env := newInternalEnv(t)
	defer env.close()

	env.mock.ExpectQuery(`WHERE token_hash = \$1`).
		WillReturnRows(sqlmock.NewRows(validateRowCols))

	plaintext, _, _, err := auth.GeneratePAT()
	if err != nil {
		t.Fatalf("GeneratePAT: %v", err)
	}
	body, _ := json.Marshal(models.ValidatePATRequest{Token: plaintext})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/meko-tokens/validate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := env.e.NewContext(req, rec)

	if err := env.handler.Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var resp handlers.APIResponse
	if dErr := json.Unmarshal(rec.Body.Bytes(), &resp); dErr != nil {
		t.Fatalf("decode: %v", dErr)
	}
	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected data map, got %T", resp.Data)
	}
	if data["valid"] != false {
		t.Errorf("expected valid=false, got %v", data["valid"])
	}
	if data["reason"] != string(models.MCPPATValidationUnknown) {
		t.Errorf("expected reason=unknown, got %v", data["reason"])
	}
}

func TestMCPPATInternalHandler_Validate_ActiveToken(t *testing.T) {
	env := newInternalEnv(t)
	defer env.close()

	now := time.Now().UTC()
	expires := now.Add(24 * time.Hour)
	env.mock.ExpectQuery(`WHERE token_hash = \$1`).
		WillReturnRows(sqlmock.NewRows(validateRowCols).AddRow(
			"pat-1", "user-1", "cursor", "mko_tkn_abcdef00",
			now.Add(-time.Hour), nil, expires, nil, patScopesLit("*"),
		))

	plaintext, _, _, err := auth.GeneratePAT()
	if err != nil {
		t.Fatalf("GeneratePAT: %v", err)
	}
	body, _ := json.Marshal(models.ValidatePATRequest{Token: plaintext})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/meko-tokens/validate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := env.e.NewContext(req, rec)

	if err := env.handler.Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp handlers.APIResponse
	if dErr := json.Unmarshal(rec.Body.Bytes(), &resp); dErr != nil {
		t.Fatalf("decode: %v", dErr)
	}
	data := resp.Data.(map[string]any)
	if data["valid"] != true {
		t.Errorf("expected valid=true, got %v", data["valid"])
	}
	if data["token_id"] != "pat-1" || data["user_id"] != "user-1" {
		t.Errorf("unexpected payload: %+v", data)
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATInternalHandler_Validate_RevokedTokenWithAudit(t *testing.T) {
	env := newInternalEnv(t)
	defer env.close()

	now := time.Now().UTC()
	env.mock.ExpectQuery(`WHERE token_hash = \$1`).
		WillReturnRows(sqlmock.NewRows(validateRowCols).AddRow(
			"pat-r", "user-1", "cursor", "mko_tkn_abcdef00",
			now.Add(-2*time.Hour), nil, now.Add(time.Hour),
			now.Add(-time.Minute), patScopesLit("*"),
		))
	env.mock.ExpectQuery(`INSERT INTO meko_system\.mcp_pat_audit_log`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("audit-1"))

	plaintext, _, _, err := auth.GeneratePAT()
	if err != nil {
		t.Fatalf("GeneratePAT: %v", err)
	}
	body, _ := json.Marshal(models.ValidatePATRequest{Token: plaintext})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/meko-tokens/validate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := env.e.NewContext(req, rec)

	if err := env.handler.Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp handlers.APIResponse
	if dErr := json.Unmarshal(rec.Body.Bytes(), &resp); dErr != nil {
		t.Fatalf("decode: %v", dErr)
	}
	data := resp.Data.(map[string]any)
	if data["valid"] != false || data["reason"] != string(models.MCPPATValidationRevoked) {
		t.Errorf("expected revoked invalid, got %+v", data)
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestMCPPATInternalHandler_Validate_ClientFieldsForwarded asserts
// that body-forwarded client_ip / client_user_agent override the
// direct HTTP connection's identity when persisting the audit row.
// This is the realistic MCP-server-forwards-Cursor-identity flow.
func TestMCPPATInternalHandler_Validate_ClientFieldsForwarded(t *testing.T) {
	env := newInternalEnv(t)
	defer env.close()

	now := time.Now().UTC()
	env.mock.ExpectQuery(`WHERE token_hash = \$1`).
		WillReturnRows(sqlmock.NewRows(validateRowCols).AddRow(
			"pat-r", "user-1", "cursor", "mko_tkn_abcdef00",
			now.Add(-2*time.Hour), nil, now.Add(time.Hour),
			now.Add(-time.Minute), patScopesLit("*"),
		))
	// Args 6 (actor_ip) and 7 (actor_app_client) must be the
	// body-forwarded values, NOT the test request's RemoteAddr / UA.
	env.mock.ExpectQuery(`INSERT INTO meko_system\.mcp_pat_audit_log`).
		WithArgs(
			"pat-r", "user-1", string(models.MCPPATAuditRevokedUsed),
			"user-1", string(models.MCPPATActorSystem),
			"203.0.113.45", "Cursor/0.42.5 (mcp-client)", sqlmock.AnyArg(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("audit-1"))

	plaintext, _, _, err := auth.GeneratePAT()
	if err != nil {
		t.Fatalf("GeneratePAT: %v", err)
	}
	body, _ := json.Marshal(models.ValidatePATRequest{
		Token:           plaintext,
		ClientIP:        "203.0.113.45",
		ClientUserAgent: "Cursor/0.42.5 (mcp-client)",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/meko-tokens/validate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Deliberately set a misleading direct UA / address so we can
	// prove the body fields take precedence over the connection.
	req.Header.Set("User-Agent", "Go-http-client/1.1")
	req.RemoteAddr = "10.0.137.22:54321"
	rec := httptest.NewRecorder()
	c := env.e.NewContext(req, rec)

	if err := env.handler.Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestMCPPATInternalHandler_Validate_ClientFieldsFallback asserts
// that when the MCP server omits client_ip / client_user_agent (older
// build, or non-MCP caller), the api_server falls back to the direct
// HTTP connection's RemoteAddr-derived IP and inbound User-Agent.
// This documents the degraded-but-functional behaviour for old MCP
// builds and lets the rename land without forcing a synchronized
// MCP server release.
func TestMCPPATInternalHandler_Validate_ClientFieldsFallback(t *testing.T) {
	env := newInternalEnv(t)
	defer env.close()

	now := time.Now().UTC()
	env.mock.ExpectQuery(`WHERE token_hash = \$1`).
		WillReturnRows(sqlmock.NewRows(validateRowCols).AddRow(
			"pat-r", "user-1", "cursor", "mko_tkn_abcdef00",
			now.Add(-2*time.Hour), nil, now.Add(time.Hour),
			now.Add(-time.Minute), patScopesLit("*"),
		))
	env.mock.ExpectQuery(`INSERT INTO meko_system\.mcp_pat_audit_log`).
		WithArgs(
			"pat-r", "user-1", string(models.MCPPATAuditRevokedUsed),
			"user-1", string(models.MCPPATActorSystem),
			"10.0.137.22", "Go-http-client/1.1", sqlmock.AnyArg(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("audit-1"))

	plaintext, _, _, err := auth.GeneratePAT()
	if err != nil {
		t.Fatalf("GeneratePAT: %v", err)
	}
	body, _ := json.Marshal(models.ValidatePATRequest{Token: plaintext})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/meko-tokens/validate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/1.1")
	req.RemoteAddr = "10.0.137.22:54321"
	rec := httptest.NewRecorder()
	c := env.e.NewContext(req, rec)

	if err := env.handler.Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
