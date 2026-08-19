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
	mock.ExpectQuery(`SELECT uuid FROM meko_system\.environments\s+WHERE enroll_token_hash = \$1`).
		WithArgs("abc123").
		WillReturnRows(sqlmock.NewRows([]string{"uuid"}).AddRow("11111111-1111-1111-1111-111111111111"))
	envUUID, found, err := repo.EnrollLookup("abc123")
	if err != nil || !found || envUUID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("EnrollLookup = (%q,%v,%v), want (11111111-1111-1111-1111-111111111111,true,nil)", envUUID, found, err)
	}

	// not found → (\"\", false, nil)
	mock.ExpectQuery(`SELECT uuid FROM meko_system\.environments WHERE enroll_token_hash = \$1`).
		WithArgs("nope").
		WillReturnError(sql.ErrNoRows)
	envUUID, found, err = repo.EnrollLookup("nope")
	if err != nil || found || envUUID != "" {
		t.Fatalf("EnrollLookup(miss) = (%q,%v,%v), want (\"\",false,nil)", envUUID, found, err)
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

	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments WHERE uuid = \$1`).
		WithArgs("99999999-9999-9999-9999-999999999999").
		WillReturnError(sql.ErrNoRows)
	e, err := repo.GetByUUID("99999999-9999-9999-9999-999999999999")
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
	mock.ExpectQuery(`SELECT kind, name, COALESCE\(spec::text, '\{\}'\), generation FROM meko_system\.environment_desired_resources`).
		WithArgs("11111111-1111-1111-1111-111111111111").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "name", "spec", "generation"}).
			AddRow("database", "meko", "{}", int64(0)).
			AddRow("mcp-server", "mcp", spec, int64(3)))

	got, err := repo.ListDesired("11111111-1111-1111-1111-111111111111")
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
	// Generation rides along so the agent can diff a mutated spec from an
	// unchanged one.
	if got[1].Generation != 3 {
		t.Errorf("generation round-trip = %d, want 3", got[1].Generation)
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

	// Upsert-and-prune: read the existing keys, upsert each desired resource
	// (generation bumped in SQL only when the spec changed), then delete the ones
	// no longer desired. Here "stale" is withdrawn and "alpha" is kept.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT kind, name FROM meko_system\.environment_desired_resources WHERE env_uuid = \$1`).
		WithArgs("11111111-1111-1111-1111-111111111111").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "name"}).
			AddRow("datapack", "alpha").
			AddRow("datapack", "stale"))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs("11111111-1111-1111-1111-111111111111", "datapack", "alpha", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM meko_system\.environment_desired_resources\s+`+
		`WHERE env_uuid=\$1 AND kind=\$2 AND name=\$3`).
		WithArgs("11111111-1111-1111-1111-111111111111", "datapack", "stale").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = repo.ReplaceDesired("11111111-1111-1111-1111-111111111111",
		[]models.DesiredResource{{Kind: "datapack", Name: "alpha"}})
	if err != nil {
		t.Fatalf("ReplaceDesired err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
