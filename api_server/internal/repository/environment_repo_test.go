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
