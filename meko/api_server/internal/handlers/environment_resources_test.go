package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

// poolUUID is a valid storage-pool name (an aeon account id): PATCH requires the pool to
// be a UUID, so the write-path tests address the pool by one. LIST stays lenient, so the
// GET tests keep using a plain slug.
const poolUUID = "aaaaaaaa-1111-2222-3333-444444444444"

// ampResourceRow is a stored MANAGED amp environment carrying the given observed
// inventory JSON. Kind "amp" so cluster/branch/endpoint are valid; type MANAGED so it
// is eligible for the region-addressed routes.
func ampResourceRow(resources string) *sqlmock.Rows {
	return sqlmock.NewRows(scopeRowCols).AddRow(
		envUUID, "acme-prod", acctA, "cloud", "us-east-1", "READY", "", 0, "",
		0, 400, "", nil, "", "", "[]", true, keyA, time.Now(), "amp", resources, nil, "MANAGED",
	)
}

func desiredRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"kind", "name", "spec", "generation"})
}

// A first-touch region PATCH: no mapping exists, so the call assigns a READY MANAGED
// environment, auto-creates the storage-pool row, applies the cluster, and reads the
// pool subtree back so the response mirrors a LIST.
func TestSetResourcesByRegion_FirstTouch(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(poolUUID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs(poolUUID, "us-east-1").
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}))
	mock.ExpectQuery(`SELECT uuid FROM meko_system\.environments\s+WHERE type = 'MANAGED' AND COALESCE\(kind, 'meko'\) = 'amp'`).
		WithArgs("us-east-1").
		WillReturnRows(sqlmock.NewRows([]string{"uuid"}).AddRow(envUUID))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_aeon_account_mapping`).
		WithArgs(envUUID, "us-east-1", poolUUID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT environment_id FROM meko_system\.environment_aeon_account_mapping`).
		WithArgs(poolUUID, "us-east-1").
		WillReturnRows(sqlmock.NewRows([]string{"environment_id"}).AddRow(envUUID))
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs(envUUID, poolUUID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	// synthesized storage-pool (flagless upsert), then the cluster (a flagless write
	// is normalized to create, so it also runs the pool-wide cluster-name check).
	mock.ExpectQuery(`SELECT COALESCE\(spec::text`).
		WithArgs(envUUID, "storage-pool", poolUUID).
		WillReturnRows(sqlmock.NewRows([]string{"spec"}))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "storage-pool", poolUUID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT COALESCE\(spec::text`).
		WithArgs(envUUID, "cluster", poolUUID+"/main-db").
		WillReturnRows(sqlmock.NewRows([]string{"spec"}))
	mock.ExpectQuery(`SELECT EXISTS\(\s*SELECT 1\s+FROM meko_system\.environment_aeon_account_mapping m`).
		WithArgs(poolUUID, poolUUID+"/main-db").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "cluster", poolUUID+"/main-db", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	// Read-back for the response: env row + its desired set.
	mock.ExpectQuery(`WHERE e\.uuid = \$1`).WithArgs(envUUID).
		WillReturnRows(ampResourceRow("[]"))
	mock.ExpectQuery(`FROM meko_system\.environment_desired_resources`).
		WithArgs(envUUID).
		WillReturnRows(desiredRows().
			AddRow("storage-pool", poolUUID, "{}", int64(0)).
			AddRow("cluster", poolUUID+"/main-db", "{}", int64(0)))

	body := `{"storage_pool":"` + poolUUID + `","regions":[` +
		`{"region":"us-east-1","resources":[{"kind":"cluster","name":"` + poolUUID + `/main-db","spec":{"pg_version":17}}]}]}`
	rec := fleetRequest(t, h.SetResourcesByRegion, http.MethodPatch,
		"/api/v1/environments/resources", "", body, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Environments []struct {
				EnvironmentID string           `json:"environment_id"`
				Region        string           `json:"region"`
				Desired       []map[string]any `json:"desired"`
			} `json:"environments"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data.Environments) != 1 || resp.Data.Environments[0].Region != "us-east-1" {
		t.Fatalf("environments = %+v, want one for us-east-1", resp.Data.Environments)
	}
	if len(resp.Data.Environments[0].Desired) != 2 {
		t.Errorf("desired = %+v, want the pool and its cluster", resp.Data.Environments[0].Desired)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A region with no READY MANAGED environment is a 404, and the batch writes nothing.
func TestSetResourcesByRegion_NoManagedEnvIs404(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(poolUUID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs(poolUUID, "us-east-1").
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}))
	mock.ExpectQuery(`SELECT uuid FROM meko_system\.environments\s+WHERE type = 'MANAGED' AND COALESCE\(kind, 'meko'\) = 'amp'`).
		WithArgs("us-east-1").
		WillReturnRows(sqlmock.NewRows([]string{"uuid"}))
	mock.ExpectRollback()

	body := `{"storage_pool":"` + poolUUID + `","regions":[` +
		`{"region":"us-east-1","resources":[{"kind":"cluster","name":"` + poolUUID + `/main-db","spec":{}}]}]}`
	rec := fleetRequest(t, h.SetResourcesByRegion, http.MethodPatch,
		"/api/v1/environments/resources", "", body, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A Postgres deadlock lost while applying the batch surfaces as a retryable 409
// CONCURRENT_UPDATE, not a 500.
func TestSetResourcesByRegion_DeadlockIs409(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(poolUUID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs(poolUUID, "us-east-1").
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}).AddRow(envUUID, "READY"))
	// Pool exists -> no synthesis; the per-item row read then loses a deadlock.
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs(envUUID, poolUUID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`SELECT COALESCE\(spec::text`).
		WithArgs(envUUID, "cluster", poolUUID+"/main-db").
		WillReturnError(&pq.Error{Code: "40P01", Message: "deadlock detected"})
	mock.ExpectRollback()

	body := `{"storage_pool":"` + poolUUID + `","regions":[` +
		`{"region":"us-east-1","resources":[{"kind":"cluster","name":"` + poolUUID + `/main-db","spec":{}}]}]}`
	rec := fleetRequest(t, h.SetResourcesByRegion, http.MethodPatch,
		"/api/v1/environments/resources", "", body, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	var resp APIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != "CONCURRENT_UPDATE" {
		t.Errorf("error = %+v, want code CONCURRENT_UPDATE", resp.Error)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Body-level refusals caught before the service is called: no DB expectations.
func TestSetResourcesByRegion_BodyRefusals(t *testing.T) {
	cases := []struct {
		name string
		body string
		code string
	}{
		{"no body at all", "", "EMPTY_BODY"},
		{"missing storage_pool", `{"regions":[{"region":"us-east-1","resources":[{"kind":"cluster","name":"acct-1/c","spec":{}}]}]}`, "VALIDATION_ERROR"},
		{"empty regions", `{"storage_pool":"acct-1","regions":[]}`, "VALIDATION_ERROR"},
		{"region missing", `{"storage_pool":"acct-1","regions":[{"resources":[{"kind":"cluster","name":"acct-1/c","spec":{}}]}]}`, "VALIDATION_ERROR"},
		{"region has no resources", `{"storage_pool":"acct-1","regions":[{"region":"us-east-1","resources":[]}]}`, "VALIDATION_ERROR"},
		{"unknown top-level field", `{"storage_pool":"acct-1","regions":[],"zzz":1}`, "INVALID_JSON"},
		{"missing kind", `{"storage_pool":"acct-1","regions":[{"region":"us-east-1","resources":[{"name":"acct-1/c"}]}]}`, "VALIDATION_ERROR"},
		{"missing name", `{"storage_pool":"acct-1","regions":[{"region":"us-east-1","resources":[{"kind":"cluster"}]}]}`, "VALIDATION_ERROR"},
		{"delete with a spec", `{"storage_pool":"acct-1","regions":[{"region":"us-east-1","resources":[{"kind":"cluster","name":"acct-1/c","spec":{"x":1},"delete":true}]}]}`, "VALIDATION_ERROR"},
		{"create and update together", `{"storage_pool":"acct-1","regions":[{"region":"us-east-1","resources":[{"kind":"cluster","name":"acct-1/c","spec":{},"create":true,"update":true}]}]}`, "VALIDATION_ERROR"},
		{"create and delete together", `{"storage_pool":"acct-1","regions":[{"region":"us-east-1","resources":[{"kind":"cluster","name":"acct-1/c","create":true,"delete":true}]}]}`, "VALIDATION_ERROR"},
		// A structurally valid body whose only fault is a non-UUID pool: the service
		// rejects it before any query, so it belongs here with the other DB-free refusals.
		{"non-uuid storage_pool", `{"storage_pool":"acct-1","regions":[{"region":"us-east-1","resources":[{"kind":"cluster","name":"acct-1/c","spec":{}}]}]}`, "VALIDATION_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, mock, done := newScopeHandler(t)
			defer done()

			rec := fleetRequest(t, h.SetResourcesByRegion, http.MethodPatch,
				"/api/v1/environments/resources", "", tc.body, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			var resp APIResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Error == nil || resp.Error.Code != tc.code {
				t.Errorf("error = %+v, want code %q", resp.Error, tc.code)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("a refused body still reached the database: %v", err)
			}
		})
	}
}

// The batch cap counts patches across all regions and is checked before the database.
func TestSetResourcesByRegion_CapExceeded(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	var b strings.Builder
	b.WriteString(`{"storage_pool":"acct-1","regions":[{"region":"us-east-1","resources":[`)
	for i := 0; i <= maxPatchesPerCall; i++ { // one over the cap
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"kind":"cluster","name":"acct-1/c","spec":{}}`)
	}
	b.WriteString(`]}]}`)

	rec := fleetRequest(t, h.SetResourcesByRegion, http.MethodPatch,
		"/api/v1/environments/resources", "", b.String(), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("an oversized batch still reached the database: %v", err)
	}
}

// A name outside the storage-pool subtree fails with 400 before any write.
func TestSetResourcesByRegion_NameOutsidePoolIs400(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	body := `{"storage_pool":"` + poolUUID + `","regions":[` +
		`{"region":"us-east-1","resources":[{"kind":"cluster","name":"other-pool/c","spec":{}}]}]}`
	rec := fleetRequest(t, h.SetResourcesByRegion, http.MethodPatch,
		"/api/v1/environments/resources", "", body, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a rejected name still reached the database: %v", err)
	}
}

// GET without a name is a 400 before any query.
func TestListResourcesByName_NameRequired(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	rec := fleetRequest(t, h.ListResourcesByName, http.MethodGet,
		"/api/v1/environments/resources", "", "", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a nameless list still reached the database: %v", err)
	}
}

// GET lists a pool's resources across the environments it is mapped to, filtered by
// the name path. The observed side proves the match runs on the full path derived from
// ID, not the leaf name.
func TestListResourcesByName_AcrossMappedEnvs(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	observed := `[` +
		`{"kind":"cluster","id":"cluster/acct-1/main-db","name":"main-db"},` +
		`{"kind":"branch","id":"branch/acct-1/main-db/feat","name":"feat"}]`

	mock.ExpectQuery(`FROM meko_system\.environment_aeon_account_mapping`).
		WithArgs("acct-1").
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "region"}).AddRow(envUUID, "us-east-1"))
	mock.ExpectQuery(`WHERE e\.uuid = \$1`).WithArgs(envUUID).
		WillReturnRows(ampResourceRow(observed))
	mock.ExpectQuery(`FROM meko_system\.environment_desired_resources`).
		WithArgs(envUUID).
		WillReturnRows(desiredRows().
			AddRow("cluster", "acct-1/main-db", "{}", int64(0)).
			AddRow("branch", "acct-1/main-db/feat", "{}", int64(0)))

	rec := fleetRequest(t, h.ListResourcesByName, http.MethodGet,
		"/api/v1/environments/resources?name=acct-1/main-db", "", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Environments []struct {
				Desired  []map[string]any `json:"desired"`
				Observed []map[string]any `json:"observed"`
			} `json:"environments"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data.Environments) != 1 {
		t.Fatalf("environments = %+v, want one", resp.Data.Environments)
	}
	// Exact name match (no subtree): just the cluster on each side.
	if len(resp.Data.Environments[0].Desired) != 1 || len(resp.Data.Environments[0].Observed) != 1 {
		t.Errorf("exact name should return one each, got desired=%v observed=%v",
			resp.Data.Environments[0].Desired, resp.Data.Environments[0].Observed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The kind and subtree filters narrow the LIST the same way the by-id list used to:
// kind keeps one kind; subtree returns the named resource and its descendants.
func TestListResourcesByName_KindAndSubtreeFilters(t *testing.T) {
	observed := `[` +
		`{"kind":"storage-pool","id":"storage-pool/acct-1","name":"acct-1"},` +
		`{"kind":"cluster","id":"cluster/acct-1/main-db","name":"main-db"},` +
		`{"kind":"branch","id":"branch/acct-1/main-db/feat","name":"feat"}]`

	// desiredAndObserved re-registers the mapping + env reads each sub-case needs.
	expect := func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(`FROM meko_system\.environment_aeon_account_mapping`).
			WithArgs("acct-1").
			WillReturnRows(sqlmock.NewRows([]string{"environment_id", "region"}).AddRow(envUUID, "us-east-1"))
		mock.ExpectQuery(`WHERE e\.uuid = \$1`).WithArgs(envUUID).
			WillReturnRows(ampResourceRow(observed))
		mock.ExpectQuery(`FROM meko_system\.environment_desired_resources`).
			WithArgs(envUUID).
			WillReturnRows(desiredRows().
				AddRow("storage-pool", "acct-1", "{}", int64(0)).
				AddRow("cluster", "acct-1/main-db", "{}", int64(0)).
				AddRow("branch", "acct-1/main-db/feat", "{}", int64(0)))
	}

	t.Run("kind filter keeps one kind", func(t *testing.T) {
		h, mock, done := newScopeHandler(t)
		defer done()
		expect(mock)

		rec := fleetRequest(t, h.ListResourcesByName, http.MethodGet,
			"/api/v1/environments/resources?name=acct-1&kind=cluster&subtree=true", "", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		env := decodeEnvResources(t, rec)[0]
		if len(env.Desired) != 1 || env.Desired[0]["kind"] != "cluster" {
			t.Errorf("desired = %v, want just the cluster", env.Desired)
		}
		if len(env.Observed) != 1 || env.Observed[0]["kind"] != "cluster" {
			t.Errorf("observed = %v, want just the cluster", env.Observed)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
	})

	t.Run("subtree returns the named resource and its descendants", func(t *testing.T) {
		h, mock, done := newScopeHandler(t)
		defer done()
		expect(mock)

		rec := fleetRequest(t, h.ListResourcesByName, http.MethodGet,
			"/api/v1/environments/resources?name=acct-1/main-db&subtree=true", "", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		env := decodeEnvResources(t, rec)[0]
		// main-db + its branch, on both sides.
		if len(env.Desired) != 2 || len(env.Observed) != 2 {
			t.Errorf("subtree should return the cluster and its branch, got desired=%v observed=%v",
				env.Desired, env.Observed)
		}
	})
}

// A region PATCH carrying a delete issues exactly the subtree DELETE inside the tx,
// then reads the pool back for the mirrored response.
func TestSetResourcesByRegion_DeleteCascades(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(poolUUID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs(poolUUID, "us-east-1").
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}).AddRow(envUUID, "READY"))
	// delete-only batch: no synthesis probe, just the subtree DELETE.
	mock.ExpectExec(`DELETE FROM meko_system\.environment_desired_resources\s+`+
		`WHERE env_uuid=\$1 AND \(name=\$2 OR starts_with\(name, \$2 \|\| '/'\)\)`).
		WithArgs(envUUID, poolUUID+"/main-db/feature-x").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	// Read-back for the mirrored response.
	mock.ExpectQuery(`WHERE e\.uuid = \$1`).WithArgs(envUUID).
		WillReturnRows(ampResourceRow("[]"))
	mock.ExpectQuery(`FROM meko_system\.environment_desired_resources`).
		WithArgs(envUUID).
		WillReturnRows(desiredRows().AddRow("storage-pool", poolUUID, "{}", int64(0)))

	body := `{"storage_pool":"` + poolUUID + `","regions":[` +
		`{"region":"us-east-1","resources":[{"kind":"branch","name":"` + poolUUID + `/main-db/feature-x","delete":true}]}]}`
	rec := fleetRequest(t, h.SetResourcesByRegion, http.MethodPatch,
		"/api/v1/environments/resources", "", body, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("delete did not issue exactly the subtree DELETE: %v", err)
	}
}

// A flagless cluster write is create-only by default; when its name is already used in
// the pool (here reported by the pool-wide check), the call is a 409 CLUSTER_NAME_IN_USE
// and writes nothing.
func TestSetResourcesByRegion_DuplicateClusterIs409(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(poolUUID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs(poolUUID, "us-east-1").
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}).AddRow(envUUID, "READY"))
	// Pool exists -> no synthesis; the cluster's pool-wide name check finds a collision.
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs(envUUID, poolUUID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`SELECT COALESCE\(spec::text`).
		WithArgs(envUUID, "cluster", poolUUID+"/main-db").
		WillReturnRows(sqlmock.NewRows([]string{"spec"}))
	mock.ExpectQuery(`SELECT EXISTS\(\s*SELECT 1\s+FROM meko_system\.environment_aeon_account_mapping m`).
		WithArgs(poolUUID, poolUUID+"/main-db").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectRollback()

	body := `{"storage_pool":"` + poolUUID + `","regions":[` +
		`{"region":"us-east-1","resources":[{"kind":"cluster","name":"` + poolUUID + `/main-db","spec":{}}]}]}`
	rec := fleetRequest(t, h.SetResourcesByRegion, http.MethodPatch,
		"/api/v1/environments/resources", "", body, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	var resp APIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != "CLUSTER_NAME_IN_USE" {
		t.Errorf("error = %+v, want code CLUSTER_NAME_IN_USE", resp.Error)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// update:true on a resource that does not exist is a 404 RESOURCE_NOT_FOUND.
func TestSetResourcesByRegion_UpdateMissingIs404(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(poolUUID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs(poolUUID, "us-east-1").
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}).AddRow(envUUID, "READY"))
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs(envUUID, poolUUID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// The locked read finds no row, so the update assertion fails.
	mock.ExpectQuery(`SELECT COALESCE\(spec::text`).
		WithArgs(envUUID, "cluster", poolUUID+"/main-db").
		WillReturnRows(sqlmock.NewRows([]string{"spec"}))
	mock.ExpectRollback()

	body := `{"storage_pool":"` + poolUUID + `","regions":[` +
		`{"region":"us-east-1","resources":[{"kind":"cluster","name":"` + poolUUID + `/main-db","spec":{"pg_version":17},"update":true}]}]}`
	rec := fleetRequest(t, h.SetResourcesByRegion, http.MethodPatch,
		"/api/v1/environments/resources", "", body, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	var resp APIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != "RESOURCE_NOT_FOUND" {
		t.Errorf("error = %+v, want code RESOURCE_NOT_FOUND", resp.Error)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// decodeEnvResources unwraps the {data:{environments:[...]}} envelope for the LIST/PATCH
// response shape.
func decodeEnvResources(t *testing.T, rec *httptest.ResponseRecorder) []struct {
	Desired  []map[string]any `json:"desired"`
	Observed []map[string]any `json:"observed"`
} {
	t.Helper()
	var resp struct {
		Data struct {
			Environments []struct {
				Desired  []map[string]any `json:"desired"`
				Observed []map[string]any `json:"observed"`
			} `json:"environments"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Data.Environments
}
