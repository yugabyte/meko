package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/labstack/echo/v4"
)

// The admin group carries no account requirement, so these tests send none and the
// lookup is the unscoped one.
func expectUnscopedEnv(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(`FROM meko_system\.environments e WHERE e\.id = \$1 LIMIT 2`).
		WithArgs("acme-prod").
		WillReturnRows(rows)
}

// sharedIDRows is two accounts' amp environments under one id.
func sharedIDRows() *sqlmock.Rows {
	rows := sqlmock.NewRows(scopeRowCols)
	for _, acct := range []string{acctA, acctB} {
		rows.AddRow(envUUID, "acme-prod", acct, "cloud", "us-east-1", "READY", "", 0, "",
			0, 400, "", nil, "", "", "[]", true, keyA, time.Now(), "amp", "[]", nil, "BYOC", false)
	}
	return rows
}

const (
	adminUpsert   = `INSERT INTO meko_system\.environment_desired_resources .* RETURNING generation`
	adminGetQuery = `SELECT COALESCE\(spec::text, '\{\}'\), generation FROM meko_system\.environment_desired_resources`
)

func adminCall(t *testing.T, h echo.HandlerFunc, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	return fleetRequest(t, h, method, "/api/v1/admin/environments/acme-prod/desired/x", "", body, "acme-prod")
}

func wantCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d: %s", rec.Code, status, rec.Body.String())
	}
	if got, _ := errBody(t, rec); got != code {
		t.Errorf("error code = %q, want %q", got, code)
	}
}

