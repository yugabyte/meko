// Package branchgc reclaims a dropped branch's data: the rows left behind in
// `amp_storage` once `amp_registry.branches.dropped_at` is stamped and its
// retention window has passed.
//
//	mark dropped  ->  wait out retention  ->  delete the rows  ->  mark gc_completed_at
//
// The first step is core.PgBackend.DeleteBranch; the rest is the Reaper below.
// The controller owns this rather than the pageserver because a dropped branch's
// pageserver is terminated with it, so `pending_gc` is one of the tables this
// deletes rather than a queue it uses.
//
// A batch is a key-only SELECT yielding `ybctid`s, then one
// `DELETE ... WHERE ybctid = ANY(...)`, resuming from a cursor so it never
// re-walks the tombstones it just wrote. Which cursor shape actually seeks
// depends on the table's key — see tableSpec.
//
// Every replica may run a Reaper: the deletes are idempotent per branch and
// `gc_completed_at` is a conditional UPDATE. What makes that safe is that
// termination is a READ — the SELECT coming back empty — never an inference from
// a DELETE's row count, which a concurrent reaper could make wrong.
//
// Rationale, measurements and the per-table cursor shapes:
// controller/architecture/sc-integration.md § Branch-drop GC.
package branchgc

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/config"
	"github.com/yugabyte/amp/controller/internal/db"
	applog "github.com/yugabyte/amp/controller/internal/log"
)

// gcBuckets and slruKinds are Go copies of Rust constants — GC_BUCKETS
// (pageserver/src/page_store.rs) and SlruKind's cardinality (pageserver_api).
// The reaper fans those tables out over the column, so a count that reads LOW
// silently strands the rows above it: drained, reported finished, never
// revisited. rust_constants_test.go fails CI when either copy stops matching.
const gcBuckets = 3

const slruKinds = 3

// maxBranchesPerTick caps the due-scan's result set. It is NOT what paces a tick
// — the row budget is, and it almost always binds first: one large branch spends
// the whole budget, so a tick that fetched this many branches routinely reaps
// one. Sized so it never becomes the binding constraint.
//
// Deliberately not a flag. A branch cap is a second ceiling in a different unit
// from the one the cluster feels, and it can only bind when branches are small
// enough that several fit inside one tick's budget — exactly when it would be
// throttling the cheap case.
const maxBranchesPerTick = 100

// tableSpec is one amp_storage table's whole-branch delete.
//
// DocDB seeks only when the predicate lets it compute one start key, so a
// table's primary key decides which cursor shape it can use. A predicate SEEKS
// if it is either:
//
//	(a) a row constructor starting at PK column 1, on an all-range key:
//	    (bucket_id, branch_id, spcnode, ...) >= (...)
//	(b) equality on every leading key column — hash or range — then ONE range
//	    comparison on the next: branch_id = ... AND kind = ... AND segno >= ...
//
// Anything else FILTERS and reads the branch's whole range: a constructor
// starting mid-key, a constructor on a hash-keyed table, `ybctid > ...`, an
// OR-chain. Measured on YB 2.31.
//
// `cursorCols` stops ABOVE each table's `lsn DESC` tail, since row comparisons
// are lexicographic-ASCENDING. Hence `>=` and not `>` on the last one: a batch's
// LIMIT can cut mid-block, and `>=` re-includes the boundary group so its
// surviving versions are not skipped. It cannot spin — progress comes from the
// delete, not from the cursor advancing. Used uniformly, even where the columns
// cover a whole key and `>` would be exact.
type tableSpec struct {
	// table is the unqualified table name in amp_storage.
	table string
	// rowConstructor picks shape (a) over shape (b) — only an all-range key may
	// set it. Anchoring at PK column 1 is what makes it a computable start key;
	// over a middle slice of the key it degrades to a filter. Being a true
	// lexicographic "everything at or after this point", it crosses bucket and
	// relation boundaries by itself, so such a table needs no fan-out.
	//
	// Left false, the cursor is shape (b): every column pinned by equality but
	// the last, which is range-compared. The only shape that seeks on a
	// hash-keyed table, and its equality confines a batch to one group — which
	// is why a table whose cursor is not its first range column fans out instead.
	rowConstructor bool
	// cursorCols names the columns the batch resumes on, in key order; for
	// rowConstructor that is the anchored leading prefix of the primary key,
	// `branch_id` included — projected and fed back like any other one rather
	// than special-cased.
	//
	// EMPTY means no cursor: the same unbounded SELECT repeats, and the deleted
	// rows are what stop it matching. Only for a table small enough that the
	// tombstones it walks past never add up — `branch_state`'s single row is the
	// extreme of that.
	cursorCols []string
	// orderTail is the rest of the key, below cursorCols: whatever the ORDER BY
	// still needs once the cursor columns are named. orderBy() is the two
	// concatenated, and deriving it is what stops them drifting — the cursor is
	// only correct while a batch is the LOWEST remaining rows, which holds only
	// while the ORDER BY starts with exactly cursorCols, in order.
	orderTail string
	// fanOut splits the table into one step per value of a leading key column,
	// bound by equality — the only way to cover a column that cannot be
	// range-walked. Why each of the two tables needs it is on its entry in specs;
	// both counts mirror a Rust constant (gcBuckets, slruKinds).
	fanOut fanOutSpec
}

