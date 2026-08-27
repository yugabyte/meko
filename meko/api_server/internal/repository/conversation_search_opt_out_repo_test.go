package repository

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestConversationSearchOptOutRepo_IsOptedOut_NoRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT 1 FROM meko_system.conversation_search_opt_outs`).
		WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}))

	repo := NewConversationSearchOptOutRepo(db)
	optedOut, err := repo.IsOptedOut("dp-1")
	if err != nil {
		t.Fatalf("IsOptedOut: %v", err)
	}
	if optedOut {
		t.Error("expected optedOut=false when no row exists")
	}
}

func TestConversationSearchOptOutRepo_IsOptedOut_RowExists(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT 1 FROM meko_system.conversation_search_opt_outs`).
		WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))

	repo := NewConversationSearchOptOutRepo(db)
	optedOut, err := repo.IsOptedOut("dp-1")
	if err != nil {
		t.Fatalf("IsOptedOut: %v", err)
	}
	if !optedOut {
		t.Error("expected optedOut=true when a row exists")
	}
}

func TestConversationSearchOptOutRepo_SetOptedOut_Upserts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO meko_system.conversation_search_opt_outs`).
		WithArgs("dp-1", "user-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewConversationSearchOptOutRepo(db)
	if err := repo.SetOptedOut("dp-1", "user-1"); err != nil {
		t.Fatalf("SetOptedOut: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestConversationSearchOptOutRepo_ClearOptedOut_Deletes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`DELETE FROM meko_system.conversation_search_opt_outs`).
		WithArgs("dp-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewConversationSearchOptOutRepo(db)
	if err := repo.ClearOptedOut("dp-1"); err != nil {
		t.Fatalf("ClearOptedOut: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
