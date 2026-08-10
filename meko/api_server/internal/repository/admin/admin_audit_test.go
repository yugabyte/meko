package admin

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func newAuditRepo(t *testing.T) (*AdminRepo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewAdminRepo(db, nil, nil), mock, func() { _ = db.Close() }
}

func TestUpdateUserTierAudited_ChangesAndAuditsInOneTx(t *testing.T) {
	repo, mock, done := newAuditRepo(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT user_type FROM meko_system\.users`).
		WithArgs("u1").
		WillReturnRows(sqlmock.NewRows([]string{"user_type"}).AddRow("free"))
	mock.ExpectExec(`UPDATE meko_system\.users`).
		WithArgs("standard", "u1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO meko_system\.admin_audit_log`).
		WithArgs("ops@example.com", "sub-1", "admin_console", "user.tier.update", "user", "u1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	res, err := repo.UpdateUserTierAudited(context.Background(), "u1", "standard",
		AuditActor{Email: "ops@example.com", UUID: "sub-1"})
	if err != nil {
		t.Fatalf("UpdateUserTierAudited: %v", err)
	}
	if !res.Found || !res.Changed || res.OldTier != "free" {
		t.Errorf("res = %+v, want Found=true Changed=true OldTier=free", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestUpdateUserTierAudited_NoOpWhenTierUnchanged(t *testing.T) {
	repo, mock, done := newAuditRepo(t)
	defer done()

	// Same tier: no UPDATE, no audit INSERT — just commit and report no change.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT user_type FROM meko_system\.users`).
		WithArgs("u1").
		WillReturnRows(sqlmock.NewRows([]string{"user_type"}).AddRow("pro"))
	mock.ExpectCommit()

	res, err := repo.UpdateUserTierAudited(context.Background(), "u1", "pro",
		AuditActor{Email: "ops@example.com"})
	if err != nil {
		t.Fatalf("UpdateUserTierAudited: %v", err)
	}
	if !res.Found || res.Changed {
		t.Errorf("res = %+v, want Found=true Changed=false", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestUpdateUserTierAudited_NotFound(t *testing.T) {
	repo, mock, done := newAuditRepo(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT user_type FROM meko_system\.users`).
		WithArgs("ghost").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	res, err := repo.UpdateUserTierAudited(context.Background(), "ghost", "pro",
		AuditActor{Email: "ops@example.com"})
	if err != nil {
		t.Fatalf("UpdateUserTierAudited: %v", err)
	}
	if res.Found {
		t.Errorf("res.Found = true, want false for a missing user")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestUpdateUserTierAudited_RejectsMissingActor(t *testing.T) {
	repo, _, done := newAuditRepo(t)
	defer done()

	// No actor email → refuse before touching the DB (no expectations set).
	_, err := repo.UpdateUserTierAudited(context.Background(), "u1", "pro", AuditActor{})
	if !errors.Is(err, ErrNoActor) {
		t.Errorf("err = %v, want ErrNoActor", err)
	}
}