// fanOutSpec is a leading key column and how many values it takes. The zero value
// means no fan-out: one step, no equality binding.
type fanOutSpec struct {
	column string
	count  int
}

// specs is the delete order, one entry per amp_storage table.
//
// `branch_state` is FIRST: it carries `ancestor_branch_id` / `ancestor_lsn`,
// which pin an ancestor's GC cutoff at this branch's fork point, so deleting it
// is what lets the parent's GC advance a retention window after the drop rather
// than never.
//
// Cursor columns must stay in each table's real primary-key order (migration
// 0003_compute_storage.up.sql) — that order is what the planner matches.
var specs = []tableSpec{
	// PK ((branch_id) HASH). Exactly one row, so one batch drains it.
	{table: "branch_state"},

	// PK (bucket_id, branch_id, spcnode, dbnode, relnode, forknum, blknum ASC,
	//     lsn DESC, is_image DESC) — all-range, so the cursor anchors at
	// bucket_id and one pass covers all six buckets.
	{table: "pages", rowConstructor: true,
		cursorCols: []string{"bucket_id", "branch_id", "spcnode", "dbnode", "relnode", "forknum", "blknum"},
		orderTail:  "lsn DESC, is_image DESC"},

	// PK (bucket_id, branch_id, segno, chunk_offset) — all-range, and the cursor
	// covers the whole key, so no version tail below it.
	{table: "wals", rowConstructor: true,
		cursorCols: []string{"bucket_id", "branch_id", "segno", "chunk_offset"}},

	// PK ((branch_id, bucket_id) HASH, due_lsn, key, gc_kind). bucket_id is inside
	// the hash key, so it can only be named, never walked. Rows sharing a due_lsn
	// are bounded by the emitter's 500-key flush cap — the residual a batch
	// boundary re-scans.
	{table: "pending_gc", fanOut: fanOutSpec{"bucket_id", gcBuckets},
		cursorCols: []string{"due_lsn"},
		orderTail:  "key, gc_kind"},

	// PK ((branch_id) HASH, kind, segno, blknum ASC, lsn DESC, is_image DESC).
	// kind is fanned out because the cursor range-compares segno, which needs kind
	// pinned above it. blknum stays out of the cursor: a (kind, segno) segment is
	// ~32 contiguous blocks, so the residual is tens of rows.
	{table: "slru_pages", fanOut: fanOutSpec{"kind", slruKinds},
		cursorCols: []string{"segno"},
		orderTail:  "blknum, lsn DESC, is_image DESC"},

	// PK ((branch_id) HASH, key ASC, lsn DESC).
	{table: "meta",
		cursorCols: []string{"key"},
		orderTail:  "lsn DESC"},

	// PK ((branch_id) HASH, namespace, key, chunk_idx). No cursor: an initdb
	// archive plus one row per endpoint (compute_ctl's LFC state — an upload
	// pre-deletes the key, so re-offloading replaces). It does grow with endpoints
	// ever created, since a retired endpoint's blob is never reclaimed, but a
	// cursor only pays once a table needs many batches.
	{table: "blobs"},
}

