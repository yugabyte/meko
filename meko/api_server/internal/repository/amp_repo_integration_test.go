package repository

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/lib/pq"
)

// ampTestConnStr matches the local YugabyteDB every other DB-backed test in this
// codebase uses; openAMPTestDB skips (not fails) when it's unreachable, so this
// runs locally against a migrated DB and is a no-op in CI.
const ampTestConnStr = "postgresql://localhost:5433/yugabyte?user=yugabyte&password=yugabyte&sslmode=disable"

func openAMPTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", ampTestConnStr)
	if err != nil {
		t.Skipf("test database unavailable: open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("test database unavailable: ping: %v", err)
	}
	var exists bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		                WHERE table_schema = 'meko_system' AND table_name = 'amp_datapack_pool')`,
	).Scan(&exists); err != nil || !exists {
		t.Skip("test database missing meko_system.amp_datapack_pool; run migrations first")
	}
	return db
}

// The per-user vCPU cap's correctness rests on two DB-level behaviors sqlmock
// can't exercise (it returns canned rows, never runs the SQL): MarkPoolFailed's
// ON CONFLICT ... WHERE status <> 'ready' guard, and ListByOwner's status filter.
// This runs both against a real YugabyteDB.
func TestAMPRepo_MarkPoolFailedAndListByOwner_Integration(t *testing.T) {
	db := openAMPTestDB(t)
	defer func() { _ = db.Close() }()
	repo := NewAMPRepo(db)
	ctx := context.Background()

	// Fixed test UUIDs (this table has no FKs); scrub before and after so a prior
	// aborted run can't skew the assertions.
	const owner = "11111111-1111-1111-1111-111111111111"
	const dpReady = "22222222-2222-2222-2222-222222222222"
	const dpNew = "33333333-3333-3333-3333-333333333333"
	scrub := func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM meko_system.amp_datapack_pool WHERE owner_user_id = $1`, owner)
	}
	scrub()
	defer scrub()

	// A ready pool (one that already has live compute) must survive MarkPoolFailed
	// — a transient EnsurePool blip can't be allowed to drop its cores out of the
	// sum and let the owner exceed the cap.
	if err := repo.Upsert(ctx, dpReady, dpReady, owner, "ready"); err != nil {
		t.Fatalf("seed ready: %v", err)
	}
	if err := repo.MarkPoolFailed(ctx, dpReady, dpReady, owner); err != nil {
		t.Fatalf("MarkPoolFailed(ready): %v", err)
	}
	if got := poolStatus(t, db, dpReady); got != "ready" {
		t.Errorf("ready row downgraded to %q; the guard must keep it 'ready'", got)
	}

	// A brand-new datapack (no row yet) is inserted as 'failed'.
	if err := repo.MarkPoolFailed(ctx, dpNew, dpNew, owner); err != nil {
		t.Fatalf("MarkPoolFailed(new): %v", err)
	}
	if got := poolStatus(t, db, dpNew); got != "failed" {
		t.Errorf("new row status = %q, want 'failed'", got)
	}

	// ListByOwner returns only the ready pool; the failed one is excluded.
	pools, err := repo.ListByOwner(ctx, owner)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(pools) != 1 || pools[0].DatapackID != dpReady {
		t.Errorf("ListByOwner = %+v, want only the ready pool %s", pools, dpReady)
	}
}

func poolStatus(t *testing.T, db *sql.DB, datapackID string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(
		`SELECT status FROM meko_system.amp_datapack_pool WHERE datapack_id = $1`, datapackID,
	).Scan(&s); err != nil {
		t.Fatalf("read status %s: %v", datapackID, err)
	}
	return s
}
