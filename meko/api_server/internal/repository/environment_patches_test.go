package repository

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
)

// applyDesiredPatchesTx runs on an existing transaction, so these tests own the
// Begin/Commit boundary the storage-pool region PATCH would otherwise supply.

// A batch commits in ONE transaction: a shallow-merge upsert (preserving unmentioned
// top-level keys) and a cascade subtree delete, both between one Begin/Commit.
func TestApplyDesiredPatchesTx_Batch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	// upsert: read existing spec (locked), then merge the patch onto it.
	mock.ExpectQuery(`SELECT COALESCE\(spec::text, '\{\}'\) FROM meko_system\.environment_desired_resources\s+`+
		`WHERE env_uuid=\$1 AND kind=\$2 AND name=\$3 FOR UPDATE`).
		WithArgs(envUUID, "cluster", "pool-a/main-db").
		WillReturnRows(sqlmock.NewRows([]string{"spec"}).AddRow(`{"pg_version":16,"extensions":["vector"]}`))
	// existing pg_version:16 + patch pg_version:17 -> extensions preserved, version bumped.
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "cluster", "pool-a/main-db",
			[]byte(`{"extensions":["vector"],"pg_version":17}`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// delete: one subtree statement.
	mock.ExpectExec(`DELETE FROM meko_system\.environment_desired_resources\s+`+
		`WHERE env_uuid=\$1 AND \(name=\$2 OR starts_with\(name, \$2 \|\| '/'\)\)`).
		WithArgs(envUUID, "pool-a/main-db/feature-x").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := applyDesiredPatchesTx(tx, envUUID, "pool-a", []models.ResourcePatch{
		{Kind: "cluster", Name: "pool-a/main-db", Spec: []byte(`{"pg_version":17}`)},
		{Kind: "branch", Name: "pool-a/main-db/feature-x", Delete: true},
	}, nil); err != nil {
		t.Fatalf("applyDesiredPatchesTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// An absent row merges onto {}, so a first patch becomes the whole spec.
func TestApplyDesiredPatchesTx_MissingRowBecomesTheSpec(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COALESCE\(spec::text, '\{\}'\)`).
		WithArgs(envUUID, "storage-pool", "pool-b").
		WillReturnRows(sqlmock.NewRows([]string{"spec"})) // no row
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "storage-pool", "pool-b", []byte(`{"cloud":"aws"}`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := applyDesiredPatchesTx(tx, envUUID, "pool-b", []models.ResourcePatch{
		{Kind: "storage-pool", Name: "pool-b", Spec: []byte(`{"cloud":"aws"}`)},
	}, nil); err != nil {
		t.Fatalf("applyDesiredPatchesTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A failing item returns an error mid-batch, and the caller rolls back: the first
// item's write does not survive.
func TestApplyDesiredPatchesTx_RollsBackOnError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COALESCE\(spec::text, '\{\}'\)`).
		WithArgs(envUUID, "storage-pool", "pool-b").
		WillReturnRows(sqlmock.NewRows([]string{"spec"}))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "storage-pool", "pool-b", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// second item's read fails -> the caller must roll back.
	mock.ExpectQuery(`SELECT COALESCE\(spec::text, '\{\}'\)`).
		WithArgs(envUUID, "cluster", "pool-b/reporting").
		WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := applyDesiredPatchesTx(tx, envUUID, "pool-b", []models.ResourcePatch{
		{Kind: "storage-pool", Name: "pool-b", Spec: []byte(`{"cloud":"aws"}`)},
		{Kind: "cluster", Name: "pool-b/reporting", Spec: []byte(`{"pg_version":17}`)},
	}, nil); err == nil {
		t.Fatal("applyDesiredPatchesTx err = nil, want the batch to fail")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a failed item did not roll the batch back: %v", err)
	}
}

// A non-cluster Create on an existing row is rejected with ErrResourceExists.
func TestApplyDesiredPatchesTx_CreateOnExisting(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COALESCE\(spec::text, '\{\}'\)`).
		WithArgs(envUUID, "storage-pool", "pool-b").
		WillReturnRows(sqlmock.NewRows([]string{"spec"}).AddRow(`{"cloud":"aws"}`))
	// No INSERT: the guard fails before the upsert.
	mock.ExpectRollback()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	err = applyDesiredPatchesTx(tx, envUUID, "pool-b", []models.ResourcePatch{
		{Kind: "storage-pool", Name: "pool-b", Spec: []byte(`{"cloud":"gcp"}`), Create: true},
	}, nil)
	if !errors.Is(err, ErrResourceExists) {
		t.Fatalf("err = %v, want ErrResourceExists", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// An Update on a missing row is rejected with ErrResourceNotFound.
func TestApplyDesiredPatchesTx_UpdateOnMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COALESCE\(spec::text, '\{\}'\)`).
		WithArgs(envUUID, "storage-pool", "pool-b").
		WillReturnRows(sqlmock.NewRows([]string{"spec"})) // no row
	mock.ExpectRollback()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	err = applyDesiredPatchesTx(tx, envUUID, "pool-b", []models.ResourcePatch{
		{Kind: "storage-pool", Name: "pool-b", Spec: []byte(`{"cloud":"aws"}`), Update: true},
	}, nil)
	if !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("err = %v, want ErrResourceNotFound", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A cluster Create whose name is already used elsewhere in the pool (another region /
// environment) is rejected with ErrClusterNameInUse. The check spans the pool via the
// mapping, not just this environment's rows.
func TestApplyDesiredPatchesTx_ClusterNameInPool(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	// The per-item locked read still runs (returns no row in this env)...
	mock.ExpectQuery(`SELECT COALESCE\(spec::text, '\{\}'\)`).
		WithArgs(envUUID, "cluster", "pool-a/main-db").
		WillReturnRows(sqlmock.NewRows([]string{"spec"}))
	// ...then the pool-wide cluster-name check finds it in a sibling environment.
	mock.ExpectQuery(`SELECT EXISTS\(\s*SELECT 1\s+FROM meko_system\.environment_aeon_account_mapping m`).
		WithArgs("pool-a", "pool-a/main-db").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectRollback()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	err = applyDesiredPatchesTx(tx, envUUID, "pool-a", []models.ResourcePatch{
		{Kind: "cluster", Name: "pool-a/main-db", Spec: []byte(`{"pg_version":17}`), Create: true},
	}, nil)
	if !errors.Is(err, ErrClusterNameInUse) {
		t.Fatalf("err = %v, want ErrClusterNameInUse", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A cluster Create whose name is free anywhere in the pool proceeds to the upsert.
func TestApplyDesiredPatchesTx_ClusterCreateSucceeds(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COALESCE\(spec::text, '\{\}'\)`).
		WithArgs(envUUID, "cluster", "pool-a/main-db").
		WillReturnRows(sqlmock.NewRows([]string{"spec"}))
	mock.ExpectQuery(`SELECT EXISTS\(\s*SELECT 1\s+FROM meko_system\.environment_aeon_account_mapping m`).
		WithArgs("pool-a", "pool-a/main-db").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "cluster", "pool-a/main-db", []byte(`{"pg_version":17}`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := applyDesiredPatchesTx(tx, envUUID, "pool-a", []models.ResourcePatch{
		{Kind: "cluster", Name: "pool-a/main-db", Spec: []byte(`{"pg_version":17}`), Create: true},
	}, nil); err != nil {
		t.Fatalf("applyDesiredPatchesTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// expectEndpointRead scripts the locked read of an endpoint's stored spec.
func expectEndpointRead(mock sqlmock.Sqlmock, name, spec string) {
	mock.ExpectQuery(`SELECT COALESCE\(spec::text, '\{\}'\)`).
		WithArgs(envUUID, "endpoint", name).
		WillReturnRows(sqlmock.NewRows([]string{"spec"}).AddRow(spec))
}

const guardEndpoint = "pool-a/main-db/main/rw"

// The guard sees an update to an existing resource with the stored and merged
// specs, and the agent's report of it when it asks; its refusal fails the batch
// before anything is written.
func TestApplyDesiredPatchesTx_GuardVetsAnUpdate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	expectEndpointRead(mock, guardEndpoint, `{"role":"read_write"}`)
	mock.ExpectQuery(`SELECT COALESCE\(resources::text, '\[\]'\) FROM meko_system\.environments WHERE uuid=\$1`).
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"resources"}).AddRow(
			`[{"kind":"endpoint","id":"endpoint/` + guardEndpoint + `","state":"rejected","attrs":{"id":"e1"}}]`))
	mock.ExpectRollback() // no INSERT

	refused := errors.New("refused")
	var gotBefore, gotAfter string
	var gotObs *ObservedResource
	guard := func(kind, name string, before, after json.RawMessage, observed func() (*ObservedResource, error)) error {
		gotBefore, gotAfter = string(before), string(after)
		gotObs, _ = observed()
		return refused
	}

	tx, _ := db.Begin()
	err = applyDesiredPatchesTx(tx, envUUID, "pool-a", []models.ResourcePatch{
		{Kind: "endpoint", Name: guardEndpoint, Spec: []byte(`{"role":"read_only"}`), Update: true},
	}, guard)
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the guard's refusal", err)
	}
	if gotBefore != `{"role":"read_write"}` || gotAfter != `{"role":"read_only"}` {
		t.Errorf("guard saw %s -> %s", gotBefore, gotAfter)
	}
	if gotObs == nil || gotObs.State != "rejected" || gotObs.Attrs["id"] != "e1" {
		t.Errorf("observed = %+v, want the reported row", gotObs)
	}
	_ = tx.Rollback()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A create landing on an existing resource is still the 409 it always was: the
// guard is for updates, and must not turn a collision into a complaint about the
// colliding spec's fields.
func TestApplyDesiredPatchesTx_GuardDoesNotRunOnACreate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	expectEndpointRead(mock, guardEndpoint, `{"role":"read_write"}`)
	mock.ExpectRollback()

	guard := func(string, string, json.RawMessage, json.RawMessage, func() (*ObservedResource, error)) error {
		t.Error("guard ran on a create")
		return nil
	}
	tx, _ := db.Begin()
	err = applyDesiredPatchesTx(tx, envUUID, "pool-a", []models.ResourcePatch{
		{Kind: "endpoint", Name: guardEndpoint, Spec: []byte(`{"role":"read_only"}`), Create: true},
	}, guard)
	if !errors.Is(err, ErrResourceExists) {
		t.Fatalf("err = %v, want ErrResourceExists", err)
	}
	_ = tx.Rollback()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A malformed inventory is an error the guard sees, not an empty one it would read
// as "nothing reported" and let every write through on.
func TestObservedResource_MalformedInventoryIsAnError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COALESCE\(resources::text`).WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"resources"}).AddRow(`{"not":"an array"}`))
	tx, _ := db.Begin()
	if _, err := observedResource(tx, envUUID, "endpoint", guardEndpoint); err == nil {
		t.Fatal("observedResource returned no error for a malformed inventory")
	}
}
