package admin

import (
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func mockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return db, mock, func() { _ = db.Close() }
}

var (
	rangeStart = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd   = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
)

// TotalTokenSavingsInRange must aggregate the append-only log filtered by
// created_at, so exec token savings can be windowed to the dashboard range.
func TestTotalTokenSavingsInRange_FiltersByCreatedAt(t *testing.T) {
	db, mock, done := mockDB(t)
	defer done()
	repo := NewAdminRepo(db, nil, nil)

	mock.ExpectQuery(`FROM meko_system\.token_savings_log\s+WHERE TRUE AND created_at >= \$1 AND created_at < \$2`).
		WithArgs(rangeStart, rangeEnd).
		WillReturnRows(sqlmock.NewRows([]string{"requests", "tokens"}).AddRow(int64(42), int64(9000)))

	req, saved, err := repo.TotalTokenSavingsInRange(&rangeStart, &rangeEnd)
	if err != nil {
		t.Fatalf("TotalTokenSavingsInRange: %v", err)
	}
	if req != 42 || saved != 9000 {
		t.Errorf("got (%d, %d), want (42, 9000)", req, saved)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TopUserTokenSavings ranges on created_at with limit following the range
// params, and emits NO min-request HAVING filter (the threshold was removed).
func TestTopUserTokenSavings_RangeAndNoMinRequestFilter(t *testing.T) {
	db, mock, done := mockDB(t)
	defer done()
	repo := NewAdminRepo(db, nil, nil)

	mock.ExpectQuery(`WHERE l\.user_id IS NOT NULL AND l\.created_at >= \$1 AND l\.created_at < \$2.*ORDER BY tokens_saved DESC\s+LIMIT \$3`).
		WithArgs(rangeStart, rangeEnd, 10).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "email", "fn", "ln", "rc", "ts"}).
			AddRow("u1", "e@x", "F", "L", int64(3), int64(500)))

	rows, err := repo.TopUserTokenSavings(&rangeStart, &rangeEnd, 10)
	if err != nil {
		t.Fatalf("TopUserTokenSavings: %v", err)
	}
	if len(rows) != 1 || rows[0].UserID != "u1" || rows[0].TokensSaved != 500 {
		t.Errorf("rows = %+v, want one row u1/500", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// CountDatapacksByWeek groups by ISO week and returns oldest-first buckets.
func TestCountDatapacksByWeek_GroupsByWeek(t *testing.T) {
	db, mock, done := mockDB(t)
	defer done()
	repo := NewAdminRepo(db, nil, nil)

	mock.ExpectQuery(`date_trunc\('week', created_at AT TIME ZONE 'UTC'\).*FROM meko_system\.datapacks\s+WHERE TRUE AND created_at >= \$1 AND created_at < \$2\s+GROUP BY 1\s+ORDER BY 1`).
		WithArgs(rangeStart, rangeEnd).
		WillReturnRows(sqlmock.NewRows([]string{"week_start", "count"}).
			AddRow("2026-05-04", int64(2)).
			AddRow("2026-05-11", int64(5)))

	out, err := repo.CountDatapacksByWeek(&rangeStart, &rangeEnd)
	if err != nil {
		t.Fatalf("CountDatapacksByWeek: %v", err)
	}
	if len(out) != 2 || out[0].WeekStart != "2026-05-04" || out[1].Count != 5 {
		t.Errorf("out = %+v, want [{2026-05-04 2} {2026-05-11 5}]", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// CountIndexedDocsBreakdownAll returns one row per document type (joining
// dist_rag.documents for the file type) and, from that single scan, the summed
// document and chunk totals the knowledge panel's tiles need.
func TestCountIndexedDocsBreakdownAll_SumsAndGroupsByType(t *testing.T) {
	db, mock, done := mockDB(t)
	defer done()
	repo := NewAdminRepo(nil, db, nil)

	mock.ExpectQuery(`LEFT JOIN dist_rag\.documents d`).
		WithArgs("sch", "idx").
		WillReturnRows(sqlmock.NewRows([]string{"document_type", "doc_count", "chunk_count"}).
			AddRow("application/pdf", int64(5), int64(50)).
			AddRow("text/markdown", int64(3), int64(12)))

	out, err := repo.CountIndexedDocsBreakdownAll("sch", "idx", nil, nil)
	if err != nil {
		t.Fatalf("CountIndexedDocsBreakdownAll: %v", err)
	}
	if out.Documents != 8 || out.Chunks != 62 {
		t.Errorf("totals = (%d docs, %d chunks), want (8, 62)", out.Documents, out.Chunks)
	}
	if len(out.ByType) != 2 || out.ByType[0].DocumentType != "application/pdf" || out.ByType[0].Count != 5 {
		t.Errorf("byType = %+v, want [{application/pdf 5} {text/markdown 3}]", out.ByType)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// CountIndexedDocsBreakdownBySourceIDs short-circuits to a zero value (no query)
// when the user owns no rag sources — the common case for a fresh user.
func TestCountIndexedDocsBreakdownBySourceIDs_EmptySourcesNoQuery(t *testing.T) {
	db, mock, done := mockDB(t)
	defer done()
	repo := NewAdminRepo(nil, db, nil)

	out, err := repo.CountIndexedDocsBreakdownBySourceIDs("sch", "idx", nil, nil, nil)
	if err != nil {
		t.Fatalf("CountIndexedDocsBreakdownBySourceIDs: %v", err)
	}
	if out.Documents != 0 || out.Chunks != 0 || len(out.ByType) != 0 {
		t.Errorf("out = %+v, want zero value", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected query ran: %v", err)
	}
}