// The generation is in the body even at 0, which models.DesiredResource's omitempty
// would drop, and a script reading it has no other way to tell a first write apart.
func TestAdminPutController_FirstWriteCarriesGenerationZero(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()
	expectUnscopedEnv(mock, ampRow("acme-prod", acctA))
	mock.ExpectQuery(adminUpsert).
		WithArgs(envUUID, "controller", "controller", []byte(`{"version":"2.1.0"}`)).
		WillReturnRows(sqlmock.NewRows([]string{"generation"}).AddRow(int64(0)))

	rec := adminCall(t, h.AdminPutController, http.MethodPut, `{"version":"2.1.0"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	data := extensionsBody(t, rec.Body.Bytes())
	if string(data["kind"]) != `"controller"` || string(data["name"]) != `"controller"` ||
		string(data["spec"]) != `{"version":"2.1.0"}` || string(data["generation"]) != "0" {
		t.Errorf("body = %s, want the stored controller row at generation 0", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// The database keeps the generation when the stored spec is unchanged and bumps it
// otherwise, so what this route owes it is the same bytes for the same request —
// whatever order the caller wrote the values in — and different bytes for a change.
func TestAdminPutController_IdenticalRePutKeepsGenerationAndChangeBumpsIt(t *testing.T) {
	const stored = `{"version":"2.1.0","values":{"a.y":{"k":true},"b.x":1}}`
	for _, step := range []struct {
		body string
		spec string
		gen  int64
	}{
		{`{"version":"2.1.0","values":{"b.x":1,"a.y":{"k":true}}}`, stored, 0},
		{`{"values":{"a.y":{"k": true},"b.x":1},"version":"2.1.0"}`, stored, 0},
		{`{"version":"2.2.0","values":{"b.x":1,"a.y":{"k":true}}}`,
			`{"version":"2.2.0","values":{"a.y":{"k":true},"b.x":1}}`, 1},
	} {
		h, mock, done := newScopeHandler(t)
		expectUnscopedEnv(mock, ampRow("acme-prod", acctA))
		mock.ExpectQuery(adminUpsert).
			WithArgs(envUUID, "controller", "controller", []byte(step.spec)).
			WillReturnRows(sqlmock.NewRows([]string{"generation"}).AddRow(step.gen))

		rec := adminCall(t, h.AdminPutController, http.MethodPut, step.body)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s: status = %d, want 202: %s", step.body, rec.Code, rec.Body.String())
		}
		var gen int64
		if err := json.Unmarshal(extensionsBody(t, rec.Body.Bytes())["generation"], &gen); err != nil {
			t.Fatalf("decode generation: %v", err)
		}
		if gen != step.gen {
			t.Errorf("%s: generation = %d, want %d", step.body, gen, step.gen)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("%s: %v", step.body, err)
		}
		done()
	}
}

func TestAdminPutController_Refusals(t *testing.T) {
	const valid = `{"version":"2.1.0"}`
	for name, tc := range map[string]struct {
		env    *sqlmock.Rows // nil: refused before the lookup
		body   string
		code   string
		status int
	}{
		"empty object":         {nil, `{}`, "VALIDATION_ERROR", http.StatusBadRequest},
		"no body":              {nil, ``, "EMPTY_BODY", http.StatusBadRequest},
		"unknown field":        {nil, `{"version":"2.1.0","chartVersion":"x"}`, "INVALID_JSON", http.StatusBadRequest},
		"values not an object": {nil, `{"version":"2.1.0","values":5}`, "INVALID_JSON", http.StatusBadRequest},
		"padded version":       {nil, `{"version":"2.1.0 "}`, "VALIDATION_ERROR", http.StatusBadRequest},
		"padded chart_version": {nil, `{"version":"2.1.0","chart_version":" 1"}`, "VALIDATION_ERROR", http.StatusBadRequest},
		"meko environment":     {scopeRow("acme-prod", acctA), valid, "VALIDATION_ERROR", http.StatusBadRequest},
		"unknown environment":  {sqlmock.NewRows(scopeRowCols), valid, "NOT_FOUND", http.StatusNotFound},
		"id two accounts hold": {sharedIDRows(), valid, "AMBIGUOUS_ID", http.StatusConflict},
	} {
		t.Run(name, func(t *testing.T) {
			h, mock, done := newScopeHandler(t)
			defer done()
			if tc.env != nil {
				expectUnscopedEnv(mock, tc.env)
			}
			wantCode(t, adminCall(t, h.AdminPutController, http.MethodPut, tc.body), tc.status, tc.code)
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("a refused write reached the database: %v", err)
			}
		})
	}
}

// Without a declared controller the agent never rolls the fleet and still reports
// READY, so the declaration is refused rather than left to wait for ever.
func TestAdminPutFleetVersion_RequiresADeclaredController(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()
	expectUnscopedEnv(mock, ampRow("acme-prod", acctA))
	mock.ExpectQuery(adminGetQuery).WithArgs(envUUID, "controller", "controller").
		WillReturnRows(sqlmock.NewRows([]string{"spec", "generation"}))

	wantCode(t, adminCall(t, h.AdminPutFleetVersion, http.MethodPut, `{"version":"2.1.0"}`),
		http.StatusConflict, "CONTROLLER_NOT_DECLARED")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the fleet-version was written without a controller: %v", err)
	}
}

func TestAdminPutFleetVersion_DeclaresIt(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()
	expectUnscopedEnv(mock, ampRow("acme-prod", acctA))
	mock.ExpectQuery(adminGetQuery).WithArgs(envUUID, "controller", "controller").
		WillReturnRows(sqlmock.NewRows([]string{"spec", "generation"}).AddRow(`{"version":"2.1.0"}`, int64(1)))
	mock.ExpectQuery(adminUpsert).
		WithArgs(envUUID, "fleet-version", "fleet", []byte(`{"version":"2.1.0"}`)).
		WillReturnRows(sqlmock.NewRows([]string{"generation"}).AddRow(int64(0)))

	rec := adminCall(t, h.AdminPutFleetVersion, http.MethodPut, `{"version":"2.1.0"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	data := extensionsBody(t, rec.Body.Bytes())
	if string(data["kind"]) != `"fleet-version"` || string(data["name"]) != `"fleet"` ||
		string(data["generation"]) != "0" {
		t.Errorf("body = %s, want the stored fleet row at generation 0", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// An inventory carrying the agent's controller row, which it reports on every tick
// whether or not one is declared, beside a row of another kind.
const controllerInventory = `[{"kind":"cluster","id":"cluster/p/c","name":"c"},` +
	`{"kind":"controller","id":"controller/controller","name":"controller","state":"unmanaged",` +
	`"attrs":{"image_tag":"2.0.0"}}]`

func adminVersionBody(t *testing.T, rec *httptest.ResponseRecorder) (desired, observed json.RawMessage) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	data := extensionsBody(t, rec.Body.Bytes())
	return data["desired"], data["observed"]
}

func TestAdminGetController_UndeclaredStillReportsObserved(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()
	expectUnscopedEnv(mock, ampResourceRow(controllerInventory))
	mock.ExpectQuery(adminGetQuery).WithArgs(envUUID, "controller", "controller").
		WillReturnRows(sqlmock.NewRows([]string{"spec", "generation"}))

	desired, observed := adminVersionBody(t, adminCall(t, h.AdminGetController, http.MethodGet, ""))
	if string(desired) != "null" {
		t.Errorf("desired = %s, want null when nothing is declared", desired)
	}
	var obs struct {
		Attrs map[string]string `json:"attrs"`
		ID    string            `json:"id"`
		State string            `json:"state"`
	}
	if err := json.Unmarshal(observed, &obs); err != nil {
		t.Fatalf("decode observed %s: %v", observed, err)
	}
	if obs.ID != "controller/controller" || obs.State != "unmanaged" || obs.Attrs["image_tag"] != "2.0.0" {
		t.Errorf("observed = %s, want the agent's controller row", observed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestAdminGetController_DeclaredReportsBoth(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()
	expectUnscopedEnv(mock, ampResourceRow(controllerInventory))
	mock.ExpectQuery(adminGetQuery).WithArgs(envUUID, "controller", "controller").
		WillReturnRows(sqlmock.NewRows([]string{"spec", "generation"}).AddRow(`{"version":"2.1.0"}`, int64(2)))

	desired, observed := adminVersionBody(t, adminCall(t, h.AdminGetController, http.MethodGet, ""))
	if string(desired) != `{"kind":"controller","name":"controller","spec":{"version":"2.1.0"},"generation":2}` {
		t.Errorf("desired = %s, want the stored row", desired)
	}
	if string(observed) == "null" {
		t.Error("observed = null, want the agent's controller row")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// The agent reports no fleet row until it has been handed a declaration.
func TestAdminGetFleetVersion_NothingDeclaredOrObserved(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()
	expectUnscopedEnv(mock, ampResourceRow(controllerInventory))
	mock.ExpectQuery(adminGetQuery).WithArgs(envUUID, "fleet-version", "fleet").
		WillReturnRows(sqlmock.NewRows([]string{"spec", "generation"}))

	desired, observed := adminVersionBody(t, adminCall(t, h.AdminGetFleetVersion, http.MethodGet, ""))
	if string(desired) != "null" || string(observed) != "null" {
		t.Errorf("desired = %s, observed = %s, want both null", desired, observed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestAdminGetFleetVersion_UnknownEnvironmentIs404(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()
	expectUnscopedEnv(mock, sqlmock.NewRows(scopeRowCols))
	wantCode(t, adminCall(t, h.AdminGetFleetVersion, http.MethodGet, ""), http.StatusNotFound, "NOT_FOUND")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
