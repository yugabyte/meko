package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

func newExtensionsHandler(t *testing.T) (*EnvironmentHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	svc := service.NewEnvironmentService(
		repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db))
	svc.SetExtensionRepo(repository.NewEnvironmentExtensionRepo(db))
	return NewEnvironmentHandler(svc), mock, func() { db.Close() }
}

// ampRow is scopeRow for an amp environment, the only kind that has a catalog.
func ampRow(id, accountID string) *sqlmock.Rows {
	return sqlmock.NewRows(scopeRowCols).AddRow(
		envUUID, id, accountID, "cloud", "us-east-1", "READY", "", 0, "",
		0, 400, "", nil, "", "", "[]", true, keyA, time.Now(), "amp", "[]", nil, "BYOC", false,
	)
}

func expectResolveAmpAgent(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`WHERE e\.uuid = \(SELECT env_uuid FROM meko_system\.environment_api_keys`).
		WithArgs(keyA).
		WillReturnRows(ampRow("amp-prod", acctA))
}

// A meko environment has no catalog, so its key cannot store one: the documented
// "meko reads null" must not depend on what the meko agent happens to send.
func TestAgentExtensions_RefusesANonAmpEnvironment(t *testing.T) {
	h, mock, done := newExtensionsHandler(t)
	defer done()
	expectResolveAgent(mock) // scopeRow is kind meko
	rec := agentRequest(t, h.AgentExtensions, http.MethodPost, "/api/v1/agent/extensions",
		echo.MIMEApplicationJSON, `{"extensions":[{"name":"pgvector","builtin":true}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a meko environment's catalog reached the DB: %v", err)
	}
}

func TestAgentExtensions_StoresTheCatalog(t *testing.T) {
	h, mock, done := newExtensionsHandler(t)
	defer done()
	expectResolveAmpAgent(mock)
	mock.ExpectExec(`INSERT INTO meko_system\.environment_extensions`).
		WithArgs(envUUID, []byte(`[{"name":"pgvector","version":"0.8.0","builtin":true,"preload":false}]`)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := agentRequest(t, h.AgentExtensions, http.MethodPost, "/api/v1/agent/extensions",
		echo.MIMEApplicationJSON, `{"extensions":[{"name":"pgvector","builtin":true,"version":"0.8.0"}]}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// An empty catalog is a fact and is stored; a missing or null one is not a catalog
// at all, and storing it as [] is exactly what would make the console offer nothing.
func TestAgentExtensions_EmptyIsStoredButMissingIsRejected(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		h, mock, done := newExtensionsHandler(t)
		defer done()
		expectResolveAmpAgent(mock)
		mock.ExpectExec(`INSERT INTO meko_system\.environment_extensions`).
			WithArgs(envUUID, []byte(`[]`)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		rec := agentRequest(t, h.AgentExtensions, http.MethodPost, "/api/v1/agent/extensions",
			echo.MIMEApplicationJSON, `{"extensions":[]}`)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
	for name, body := range map[string]string{
		"missing":      `{}`,
		"null":         `{"extensions":null}`,
		"unnamed item": `{"extensions":[{"builtin":true}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			h, mock, done := newExtensionsHandler(t)
			defer done()
			expectResolveAmpAgent(mock)
			rec := agentRequest(t, h.AgentExtensions, http.MethodPost, "/api/v1/agent/extensions",
				echo.MIMEApplicationJSON, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			// Only the agent lookup was registered, so a write fails this.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("a rejected catalog reached the DB: %v", err)
			}
		})
	}
}

func extensionsBody(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var body struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return body.Data
}

// The console's fallback rides on this: never reported must be null, not [].
func TestExtensions_NeverReportedIsNull(t *testing.T) {
	h, mock, done := newExtensionsHandler(t)
	defer done()
	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctA, "acme-prod").
		WillReturnRows(scopeRow("acme-prod", acctA))
	mock.ExpectQuery(`FROM meko_system\.environment_extensions`).
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"catalog", "reported_at"}))

	rec := fleetRequest(t, h.Extensions, http.MethodGet,
		"/api/v1/environments/acme-prod/extensions", acctA, "", "acme-prod")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	data := extensionsBody(t, rec.Body.Bytes())
	if string(data["extensions"]) != "null" || string(data["reported_at"]) != "null" {
		t.Fatalf("got %s, want extensions and reported_at null", rec.Body.String())
	}
}

func TestExtensions_ServesTheRelayedCatalog(t *testing.T) {
	h, mock, done := newExtensionsHandler(t)
	defer done()
	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctA, "acme-prod").
		WillReturnRows(scopeRow("acme-prod", acctA))
	mock.ExpectQuery(`FROM meko_system\.environment_extensions`).
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"catalog", "reported_at"}).
			AddRow([]byte(`[{"name":"my_ext","builtin":false,"preload":true}]`), time.Now()))

	rec := fleetRequest(t, h.Extensions, http.MethodGet,
		"/api/v1/environments/acme-prod/extensions", acctA, "", "acme-prod")
	data := extensionsBody(t, rec.Body.Bytes())
	var exts []map[string]any
	if err := json.Unmarshal(data["extensions"], &exts); err != nil || len(exts) != 1 ||
		exts[0]["name"] != "my_ext" || exts[0]["preload"] != true {
		t.Fatalf("got %s, want the relayed my_ext entry", rec.Body.String())
	}
}

// Same authorization rule as the sibling fleet routes: another account's
// environment is a 404, and the catalog table is never read.
func TestExtensions_OtherAccountIsNotFound(t *testing.T) {
	h, mock, done := newExtensionsHandler(t)
	defer done()
	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctB, "acme-prod").
		WillReturnRows(sqlmock.NewRows(scopeRowCols))

	rec := fleetRequest(t, h.Extensions, http.MethodGet,
		"/api/v1/environments/acme-prod/extensions", acctB, "", "acme-prod")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the catalog was read for an environment the caller cannot see: %v", err)
	}
}

const regionExtensionsPath = "/api/v1/environments/resources/extensions"

// managedRow is a READY MANAGED amp environment. accountID is empty for the usual
// operator registration, which belongs to no account.
func managedRow(accountID string) *sqlmock.Rows {
	return sqlmock.NewRows(scopeRowCols).AddRow(
		envUUID, "amp-managed-use1", accountID, "cloud", "us-east-1", "READY", "", 0, "",
		0, 400, "", nil, "", "", "[]", true, keyA, time.Now(), "amp", "[]", nil, "MANAGED", false,
	)
}

const pickQueryPrefix = `SELECT uuid FROM meko_system\.environments\s+` +
	`WHERE type = 'MANAGED' AND COALESCE\(kind, 'meko'\) = 'amp'`

// expectPickManagedEnv is the pick finding envUUID in us-east-1, inside the read-only
// transaction PickManagedEnv rolls back, then the environment read back by uuid.
func expectPickManagedEnv(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectQuery(pickQueryPrefix).
		WithArgs("us-east-1").
		WillReturnRows(sqlmock.NewRows([]string{"uuid"}).AddRow(envUUID))
	mock.ExpectRollback()
	mock.ExpectQuery(`WHERE e\.uuid = \$1`).
		WithArgs(envUUID).
		WillReturnRows(managedRow(""))
}

func TestExtensionsByRegion_RequiresARegion(t *testing.T) {
	h, mock, done := newExtensionsHandler(t)
	defer done()
	for _, target := range []string{regionExtensionsPath, regionExtensionsPath + "?region=%20"} {
		rec := fleetRequest(t, h.ExtensionsByRegion, http.MethodGet, target, "", "", "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400: %s", target, rec.Code, rec.Body.String())
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a request with no region reached the DB: %v", err)
	}
}

// The same answers the region PATCH gives when nothing is eligible: 404 for a region
// with no MANAGED environment, a retryable 503 for one whose environments cannot take
// a new pool right now.
func TestExtensionsByRegion_NothingEligible(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
		served bool
	}{
		{"NOT_FOUND", http.StatusNotFound, false},
		{"ENVIRONMENT_UNAVAILABLE", http.StatusServiceUnavailable, true},
	} {
		t.Run(tc.code, func(t *testing.T) {
			h, mock, done := newExtensionsHandler(t)
			defer done()
			mock.ExpectBegin()
			mock.ExpectQuery(pickQueryPrefix).
				WithArgs("us-east-1").
				WillReturnRows(sqlmock.NewRows([]string{"uuid"}))
			mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM meko_system\.environments`).
				WithArgs("us-east-1").
				WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(tc.served))
			mock.ExpectRollback()

			rec := fleetRequest(t, h.ExtensionsByRegion, http.MethodGet,
				regionExtensionsPath+"?region=us-east-1", "", "", "")
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
				t.Fatalf("status = %d, want %d %s: %s", rec.Code, tc.status, tc.code, rec.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
		})
	}
}