// step is one ready-to-run unit of the reap: a table, and the value it binds
// when the table fans out.
type step struct {
	spec tableSpec
	// bucket is the fanned-out value this step binds; meaningful only when
	// spec.fanOut.count > 0, which is also the test for whether it is bound at all.
	bucket int

	// SQL rendered once at init. initSelectSQL omits the cursor predicate
	// altogether, which is how a position starts: it sidesteps inventing a
	// minimum sentinel for whatever the cursor columns are typed as. It is safe
	// even on `pages` / `wals`, where `branch_id` is not the leading column —
	// YB's skip-scan seeks to the branch's offset inside each bucket on its own
	// (measured identical to supplying an explicit lower bound).
	initSelectSQL   string
	resumeSelectSQL string
	deleteSQL       string
}

// steps is specs expanded to one step per (table, fan-out value), in execution order.
var steps = buildSteps()

func buildSteps() []step {
	var p []step
	for _, spec := range specs {
		for bucket := range max(spec.fanOut.count, 1) {
			st := step{spec: spec, bucket: bucket}
			st.initSelectSQL = spec.selectSQL(false)
			st.resumeSelectSQL = spec.selectSQL(true)
			st.deleteSQL = fmt.Sprintf(
				"DELETE FROM amp_storage.%s WHERE ybctid = ANY($1::bytea[])", spec.table)
			p = append(p, st)
		}
	}
	return p
}

// orderBy is the full ORDER BY: the cursor columns, then whatever sorts below
// them. Derived rather than stored so it cannot disagree with `cursor`.
func (s tableSpec) orderBy() string {
	if len(s.cursorCols) == 0 {
		return "" // no cursor to keep honest, so ordering would only cost a sort
	}
	cols := strings.Join(s.cursorCols, ", ")
	if s.orderTail == "" {
		return cols
	}
	return cols + ", " + s.orderTail
}

// selectSQL renders a batch's key-only SELECT. Arguments run branch, the fan-out
// value (when the spec fans out), cursor values, and the row limit LAST — the
// order selectRowKeys appends them in.
func (s tableSpec) selectSQL(withCursor bool) string {
	proj := "ybctid"
	if len(s.cursorCols) > 0 {
		proj += ", " + strings.Join(s.cursorCols, ", ")
	}
	where, next := "branch_id = $1::uuid", 2
	if s.fanOut.count > 0 {
		where += fmt.Sprintf(" AND %s = $2", s.fanOut.column)
		next = 3
	}
	if withCursor && len(s.cursorCols) > 0 {
		where += " AND " + s.cursorPredicate(next)
		next += len(s.cursorCols)
	}
	q := fmt.Sprintf("SELECT %s FROM amp_storage.%s WHERE %s", proj, s.table, where)
	if o := s.orderBy(); o != "" {
		q += " ORDER BY " + o
	}
	return fmt.Sprintf("%s LIMIT $%d", q, next)
}

// cursorPredicate renders the cursor comparison, placeholders starting at
// `first`. The last column uses `>=` rather than `>` because the cursor stops
// above each table's DESC tail — see tableSpec.
func (s tableSpec) cursorPredicate(first int) string {
	cols, n := s.cursorCols, len(s.cursorCols)
	ph := make([]string, n)
	for i := range cols {
		ph[i] = fmt.Sprintf("$%d", first+i)
	}
	if s.rowConstructor {
		return fmt.Sprintf("(%s) >= (%s)", strings.Join(cols, ", "), strings.Join(ph, ", "))
	}
	parts := make([]string, n)
	for i, c := range cols {
		op := "="
		if i == n-1 {
			op = ">="
		}
		parts[i] = c + " " + op + " " + ph[i]
	}
	return strings.Join(parts, " AND ")
}

