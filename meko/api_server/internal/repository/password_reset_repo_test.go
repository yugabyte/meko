package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newPasswordResetMock(t *testing.T) (*PasswordResetRepo, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewPasswordResetRepo(db), mock
}

func TestPasswordResetRepo_RecordResetRequest_PrunesThenInserts(t *testing.T) {
	repo, mock := newPasswordResetMock(t)
	insert := `INSERT INTO meko_system.password_reset_requests \(email\) VALUES \(\$1\) RETURNING id`

	// First call: the email's old rows, then every email's (the global prune
	// is due), then the insert.
	mock.ExpectExec(`DELETE FROM meko_system.password_reset_requests\s+WHERE email = \$1 AND requested_at < \$2`).
		WithArgs("user@example.com", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`DELETE FROM meko_system.password_reset_requests WHERE requested_at < \$1`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 7))
	mock.ExpectQuery(insert).WithArgs("user@example.com").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("req-1"))
	// Second call within the prune interval: no global prune.
	mock.ExpectExec(`DELETE FROM meko_system.password_reset_requests\s+WHERE email = \$1`).
		WithArgs("other@example.com", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(insert).WithArgs("other@example.com").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("req-2"))

	if id, err := repo.RecordResetRequest(context.Background(), "user@example.com"); err != nil || id != "req-1" {
		t.Fatalf("RecordResetRequest = %q, %v", id, err)
	}
	if id, err := repo.RecordResetRequest(context.Background(), "other@example.com"); err != nil || id != "req-2" {
		t.Fatalf("RecordResetRequest = %q, %v", id, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestPasswordResetRepo_LatestResetRequest_NoRows(t *testing.T) {
	repo, mock := newPasswordResetMock(t)
	mock.ExpectQuery(`SELECT id, email, requested_at, code_hash`).
		WithArgs("user@example.com").
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "email", "requested_at", "code_hash", "expires_at", "used_at", "attempts"}))

	got, err := repo.LatestResetRequest(context.Background(), "user@example.com")
	if err != nil || got != nil {
		t.Fatalf("expected nil, nil; got %+v, %v", got, err)
	}
}

func TestPasswordResetRepo_LatestResetRequest_CognitoRequestHasNoCode(t *testing.T) {
	repo, mock := newPasswordResetMock(t)
	now := time.Now()
	mock.ExpectQuery(`SELECT id, email, requested_at, code_hash`).
		WithArgs("user@example.com").
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "email", "requested_at", "code_hash", "expires_at", "used_at", "attempts"}).
			AddRow("req-2", "user@example.com", now, nil, nil, nil, 0))

	got, err := repo.LatestResetRequest(context.Background(), "user@example.com")
	if err != nil || got == nil || got.CodeHash != "" || got.ExpiresAt != nil {
		t.Fatalf("expected a code-less request, got %+v, %v", got, err)
	}
}

func TestPasswordResetRepo_ClaimResetCode_OnlyOnce(t *testing.T) {
	repo, mock := newPasswordResetMock(t)
	mock.ExpectExec(`SET used_at = NOW\(\)\s+WHERE email = \$1 AND id = \$2 AND used_at IS NULL`).
		WithArgs("user@example.com", "req-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE meko_system.password_reset_requests`).
		WithArgs("user@example.com", "req-1").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if ok, err := repo.ClaimResetCode(context.Background(), "user@example.com", "req-1"); err != nil || !ok {
		t.Fatalf("first claim = %v, %v; want true", ok, err)
	}
	if ok, err := repo.ClaimResetCode(context.Background(), "user@example.com", "req-1"); err != nil || ok {
		t.Fatalf("second claim = %v, %v; want false", ok, err)
	}
}

func TestPasswordResetRepo_ReserveResetAttempt_ConditionalInOneStatement(t *testing.T) {
	repo, mock := newPasswordResetMock(t)
	reserve := `SET attempts = attempts \+ 1\s+WHERE email = \$1 AND id = \$2 AND code_hash IS NOT NULL\s+` +
		`AND used_at IS NULL\s+AND expires_at > NOW\(\) AND attempts < \$3`
	mock.ExpectExec(reserve).WithArgs("user@example.com", "req-1", 5).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(reserve).WithArgs("user@example.com", "req-1", 5).WillReturnResult(sqlmock.NewResult(0, 0))

	if ok, err := repo.ReserveResetAttempt(context.Background(), "user@example.com", "req-1", 5); err != nil || !ok {
		t.Fatalf("first reserve = %v, %v; want true", ok, err)
	}
	if ok, err := repo.ReserveResetAttempt(context.Background(), "user@example.com", "req-1", 5); err != nil || ok {
		t.Fatalf("reserve with no row matched = %v, %v; want false", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestPasswordResetRepo_DeleteResetRequest(t *testing.T) {
	repo, mock := newPasswordResetMock(t)
	mock.ExpectExec(`DELETE FROM meko_system.password_reset_requests WHERE email = \$1 AND id = \$2`).
		WithArgs("user@example.com", "req-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.DeleteResetRequest(context.Background(), "user@example.com", "req-1"); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestPasswordResetRepo_UnclaimResetCode(t *testing.T) {
	repo, mock := newPasswordResetMock(t)
	mock.ExpectExec(`SET used_at = NULL\s+WHERE email = \$1 AND id = \$2`).
		WithArgs("user@example.com", "req-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.UnclaimResetCode(context.Background(), "user@example.com", "req-1"); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
