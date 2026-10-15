package repository

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestPendingUploadRepo_Promise(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	expires := time.Now().Add(15 * time.Minute)
	mock.ExpectExec(`INSERT INTO meko_system\.pending_uploads`).
		WithArgs("u/dp/hash/report.pdf", "dp-1", "u-owner", "u-uploader",
			"report.pdf", int64(1234), int64(5<<20), expires).
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewPendingUploadRepo(db)
	if err := repo.Promise(PendingUpload{
		S3Key:        "u/dp/hash/report.pdf",
		DatapackID:   "dp-1",
		OwnerUserID:  "u-owner",
		UploaderID:   "u-uploader",
		Filename:     "report.pdf",
		DeclaredSize: 1234,
		MaxSizeBytes: 5 << 20,
		ExpiresAt:    expires,
	}); err != nil {
		t.Fatalf("Promise: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A conflict with a terminal row revives it from scratch: AWAITING_UPLOAD
// whatever it was before (a row abandoned out of AWAITING_DISCOVERY was waiting
// on a discovery that is over and has to be resolved again), attempts reset,
// and a fresh created_at so Health doesn't read the retry as a stale row.
func TestPendingUploadRepo_Promise_RevivesTerminalRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`(?s)INSERT INTO meko_system\.pending_uploads.*DO UPDATE SET\s*` +
		`state = CASE WHEN (?P<live>meko_system\.pending_uploads\.state IN \('IN_PROGRESS', 'AWAITING_DISCOVERY'\)) ` +
		`THEN meko_system\.pending_uploads\.state ELSE 'AWAITING_UPLOAD' END,\s*` +
		`prior_state = CASE WHEN .*? ELSE NULL END,\s*` +
		`created_at = CASE WHEN .*? ELSE NOW\(\) END,\s*` +
		`attempts = CASE WHEN .*? ELSE 0 END,`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := NewPendingUploadRepo(db).Promise(PendingUpload{S3Key: "u/dp/h/a.pdf"}); err != nil {
		t.Fatalf("Promise: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A conflict with a live row (IN_PROGRESS on a pod, AWAITING_DISCOVERY on
// dist_rag) must not reset the work, so every column keeps its value — except
// the deadline, which moves out to the newly signed URL's. Left on the old
// deadline, the row would be abandoned before an upload under the new URL
// could arrive.
func TestPendingUploadRepo_Promise_LiveRowKeepsWorkButTakesTheLaterDeadline(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	keeps := func(col string) string {
		return col + ` = CASE WHEN meko_system\.pending_uploads\.state IN \('IN_PROGRESS', 'AWAITING_DISCOVERY'\) ` +
			`THEN meko_system\.pending_uploads\.` + col + ` ELSE `
	}
	mock.ExpectExec(`(?s)DO UPDATE SET.*` + keeps("state") + `.*` + keeps("attempts") + `.*` +
		keeps("lease_expires_at") + `.*` + keeps("updated_at") + `.*` +
		`expires_at = CASE WHEN meko_system\.pending_uploads\.state IN \('IN_PROGRESS', 'AWAITING_DISCOVERY'\)\s*` +
		`THEN GREATEST\(meko_system\.pending_uploads\.expires_at, EXCLUDED\.expires_at\)\s*` +
		`ELSE EXCLUDED\.expires_at END`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := NewPendingUploadRepo(db).Promise(PendingUpload{S3Key: "u/dp/h/a.pdf"}); err != nil {
		t.Fatalf("Promise: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// While the old URL may still be used, a re-presign keeps the larger declared
// size and cap rather than overwriting them: the quota must not count less
// than an upload still possible under either URL, and the oversized backstop
// must not delete an object the old URL allowed.
func TestPendingUploadRepo_Promise_KeepsTheLargerSizeAndCapWhileTheOldURLMayBeLive(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	larger := func(col string) string {
		return col + ` = CASE WHEN meko_system\.pending_uploads\.expires_at > NOW\(\)\s*` +
			`THEN GREATEST\(meko_system\.pending_uploads\.` + col + `, EXCLUDED\.` + col + `\)\s*` +
			`ELSE EXCLUDED\.` + col + ` END`
	}
	mock.ExpectExec(`(?s)DO UPDATE SET.*` + larger("declared_size") + `.*` + larger("max_size_bytes")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := NewPendingUploadRepo(db).Promise(PendingUpload{S3Key: "u/dp/h/a.pdf", DeclaredSize: 10}); err != nil {
		t.Fatalf("Promise: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Claim must be a single statement: the pass that follows it runs outside any
// transaction, so if the claim were a read followed by a write two pods could
// both take the same row and both do the S3 work.
//
// The mocked "prior_state" column carries whatever the query's RETURNING list
// names — this test can't verify the SQL actually persists it across a claim
// (that needs a real database), but it locks in the Go-level contract: callers
// must see the row's state before this claim, never the IN_PROGRESS this claim
// just wrote, or resolve()'s AWAITING_DISCOVERY branch goes unreachable.
func TestPendingUploadRepo_Claim(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	created := time.Now().Add(-time.Minute)
	expires := time.Now().Add(10 * time.Minute)
	lease := time.Now().Add(2 * time.Minute)
	mock.ExpectQuery(`UPDATE meko_system\.pending_uploads`).
		WithArgs(2, int64(120_000_000), int64(15_000_000)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "s3_key", "datapack_id", "owner_user_id", "uploader_id",
			"filename", "declared_size", "max_size_bytes", "prior_state", "attempts",
			"created_at", "expires_at", "lease_expires_at",
		}).AddRow(
			"pu-1", "u/dp/h1/a.pdf", "dp-1", "u-owner", "u-uploader",
			"a.pdf", int64(10), int64(5<<20), PendingUploadAwaiting, 1,
			created, expires, lease,
		).AddRow(
			"pu-2", "u/dp/h2/b.md", "dp-1", "u-owner", "u-uploader",
			"b.md", int64(20), int64(5<<20), PendingUploadAwaitingDiscovery, 3,
			created, expires, lease,
		))

	repo := NewPendingUploadRepo(db)
	claimed, err := repo.Claim(2, 2*time.Minute, 15*time.Second)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("expected 2 claimed rows, got %d", len(claimed))
	}
	if claimed[0].ID != "pu-1" || claimed[1].S3Key != "u/dp/h2/b.md" {
		t.Errorf("unexpected rows: %+v", claimed)
	}
	// Neither row comes back IN_PROGRESS: that's what this claim just set, so
	// seeing it here would mean the query stopped returning the pre-claim state.
	if claimed[0].State != PendingUploadAwaiting {
		t.Errorf("state = %q, want the pre-claim AWAITING_UPLOAD", claimed[0].State)
	}
	if claimed[1].State != PendingUploadAwaitingDiscovery {
		t.Errorf("state = %q, want the pre-claim AWAITING_DISCOVERY", claimed[1].State)
	}
	// attempts comes back already incremented, which is what makes a row stuck
	// in a loop visible without a second query.
	if claimed[1].Attempts != 3 {
		t.Errorf("expected attempts to be returned, got %d", claimed[1].Attempts)
	}
	// The lease is the fencing token every later write passes back.
	if !claimed[0].LeaseExpiresAt.Equal(lease) {
		t.Errorf("LeaseExpiresAt = %v, want %v", claimed[0].LeaseExpiresAt, lease)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A NULL prior_state is schema-legal (the CHECK constraint explicitly allows
// it) and reachable in principle for a row recovered via the lease-expiry
// branch, so Claim must degrade to a safe default instead of a Scan error
// that would abort the whole batch -- discarding every other row this same
// call already claimed, whose UPDATE committed regardless.
func TestPendingUploadRepo_Claim_NullPriorState_DefaultsToAwaitingUpload(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	created := time.Now().Add(-time.Minute)
	expires := time.Now().Add(10 * time.Minute)
	lease := time.Now().Add(2 * time.Minute)
	mock.ExpectQuery(`UPDATE meko_system\.pending_uploads`).
		WithArgs(2, int64(120_000_000), int64(15_000_000)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "s3_key", "datapack_id", "owner_user_id", "uploader_id",
			"filename", "declared_size", "max_size_bytes", "prior_state", "attempts",
			"created_at", "expires_at", "lease_expires_at",
		}).AddRow(
			"pu-1", "u/dp/h1/a.pdf", "dp-1", "u-owner", "u-uploader",
			"a.pdf", int64(10), int64(5<<20), nil, 1,
			created, expires, lease,
		).AddRow(
			"pu-2", "u/dp/h2/b.md", "dp-1", "u-owner", "u-uploader",
			"b.md", int64(20), int64(5<<20), PendingUploadAwaitingDiscovery, 3,
			created, expires, lease,
		))

	repo := NewPendingUploadRepo(db)
	claimed, err := repo.Claim(2, 2*time.Minute, 15*time.Second)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("expected both rows claimed despite the NULL, got %d", len(claimed))
	}
	if claimed[0].State != PendingUploadAwaiting {
		t.Errorf("state = %q, want the safe default AWAITING_UPLOAD", claimed[0].State)
	}
	// The other row in the same batch must not be collateral damage.
	if claimed[1].State != PendingUploadAwaitingDiscovery {
		t.Errorf("state = %q, want the pre-claim AWAITING_DISCOVERY", claimed[1].State)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// fenced is the WHERE tail every claimant write must carry; without it a pod
// whose lease lapsed would overwrite the row's next owner.
const fenced = `state = 'IN_PROGRESS' AND lease_expires_at = \$`

func TestPendingUploadRepo_Retire(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	lease := time.Now().Add(time.Minute)
	mock.ExpectExec(`(?s)DELETE FROM meko_system\.pending_uploads.*`+fenced+`2`).
		WithArgs("pu-1", lease).
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewPendingUploadRepo(db)
	if err := repo.Retire("pu-1", lease); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestPendingUploadRepo_Fail(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	lease := time.Now().Add(time.Minute)
	mock.ExpectExec(`(?s)UPDATE meko_system\.pending_uploads.*`+fenced+`4`).
		WithArgs("pu-1", PendingUploadOversized, "too big", lease).
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewPendingUploadRepo(db)
	if err := repo.Fail("pu-1", lease, PendingUploadOversized, "too big"); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A non-terminal state must not reach the database: a row parked in
// AWAITING_UPLOAD by Fail would be claimed forever, and one in IN_PROGRESS
// would hold a lease nothing releases.
func TestPendingUploadRepo_Fail_RejectsNonTerminalState(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	repo := NewPendingUploadRepo(db)
	if err := repo.Fail("pu-1", time.Now(), PendingUploadAwaiting, "nope"); err == nil {
		t.Fatal("expected an error for a non-terminal state")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no query should have run: %v", err)
	}
}

func TestPendingUploadRepo_Recheck(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	lease := time.Now().Add(time.Minute)
	mock.ExpectExec(`(?s)UPDATE meko_system\.pending_uploads.*`+fenced+`3`).
		WithArgs("pu-1", "head object: timeout", lease).
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewPendingUploadRepo(db)
	if err := repo.Recheck("pu-1", lease, "head object: timeout"); err != nil {
		t.Fatalf("Recheck: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// RecheckAwaitingDiscovery must not touch last_error: there is no error, only
// a document dist_rag hasn't created yet.
func TestPendingUploadRepo_RecheckAwaitingDiscovery(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	lease := time.Now().Add(time.Minute)
	mock.ExpectExec(`(?s)UPDATE meko_system\.pending_uploads.*`+fenced+`2`).
		WithArgs("pu-1", lease).
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewPendingUploadRepo(db)
	if err := repo.RecheckAwaitingDiscovery("pu-1", lease); err != nil {
		t.Fatalf("RecheckAwaitingDiscovery: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A claimant write that matches no row means the lease lapsed and the row is
// someone else's. It must say so rather than report success, so the watcher
// drops the row instead of believing its outcome was recorded.
func TestPendingUploadRepo_WritesReportLostLease(t *testing.T) {
	lease := time.Now().Add(time.Minute)
	writes := map[string]func(*PendingUploadRepo) error{
		"Retire":  func(r *PendingUploadRepo) error { return r.Retire("pu-1", lease) },
		"Fail":    func(r *PendingUploadRepo) error { return r.Fail("pu-1", lease, PendingUploadAbandoned, "x") },
		"Recheck": func(r *PendingUploadRepo) error { return r.Recheck("pu-1", lease, "") },
		"RecheckAwaitingDiscovery": func(r *PendingUploadRepo) error {
			return r.RecheckAwaitingDiscovery("pu-1", lease)
		},
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()

			mock.ExpectExec(`meko_system\.pending_uploads`).
				WillReturnResult(sqlmock.NewResult(0, 0))
			if err := write(NewPendingUploadRepo(db)); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("error = %v, want ErrLeaseLost", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet expectations: %v", err)
			}
		})
	}
}

// Abandon is the non-claimant's write, so it carries no lease fence, but it
// only ever touches a live row: a terminal one keeps the reason it has.
func TestPendingUploadRepo_Abandon(t *testing.T) {
	for _, tc := range []struct {
		name     string
		affected int64
	}{{"live row", 1}, {"no live row", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()

			mock.ExpectExec(`(?s)UPDATE meko_system\.pending_uploads.*`+
				`state IN \('AWAITING_UPLOAD', 'IN_PROGRESS', 'AWAITING_DISCOVERY'\)`).
				WithArgs("u/dp/h/a.pdf", "dp-1", "not a PDF").
				WillReturnResult(sqlmock.NewResult(0, tc.affected))

			changed, err := NewPendingUploadRepo(db).Abandon("dp-1", "u/dp/h/a.pdf", "not a PDF")
			if err != nil {
				t.Fatalf("Abandon: %v", err)
			}
			if changed != (tc.affected == 1) {
				t.Errorf("changed = %v, want %v", changed, tc.affected == 1)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet expectations: %v", err)
			}
		})
	}
}

func TestPendingUploadRepo_SumPendingSizes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`(?s)SUM\(declared_size\) FILTER \(WHERE s3_key <> \$2\).*`+
		`MAX\(declared_size\) FILTER \(WHERE s3_key = \$2 AND expires_at > NOW\(\)\).*`+
		`AND registered_at IS NULL`).
		WithArgs("u-owner", "u/dp/h/a.pdf").
		WillReturnRows(sqlmock.NewRows([]string{"others", "this_key"}).AddRow(int64(300), int64(100)))

	others, thisKey, err := NewPendingUploadRepo(db).SumPendingSizes("u-owner", "u/dp/h/a.pdf")
	if err != nil || others != 300 || thisKey != 100 {
		t.Fatalf("SumPendingSizes = %d, %d, %v; want 300, 100, nil", others, thisKey, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestPendingUploadRepo_MarkRegistered(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	lease := time.Now().Add(time.Minute)
	mock.ExpectExec(`(?s)UPDATE meko_system\.pending_uploads\s+SET registered_at = NOW\(\).*`+fenced+`2`).
		WithArgs("pu-1", lease).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := NewPendingUploadRepo(db).MarkRegistered("pu-1", lease); err != nil {
		t.Fatalf("MarkRegistered: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestPendingUploadRepo_PurgeTerminal(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`(?s)DELETE FROM meko_system\.pending_uploads.*`+
		`state IN \('ABANDONED', 'OVERSIZED'\).*LIMIT \$2.*state IN \('ABANDONED', 'OVERSIZED'\)`).
		WithArgs(int64(7*24*time.Hour/time.Microsecond), 500).
		WillReturnResult(sqlmock.NewResult(0, 3))

	n, err := NewPendingUploadRepo(db).PurgeTerminal(7*24*time.Hour, 500)
	if err != nil || n != 3 {
		t.Fatalf("PurgeTerminal = %d, %v; want 3, nil", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestPendingUploadRepo_Health(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	oldest := time.Now().Add(-90 * time.Second)
	nextDue := time.Now().Add(4 * time.Second)
	mock.ExpectQuery(`FROM meko_system\.pending_uploads`).
		WithArgs(int64(120_000_000), int64(15_000_000)).
		WillReturnRows(sqlmock.NewRows([]string{"count", "min", "high", "next_due"}).
			AddRow(3, oldest, 1, nextDue))

	repo := NewPendingUploadRepo(db)
	h, err := repo.Health(2*time.Minute, 15*time.Second)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Live != 3 || h.StuckRows != 1 {
		t.Errorf("unexpected health: %+v", h)
	}
	if h.OldestAge < 90*time.Second {
		t.Errorf("expected an age of at least 90s, got %s", h.OldestAge)
	}
	// The watcher sleeps on this, so it must be a remaining duration and not
	// the timestamp itself.
	if h.NextDueIn <= 0 || h.NextDueIn > 4*time.Second {
		t.Errorf("expected NextDueIn within 4s, got %s", h.NextDueIn)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A queue with no live rows must report a zero age rather than "since the zero
// time", which would look like a decades-old stuck upload. NextDueIn is zero for
// the same reason: nothing is due, and the watcher reads it only when Live > 0.
func TestPendingUploadRepo_Health_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM meko_system\.pending_uploads`).
		WithArgs(int64(120_000_000), int64(15_000_000)).
		WillReturnRows(sqlmock.NewRows([]string{"count", "min", "high", "next_due"}).
			AddRow(0, nil, 0, nil))

	repo := NewPendingUploadRepo(db)
	h, err := repo.Health(2*time.Minute, 15*time.Second)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Live != 0 || h.OldestAge != 0 || h.NextDueIn != 0 {
		t.Errorf("unexpected health: %+v", h)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// GetByKey is the status endpoint's read. Scoped by datapack as well as key, so
// a caller cannot be told about an upload in a datapack they were not
// authorized for.
func TestPendingUploadRepo_GetByKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	expires := time.Now().Add(10 * time.Minute)
	mock.ExpectQuery(`SELECT id, s3_key, datapack_id`).
		WithArgs("u/dp/h1/a.pdf", "dp-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "s3_key", "datapack_id", "owner_user_id", "uploader_id",
			"filename", "declared_size", "max_size_bytes", "state", "attempts",
			"last_error", "created_at", "expires_at",
		}).AddRow(
			"pu-1", "u/dp/h1/a.pdf", "dp-1", "u-owner", "u-uploader", "a.pdf",
			int64(1234), int64(5<<20), PendingUploadOversized, 3,
			"object is 99 bytes, over the 10-byte cap it was authorized under",
			time.Now(), expires,
		))

	repo := NewPendingUploadRepo(db)
	p, err := repo.GetByKey("dp-1", "u/dp/h1/a.pdf")
	if err != nil {
		t.Fatalf("GetByKey: %v", err)
	}
	if p == nil {
		t.Fatal("GetByKey: got nil, want a row")
	}
	if p.State != PendingUploadOversized || p.Attempts != 3 {
		t.Errorf("state/attempts = %q/%d, want %q/3", p.State, p.Attempts, PendingUploadOversized)
	}
	// The recorded reason is the only explanation the caller ever gets, so a
	// NULL-safe scan of it is the point of this column.
	if p.LastError == "" {
		t.Error("LastError is empty, want the recorded reason")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// No row is the normal answer for a *registered* upload, since Retire deletes
// it. It must not read as an error, or the status endpoint would report a
// finished upload as broken.
func TestPendingUploadRepo_GetByKeyAbsent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT id, s3_key, datapack_id`).
		WithArgs("u/dp/h1/gone.pdf", "dp-1").
		WillReturnError(sql.ErrNoRows)

	repo := NewPendingUploadRepo(db)
	p, err := repo.GetByKey("dp-1", "u/dp/h1/gone.pdf")
	if err != nil {
		t.Fatalf("GetByKey: unexpected error: %v", err)
	}
	if p != nil {
		t.Errorf("GetByKey: got %+v, want nil", p)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
