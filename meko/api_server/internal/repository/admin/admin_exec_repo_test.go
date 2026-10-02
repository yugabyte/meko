package admin

import (
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
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

// CountIndexedDocsBreakdownAll reads the base pipeline_details/documents tables
// (not the view), inside a statement_timeout tx, and from that single scan gives
// one row per document type plus the summed document and chunk totals the
// knowledge panel's tiles need.
func TestCountIndexedDocsBreakdownAll_SumsAndGroupsByType(t *testing.T) {
	db, mock, done := mockDB(t)
	defer done()
	repo := NewAdminRepo(nil, db, nil)

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM\s+dist_rag\.pipeline_details pd`).
		WillReturnRows(sqlmock.NewRows([]string{"document_type", "doc_count", "chunk_count"}).
			AddRow("application/pdf", int64(5), int64(50)).
			AddRow("text/markdown", int64(3), int64(12)))
	mock.ExpectCommit()

	out, err := repo.CountIndexedDocsBreakdownAll(nil, nil)
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

	out, err := repo.CountIndexedDocsBreakdownBySourceIDs(nil, nil, nil)
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

// CountCompletedDocsAndChunksAll runs the 2-column totals query inside the
// statement_timeout tx (Begin -> SET LOCAL -> query -> Commit) against
// pipeline_details, with no source join.
func TestCountCompletedDocsAndChunksAll_TotalsUnderTimeoutTx(t *testing.T) {
	db, mock, done := mockDB(t)
	defer done()
	repo := NewAdminRepo(nil, db, nil)

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM\s+dist_rag\.pipeline_details pd`).
		WillReturnRows(sqlmock.NewRows([]string{"documents", "chunks"}).AddRow(int64(11), int64(220)))
	mock.ExpectCommit()

	c, err := repo.CountCompletedDocsAndChunksAll(nil, nil)
	if err != nil {
		t.Fatalf("CountCompletedDocsAndChunksAll: %v", err)
	}
	if c.Documents != 11 || c.Chunks != 220 {
		t.Errorf("got (%d, %d), want (11, 220)", c.Documents, c.Chunks)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// CountCompletedDocsAndChunksBySourceIDs joins documents and filters on
// source_id = ANY($1), with the range args following at $2.
func TestCountCompletedDocsAndChunksBySourceIDs_JoinAndArgOrder(t *testing.T) {
	db, mock, done := mockDB(t)
	defer done()
	repo := NewAdminRepo(nil, db, nil)
	ids := []string{"11111111-1111-1111-1111-111111111111"}

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`(?s)JOIN\s+dist_rag\.documents d0.*d0\.source_id = ANY\(\$1::uuid\[\]\).*created_at >= \$2 AND .*created_at < \$3`).
		WithArgs(pq.Array(ids), rangeStart, rangeEnd).
		WillReturnRows(sqlmock.NewRows([]string{"documents", "chunks"}).AddRow(int64(4), int64(40)))
	mock.ExpectCommit()

	c, err := repo.CountCompletedDocsAndChunksBySourceIDs(ids, &rangeStart, &rangeEnd)
	if err != nil {
		t.Fatalf("CountCompletedDocsAndChunksBySourceIDs: %v", err)
	}
	if c.Documents != 4 || c.Chunks != 40 {
		t.Errorf("got (%d, %d), want (4, 40)", c.Documents, c.Chunks)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// CountIndexedDocsBreakdownBySourceIDs (non-empty) joins documents for the
// source filter, keeps the LEFT JOIN for the file type, and sums per-type rows.
func TestCountIndexedDocsBreakdownBySourceIDs_JoinAndSums(t *testing.T) {
	db, mock, done := mockDB(t)
	defer done()
	repo := NewAdminRepo(nil, db, nil)
	ids := []string{"22222222-2222-2222-2222-222222222222"}

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`(?s)d0\.source_id = ANY\(\$1::uuid\[\]\).*LEFT JOIN dist_rag\.documents d`).
		WithArgs(pq.Array(ids)).
		WillReturnRows(sqlmock.NewRows([]string{"document_type", "doc_count", "chunk_count"}).
			AddRow("application/pdf", int64(2), int64(20)).
			AddRow("text/plain", int64(1), int64(3)))
	mock.ExpectCommit()

	out, err := repo.CountIndexedDocsBreakdownBySourceIDs(ids, nil, nil)
	if err != nil {
		t.Fatalf("CountIndexedDocsBreakdownBySourceIDs: %v", err)
	}
	if out.Documents != 3 || out.Chunks != 23 {
		t.Errorf("totals = (%d, %d), want (3, 23)", out.Documents, out.Chunks)
	}
	if len(out.ByType) != 2 || out.ByType[0].DocumentType != "application/pdf" {
		t.Errorf("byType = %+v", out.ByType)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The rag backlog reads the base pipeline_details table under the KB timeout,
// never the view whose global sort timed out the exec panel (MEKO-728).
func TestCountDocumentsByStatus_ReadsBaseTablesUnderTimeout(t *testing.T) {
	db, mock, done := mockDB(t)
	defer done()
	repo := NewAdminRepo(nil, db, nil)

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM   dist_rag\.pipeline_details pd\s+WHERE  TRUE\s+ORDER  BY pd\.document_id`).
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}).
			AddRow("COMPLETED", int64(40)).AddRow("FAILED", int64(2)).AddRow(nil, int64(1)))
	mock.ExpectCommit()

	got, err := repo.CountDocumentsByStatus()
	if err != nil {
		t.Fatalf("CountDocumentsByStatus: %v", err)
	}
	if got["COMPLETED"] != 40 || got["FAILED"] != 2 || got["unknown"] != 1 {
		t.Errorf("counts = %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
