package admin

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	adminrepo "meko/api_server/internal/repository/admin"
)

func execSvc(t *testing.T) (*AdminMetricsService, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	// systemDB only — the required-path panels (growth, savings) never touch the
	// data plane or Langfuse.
	return NewAdminMetricsService(adminrepo.NewAdminRepo(db, nil, nil)), mock, func() { _ = db.Close() }
}

var (
	execStart = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	execEnd   = time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
)

// Growth returns exactly the users + datapacks counts and the weekly trend,
// aggregating its three control-plane queries under the shared date window.
func TestGrowth_UsersDatapacksAndWeekly(t *testing.T) {
	svc, mock, done := execSvc(t)
	defer done()

	mock.ExpectQuery(`FROM meko_system\.users`).
		WillReturnRows(sqlmock.NewRows([]string{"total", "free", "standard", "pro"}).
			AddRow(int64(7), int64(4), int64(2), int64(1)))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM meko_system\.datapacks`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(9)))
	mock.ExpectQuery(`date_trunc\('week', created_at AT TIME ZONE 'UTC'\)`).
		WillReturnRows(sqlmock.NewRows([]string{"week_start", "count"}).
			AddRow("2026-05-04", int64(3)).AddRow("2026-05-11", int64(6)))

	out, err := svc.Growth(context.Background(), &execStart, &execEnd)
	if err != nil {
		t.Fatalf("Growth: %v", err)
	}
	if out.UsersCreated != 7 || out.DatapacksCreated != 9 {
		t.Errorf("counts = %d/%d, want 7/9", out.UsersCreated, out.DatapacksCreated)
	}
	if len(out.DatapacksByWeek) != 2 || out.DatapacksByWeek[1].Count != 6 {
		t.Errorf("weekly = %+v, want 2 buckets ending in 6", out.DatapacksByWeek)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Savings returns the token totals and the leaderboard (log-aggregated because a
// range is set), and never carries conversation/KB fields.
func TestSavings_TotalsAndLeaderboard(t *testing.T) {
	svc, mock, done := execSvc(t)
	defer done()

	mock.ExpectQuery(`FROM meko_system\.token_savings_log\s+WHERE TRUE AND created_at`).
		WillReturnRows(sqlmock.NewRows([]string{"requests", "tokens"}).AddRow(int64(120), int64(45000)))
	mock.ExpectQuery(`FROM meko_system\.token_savings_log l\s+JOIN meko_system\.users`).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "email", "fn", "ln", "rc", "ts"}).
			AddRow("u1", "e@x", "F", "L", int64(20), int64(9000)))

	out, err := svc.Savings(context.Background(), &execStart, &execEnd)
	if err != nil {
		t.Fatalf("Savings: %v", err)
	}
	if out.TotalRequests != 120 || out.TotalTokensSaved != 45000 {
		t.Errorf("totals = %d/%d, want 120/45000", out.TotalRequests, out.TotalTokensSaved)
	}
	if len(out.TopUsers) != 1 || out.TopUsers[0].UserID != "u1" || out.TopUsers[0].TokensSaved != 9000 {
		t.Errorf("top users = %+v, want one row u1/9000", out.TopUsers)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Executive is the legacy /admin/token-savings shim for meko_ui — the one place
// users_created/datapacks_created are re-derived under the old
// total_users/total_datapacks JSON names, and it must fetch ONLY the control
// plane + one Langfuse count (no data-plane promoted/by-agent queries). With no
// Langfuse DB, conversations stay zero; the summary counter serves the all-time
// totals.
func TestExecutive_MergesPanelsIntoLegacyShape(t *testing.T) {
	svc, mock, done := execSvc(t)
	defer done()

	// User + datapack totals only — the shim fetches these directly and does NOT
	// run Growth's week-over-week CountDatapacksByWeek (ExecutiveSummary has no
	// field for it).
	mock.ExpectQuery(`FROM meko_system\.users`).
		WillReturnRows(sqlmock.NewRows([]string{"total", "free", "standard", "pro"}).
			AddRow(int64(5), int64(3), int64(1), int64(1)))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM meko_system\.datapacks`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(8)))
	// Savings (all-time): the cheap summary counter, then the leaderboard. No
	// data-plane (promoted) or by-agent query fires.
	mock.ExpectQuery(`FROM meko_system\.token_savings_summary`).
		WillReturnRows(sqlmock.NewRows([]string{"requests", "tokens"}).AddRow(int64(200), int64(60000)))
	mock.ExpectQuery(`FROM meko_system\.token_savings_log l`).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "email", "fn", "ln", "rc", "ts"}).
			AddRow("u1", "e@x", "F", "L", int64(40), int64(12000)))

	out, err := svc.Executive(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Executive: %v", err)
	}
	if out.TotalUsers != 5 || out.TotalDatapacks != 8 {
		t.Errorf("growth mapping: users=%d datapacks=%d, want 5/8", out.TotalUsers, out.TotalDatapacks)
	}
	if out.TotalRequests != 200 || out.TotalTokensSaved != 60000 {
		t.Errorf("savings mapping: req=%d saved=%d, want 200/60000", out.TotalRequests, out.TotalTokensSaved)
	}
	if len(out.TopUsers) != 1 || out.TopUsers[0].UserID != "u1" {
		t.Errorf("top users = %+v, want u1", out.TopUsers)
	}
	if out.LangfuseEnabled || out.TotalConversations != 0 {
		t.Errorf("no langfuse db -> conversations zeroed; got enabled=%v n=%d", out.LangfuseEnabled, out.TotalConversations)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