// Reaper is the periodic branch-drop reclamation loop. Construct with New and run
// with Run.
type Reaper struct {
	pool *db.Pool
	cfg  config.BranchGCConfig
	log  *slog.Logger
	// progress is where each branch's reap resumes, and it MUST outlive a tick: a
	// tick ends when its row budget is spent, and a step that has already
	// been drained is a pure tombstone range that costs a full scan to re-confirm
	// empty. Held in memory rather than persisted — losing it on a process restart
	// costs one re-scan of whatever tombstones compaction has not yet reclaimed
	// (bounded by `timestamp_history_retention_interval_sec`, since compacted
	// tombstones are gone from the SSTs rather than traversed), not a re-scan
	// proportional to everything deleted so far.
	//
	// An entry is dropped when its branch is reclaimed, which is the only way a
	// branch leaves the due set while one controller writes `gc_completed_at`.
	// Should the controller ever run more than one instance, a branch another
	// instance finishes would strand its entry here. Run is one goroutine, so no
	// locking.
	progress map[uuid.UUID]*resumePoint
}

// resumePoint is where one branch's reap continues: which step it is on, and the
// cursor within that step. A nil cursor means the step has not run a batch yet,
// which is what selects the cursor-free SELECT.
type resumePoint struct {
	stepIdx int
	cursor  []any
}

// New returns a Reaper over pool. The logger is scoped once here, so every record
// the reaper emits carries the same component attribute.
func New(pool *db.Pool, cfg config.BranchGCConfig, logger *slog.Logger) *Reaper {
	return &Reaper{
		pool:     pool,
		cfg:      cfg,
		log:      logger.With("component", "branch_gc"),
		progress: map[uuid.UUID]*resumePoint{},
	}
}

// Run ticks until ctx is cancelled. It returns immediately when the reaper is
// disabled, so the caller can start it unconditionally — the same contract as
// gc.Sweeper.Run, which keeps the controller's two periodic reclaimers wired
// alike.
//
// A tick that fails is logged and retried on the next one: every step is
// idempotent, so there is nothing to unwind and no reason to stop the loop (the
// usual failure is a database blip, which the next tick finds resolved).
func (r *Reaper) Run(ctx context.Context) {
	if !r.cfg.Enabled {
		r.log.Info("branch-drop reaper disabled (--branch-gc=false); dropped branches keep their data")
		return
	}
	r.log.Info("branch-drop reaper started",
		"interval", r.cfg.Interval, "retention", r.cfg.Retention,
		"rows_per_delete", r.cfg.RowsPerDelete, "rows_per_tick", r.cfg.RowsPerTick)
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.log.Info("branch-drop reaper stopped")
			return
		case <-ticker.C:
			if _, err := r.tick(ctx); err != nil {
				// A cancelled context is the shutdown path, not a failure.
				if ctx.Err() != nil {
					continue
				}
				r.log.Error("branch-drop reap tick failed", "error", err)
			}
		}
	}
}

// tickStats reports what one tick did. Every branch-level outcome is also logged
// as it happens; these counters are what the tests assert on.
type tickStats struct {
	// BranchesReclaimed is how many branches the tick finished and stamped
	// gc_completed_at on.
	BranchesReclaimed int
	// RowsDeleted counts every amp_storage row the tick removed, across all
	// branches and tables — the number the row budget bounds.
	RowsDeleted int
	// BudgetSpent reports that the tick stopped on its row budget rather than
	// because it ran out of work — the signal that reclamation is spread over
	// later ticks.
	BudgetSpent bool
}

