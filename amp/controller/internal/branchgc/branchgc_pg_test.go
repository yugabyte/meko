package branchgc

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yugabyte/amp/controller/internal/config"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/db"
	"github.com/yugabyte/amp/controller/internal/registry"
)

// The reaper is nothing but SQL over the real amp_storage tables — cursored
// key-only SELECTs, deletes by ybctid, a partial-index due-scan — so there is no
// in-memory backend that could exercise it. Every test here therefore needs a
// YugabyteDB (the DDL is YB-specific: HASH keys, SPLIT AT VALUES) and skips
// without one, the same way the core/registry store tests do. CI's "Migrations &
// live-DB store tests" job supplies it.

// scratchURL is the throwaway database this package's tests share, provisioned
// once by TestMain. Empty when AMP_TEST_DATABASE_URL is unset, which is what makes
// the tests skip.
var scratchURL string

// migrateSchema is the --db-schema default: where the migrator puts the
// controller's own tables. The resource model (amp_registry) and the data plane
// (amp_storage) are fixed schemas, addressed schema-qualified.
const migrateSchema = "controller"

func TestMain(m *testing.M) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	u, drop, err := db.ScratchMigrated(ctx, url, "amp_test_branchgc", migrateSchema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "provision scratch database: %v\n", err)
		os.Exit(1)
	}
	scratchURL = u
	code := m.Run()
	drop()
	os.Exit(code)
}

