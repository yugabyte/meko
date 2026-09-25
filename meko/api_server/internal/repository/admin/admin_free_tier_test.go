package admin

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// CountAuditEventsInRange must window free_tier_audit_log by created_at (the
// date-filterable source for the per-user search tiles), passing user + metric +
// both range bounds as args.
func TestCountAuditEventsInRange_FiltersByCreatedAt(t *testing.T) {
	db, mock, done := mockDB(t)
	defer done()
	repo := NewAdminRepo(db, nil, nil)

	mock.ExpectQuery(`FROM meko_system\.free_tier_audit_log\s+WHERE user_id = \$1 AND metric_name = \$2 AND created_at >= \$3 AND created_at < \$4`).
		WithArgs("u1", "memory_search", rangeStart, rangeEnd).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(20)))

	n, err := repo.CountAuditEventsInRange("u1", "memory_search", &rangeStart, &rangeEnd)
	if err != nil {
		t.Fatalf("CountAuditEventsInRange: %v", err)
	}
	if n != 20 {
		t.Errorf("count = %d, want 20", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// FreeTierSummaryCount returns 0 (not an error) when the user has never invoked
// the metric — a missing summary row is a zero count, not a failure.
func TestFreeTierSummaryCount_ZeroOnNoRow(t *testing.T) {
	db, mock, done := mockDB(t)
	defer done()
	repo := NewAdminRepo(db, nil, nil)

	// No rows returned -> sql.ErrNoRows, which the method maps to 0.
	mock.ExpectQuery(`SELECT request_count FROM meko_system\.free_tier_usage`).
		WithArgs("u1", "context_search").
		WillReturnRows(sqlmock.NewRows([]string{"request_count"}))

	n, err := repo.FreeTierSummaryCount("u1", "context_search")
	if err != nil {
		t.Fatalf("FreeTierSummaryCount: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0 on no row", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
