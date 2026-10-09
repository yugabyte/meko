package handlers

import (
	"net/http"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// ctEndpoint is the ticket's endpoint (CLOUDGA-37410), under the test pool.
const ctEndpoint = poolUUID + "/elegant-dog/main/rw"

const ctStored = `{"connectivity":{"mode":"nlb_external","allowed_cidrs":["103.200.215.98/32"]}}`

// expectEndpointUpdate scripts a region PATCH up to the endpoint's locked spec
// read: the pinned environment, an existing pool, then the stored spec.
func expectEndpointUpdate(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(poolUUID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs(poolUUID, "us-west1").
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}).AddRow(envUUID, "READY"))
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs(envUUID, poolUUID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`SELECT COALESCE\(spec::text`).
		WithArgs(envUUID, "endpoint", ctEndpoint).
		WillReturnRows(sqlmock.NewRows([]string{"spec"}).AddRow(ctStored))
}

func endpointPatchBody(spec string) string {
	return `{"storage_pool":"` + poolUUID + `","regions":[{"region":"us-west1","resources":[` +
		`{"kind":"endpoint","name":"` + ctEndpoint + `","update":true,"spec":` + spec + `}]}]}`
}

// The ticket's request: adding a CIDR is applied in place, so it is stored and
// answered 200 without consulting the inventory.
func TestSetResourcesByRegion_AllowedCidrsChangeIsStored(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	expectEndpointUpdate(mock)
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "endpoint", ctEndpoint,
			[]byte(`{"connectivity":{"mode":"nlb_external","allowed_cidrs":["103.200.215.98/32","192.0.2.10/32"]}}`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(`WHERE e\.uuid = \$1`).WithArgs(envUUID).
		WillReturnRows(ampResourceRow("[]"))
	mock.ExpectQuery(`FROM meko_system\.environment_desired_resources`).
		WithArgs(envUUID).
		WillReturnRows(desiredRows())

	rec := fleetRequest(t, h.SetResourcesByRegion, http.MethodPatch, "/api/v1/environments/resources", "",
		endpointPatchBody(`{"connectivity":{"mode":"nlb_external",`+
			`"allowed_cidrs":["103.200.215.98/32","192.0.2.10/32"]}}`), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A mode change on an endpoint the agent lists cannot be applied, so it is a 400
// that names the field and writes nothing — not a 200 left as drift.
func TestSetResourcesByRegion_ModeChangeOnAListedEndpointIs400(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	expectEndpointUpdate(mock)
	mock.ExpectQuery(`SELECT COALESCE\(resources::text, '\[\]'\) FROM meko_system\.environments WHERE uuid=\$1`).
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"resources"}).AddRow(
			`[{"kind":"endpoint","id":"endpoint/` + ctEndpoint + `","state":"ready","attrs":{"id":"66e59001"}}]`))
	mock.ExpectRollback()

	rec := fleetRequest(t, h.SetResourcesByRegion, http.MethodPatch, "/api/v1/environments/resources", "",
		endpointPatchBody(`{"connectivity":{"mode":"nlb_internal","allowed_cidrs":["103.200.215.98/32"]}}`), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if b := rec.Body.String(); !strings.Contains(b, "CREATE_TIME_FIELD") || !strings.Contains(b, "connectivity.mode") {
		t.Errorf("body = %s, want CREATE_TIME_FIELD naming connectivity.mode", b)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