func migratedPool(t *testing.T) *db.Pool {
	t.Helper()
	if scratchURL == "" {
		t.Skip("AMP_TEST_DATABASE_URL not set; the reaper's tests need a YugabyteDB")
	}
	pool, err := db.Connect(t.Context(), scratchURL, migrateSchema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// testCfg is a reaper config with the pacing knobs opened up, so a test that is
// not about pacing reclaims a seeded branch in one tick.
func testCfg() config.BranchGCConfig {
	return config.BranchGCConfig{
		Enabled:       true,
		Interval:      time.Minute,
		Retention:     0,
		RowsPerDelete: 1000,
		RowsPerTick:   100000,
	}
}

func newReaper(t *testing.T, pool *db.Pool, cfg config.BranchGCConfig) *Reaper {
	t.Helper()
	return New(pool, cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
}

// --- fixtures ---------------------------------------------------------------

// seedCluster creates a user, a storage pool and a cluster to hang branches off.
func seedCluster(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	owner, poolID, clusterID := uuid.New(), uuid.New(), uuid.New()
	name := "branchgc-test-" + poolID.String()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO amp_registry.users (id) VALUES ($1::uuid)`, []any{owner.String()}},
		{`INSERT INTO amp_registry.storage_pools (id, name, owner_user_id) VALUES ($1::uuid, $2, $3::uuid)`,
			[]any{poolID.String(), name, owner.String()}},
		{`INSERT INTO amp_registry.clusters (id, storage_pool_id, name) VALUES ($1::uuid, $2::uuid, $3)`,
			[]any{clusterID.String(), poolID.String(), name}},
	} {
		if _, err := pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed %q: %v", stmt.sql, err)
		}
	}
	return clusterID
}

// seedBranch records a live branch through the production writer and returns its
// uuid every statement below binds. parent may be
// uuid.Nil for a root.
//
// registry.RecordBranch rather than a hand-written INSERT: it is the single writer
// of a branch's creation record AND its `amp_storage.branch_state` seed, so the
// fixture is the row shape the product actually produces — including the fork
// validations, which a `parent` here goes through for real.
func seedBranch(t *testing.T, pool *db.Pool, clusterID, parent uuid.UUID, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	rec := registry.BranchCreateRecord{BranchName: name, PgVersion: 18, StartLSN: "0/0"}
	if parent != uuid.Nil {
		rec.AncestorBranchID = &parent
	}
	if err := registry.NewStore(pool).RecordBranch(t.Context(), clusterID, id, rec); err != nil {
		t.Fatalf("record branch %s: %v", name, err)
	}
	return id
}

// storageTables is every amp_storage table a branch has rows in — the reaper's
// delete plan, restated independently so the test fails if the plan loses one.
var storageTables = []string{
	"branch_state", "pages", "wals", "pending_gc", "blobs", "slru_pages", "meta",
}

// seedStorage writes n rows per branch-keyed amp_storage table for the branch, so a
// reap has something to reclaim in every one of them. `branch_state` is not seeded
// here — seedBranch's RecordBranch already created it, which is where it comes from
// in production too.
//
// Every bucketed table gets rows in EVERY one of its buckets, which is what makes a
// bucket-count regression in the reaper's fan-out fail loudly instead of silently
// leaving rows behind (see the bucket constants' comment): `pages` steps `blknum` by
// 256 so the writer's formula walks all 6 buckets, `wals` cycles 6, `pending_gc`
// cycles 3 — so seed at least 6 rows to cover them all. `pages.bucket_id` is
// computed by the same expression the table's CHECK constraint carries.
//
// `slru_pages` is spread over all three SlruKinds and several segnos on purpose:
// it is fanned out over `kind`, so only rows in every kind prove the fan-out
// covers all of them. Seeded with a single kind, a count that read low would
// still pass — the same reason the bucketed tables are seeded in every bucket.
func seedStorage(t *testing.T, pool *db.Pool, branchID uuid.UUID, n int) {
	t.Helper()
	ctx := t.Context()
	for _, stmt := range []string{
		`INSERT INTO amp_storage.pages
		   (bucket_id, branch_id, spcnode, dbnode, relnode, forknum, blknum, is_image, lsn, will_init, payload)
		 SELECT ((0 / 7) + (0 / 13) + ((g * 256) / 256)) % 6, $1, 1663, 5, 0, 0, g * 256, true, 1000 + g, true, '\\x00'::bytea
		   FROM generate_series(0, $2::int - 1) g`,
		`INSERT INTO amp_storage.wals (bucket_id, branch_id, segno, chunk_offset, chunk_size, data, last_modified_us)
		 SELECT g % 6, $1, 1, g * 1024, 1024, '\\x00'::bytea, 0 FROM generate_series(0, $2::int - 1) g`,
		`INSERT INTO amp_storage.pending_gc (branch_id, bucket_id, due_lsn, key, gc_kind)
		 SELECT $1, g % 3, 1000 + g, decode(lpad(to_hex(g), 36, '0'), 'hex'), 0
		   FROM generate_series(0, $2::int - 1) g`,
		`INSERT INTO amp_storage.blobs
		   (branch_id, namespace, key, chunk_idx, data, size_bytes, etag, last_modified_us)
		 SELECT $1, 'pageserver'::amp_storage.blob_namespace, 'blob-' || g, 0, '\\x00'::bytea, 1, 'etag', 0
		   FROM generate_series(0, $2::int - 1) g`,
		`INSERT INTO amp_storage.slru_pages (branch_id, kind, segno, blknum, is_image, lsn, will_init, payload)
		 SELECT $1, g % 3, g / 4, g, true, 1000 + g, true, '\\x00'::bytea
		   FROM generate_series(0, $2::int - 1) g`,
		`INSERT INTO amp_storage.meta (branch_id, key, lsn, payload)
		 SELECT $1, decode(lpad(to_hex(g), 36, '0'), 'hex'), 1000 + g, '\\x00'::bytea
		   FROM generate_series(0, $2::int - 1) g`,
	} {
		if _, err := pool.Exec(ctx, stmt, branchID, n); err != nil {
			t.Fatalf("seed storage rows (%.60s...): %v", stmt, err)
		}
	}
}

// tombstone marks the branch dropped with a reap deadline `in` from now (negative
// = already due). It writes the columns core.PgBackend.DeleteBranch writes; the
// stamping itself is covered in internal/registry.
func tombstone(t *testing.T, pool *db.Pool, branchID uuid.UUID, in time.Duration) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`UPDATE amp_registry.branches
		    SET dropped_at = now(), reap_after = now() + make_interval(secs => $2::double precision)
		  WHERE branch_id = $1::uuid`,
		branchID.String(), in.Seconds()); err != nil {
		t.Fatalf("tombstone branch: %v", err)
	}
}

// leaveTheDueSet marks the branch reclaimed at the end of the test. The package
// shares one scratch database, so a branch a test leaves tombstoned-and-unreaped
// would otherwise linger in every later test's due-scan, spending their row
// budget. A test whose branch is actually reaped needs no cleanup.
func leaveTheDueSet(t *testing.T, pool *db.Pool, branchID uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`UPDATE amp_registry.branches SET gc_completed_at = now() WHERE branch_id = $1::uuid`,
			branchID.String()); err != nil {
			t.Logf("could not retire the test's branch from the due set: %v", err)
		}
	})
}

// storageRows counts the branch's rows in one amp_storage table.
func storageRows(t *testing.T, pool *db.Pool, table string, branchID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(),
		fmt.Sprintf(`SELECT count(*) FROM amp_storage.%s WHERE branch_id = $1`, table),
		branchID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// totalStorageRows counts the branch's rows across every table in the plan.
func totalStorageRows(t *testing.T, pool *db.Pool, branchID uuid.UUID) int {
	t.Helper()
	total := 0
	for _, table := range storageTables {
		total += storageRows(t, pool, table, branchID)
	}
	return total
}

func gcCompleted(t *testing.T, pool *db.Pool, branchID uuid.UUID) bool {
	t.Helper()
	var done bool
	if err := pool.QueryRow(t.Context(),
		`SELECT gc_completed_at IS NOT NULL FROM amp_registry.branches WHERE branch_id = $1::uuid`,
		branchID.String()).Scan(&done); err != nil {
		t.Fatalf("read gc_completed_at: %v", err)
	}
	return done
}

// --- tests ------------------------------------------------------------------

// TestPgReapDeletesDueBranchOnly is the core contract: a due branch loses its rows
// in every amp_storage table and is marked reclaimed, while a live branch's rows in
// those same (shared) tables are untouched. The two branches are seeded side by
// side deliberately — every delete is bucket- or hash-scoped rather than
// branch-filtered at the tablet level, so "deleted the right branch's rows" is the
// property most worth pinning.
func TestPgReapDeletesDueBranchOnly(t *testing.T) {
	pool := migratedPool(t)
	clusterID := seedCluster(t, pool)

	doomed := seedBranch(t, pool, clusterID, uuid.Nil, "doomed")
	survivor := seedBranch(t, pool, clusterID, uuid.Nil, "survivor")
	const rows = 40
	seedStorage(t, pool, doomed, rows)
	seedStorage(t, pool, survivor, rows)
	before := totalStorageRows(t, pool, survivor)
	tombstone(t, pool, doomed, -time.Minute)

	stats, err := newReaper(t, pool, testCfg()).tick(t.Context())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if stats.BranchesReclaimed < 1 {
		t.Errorf("tick stats = %+v, want a completed branch and nothing skipped", stats)
	}
	if stats.BudgetSpent {
		t.Errorf("tick reported the row budget spent on %d rows, want the branch finished in one tick", stats.RowsDeleted)
	}

	for _, table := range storageTables {
		if n := storageRows(t, pool, table, doomed); n != 0 {
			t.Errorf("amp_storage.%s still holds %d rows of the reaped branch, want 0", table, n)
		}
	}
	if !gcCompleted(t, pool, doomed) {
		t.Error("gc_completed_at is unset on the reaped branch")
	}
	if after := totalStorageRows(t, pool, survivor); after != before {
		t.Errorf("the live branch lost rows: %d -> %d", before, after)
	}
	if gcCompleted(t, pool, survivor) {
		t.Error("gc_completed_at got stamped on the live branch")
	}

	// The branch row itself survives: a branch id is never reused, so "reclaimed"
	// has to stay distinguishable from "never existed".
	var kept bool
	if err := pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM amp_registry.branches WHERE branch_id = $1::uuid)`,
		doomed.String()).Scan(&kept); err != nil {
		t.Fatalf("check the branch row survived: %v", err)
	}
	if !kept {
		t.Error("the reaped branch's registry row was deleted, want it kept and marked")
	}
}

// TestPgReapHoldsBack covers every reason a due-looking branch must keep its rows.
// One body, because the assertion set is the same in all three: nothing deleted,
// nothing marked. What differs is only why the reaper must leave it alone.
//
//   - the retention window has not elapsed — the window is what makes the drop
//     reversible in principle, so nothing may touch the data before it does;
//   - a LIVE CHILD FORK reads its pre-fork history under this branch's id, so
//     reclaiming here would delete data out from under a live branch (the backstop
//     for the fork ↔ drop race);
//   - an ENDPOINT record means a compute may still be serving the branch.
//
// The last two are SKIPPED rather than failed: a retry cannot fix them, and the
// only unrecoverable move is to delete.
func TestPgReapHoldsBack(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deadline time.Duration // relative to now; negative = already due
		refer    func(t *testing.T, pool *db.Pool, clusterID, branchID uuid.UUID)
	}{
		{name: "retention has not elapsed", deadline: time.Hour},
		{
			name: "a live child fork", deadline: -time.Minute,
			refer: func(t *testing.T, pool *db.Pool, clusterID, branchID uuid.UUID) {
				seedBranch(t, pool, clusterID, branchID, "held-back-child")
			},
		},
		{
			name: "an endpoint on the branch", deadline: -time.Minute,
			refer: func(t *testing.T, pool *db.Pool, clusterID, branchID uuid.UUID) {
				if _, err := pool.Exec(t.Context(),
					`INSERT INTO amp_registry.endpoints (id, branch_id, role)
					 VALUES ($1::uuid, $2::uuid, 'read_write')`,
					uuid.New().String(), branchID.String()); err != nil {
					t.Fatalf("seed endpoint: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			clusterID := seedCluster(t, pool)
			branch := seedBranch(t, pool, clusterID, uuid.Nil, "held-back")
			seedStorage(t, pool, branch, 10)
			if tc.refer != nil {
				tc.refer(t, pool, clusterID, branch)
			}
			before := totalStorageRows(t, pool, branch)
			tombstone(t, pool, branch, tc.deadline)
			// Held back or not yet due, this branch stays tombstoned-and-unreaped, so
			// retire it from the due set for the tests that follow.
			leaveTheDueSet(t, pool, branch)

			stats, err := newReaper(t, pool, testCfg()).tick(t.Context())
			if err != nil {
				t.Fatalf("tick: %v", err)
			}
			if stats.RowsDeleted != 0 || stats.BranchesReclaimed != 0 {
				t.Errorf("tick stats = %+v, want nothing deleted and nothing completed", stats)
			}
			if after := totalStorageRows(t, pool, branch); after != before {
				t.Errorf("the branch lost rows: %d -> %d", before, after)
			}
			if gcCompleted(t, pool, branch) {
				t.Error("gc_completed_at is set on a branch that was held back")
			}
		})
	}
}

// TestPgReapChunksAcrossTicks: the pacing contract. With a row budget far smaller
// than the branch, one tick deletes exactly the budget and reports itself unfinished
// — no completion marker — and repeated ticks converge without a persisted cursor,
// because the rows a chunk deletes stop matching the next chunk's inner SELECT.
// TestPgReapBlockedBranchesDoNotStarveOthers pins that a held-back branch cannot
// monopolise the due-scan's window. Such a branch keeps `dropped_at` set,
// `gc_completed_at` NULL and `reap_after` in the past, so it is permanently due
// AND permanently among the oldest — if the scan returned it, maxBranchesPerTick
// of them would fill every window and no other branch would ever be selected
// again for the life of the process.
//
// The blockers are seeded OLDER than the reclaimable branch, so they sort ahead
// of it and it is reached only because the scan refuses to return them at all.
func TestPgReapBlockedBranchesDoNotStarveOthers(t *testing.T) {
	pool := migratedPool(t)
	clusterID := seedCluster(t, pool)

	// Any number > 0 reproduces the stall if the guards are not in the predicate.
	const blockers = 5

	for i := range blockers {
		blocked := seedBranch(t, pool, clusterID, uuid.Nil, fmt.Sprintf("blocker-%d", i))
		// An endpoint on the branch is one of the two guards; the drop path refuses
		// it, so reaching this state at all is a bug the reaper must not act on.
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO amp_registry.endpoints (id, branch_id, role)
			 VALUES ($1::uuid, $2::uuid, 'read_write')`,
			uuid.New().String(), blocked.String()); err != nil {
			t.Fatalf("seed endpoint: %v", err)
		}
		tombstone(t, pool, blocked, -time.Hour)
		leaveTheDueSet(t, pool, blocked)
	}

	reapable := seedBranch(t, pool, clusterID, uuid.Nil, "starved")
	seedStorage(t, pool, reapable, 10)
	tombstone(t, pool, reapable, -time.Minute)
	leaveTheDueSet(t, pool, reapable)

	// ONE tick: the blockers are filtered out by the due-scan, so the reclaimable
	// branch is selected despite sorting behind all of them.
	stats, err := newReaper(t, pool, testCfg()).tick(t.Context())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if stats.BranchesReclaimed != 1 {
		t.Errorf("tick stats = %+v, want the reclaimable branch completed", stats)
	}
	if !gcCompleted(t, pool, reapable) {
		t.Error("the reclaimable branch was starved by the blocked ones")
	}
	if rows := totalStorageRows(t, pool, reapable); rows != 0 {
		t.Errorf("the reclaimable branch kept %d rows", rows)
	}
}

func TestPgReapChunksAcrossTicks(t *testing.T) {
	pool := migratedPool(t)
	clusterID := seedCluster(t, pool)
	branch := seedBranch(t, pool, clusterID, uuid.Nil, "chunked")
	const perTable = 12
	seedStorage(t, pool, branch, perTable)
	total := totalStorageRows(t, pool, branch)
	tombstone(t, pool, branch, -time.Minute)

	cfg := testCfg()
	cfg.RowsPerDelete = 3
	cfg.RowsPerTick = 5
	r := newReaper(t, pool, cfg)

	first, err := r.tick(t.Context())
	if err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if first.RowsDeleted != cfg.RowsPerTick {
		t.Errorf("first tick deleted %d rows, want exactly the budget (%d)", first.RowsDeleted, cfg.RowsPerTick)
	}
	if !first.BudgetSpent || first.BranchesReclaimed != 0 {
		t.Errorf("first tick stats = %+v, want the budget spent and nothing completed", first)
	}
	if gcCompleted(t, pool, branch) {
		t.Error("gc_completed_at is set while rows remain")
	}

	// Converge. The bound is generous but finite: a reap that fails to make progress
	// (a chunk that deletes nothing while rows remain) would spin here forever, so
	// the loop failing is itself the assertion.
	deleted := first.RowsDeleted
	ticks := 1
	for maxTicks := 2 + total/cfg.RowsPerTick; !gcCompleted(t, pool, branch); {
		stats, err := r.tick(t.Context())
		if err != nil {
			t.Fatalf("tick %d: %v", ticks+1, err)
		}
		ticks++
		deleted += stats.RowsDeleted
		if ticks > maxTicks {
			t.Fatalf("still not reclaimed after %d ticks (%d of %d rows deleted); the chunk loop is not advancing",
				ticks, deleted, total)
		}
	}
	if deleted != total {
		t.Errorf("deleted %d rows across %d ticks, want the seeded %d", deleted, ticks, total)
	}
	if left := totalStorageRows(t, pool, branch); left != 0 {
		t.Errorf("%d rows left after the branch was marked reclaimed", left)
	}
}

// TestPgReapIsIdempotent: a second tick over an already-reclaimed branch finds
// nothing to do. This is what makes running a reaper on every controller replica
// safe without leader election or an in-progress marker.
func TestPgReapIsIdempotent(t *testing.T) {
	pool := migratedPool(t)
	clusterID := seedCluster(t, pool)
	branch := seedBranch(t, pool, clusterID, uuid.Nil, "twice")
	seedStorage(t, pool, branch, 10)
	tombstone(t, pool, branch, -time.Minute)

	r := newReaper(t, pool, testCfg())
	if _, err := r.tick(t.Context()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	var stamped time.Time
	if err := pool.QueryRow(t.Context(),
		`SELECT gc_completed_at FROM amp_registry.branches WHERE branch_id = $1::uuid`,
		branch.String()).Scan(&stamped); err != nil {
		t.Fatalf("read gc_completed_at: %v", err)
	}

	second, err := r.tick(t.Context())
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if second.RowsDeleted != 0 {
		t.Errorf("second tick stats = %+v, want no rows deleted", second)
	}
	var again time.Time
	if err := pool.QueryRow(t.Context(),
		`SELECT gc_completed_at FROM amp_registry.branches WHERE branch_id = $1::uuid`,
		branch.String()).Scan(&again); err != nil {
		t.Fatalf("re-read gc_completed_at: %v", err)
	}
	if !again.Equal(stamped) {
		t.Errorf("gc_completed_at moved on a re-reap: %s -> %s", stamped, again)
	}
}

// pkColumn is one primary-key column as DocDB actually orders it.
type pkColumn struct {
	name string
	desc bool
}

// primaryKey reads a table's real primary key: its columns in key order, each
// one's direction, and how many of the leading ones are HASH (0 = an all-range
// key). This is the catalog's answer, not the migration's text, so it stays true
// through anything that reshapes the key.
func primaryKey(t *testing.T, pool *db.Pool, table string) ([]pkColumn, int) {
	t.Helper()
	qualified := "amp_storage." + table
	rows, err := pool.Query(t.Context(), `
		SELECT a.attname, (i.indoption[k.ord - 1] & 1) = 1
		  FROM pg_index i
		  JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
		  JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		 WHERE i.indrelid = $1::regclass AND i.indisprimary
		 ORDER BY k.ord`, qualified)
	if err != nil {
		t.Fatalf("read %s's primary key: %v", qualified, err)
	}
	defer rows.Close()
	var cols []pkColumn
	for rows.Next() {
		var c pkColumn
		if err := rows.Scan(&c.name, &c.desc); err != nil {
			t.Fatalf("scan %s's primary key: %v", qualified, err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s's primary key: %v", qualified, err)
	}
	if len(cols) == 0 {
		t.Fatalf("%s has no primary key", qualified)
	}
	var hash int
	if err := pool.QueryRow(t.Context(),
		`SELECT num_hash_key_columns FROM yb_table_properties($1::regclass)`,
		qualified).Scan(&hash); err != nil {
		t.Fatalf("read %s's hash column count: %v", qualified, err)
	}
	return cols, hash
}

// TestSpecsMatchPrimaryKeys is the drift guard on `specs`, which transcribes six
// tables' key order out of migration 0003.
//
// Why this needs a test at all, when a wrong ORDER BY changes no result: the
// cursor is only correct while a batch is the LOWEST remaining rows, and that
// holds only while the ORDER BY is exactly the key below whatever equality pins.
// Get it wrong and the SQL stays valid AND the rows stay correct — the ORDER BY
// is explicit, so YSQL simply stops seeking and sorts instead. Nothing fails,
// nothing logs; every batch quietly becomes a full scan of the branch's range,
// the 33ms -> 1015ms cliff the cursor exists to avoid. The only symptom is a reap
// that never finishes, noticed weeks later as "storage isn't shrinking".
//
// Primary keys here move: 5e454fdae re-keyed blobs/slru_pages/meta one commit
// before this package landed.
func TestSpecsMatchPrimaryKeys(t *testing.T) {
	pool := migratedPool(t)
	for _, spec := range specs {
		t.Run(spec.table, func(t *testing.T) {
			pk, hash := primaryKey(t, pool, spec.table)

			// A row constructor can only anchor on an all-range key; on a hashed
			// leading column it silently degrades to a filter.
			if spec.rowConstructor && hash != 0 {
				t.Fatalf("rowConstructor is set, but %s has %d HASH key column(s); "+
					"an anchored constructor only seeks on an all-range key",
					spec.table, hash)
			}

			// Everything the SELECT pins by equality, and so leaves out of the
			// ORDER BY: the hash columns, plus the fanned-out one when it sits
			// below them. A row constructor pins nothing — branch_id rides inside
			// the cursor like any other column.
			pinned := hash
			if spec.rowConstructor {
				pinned = 0
			} else if spec.fanOut.count > 0 {
				at := -1
				for i, c := range pk {
					if c.name == spec.fanOut.column {
						at = i
						break
					}
				}
				switch {
				case at < 0:
					t.Fatalf("fans out over %q, which is not in %s's primary key",
						spec.fanOut.column, spec.table)
				case at < hash: // inside the HASH key: already pinned
				case at == hash:
					pinned++
				default:
					t.Fatalf("fans out over %q at key position %d, but the first "+
						"unpinned position is %d; a fanned-out column must be "+
						"directly below the equality prefix or nothing seeks",
						spec.fanOut.column, at, hash)
				}
			}

			if len(spec.cursorCols) == 0 {
				return // no cursor, so no ordering to keep honest
			}

			// The cursor compares with `>=`, which is only a "everything at or
			// after this point" while its columns sort ASCENDING. A DESC cursor
			// column would skip rows silently.
			for _, name := range spec.cursorCols {
				for _, c := range pk {
					if c.name == name && c.desc {
						t.Errorf("cursor column %q is DESC in %s's key; the cursor's "+
							">= assumes ascending order and would skip rows",
							name, spec.table)
					}
				}
			}

			var want []string
			for _, c := range pk[pinned:] {
				if c.desc {
					want = append(want, c.name+" DESC")
					continue
				}
				want = append(want, c.name)
			}
			if got := spec.orderBy(); got != strings.Join(want, ", ") {
				t.Errorf("ORDER BY is %q, but %s's key below the equality prefix is %q.\n"+
					"cursorCols+orderTail must be exactly that, in order, or batches "+
					"stop seeking and every one becomes a full scan plus a sort.",
					got, spec.table, strings.Join(want, ", "))
			}
		})
	}
}

// TestPgStampsReapDeadlines: the reaper, not the drop path, decides when a
// tombstone becomes due. A drop writes `dropped_at` and nothing else; the next
// tick fills `reap_after` from it.
//
// Retargeted from registry's TestPgTombstoneStampsTheReapDeadline when ownership
// moved. The invariants it guarded all survive the move and are asserted here:
// both tombstone writers (a branch delete, and the cluster cascade that
// tombstones every branch under it) produce a stampable tombstone, the window is
// measured from `dropped_at`, and a replayed cascade does not slide the deadline
// forward.
//
// The last case is the one the move makes newly testable: computing from the
// STORED `dropped_at` rather than now() is what keeps the window a snapshot, so
// a retention change must not move a deadline already stamped.
func TestPgStampsReapDeadlines(t *testing.T) {
	ctx := t.Context()
	pool := migratedPool(t)
	backend := core.NewPgBackend(pool)
	const retention = 90 * time.Minute

	// window reads back (reap_after - dropped_at): the retention the row actually
	// recorded. NULL until a tick stamps it, which is the point.
	window := func(branchID uuid.UUID) (time.Duration, bool) {
		t.Helper()
		var secs *float64
		if err := pool.QueryRow(ctx,
			`SELECT extract(epoch FROM reap_after - dropped_at)
			   FROM amp_registry.branches WHERE branch_id = $1::uuid`,
			branchID.String()).Scan(&secs); err != nil {
			t.Fatalf("read the reap deadline: %v", err)
		}
		if secs == nil {
			return 0, false
		}
		return time.Duration(*secs * float64(time.Second)), true
	}

	cfg := testCfg()
	cfg.Retention = retention
	reaper := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	clusterID := seedCluster(t, pool)
	branchID := seedBranch(t, pool, clusterID, uuid.Nil, "reap-deadline")
	if err := backend.DeleteBranch(ctx, clusterID, branchID); err != nil {
		t.Fatalf("delete branch: %v", err)
	}
	// The drop leaves it unstamped: nothing but the reaper writes reap_after.
	if _, ok := window(branchID); ok {
		t.Error("the drop path stamped reap_after; only the reaper should")
	}
	if err := reaper.stampDeadlines(ctx); err != nil {
		t.Fatalf("stamp deadlines: %v", err)
	}
	if got, ok := window(branchID); !ok || got != retention {
		t.Errorf("branch delete: reap_after = dropped_at + %s (stamped=%v), want + %s",
			got, ok, retention)
	}

	// The cluster cascade tombstones its branches the same way, so a cluster
	// delete needs no reap path of its own.
	cascadeID := seedBranch(t, pool, clusterID, uuid.Nil, "reap-deadline-cascade")
	if err := backend.DeleteCluster(ctx, clusterID); err != nil {
		t.Fatalf("delete cluster: %v", err)
	}
	if err := reaper.stampDeadlines(ctx); err != nil {
		t.Fatalf("stamp deadlines after cascade: %v", err)
	}
	if got, ok := window(cascadeID); !ok || got != retention {
		t.Errorf("cluster cascade: reap_after = dropped_at + %s (stamped=%v), want + %s",
			got, ok, retention)
	}

	// A replayed cascade must not move a deadline the first one set, or the
	// reaper's window slides forward on every task replay.
	if err := backend.DeleteCluster(ctx, clusterID); err != nil {
		t.Fatalf("replay delete cluster: %v", err)
	}
	if err := reaper.stampDeadlines(ctx); err != nil {
		t.Fatalf("stamp deadlines after replay: %v", err)
	}
	if got, _ := window(cascadeID); got != retention {
		t.Errorf("a replayed cascade moved the deadline to dropped_at + %s, want + %s",
			got, retention)
	}

	// Snapshot: a stamped deadline is immune to a later retention change. Only
	// branches still unstamped pick the new window up.
	changed := testCfg()
	changed.Retention = 5 * time.Hour
	if err := New(pool, changed, slog.New(slog.NewTextHandler(io.Discard, nil))).
		stampDeadlines(ctx); err != nil {
		t.Fatalf("stamp deadlines with a changed retention: %v", err)
	}
	if got, _ := window(branchID); got != retention {
		t.Errorf("a retention change moved an already-stamped deadline to + %s, want + %s",
			got, retention)
	}
}

// TestPgDeletePlansTidScan: a full batch's DELETE is a Tid Scan on every table, on
// a database nothing has ANALYZEd — the state every universe starts in (#970).
func TestPgDeletePlansTidScan(t *testing.T) {
	pool := migratedPool(t)
	ids := make([][]byte, config.DefaultBranchGCRowsPerDelete)
	for i := range ids {
		id := uuid.New()
		ids[i] = id[:]
	}
	for _, spec := range specs {
		rows, _ := pool.Query(t.Context(), "EXPLAIN "+spec.deleteSQL(), ids)
		plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("explain %s: %v", spec.table, err)
		}
		if p := strings.Join(plan, "\n"); !strings.Contains(p, "Tid Scan") {
			t.Errorf("%s: DELETE of %d ybctids is not a Tid Scan:\n%s", spec.table, len(ids), p)
		}
	}
}
