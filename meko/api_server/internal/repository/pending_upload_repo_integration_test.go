package repository

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// These run the pending_uploads SQL against a real YugabyteDB, which sqlmock
// can't: it only matches statement text, so it never evaluates the ON CONFLICT
// CASEs, the claim's SKIP LOCKED subquery, the partial indexes or the interval
// arithmetic. Each test gets a throwaway database holding only migration 000028,
// so a developer's own meko_system is never touched.
//
// MEKO_TEST_DB_URL points them at a cluster other than localhost:5433, the
// address the other integration tests here assume.

const pendingUploadsMigration = "../db/migrations/system/000028_pending_uploads.up.sql"

func openPendingUploadsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	connStr := ampTestConnStr
	if v := os.Getenv("MEKO_TEST_DB_URL"); v != "" {
		connStr = v
	}
	admin, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Skipf("test database unavailable: open: %v", err)
	}
	if perr := admin.Ping(); perr != nil {
		_ = admin.Close()
		t.Skipf("test database unavailable: ping: %v", perr)
	}

	name := fmt.Sprintf("pending_uploads_it_%d", time.Now().UnixNano())
	if _, cerr := admin.Exec(`CREATE DATABASE ` + name); cerr != nil {
		_ = admin.Close()
		t.Fatalf("create database: %v", cerr)
	}
	db, err := sql.Open("postgres", strings.Replace(connStr, "/yugabyte?", "/"+name+"?", 1))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		if _, derr := admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`); derr != nil {
			t.Logf("drop database %s: %v", name, derr)
		}
		_ = admin.Close()
	})

	ddl, err := os.ReadFile(pendingUploadsMigration)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := db.Exec(`CREATE SCHEMA meko_system`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	return db
}

const (
	itOwner    = "00000000-0000-0000-0000-00000000000a"
	itUploader = "00000000-0000-0000-0000-00000000000b"
	itDatapack = "00000000-0000-0000-0000-00000000000c"
)

func itPromise(key string, size int64, expires time.Time) PendingUpload {
	return PendingUpload{
		S3Key: key, DatapackID: itDatapack, OwnerUserID: itOwner, UploaderID: itUploader,
		Filename: "a.pdf", DeclaredSize: size, MaxSizeBytes: 5 << 20, ExpiresAt: expires,
	}
}

type itRow struct {
	expiresAt         time.Time
	state, priorState string
	declared, maxSize int64
	attempts          int
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func TestPendingUploadRepo_Promise_Integration(t *testing.T) {
	db := openPendingUploadsTestDB(t)
	repo := NewPendingUploadRepo(db)
	const key = "own/dp/hash/a.pdf"
	readRow := func() itRow {
		t.Helper()
		var (
			r     itRow
			prior sql.NullString
		)
		if err := db.QueryRow(
			`SELECT state, prior_state, declared_size, max_size_bytes, attempts, expires_at
			   FROM meko_system.pending_uploads WHERE s3_key = $1`, key,
		).Scan(&r.state, &prior, &r.declared, &r.maxSize, &r.attempts, &r.expiresAt); err != nil {
			t.Fatalf("read row: %v", err)
		}
		r.priorState = prior.String
		return r
	}
	first := time.Now().Add(15 * time.Minute).UTC().Truncate(time.Microsecond)

	if err := repo.Promise(itPromise(key, 100, first)); err != nil {
		t.Fatalf("first Promise: %v", err)
	}

	// A re-presign of the same size while the first URL may still be used:
	// accepted, and the cap becomes the larger of the two.
	again := itPromise(key, 100, first.Add(time.Minute))
	again.MaxSizeBytes = 50 << 20
	if err := repo.Promise(again); err != nil {
		t.Fatalf("same-size re-Promise: %v", err)
	}
	if r := readRow(); r.maxSize != 50<<20 || !r.expiresAt.Equal(first.Add(time.Minute)) {
		t.Fatalf("after same-size re-Promise: %+v", r)
	}
	lower := itPromise(key, 100, first)
	lower.MaxSizeBytes = 1 << 20
	if err := repo.Promise(lower); err != nil {
		t.Fatalf("lower-cap re-Promise: %v", err)
	}
	if r := readRow(); r.maxSize != 50<<20 {
		t.Fatalf("a still-valid URL's larger cap was lowered: %+v", r)
	}

	// A different size while the old URL may still be used keeps the larger
	// one: a corrected size goes up, and a wrong smaller one can't shrink what
	// the quota counts for an upload the old URL can still land.
	if err := repo.Promise(itPromise(key, 200, first)); err != nil {
		t.Fatalf("larger-size re-Promise: %v", err)
	}
	if r := readRow(); r.declared != 200 {
		t.Fatalf("a larger re-presigned size was not taken: %+v", r)
	}
	if err := repo.Promise(itPromise(key, 150, first)); err != nil {
		t.Fatalf("smaller-size re-Promise: %v", err)
	}
	if r := readRow(); r.declared != 200 {
		t.Fatalf("a smaller re-presigned size shrank a still-usable URL's: %+v", r)
	}

	// Once the old deadline has passed no URL for it is usable, so a
	// different size revives the row with the new values.
	mustExec(t, db, `UPDATE meko_system.pending_uploads
	                    SET state = 'ABANDONED', expires_at = NOW() - INTERVAL '1 second', attempts = 9
	                  WHERE s3_key = $1`, key)
	revived := itPromise(key, 200, first)
	revived.MaxSizeBytes = 2 << 20
	if err := repo.Promise(revived); err != nil {
		t.Fatalf("re-Promise after expiry: %v", err)
	}
	if r := readRow(); r.state != PendingUploadAwaiting || r.declared != 200 ||
		r.maxSize != 2<<20 || r.attempts != 0 {
		t.Fatalf("expired row not revived: %+v", r)
	}

	// A live row keeps its work and takes the later deadline.
	mustExec(t, db, `UPDATE meko_system.pending_uploads
	                    SET state = 'IN_PROGRESS', prior_state = 'AWAITING_DISCOVERY', attempts = 3,
	                        lease_expires_at = NOW() + INTERVAL '1 minute'
	                  WHERE s3_key = $1`, key)
	later := itPromise(key, 200, first.Add(time.Hour))
	if err := repo.Promise(later); err != nil {
		t.Fatalf("re-Promise of a live row: %v", err)
	}
	if r := readRow(); r.state != PendingUploadInProgress ||
		r.priorState != PendingUploadAwaitingDiscovery || r.attempts != 3 ||
		!r.expiresAt.Equal(first.Add(time.Hour)) {
		t.Fatalf("live row lost its work or kept the earlier deadline: %+v", r)
	}
}

func TestPendingUploadRepo_Claim_Integration(t *testing.T) {
	db := openPendingUploadsTestDB(t)
	repo := NewPendingUploadRepo(db)
	expires := time.Now().Add(15 * time.Minute)
	for i := range 6 {
		if err := repo.Promise(itPromise(fmt.Sprintf("own/dp/h%d/a.pdf", i), 1, expires)); err != nil {
			t.Fatalf("Promise: %v", err)
		}
	}

	// Concurrent claims take disjoint rows and, between them, every row.
	var (
		mu      sync.Mutex
		claimed = map[string]int{}
		wg      sync.WaitGroup
	)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := repo.Claim(2, time.Minute, 15*time.Second)
			if err != nil {
				t.Errorf("concurrent Claim: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, r := range rows {
				claimed[r.ID]++
			}
		}()
	}
	wg.Wait()
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("row %s claimed %d times", id, n)
		}
	}
	t.Logf("concurrent claims took %d of 6 rows", len(claimed))

	// A sub-second lease is honored rather than rounded down to nothing.
	mustExec(t, db, `TRUNCATE meko_system.pending_uploads`)
	const key = "own/dp/hx/a.pdf"
	if err := repo.Promise(itPromise(key, 1, expires)); err != nil {
		t.Fatalf("Promise: %v", err)
	}
	rows, err := repo.Claim(1, 500*time.Millisecond, 15*time.Second)
	if err != nil || len(rows) != 1 {
		t.Fatalf("Claim = %d rows, %v", len(rows), err)
	}
	if d := time.Until(rows[0].LeaseExpiresAt); d <= 0 || d > 500*time.Millisecond {
		t.Fatalf("lease expires in %s, want within (0, 500ms]", d)
	}

	// prior_state survives a claim and a lease recovery.
	if rerr := repo.RecheckAwaitingDiscovery(rows[0].ID, rows[0].LeaseExpiresAt); rerr != nil {
		t.Fatalf("RecheckAwaitingDiscovery: %v", rerr)
	}
	// Due after attempts=1's one-second backoff, not before.
	if early, cerr := repo.Claim(1, time.Minute, 15*time.Second); cerr != nil || len(early) != 0 {
		t.Fatalf("claimed a row inside its backoff: %d rows, %v", len(early), cerr)
	}
	time.Sleep(1100 * time.Millisecond)
	rows, err = repo.Claim(1, time.Minute, 15*time.Second)
	if err != nil || len(rows) != 1 || rows[0].State != PendingUploadAwaitingDiscovery {
		t.Fatalf("Claim after backoff = %+v, %v", rows, err)
	}
	mustExec(t, db, `UPDATE meko_system.pending_uploads
	                    SET lease_expires_at = NOW() - INTERVAL '1 second' WHERE s3_key = $1`, key)
	rows, err = repo.Claim(1, time.Minute, 15*time.Second)
	if err != nil || len(rows) != 1 || rows[0].State != PendingUploadAwaitingDiscovery || rows[0].Attempts != 3 {
		t.Fatalf("lease recovery = %+v, %v", rows, err)
	}

	// A row looked at far more times than the shift's clamp still computes a
	// backoff (LEAST of BIGINTs) rather than overflowing.
	mustExec(t, db, `UPDATE meko_system.pending_uploads
	                    SET state = 'AWAITING_UPLOAD', attempts = 1000,
	                        updated_at = NOW() - INTERVAL '1 minute' WHERE s3_key = $1`, key)
	if rows, err := repo.Claim(1, time.Minute, 15*time.Second); err != nil || len(rows) != 1 {
		t.Fatalf("Claim of a long-lived row = %d rows, %v", len(rows), err)
	}
}

// The claim, quota and purge queries must each be served by their own index. A
// query whose WHERE doesn't imply a partial index's predicate can't use it at
// all, and the claim and quota indexes share a predicate, so on an empty,
// unanalyzed table the planner treats them as interchangeable. The table is
// therefore filled with the shape it has in production -- many owners, mostly
// terminal rows -- and analyzed; seq scans are off so the plan reflects the
// best index rather than a table small enough to read whole.
func TestPendingUploadRepo_PartialIndexesServeTheirQueries_Integration(t *testing.T) {
	db := openPendingUploadsTestDB(t)
	mustExec(t, db, `INSERT INTO meko_system.pending_uploads
	    (s3_key, datapack_id, owner_user_id, uploader_id, filename, declared_size, max_size_bytes,
	     expires_at, state)
	  SELECT 'k' || g, $1::uuid,
	         ('00000000-0000-0000-0000-' || lpad(to_hex(g % 200), 12, '0'))::uuid,
	         $1::uuid, 'f', 1, 1, NOW() + INTERVAL '15 minutes',
	         CASE WHEN g % 10 = 0 THEN 'AWAITING_UPLOAD' ELSE 'ABANDONED' END
	    FROM generate_series(1, 5000) g`, itDatapack)
	mustExec(t, db, `ANALYZE meko_system.pending_uploads`)
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), `SET enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}

	for _, tc := range []struct{ index, query string }{
		{"idx_pending_uploads_claim", `SELECT id FROM meko_system.pending_uploads
		    WHERE (state IN ('AWAITING_UPLOAD', 'AWAITING_DISCOVERY')
		           AND updated_at + ` + backoff("15000000") + ` <= NOW())
		       OR (state = 'IN_PROGRESS' AND lease_expires_at < NOW())
		    ORDER BY created_at LIMIT 20 FOR UPDATE SKIP LOCKED`},
		{"idx_pending_uploads_owner_live", `SELECT COALESCE(SUM(declared_size) FILTER (WHERE s3_key <> 'k1'), 0),
		        COALESCE(MAX(declared_size) FILTER (WHERE s3_key = 'k1' AND expires_at > NOW()), 0)
		    FROM meko_system.pending_uploads
		   WHERE owner_user_id = '` + itOwner + `'::uuid
		     AND state IN ('AWAITING_UPLOAD', 'IN_PROGRESS', 'AWAITING_DISCOVERY')
		     AND registered_at IS NULL`},
		{"idx_pending_uploads_terminal", `SELECT id FROM meko_system.pending_uploads
		   WHERE state IN ('ABANDONED', 'OVERSIZED') AND updated_at < NOW()
		   ORDER BY updated_at LIMIT 500`},
	} {
		rows, err := conn.QueryContext(t.Context(), `EXPLAIN `+tc.query)
		if err != nil {
			t.Fatalf("explain for %s: %v", tc.index, err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan plan: %v", err)
			}
			plan.WriteString(line + "\n")
		}
		_ = rows.Close()
		if !strings.Contains(plan.String(), tc.index) {
			t.Errorf("query does not use %s:\n%s", tc.index, plan.String())
		}
	}
}

