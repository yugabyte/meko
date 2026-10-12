package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// userColumns enumerates the columns returned by the meko_system.users
// SELECT/INSERT...RETURNING used in AccountRepo.
var userColumns = []string{
	"user_id", "user_first_name", "user_last_name",
	"user_email", "user_type", "user_org", "cognito_sub",
	"email_verified_at", "has_completed_onboarding",
	"created_at", "updated_at",
}

func newUserRow(userID, email string) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows(userColumns).AddRow(
		userID, "Jane", "Doe",
		email, "free", nil, "sub-123",
		nil, false,
		now, now,
	)
}

func TestAccountRepo_Create_InsertsUserAndDefaultAgent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO meko_system\.users`).
		WithArgs("Jane", "Doe", "jane@example.com", "sub-123", false).
		WillReturnRows(newUserRow("user-1", "jane@example.com"))
	mock.ExpectExec(`INSERT INTO meko_system\.agents`).
		WithArgs("user-1", DefaultAgentID).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	repo := NewAccountRepo(db, false)
	acct, err := repo.Create(context.Background(), CreateAccountParams{
		FirstName:  "Jane",
		LastName:   "Doe",
		Email:      "jane@example.com",
		CognitoSub: "sub-123",
	})
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	if acct == nil || acct.ID != "user-1" {
		t.Fatalf("Create: expected user-1, got %+v", acct)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAccountRepo_Create_RollsBackWhenAgentInsertFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO meko_system\.users`).
		WithArgs("Jane", "Doe", "jane@example.com", "sub-123", false).
		WillReturnRows(newUserRow("user-2", "jane@example.com"))
	mock.ExpectExec(`INSERT INTO meko_system\.agents`).
		WithArgs("user-2", DefaultAgentID).
		WillReturnError(errors.New("agents table missing"))
	mock.ExpectRollback()

	repo := NewAccountRepo(db, false)
	_, err = repo.Create(context.Background(), CreateAccountParams{
		FirstName:  "Jane",
		LastName:   "Doe",
		Email:      "jane@example.com",
		CognitoSub: "sub-123",
	})
	if err == nil {
		t.Fatal("Create: expected error, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAccountRepo_Create_RollsBackWhenUserInsertFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO meko_system\.users`).
		WithArgs("Jane", "Doe", "jane@example.com", "sub-123", false).
		WillReturnError(errors.New("duplicate email"))
	mock.ExpectRollback()

	repo := NewAccountRepo(db, false)
	_, err = repo.Create(context.Background(), CreateAccountParams{
		FirstName:  "Jane",
		LastName:   "Doe",
		Email:      "jane@example.com",
		CognitoSub: "sub-123",
	})
	if err == nil {
		t.Fatal("Create: expected error, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAccountRepo_GetByCognitoSub_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM meko_system\.users\s+WHERE cognito_sub`).
		WithArgs("missing").
		WillReturnRows(sqlmock.NewRows(userColumns))

	repo := NewAccountRepo(db, false)
	acct, err := repo.GetByCognitoSub(context.Background(), "missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acct != nil {
		t.Errorf("expected nil account, got %+v", acct)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAccountRepo_GetMCPCredentials_NoRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM meko_system\.users\s+WHERE user_id`).
		WithArgs("user-x").
		WillReturnRows(sqlmock.NewRows([]string{"u", "p"}).AddRow(nil, nil))

	repo := NewAccountRepo(db, false)
	creds, err := repo.GetMCPCredentials(context.Background(), "user-x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds != nil {
		t.Errorf("expected nil credentials, got %+v", creds)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAccountRepo_GetMCPCredentials_Returns(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM meko_system\.users\s+WHERE user_id`).
		WithArgs("user-y").
		WillReturnRows(sqlmock.NewRows([]string{"u", "p"}).
			AddRow("mcp_user_y", "secret"))

	repo := NewAccountRepo(db, false)
	creds, err := repo.GetMCPCredentials(context.Background(), "user-y")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds == nil ||
		creds.UserName != "mcp_user_y" ||
		creds.Password != "secret" {
		t.Errorf("got creds = %+v", creds)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAccountRepo_SetMCPCredentials_OK(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE meko_system\.users\s+SET user_details`).
		WithArgs("mcp_user_z", "pw", "user-z").
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewAccountRepo(db, false)
	if err := repo.SetMCPCredentials(
		context.Background(), "user-z", "mcp_user_z", "pw",
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAccountRepo_SetMCPCredentials_NoSuchUser(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE meko_system\.users\s+SET user_details`).
		WithArgs("mcp_user_q", "pw", "ghost").
		WillReturnResult(sqlmock.NewResult(0, 0))

	repo := NewAccountRepo(db, false)
	err = repo.SetMCPCredentials(
		context.Background(), "ghost", "mcp_user_q", "pw",
	)
	if err == nil {
		t.Fatal("expected error for missing user, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The tier a new user lands on is the whole point of BYOC mode: 'free' would
// meter the customer against plan caps on infrastructure they are paying for
// themselves. Both directions are pinned — a hosted deployment must keep
// omitting the column so the schema default stands.
func TestAccountRepo_Create_BYOCInsertsProTier(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO meko_system\.users\s*\n?\s*\(user_first_name, user_last_name,\s*\n?\s*user_email, cognito_sub, email_verified_at, user_type\)`).
		WithArgs("Jane", "Doe", "jane@example.com", "sub-123", false).
		WillReturnRows(newUserRow("user-1", "jane@example.com"))
	mock.ExpectExec(`INSERT INTO meko_system\.agents`).
		WithArgs("user-1", DefaultAgentID).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	repo := NewAccountRepo(db, true)
	if _, err := repo.Create(context.Background(), CreateAccountParams{
		FirstName: "Jane", LastName: "Doe",
		Email: "jane@example.com", CognitoSub: "sub-123",
	}); err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAccountRepo_Create_HostedOmitsUserType(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	// No user_type column: the INSERT must fall through to the schema's
	// DEFAULT 'free' rather than naming a tier.
	mock.ExpectQuery(`email_verified_at\)`).
		WithArgs("Jane", "Doe", "jane@example.com", "sub-123", false).
		WillReturnRows(newUserRow("user-1", "jane@example.com"))
	mock.ExpectExec(`INSERT INTO meko_system\.agents`).
		WithArgs("user-1", DefaultAgentID).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	repo := NewAccountRepo(db, false)
	if _, err := repo.Create(context.Background(), CreateAccountParams{
		FirstName: "Jane", LastName: "Doe",
		Email: "jane@example.com", CognitoSub: "sub-123",
	}); err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAccountRepo_GetByID_ReadsUserWithoutCognitoSub(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	now := time.Now()
	mock.ExpectQuery(`WHERE user_id = \$1`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows(userColumns).AddRow(
			"user-1", "Jane", "Doe",
			"jane@example.com", "free", nil, nil,
			nil, false,
			now, now,
		))

	acct, err := NewAccountRepo(db, false).GetByID(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GetByID: unexpected error: %v", err)
	}
	if acct == nil || acct.ID != "user-1" || acct.CognitoSub != "" {
		t.Fatalf("GetByID = %+v, want user-1 with an empty cognito_sub", acct)
	}
}