// tick runs one reclamation pass: select the due branches and reap each one until
// the tick's row budget is spent.
func (r *Reaper) tick(ctx context.Context) (tickStats, error) {
	var stats tickStats
	if err := r.stampDeadlines(ctx); err != nil {
		return stats, err
	}
	due, err := r.dueBranches(ctx)
	if err != nil {
		return stats, err
	}
	budget := r.cfg.RowsPerTick
	for _, id := range due {
		if budget <= 0 {
			stats.BudgetSpent = true
			break
		}
		rows, done, err := r.reapBranch(ctx, id, budget)
		stats.RowsDeleted += rows
		budget -= rows
		if err != nil {
			return stats, fmt.Errorf("reap branch %s: %w", id, err)
		}
		if !done {
			stats.BudgetSpent = true
			r.log.Info("branch-drop reap paused on the tick's row budget; resuming next tick",
				applog.KeyBranchID, id.String(), "rows_deleted", rows)
			break
		}
		if err := r.markComplete(ctx, id); err != nil {
			return stats, err
		}
		delete(r.progress, id)
		stats.BranchesReclaimed++
		r.log.Info("branch data reclaimed",
			applog.KeyBranchID, id.String(), "rows_deleted", rows)
	}
	return stats, nil
}

// stampDeadlines gives every tombstone still missing a `reap_after` one. The drop
// path writes `dropped_at` alone, so a tombstone from ANY path is picked up here
// rather than stranded as never-due.
//
// From the stored `dropped_at`, never now(): the deadline must not depend on which
// tick noticed the row, or it stops being a snapshot under a later retention change.
//
// `reap_after IS NULL` is an Index Cond on the partial branches_gc_due, not a
// filter — 2.7ms against 10k tombstones on the pass that updates none.
func (r *Reaper) stampDeadlines(ctx context.Context) error {
	if _, err := r.pool.Exec(ctx, `
		UPDATE amp_registry.branches
		   SET reap_after = dropped_at + make_interval(secs => $1::double precision)
		 WHERE dropped_at IS NOT NULL AND gc_completed_at IS NULL
		   AND reap_after IS NULL`,
		r.cfg.Retention.Seconds()); err != nil {
		return fmt.Errorf("stamp reap deadlines: %w", err)
	}
	return nil
}

