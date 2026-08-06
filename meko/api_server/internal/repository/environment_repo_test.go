package repository

import (
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
)

func TestEnvironmentRepo_EnrollLookup(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	// found
	mock.ExpectQuery(`SELECT id FROM meko_system\.environments WHERE enroll_token_hash = \$1 AND enroll_token_expires_at > now`).
		WithArgs("abc123").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("acme-prod"))
	id, found, err := repo.EnrollLookup("abc123")
	if err != nil || !found || id != "acme-prod" {
		t.Fatalf("EnrollLookup = (%q,%v,%v), want (acme-prod,true,nil)", id, found, err)
	}

	// not found → (\"\", false, nil)
	mock.ExpectQuery(`SELECT id FROM meko_system\.environments WHERE enroll_token_hash = \$1`).
		WithArgs("nope").
		WillReturnError(sql.ErrNoRows)
	id, found, err = repo.EnrollLookup("nope")
	if err != nil || found || id != "" {
		t.Fatalf("EnrollLookup(miss) = (%q,%v,%v), want (\"\",false,nil)", id, found, err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestEnvironmentRepo_Get_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments WHERE id = \$1`).
		WithArgs("ghost").
		WillReturnError(sql.ErrNoRows)
	e, err := repo.Get("ghost")
	if err != nil {
		t.Fatalf("Get err = %v, want nil", err)
	}
	if e != nil {
		t.Fatalf("Get = %+v, want nil (not found)", e)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ListDesired feeds two callers with the same rows: the agent, which reconciles
// them, and the console, which shows an operator what an environment was told to
// run. The spec has to come back as the JSON it went in as — a spec that decodes to
// nothing is an agent that deploys a defaulted workload instead of the declared one.
func TestEnvironmentRepo_ListDesired(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	spec := `{"image":"registry/meko-mcp-server:2.1.0","image_pull_secret":"meko-ecr"}`
	mock.ExpectQuery(`SELECT kind, name, COALESCE\(spec::text, '\{\}'\) FROM meko_system\.environment_desired_resources`).
		WithArgs("acme").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "name", "spec"}).
			AddRow("database", "meko", "{}").
			AddRow("mcp-server", "mcp", spec))

	got, err := repo.ListDesired("acme")
	if err != nil {
		t.Fatalf("ListDesired err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListDesired returned %d resources, want 2", len(got))
	}
	if got[0].Kind != "database" || got[0].Name != "meko" {
		t.Errorf("first resource = %s/%s, want database/meko", got[0].Kind, got[0].Name)
	}
	if string(got[1].Spec) != spec {
		t.Errorf("spec round-trip = %s, want %s", got[1].Spec, spec)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestEnvironmentRepo_ReplaceDesired(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM meko_system\.environment_desired_resources WHERE env_id = \$1`).
		WithArgs("acme").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs("acme", "datapack", "alpha", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = repo.ReplaceDesired("acme", []models.DesiredResource{{Kind: "datapack", Name: "alpha"}})
	if err != nil {
		t.Fatalf("ReplaceDesired err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
