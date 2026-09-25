package admin

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	adminrepo "meko/api_server/internal/repository/admin"
)

// userSvc builds a service whose systemDB is a sqlmock — enough for the
// control-plane paths (user lookup, datapacks, shares, free-tier counters).
func userSvc(t *testing.T) (*AdminMetricsService, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewAdminMetricsService(adminrepo.NewAdminRepo(db, nil, nil)), mock, func() { _ = db.Close() }
}

// A foreign or unknown user id must 404 before any panel runs, so the shared
// Langfuse DB is never queried for another deployment's user (MG-138). Both
// EnsureUserExists (the handler gate) and UserOverview map no-row to
// ErrUserNotFound.
func TestEnsureUserExistsAndOverview_UnknownUserNotFound(t *testing.T) {
	svc, mock, done := userSvc(t)
	defer done()

	mock.ExpectQuery(`SELECT user_type FROM meko_system\.users`).
		WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows([]string{"user_type"})) // no rows

	if err := svc.EnsureUserExists(context.Background(), "ghost"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("EnsureUserExists err = %v, want ErrUserNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// searchCount reads the exact free_tier_usage summary for the all-time
// (unbounded) case — the audit log's best-effort writes could undercount there.
func TestSearchCount_AllTimeReadsSummary(t *testing.T) {
	svc, mock, done := userSvc(t)
	defer done()

	mock.ExpectQuery(`SELECT request_count FROM meko_system\.free_tier_usage`).
		WithArgs("u1", "knowledgebase_search").
		WillReturnRows(sqlmock.NewRows([]string{"request_count"}).AddRow(int64(42)))

	n, err := svc.searchCount("u1", "knowledgebase_search", nil, nil)
	if err != nil {
		t.Fatalf("searchCount: %v", err)
	}
	if n != 42 {
		t.Errorf("count = %d, want 42 (summary)", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// With a date range, searchCount counts the free_tier_audit_log event log
// instead — the only date-filterable source.
func TestSearchCount_RangeReadsAuditLog(t *testing.T) {
	svc, mock, done := userSvc(t)
	defer done()

	mock.ExpectQuery(`FROM meko_system\.free_tier_audit_log`).
		WithArgs("u1", "memory_search", execStart, execEnd).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(9)))

	n, err := svc.searchCount("u1", "memory_search", &execStart, &execEnd)
	if err != nil {
		t.Fatalf("searchCount: %v", err)
	}
	if n != 9 {
		t.Errorf("count = %d, want 9 (audit log)", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
