package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestTokenSavingsRepo_GetSummary_PreservesRequestedOrder(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	// Rows arrive in the opposite order from how the caller asked for the
	// datapacks — the response must still follow the caller's order, not
	// row-arrival order.
	mock.ExpectQuery(`SELECT datapack_id, source, request_count, tokens_saved_total`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"datapack_id", "source", "request_count", "tokens_saved_total"}).
			AddRow("dp-3", "conversation", 2, 18000).
			AddRow("dp-1", "conversation", 1, 9000).
			AddRow("dp-2", "memory", 4, 20000))

	repo := NewTokenSavingsRepo(db)
	summary, err := repo.GetSummary(context.Background(), []string{"dp-1", "dp-2", "dp-3"})
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}

	if got := len(summary.ByDatapack); got != 3 {
		t.Fatalf("expected 3 by_datapack entries, got %d", got)
	}
	wantOrder := []string{"dp-1", "dp-2", "dp-3"}
	for i, want := range wantOrder {
		if got := summary.ByDatapack[i].DatapackID; got != want {
			t.Errorf("by_datapack[%d] = %q, want %q (requested order not preserved)", i, got, want)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestTokenSavingsRepo_GetSummary_IncludesDatapacksWithNoSavings(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	// Only "dp-1" has any rows; "dp-2" was requested but has never recorded
	// a token-savings call.
	mock.ExpectQuery(`SELECT datapack_id, source, request_count, tokens_saved_total`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"datapack_id", "source", "request_count", "tokens_saved_total"}).
			AddRow("dp-1", "conversation", 1, 9000))

	repo := NewTokenSavingsRepo(db)
	summary, err := repo.GetSummary(context.Background(), []string{"dp-1", "dp-2"})
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}

	if got := len(summary.ByDatapack); got != 2 {
		t.Fatalf("expected 2 by_datapack entries (one per requested datapack), got %d", got)
	}
	if summary.ByDatapack[0].DatapackID != "dp-1" || summary.ByDatapack[0].TotalTokensSaved != 9000 {
		t.Errorf("unexpected dp-1 entry: %+v", summary.ByDatapack[0])
	}
	if summary.ByDatapack[1].DatapackID != "dp-2" {
		t.Errorf("expected dp-2 to be present even with no rows, got %+v", summary.ByDatapack[1])
	}
	if summary.ByDatapack[1].TotalRequests != 0 || summary.ByDatapack[1].TotalTokensSaved != 0 {
		t.Errorf("expected dp-2 to have zero counts, got %+v", summary.ByDatapack[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
