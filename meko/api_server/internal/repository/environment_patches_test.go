package repository

import (
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
	if err := applyDesiredPatchesTx(tx, envUUID, []models.ResourcePatch{
		{Kind: "cluster", Name: "pool-a/main-db", Spec: []byte(`{"pg_version":17}`)},
		{Kind: "branch", Name: "pool-a/main-db/feature-x", Delete: true},
	}); err != nil {
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
	if err := applyDesiredPatchesTx(tx, envUUID, []models.ResourcePatch{
		{Kind: "storage-pool", Name: "pool-b", Spec: []byte(`{"cloud":"aws"}`)},
	}); err != nil {
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
	if err := applyDesiredPatchesTx(tx, envUUID, []models.ResourcePatch{
		{Kind: "storage-pool", Name: "pool-b", Spec: []byte(`{"cloud":"aws"}`)},
		{Kind: "cluster", Name: "pool-b/reporting", Spec: []byte(`{"pg_version":17}`)},
	}); err == nil {
		t.Fatal("applyDesiredPatchesTx err = nil, want the batch to fail")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a failed item did not roll the batch back: %v", err)
	}
}