func TestPendingUploadRepo_HealthAndCleanup_Integration(t *testing.T) {
	db := openPendingUploadsTestDB(t)
	repo := NewPendingUploadRepo(db)
	now := time.Now()
	for key, exp := range map[string]time.Time{
		"own/dp/h1/fresh.pdf":   now.Add(15 * time.Minute),
		"own/dp/h2/overdue.pdf": now.Add(-10 * time.Minute),
		"own/dp/h3/failed.pdf":  now.Add(-time.Hour),
	} {
		if err := repo.Promise(itPromise(key, 1, exp)); err != nil {
			t.Fatalf("Promise %s: %v", key, err)
		}
	}
	// Many looks at an unexpired row are not "stuck"; a live row well past its
	// deadline is.
	mustExec(t, db, `UPDATE meko_system.pending_uploads SET attempts = 500 WHERE s3_key = 'own/dp/h1/fresh.pdf'`)
	mustExec(t, db, `UPDATE meko_system.pending_uploads
	                    SET state = 'ABANDONED', updated_at = NOW() - INTERVAL '8 days'
	                  WHERE s3_key = 'own/dp/h3/failed.pdf'`)

	h, err := repo.Health(2*time.Minute, 15*time.Second)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Live != 2 || h.StuckRows != 1 {
		t.Fatalf("Health = %+v, want 2 live and 1 stuck", h)
	}

	if n, err := repo.PurgeTerminal(7*24*time.Hour, 500); err != nil || n != 1 {
		t.Fatalf("PurgeTerminal = %d, %v; want 1", n, err)
	}

	// Abandon leaves a row whose URL may still be used (deadline more than the
	// grace away) and takes one whose URL has expired.
	const grace = 10 * time.Minute
	if changed, err := repo.Abandon(itDatapack, "own/dp/h1/fresh.pdf", "deleted", grace); err != nil || changed {
		t.Fatalf("Abandon with a usable URL = %v, %v; want left live", changed, err)
	}
	if changed, err := repo.Abandon(itDatapack, "own/dp/h2/overdue.pdf", "deleted", grace); err != nil || !changed {
		t.Fatalf("Abandon with an expired URL = %v, %v; want abandoned", changed, err)
	}
}