// A MANAGED environment has no account, so an account header must not narrow the
// lookup: the mock is strict, and an account-scoped query would fail it.
func TestExtensionsByRegion_ServesThePickedEnvironmentsCatalog(t *testing.T) {
	h, mock, done := newExtensionsHandler(t)
	defer done()
	expectPickManagedEnv(mock)
	mock.ExpectQuery(`FROM meko_system\.environment_extensions`).
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"catalog", "reported_at"}).
			AddRow([]byte(`[{"name":"pgvector","version":"0.8.2","builtin":true,"preload":false}]`), time.Now()))

	rec := fleetRequest(t, h.ExtensionsByRegion, http.MethodGet,
		regionExtensionsPath+"?region=us-east-1", acctB, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	data := extensionsBody(t, rec.Body.Bytes())
	if string(data["environment_id"]) != `"amp-managed-use1"` || string(data["region"]) != `"us-east-1"` ||
		string(data["reported_at"]) == "null" {
		t.Fatalf("got %s, want amp-managed-use1 in us-east-1 with a reported_at", rec.Body.String())
	}
	var exts []map[string]any
	if err := json.Unmarshal(data["extensions"], &exts); err != nil || len(exts) != 1 || exts[0]["name"] != "pgvector" {
		t.Fatalf("got %s, want the relayed pgvector entry", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestExtensionsByRegion_NeverReportedIsNull(t *testing.T) {
	h, mock, done := newExtensionsHandler(t)
	defer done()
	expectPickManagedEnv(mock)
	mock.ExpectQuery(`FROM meko_system\.environment_extensions`).
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"catalog", "reported_at"}))

	rec := fleetRequest(t, h.ExtensionsByRegion, http.MethodGet,
		regionExtensionsPath+"?region=us-east-1", "", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	data := extensionsBody(t, rec.Body.Bytes())
	if string(data["extensions"]) != "null" || string(data["reported_at"]) != "null" {
		t.Fatalf("got %s, want extensions and reported_at null", rec.Body.String())
	}
}