// dueBranches selects the branches whose retention window has passed, oldest
// deadline first so a backlog drains in drop order. Capped at maxBranchesPerTick:
// one tick is not meant to be a fleet-wide sweep.
//
// Neither NOT EXISTS can match (#536/#537 closed those races) — checked anyway
// because deleting is the only irreversible step here.
func (r *Reaper) dueBranches(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT branch_id::text
		   FROM amp_registry.branches b
		  WHERE dropped_at IS NOT NULL AND gc_completed_at IS NULL
		    AND reap_after IS NOT NULL AND now() >= reap_after
		    AND NOT EXISTS (SELECT 1 FROM amp_registry.branches c
		                     WHERE c.parent_branch_id = b.branch_id AND c.dropped_at IS NULL)
		    AND NOT EXISTS (SELECT 1 FROM amp_registry.endpoints e
		                     WHERE e.branch_id = b.branch_id)
		  ORDER BY reap_after
		  LIMIT $1`,
		maxBranchesPerTick)
	if err != nil {
		return nil, fmt.Errorf("select due branches: %w", err)
	}
	defer rows.Close()
	var due []uuid.UUID
	for rows.Next() {
		// Projected ::text and parsed, the convention the other stores here follow
		// to avoid a pgx uuid codec.
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan due branch: %w", err)
		}
		parsed, err := uuid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("parse due branch id %q: %w", id, err)
		}
		due = append(due, parsed)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read due branches: %w", err)
	}
	return due, nil
}

// reapBranch deletes up to budget rows of the branch's amp_storage data, resuming
// from wherever the last tick left this branch and walking the steps in order. It
// reports the rows deleted and whether the branch is finished — done is false only
// when the budget ran out with positions still to go.
func (r *Reaper) reapBranch(ctx context.Context, id uuid.UUID, budget int) (int, bool, error) {
	p := r.progress[id]
	if p == nil {
		p = &resumePoint{}
		r.progress[id] = p
	}
	total := 0
	for p.stepIdx < len(steps) {
		if total >= budget {
			return total, false, nil
		}
		deleted, drained, err := r.drainStep(ctx, steps[p.stepIdx], id, budget-total, p)
		total += deleted
		if err != nil {
			return total, false, err
		}
		if !drained {
			return total, false, nil
		}
		p.stepIdx++
		p.cursor = nil
	}
	return total, true, nil
}

// drainStep empties one step, --branch-gc-rows-per-delete rows at a time, until its
// SELECT comes back empty (nothing of this branch is left where the statement
// looks) or the budget is spent. Reports the rows deleted and whether it drained
// rather than ran out of budget.
func (r *Reaper) drainStep(ctx context.Context, st step, branchID uuid.UUID, budget int, p *resumePoint) (int, bool, error) {
	total := 0
	for total < budget {
		// Never exceed the budget just because the chunk size is bigger than what
		// is left of it.
		limit := min(r.cfg.RowsPerDelete, budget-total)
		ids, last, err := r.selectRowKeys(ctx, st, branchID, limit, p.cursor)
		if err != nil {
			return total, false, err
		}
		if len(ids) == 0 {
			// Nothing at or after the cursor: this step is drained. A fanned-out
			// table's other values are steps of their own, so the walk reaches them;
			// there is nothing to walk up to here.
			return total, true, nil
		}
		tag, err := r.pool.Exec(ctx, st.deleteSQL, ids)
		if err != nil {
			return total, false, fmt.Errorf("delete from %s (bucket %d): %w", st.spec.table, st.bucket, err)
		}
		total += int(tag.RowsAffected())
		// Only after the delete succeeded: a failed batch has to be re-read, and
		// `>=` makes re-reading free.
		p.cursor = last
	}
	return total, false, nil
}

// selectRowKeys reads one batch's ybctids and the cursor value to resume from — the
// cursor columns of the LAST row, since the batch is the lowest remaining rows in
// key order. A nil `cursor` selects the cursor-free variant, which is how a
// position starts.
func (r *Reaper) selectRowKeys(ctx context.Context, st step, branchID uuid.UUID, limit int, cursor []any) ([][]byte, []any, error) {
	sql := st.initSelectSQL
	// Bound as text and cast in SQL (`branch_id = $1::uuid`), like every other
	// statement here — see the note on `dueBranches`.
	args := []any{branchID.String()}
	if st.spec.fanOut.count > 0 {
		args = append(args, st.bucket)
	}
	if cursor != nil {
		sql = st.resumeSelectSQL
		args = append(args, cursor...)
	}
	args = append(args, limit)
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("select from %s (bucket %d): %w", st.spec.table, st.bucket, err)
	}
	defer rows.Close()
	var ids [][]byte
	var last []any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, nil, fmt.Errorf("scan %s batch: %w", st.spec.table, err)
		}
		id, ok := vals[0].([]byte)
		if !ok {
			return nil, nil, fmt.Errorf("scan %s batch: ybctid is %T, not bytea", st.spec.table, vals[0])
		}
		ids = append(ids, id)
		last = vals[1:]
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read %s batch: %w", st.spec.table, err)
	}
	return ids, last, nil
}

// markComplete records that the branch's data is gone. The branch row itself is
// KEPT — a branch id is never reused, and dropping the row would make "reclaimed"
// indistinguishable from "never existed". Conditional on the column being unset so
// two replicas reaping the same branch cannot move an existing stamp.
func (r *Reaper) markComplete(ctx context.Context, id uuid.UUID) error {
	if _, err := r.pool.Exec(ctx,
		`UPDATE amp_registry.branches SET gc_completed_at = now()
		  WHERE branch_id = $1::uuid AND gc_completed_at IS NULL`,
		id.String()); err != nil {
		return fmt.Errorf("mark branch %s reclaimed: %w", id, err)
	}
	return nil
}
