package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
)

// In ExtensionImport's field order, since the stored spec is its encoding.
const postgisImport = `{"version":"3.5","layout":{"share_path":"share","env_paths":{"PROJ_DATA":"share/proj"}},` +
	`"image":"quay.io/acme/postgis:3.5"}`

// extensionRequest is fleetRequest with the :name parameter as well.
func extensionRequest(
	t *testing.T, h echo.HandlerFunc, method, name, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/v1/environments/acme-prod/extensions/" + url.PathEscape(name)
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.AccountIDHeader, acctA)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("id", "name")
	c.SetParamValues("acme-prod", name)
	if err := h(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	return rec
}

func expectScopedEnv(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctA, "acme-prod").
		WillReturnRows(rows)
}

func TestImportExtension_DeclaresIt(t *testing.T) {
	h, mock, done := newExtensionsHandler(t)
	defer done()
	expectScopedEnv(mock, ampRow("acme-prod", acctA))
	mock.ExpectQuery(`INSERT INTO meko_system\.environment_desired_resources .* RETURNING generation`).
		WithArgs(envUUID, "extension", "postgis", []byte(postgisImport)).
		WillReturnRows(sqlmock.NewRows([]string{"generation"}).AddRow(2))

	rec := extensionRequest(t, h.ImportExtension, http.MethodPut, "postgis", postgisImport)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	data := extensionsBody(t, rec.Body.Bytes())
	if string(data["kind"]) != `"extension"` || string(data["name"]) != `"postgis"` || string(data["generation"]) != "2" {
		t.Errorf("body = %s, want the stored extension row at generation 2", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Every refusal is decided before anything is written.
func TestImportExtension_Refusals(t *testing.T) {
	for name, tc := range map[string]struct {
		env  *sqlmock.Rows // nil: the request never gets as far as the lookup
		ext  string
		body string
		want int
	}{
		"unknown field":  {nil, "postgis", `{"image":"quay.io/acme/postgis:3.5","imagee":"x"}`, http.StatusBadRequest},
		"nested unknown": {nil, "postgis", `{"image":"q/p:3","layout":{"shar_path":"x"}}`, http.StatusBadRequest},
		"empty body":     {nil, "postgis", ``, http.StatusBadRequest},
		"missing image":  {ampRow("acme-prod", acctA), "postgis", `{"version":"3.5"}`, http.StatusBadRequest},
		"meko env":       {scopeRow("acme-prod", acctA), "postgis", postgisImport, http.StatusBadRequest},
		"path name":      {ampRow("acme-prod", acctA), "a/b", postgisImport, http.StatusBadRequest},
		"padded name":    {ampRow("acme-prod", acctA), " postgis", postgisImport, http.StatusBadRequest},
		"padded image":   {ampRow("acme-prod", acctA), "postgis", `{"image":" q/p:3"}`, http.StatusBadRequest},
		"other account":  {sqlmock.NewRows(scopeRowCols), "postgis", postgisImport, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			h, mock, done := newExtensionsHandler(t)
			defer done()
			if tc.env != nil {
				expectScopedEnv(mock, tc.env)
			}
			rec := extensionRequest(t, h.ImportExtension, http.MethodPut, tc.ext, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("a refused import reached the DB: %v", err)
			}
		})
	}
}

// The delete is scoped to the extension kind, so a same-named pool is untouched.
func TestRemoveExtension_WithdrawsOnlyTheExtension(t *testing.T) {
	h, mock, done := newExtensionsHandler(t)
	defer done()
	expectScopedEnv(mock, ampRow("acme-prod", acctA))
	mock.ExpectExec(`DELETE FROM meko_system\.environment_desired_resources WHERE env_uuid=\$1 AND kind=\$2 AND name=\$3`).
		WithArgs(envUUID, "extension", "postgis").
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := extensionRequest(t, h.RemoveExtension, http.MethodDelete, "postgis", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestRemoveExtension_RefusesAMekoEnvironment(t *testing.T) {
	h, mock, done := newExtensionsHandler(t)
	defer done()
	expectScopedEnv(mock, scopeRow("acme-prod", acctA))
	rec := extensionRequest(t, h.RemoveExtension, http.MethodDelete, "postgis", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