// The quota counts each pending file once: not a second time against its own
// re-presign, not at all once its document is registered, and not past the
// point where its URL can still land anything.
func TestPendingUploadRepo_SumPendingSizes_Integration(t *testing.T) {
	db := openPendingUploadsTestDB(t)
	repo := NewPendingUploadRepo(db)
	soon := time.Now().Add(15 * time.Minute)
	for key, size := range map[string]int64{
		"own/dp/h1/a.pdf": 100, "own/dp/h2/b.pdf": 200, "own/dp/h3/c.pdf": 400, "own/dp/h4/d.pdf": 800,
	} {
		if err := repo.Promise(itPromise(key, size, soon)); err != nil {
			t.Fatalf("Promise %s: %v", key, err)
		}
	}
	mustExec(t, db, `UPDATE meko_system.pending_uploads SET registered_at = NOW() WHERE s3_key = 'own/dp/h3/c.pdf'`)
	mustExec(t, db, `UPDATE meko_system.pending_uploads SET state = 'ABANDONED' WHERE s3_key = 'own/dp/h4/d.pdf'`)

	others, thisKey, err := repo.SumPendingSizes(itOwner, "own/dp/h1/a.pdf")
	if err != nil || others != 200 || thisKey != 100 {
		t.Fatalf("SumPendingSizes = %d, %d, %v; want 200 (b only), 100", others, thisKey, err)
	}

	mustExec(t, db, `UPDATE meko_system.pending_uploads SET expires_at = NOW() - INTERVAL '1 second'
	                  WHERE s3_key = 'own/dp/h1/a.pdf'`)
	if _, thisKey, err := repo.SumPendingSizes(itOwner, "own/dp/h1/a.pdf"); err != nil || thisKey != 0 {
		t.Fatalf("thisKey past the URL's deadline = %d, %v; want 0", thisKey, err)
	}
}
