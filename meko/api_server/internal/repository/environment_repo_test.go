package repository

import (
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
)

func TestEnvironmentRepo_GetByAPIKeyID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	// found: an exact hit on the unique api_key_id index.
	mock.ExpectQuery(`FROM meko_system\.environment_api_keys WHERE api_key_id = \$1`).
		WithArgs(keyA).
		WillReturnRows(listRow("acme-prod"))
	e, err := repo.GetByAPIKeyID(keyA)
	if err != nil || e == nil {
		t.Fatalf("GetByAPIKeyID = (%v, %v), want the bound environment", e, err)
	}
	if e.APIKeyID != keyA {
		t.Errorf("api key id = %q, want %q", e.APIKeyID, keyA)
	}

	// unbound key → (nil, nil), which is also how a revoked key surfaces.
	mock.ExpectQuery(`FROM meko_system\.environment_api_keys WHERE api_key_id = \$1`).
		WithArgs(keyB).
		WillReturnError(sql.ErrNoRows)
	e, err = repo.GetByAPIKeyID(keyB)
	if err != nil || e != nil {
		t.Fatalf("GetByAPIKeyID(miss) = (%v, %v), want (nil, nil)", e, err)
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

	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments e WHERE e\.uuid = \$1`).
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
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "name", "spec", "generation"}).
			AddRow("database", "meko", "{}", int64(0)).
			AddRow("mcp-server", "mcp", spec, int64(3)))

	got, err := repo.ListDesired(envUUID)
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
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "name"}).
			AddRow("datapack", "alpha").
			AddRow("datapack", "stale"))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "datapack", "alpha", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM meko_system\.environment_desired_resources\s+`+
		`WHERE env_uuid=\$1 AND kind=\$2 AND name=\$3`).
		WithArgs(envUUID, "datapack", "stale").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = repo.ReplaceDesired(envUUID,
		[]models.DesiredResource{{Kind: "datapack", Name: "alpha"}})
	if err != nil {
		t.Fatalf("ReplaceDesired err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
