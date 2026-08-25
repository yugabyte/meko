package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/controller/internal/db"
)

// Sentinel errors classifying a failure for the caller. No handler maps these
// to a status code: in production they reach the record_branch_recipe step,
// which fails the task and keeps the message. Tests assert on them directly.
var (
	// ErrNotFound is returned when an addressed row does not exist.
	ErrNotFound = errors.New("registry: not found")
	// ErrConflict is returned when a request collides with existing state.
	ErrConflict = errors.New("registry: conflict")
	// ErrInvalid is returned when the target exists but the operation is
	// impossible against current state, such as a fork point below the
	// ancestor's GC cutoff.
	ErrInvalid = errors.New("registry: invalid request")
)

// Store is the storage layer's view of the control plane's resource model: the
// SAME `branches` and `endpoints` tables the product API serves, addressed the
// way the storage layer addresses them.
//
// There is no second copy of a branch, and no second spelling: both surfaces key
// on the same `branch_id` uuid — it is the storage layer's BranchId and globally
// unique, so it is the whole primary key. This store is that shared view, not a
// separate registry, which is what makes the two incapable of drifting: a branch
// dropped through either one is the same row with the same `dropped_at` stamp.
//
// A caller still has to say which cluster it means. Not to identify the branch —
// the id alone does that — but to scope the operation: `cluster_id` is an
// ordinary column on `branches`, and every read and write here is filtered by it
// so one cluster's launcher cannot reach another's rows. An endpoint carries no
// cluster at all and is scoped through its branch.
//
// Statements over the resource model are unqualified and resolve via the pool's
// search_path (--db-schema); the data plane is qualified `amp_storage`.
type Store struct {
	pool *db.Pool
}

// NewStore returns a Store backed by pool.
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool}
}

// --- error classification ---------------------------------------------------

// mapWriteErr translates the constraint violations a resource-model write can
// raise into this package's sentinels: a name already taken among live rows, or a
// second writer on a branch (branches_name_per_cluster /
// endpoints_one_writer_per_branch) -> ErrConflict; a missing cluster or branch
// (foreign key) -> ErrNotFound.
func mapWriteErr(what string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w: %s", ErrConflict, what)
		case "23503": // foreign_key_violation
			return fmt.Errorf("%w: %s", ErrNotFound, what)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

// RegistryCounts returns the live inventory size the metrics collector exports
// as a gauge: the number of live branches, across every cluster. Queried on each
// scrape, so it must stay cheap.
//
// Tombstones are excluded — they are dropped branches, and counting them would
// make the gauge a monotonically rising total rather than an inventory.
func (s *Store) RegistryCounts(ctx context.Context) (branches int64, err error) {
	if err = s.pool.QueryRow(ctx,
		`SELECT count(*) FROM amp_registry.branches WHERE dropped_at IS NULL`).Scan(&branches); err != nil {
		return 0, fmt.Errorf("count branches: %w", err)
	}
	return branches, nil
}

// --- PostgreSQL version -----------------------------------------------------

// pgVersionNumPG18 is `PG_VERSION_NUM` of the PostgreSQL 18 build amp ships
// (18.2 → 180002; see pg_install/v18 pg_config.h). It is the seed value the
// controller records for a new branch's `pg_version`.
const pgVersionNumPG18 = 180002

// FullPgVersionID maps a PG major version to the full PgVersionId to record for
// a branch.
//
// PG 18 is the only major amp ships, and this is the sole writer of
// `amp_storage.branch_state.pg_version`, so anything else is a caller bug rather
// than a version to guess at. Synthesising `major*10000` would write a version
// no build in the tree can serve and defer the failure to the safekeeper's
// greeting check on a branch that is already provisioned; refuse it here, where
// the branch has not been created yet.
func FullPgVersionID(major int) (int, error) {
	if major != 18 {
		return 0, fmt.Errorf("unsupported PG major version %d: amp ships PG 18 only", major)
	}
	return pgVersionNumPG18, nil
}

// --- branch name registry ---------------------------------------------------

// --- branch creation -------------------------------------------------------

// RecordBranch declares a branch and seeds the storage-layer state it needs to
// be materialized. Idempotent on (clusterID, branchID).
//
// RecordBranch is reached by every branch-creation path — yb_amp_local's `branch
// create` and the controller's record_branch_recipe step — and it provisions
// both of the branch's rows in one transaction, before any pageserver,
// safekeeper, or endpoint supervisor for the branch is started:
//
//   - branch — the branch's identity and creation record (its name, PG version,
//     and how to create it: a NULL parent means bootstrap, a set one means fork
//     that ancestor at fork_lsn). This insert is the row's only creator on both
//     paths: the product path plans record_branch_recipe alone, and create_branch
//     is registered for task replay but planned by nothing.
//   - amp_storage.branch_state — the per-branch state row the pageserver and
//     safekeeper share. Both daemons only ever validate that it exists and
//     UPDATE their own column group; neither creates it, so it has to be here
//     before either starts.
//
// A fork (AncestorBranchID set) is additionally validated twice: the ancestor must
// still be live, and the fork point must be at or above its GC cutoff. Each
// condition is the WHERE clause of an UPDATE of the ancestor's row, so this
// transaction conflicts with every other writer of it — a drop, another fork, an
// endpoint create on it, GC advancing its cutoff — instead of write-skewing past
// them. Described per race in the body.
//
// The loser is aborted with a serialization failure, and the task worker re-runs
// the step on it (worker.go). What makes that safe is this method being ONE
// transaction: a rejected attempt left neither row behind. Keep it that way.
//
// The abort is returned under no sentinel, only a legible message — it is not a
// verdict on the request. See explainAbort.
func (s *Store) RecordBranch(ctx context.Context, clusterID, branchID uuid.UUID, req BranchCreateRecord) error {
	return explainAbort(s.recordBranch(ctx, clusterID, branchID, req))
}

// explainAbort names the interlock a lost registration collided with, WITHOUT
// changing its class: the driver error stays in the chain through %w, and that is
// what the task worker matches on to decide the step is worth re-running
// (db.IsAbortedTransaction). Wrapping with %v instead would still read correctly
// in a log while silently turning every fork conflict back into a permanent task
// failure — which is exactly #578. So the chain is a contract, not an incidental
// formatting choice, and abort_test.go pins it without needing a database or a
// real race to do it. Anything else is returned untouched.
func explainAbort(err error) error {
	if !db.IsAbortedTransaction(err) {
		return err
	}
	return fmt.Errorf(
		"branch registration conflicted with a concurrent write to the ancestor "+
			"(a drop, another fork, an endpoint create, or a GC cutoff advance): %w",
		err)
}

func (s *Store) recordBranch(ctx context.Context, clusterID, branchID uuid.UUID, req BranchCreateRecord) error {
	// pg_version is the branch's major; refuse an unmappable one here, before the
	// branch exists, rather than seeding a version no build can serve (see
	// FullPgVersionID).
	pgVersion, err := FullPgVersionID(req.PgVersion)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin record branch: %w", err)
	}
	defer tx.Rollback(ctx)

	// A fork inherits BOTH its externalized-extension set and its pg_settings from
	// the ancestor branch at fork time — each is per-branch state, and every
	// endpoint resolves against its own branch's row. They are resolved in a
	// *separate* SELECT and passed as plain parameters rather than subqueries in
	// the INSERT: YB does not support a table-referencing subquery inside an
	// `INSERT ... ON CONFLICT`. The scalar COALESCEs always return one row
	// (NULL -> '[]' / '{}'), so there is no ErrNoRows to handle. extensions is a
	// pure copy (createBranch takes no extensions); pg_settings additionally
	// merges the caller's overrides below.
	// See compute/architecture/extensions.md.
	inheritedExts := []byte("[]")
	inheritedSettings := []byte("{}")
	if req.AncestorBranchID != nil {
		var settings, exts []byte
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(pg_settings, '{}'::jsonb), COALESCE(extensions, '[]'::jsonb)
			   FROM amp_registry.branches WHERE branch_id = $1::uuid`,
			req.AncestorBranchID.String()).Scan(&settings, &exts); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read ancestor settings/extensions: %w", err)
		} else if err == nil {
			inheritedSettings = settings
			inheritedExts = exts
		}
	}

	// The caller's settings merge OVER the inherited ones and win on a key both
	// define, so a fork can override one of its ancestor's without restating the
	// rest. Merged here rather than in SQL because the inherited side is already
	// in hand.
	if len(req.PgSettings) > 0 {
		merged := map[string]string{}
		if err := json.Unmarshal(inheritedSettings, &merged); err != nil {
			return fmt.Errorf("decode ancestor pg_settings: %w", err)
		}
		for k, v := range req.PgSettings {
			merged[k] = v
		}
		blob, err := json.Marshal(merged)
		if err != nil {
			return fmt.Errorf("encode branch pg_settings: %w", err)
		}
		inheritedSettings = blob
	}

	// is_default is false: a cluster's default branch is created with the cluster
	// itself, and every branch declared here hangs off it (or stands alone as
	// another root). ON CONFLICT DO NOTHING makes a re-record converge, and leaves
	// an existing creation record untouched rather than rewriting it — which is
	// also what keeps a re-record from clobbering a fork's inherited `extensions` /
	// `pg_settings` (and any later per-branch extension add) after this row was
	// first written.
	//
	// This INSERT is the branch's *only* creator on the fork path: the branch task
	// is one step so that the row, the branch_state seed, and the validation below
	// all commit or roll back together. A fork therefore selects its parent rather
	// than naming it — the FK alone cannot check liveness, since a tombstoned
	// parent's row is still there.
	//
	var tag pgconn.CommandTag
	if req.AncestorBranchID != nil {
		tag, err = tx.Exec(ctx,
			`INSERT INTO amp_registry.branches
			   (cluster_id, branch_id, name, pg_version, is_default, parent_branch_id, fork_lsn, extensions, pg_settings)
			 SELECT $1::uuid, $2::uuid, $3, $4, false, p.branch_id, $6::pg_lsn, $7::jsonb, $8::jsonb
			   FROM amp_registry.branches p
			  WHERE p.branch_id = $5::uuid AND p.cluster_id = $1::uuid AND p.dropped_at IS NULL
			 ON CONFLICT (branch_id) DO NOTHING`,
			clusterID.String(), branchID.String(), req.BranchName, req.PgVersion,
			req.AncestorBranchID.String(), req.StartLSN, string(inheritedExts), string(inheritedSettings))
	} else {
		tag, err = tx.Exec(ctx,
			`INSERT INTO amp_registry.branches
			   (cluster_id, branch_id, name, pg_version, is_default, parent_branch_id, fork_lsn, extensions, pg_settings)
			 VALUES ($1::uuid, $2::uuid, $3, $4, false, NULL, $5::pg_lsn, $6::jsonb, $7::jsonb)
			 ON CONFLICT (branch_id) DO NOTHING`,
			clusterID.String(), branchID.String(), req.BranchName, req.PgVersion,
			req.StartLSN, string(inheritedExts), string(inheritedSettings))
	}
	if err != nil {
		return mapWriteErr(fmt.Sprintf("declare branch %s (%q)", branchID, req.BranchName), err)
	}
	// Zero rows is either an idempotent re-record (the branch already exists) or a
	// parent that is gone or tombstoned. Only the latter is an error.
	if tag.RowsAffected() == 0 && req.AncestorBranchID != nil {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM amp_registry.branches
			                 WHERE cluster_id = $1::uuid AND branch_id = $2::uuid)`,
			clusterID.String(), branchID.String()).Scan(&exists); err != nil {
			return fmt.Errorf("classify branch declare no-op: %w", err)
		}
		if !exists {
			return fmt.Errorf("%w: fork parent %s is not a live branch in cluster %s",
				ErrNotFound, *req.AncestorBranchID, clusterID)
		}
	}

	// The parent must still be live, and a plain read cannot establish that: the drop
	// tombstones the parent's row while checking `NOT EXISTS (live children)`, and
	// neither side writes what the other reads, so both commit (time increases
	// downwards):
	//
	//   fork a child off P                 drop P
	//   ──────────────────                 ──────
	//   read P: dropped_at IS NULL ✓
	//                                      read P's children: none ✓
	//   insert the child row
	//                                      UPDATE P SET dropped_at = now()
	//   P is tombstoned with a live child ✗
	//
	// The child's pre-fork history lives under P's branch_id, so branch-drop GC
	// would then reclaim it out from under a live branch.
	//
	// So make the fork WRITE P's row: both sides then write it, and the write-write
	// conflict forces the drop's guard to re-run and see the child.
	//
	// Deliberately serialized by writing the same row in both transactions, not by a
	// row lock. A lock serializes this too under read committed, but the drop would
	// then need FOR NO KEY UPDATE as a statement of its own ahead of its guarded
	// UPDATE -- one extra YB round trip. (The fork side is a wash: FOR SHARE in
	// place of this UPDATE.) And under repeatable read the drop would not see the
	// newly inserted fork at all: its read time is fixed when it asks for the lock,
	// before the fork commits.
	//
	// DeleteCluster tombstones every branch row in the cluster, P's included, so it
	// conflicts here the same way.
	if req.AncestorBranchID != nil && tag.RowsAffected() == 1 {
		live, err := tx.Exec(ctx,
			`UPDATE amp_registry.branches SET dropped_at = dropped_at
			  WHERE branch_id = $1::uuid AND cluster_id = $2::uuid AND dropped_at IS NULL`,
			req.AncestorBranchID.String(), clusterID.String())
		if err != nil {
			return fmt.Errorf("validate fork parent %s liveness: %w", *req.AncestorBranchID, err)
		}
		if live.RowsAffected() == 0 {
			// Rolled back with the txn: the child row above never lands.
			return fmt.Errorf("%w: fork parent %s was dropped concurrently",
				ErrNotFound, *req.AncestorBranchID)
		}
	}

	// The seed the pageserver reads at attach to decide how to create the
	// timeline: a NULL ancestor means bootstrap (initdb + import at pg_version),
	// a set one means branch off that ancestor at ancestor_lsn. This is why the
	// row has to exist before any pageserver starts.
	//
	// `pg_version` here holds a full PgVersionId, not the major the branch row
	// carries. This is the column's only writer — both daemons just read it.
	//
	// Keyed by the same branch uuid the branch row carries, and deliberately NOT
	// cluster-scoped (the storage layer has no clusters).
	// Both nullable args for this statement: NULL ancestor means bootstrap. Bound
	// as text against the `::uuid` cast, like every other id in this file.
	var ancestorID, ancestorLSN *string
	if req.AncestorBranchID != nil {
		id := req.AncestorBranchID.String()
		ancestorID, ancestorLSN = &id, &req.StartLSN
	}
	stateRow, err := tx.Exec(ctx,
		`INSERT INTO amp_storage.branch_state
		   (branch_id, pg_version, ancestor_branch_id, ancestor_lsn)
		 VALUES ($1::uuid, $2, $3::uuid, ($4::pg_lsn - '0/0'::pg_lsn)::bigint)
		 ON CONFLICT (branch_id) DO NOTHING`,
		branchID.String(), pgVersion, ancestorID, ancestorLSN)
	if err != nil {
		return fmt.Errorf("record branch state: %w", err)
	}

	// There is a race between forking a child off the ancestor and the
	// ancestor's GC advancing its cutoff (time increases downwards):
	//
	//   initial state: cutoff = 100
	//
	//   fork child @110                    GC on the ancestor
	//   ───────────────                    ──────────────────
	//   validate: cutoff = 100 ✓
	//                                      compute cutoff = 120;
	//                                      sees no child row
	//                                      (not committed yet)
	//   commit child row
	//                                      publish 120, trim ≤ 120
	//   child's fork point is gone ✗
	//
	// It is handled by making the fork perform a dummy UPDATE on the
	// ancestor's branch_state row, in the same transaction as the child
	// row INSERT: the fork and the cutoff advance both write the same row
	// and hence are serialized. The WHERE is the validation, evaluated
	// inside that serialized statement. Gated on the child row being
	// NEWLY inserted: a re-record must stay a no-op even after the cutoff
	// advances. Mirrored by `fork_txn` in
	// compute/pageserver/src/page_store.rs (tests) — keep in lockstep.
	//
	// Serialized by writing the same row rather than by a row lock, as with the drop
	// interlock above. A lock would cost more here: its extra FOR NO KEY UPDATE would
	// land on the GC cutoff advance, which runs far more often than a drop.
	if req.AncestorBranchID != nil && stateRow.RowsAffected() == 1 {
		touched, err := tx.Exec(ctx,
			`UPDATE amp_storage.branch_state
			    SET applied_gc_cutoff_lsn = applied_gc_cutoff_lsn
			  WHERE branch_id = $1::uuid
			    AND COALESCE(applied_gc_cutoff_lsn, 0) <= ($2::pg_lsn - '0/0'::pg_lsn)::bigint`,
			req.AncestorBranchID.String(), req.StartLSN)
		if err != nil {
			return fmt.Errorf("validate fork point: %w", err)
		}
		if touched.RowsAffected() == 0 {
			// Rolled back with the txn: the child row above never lands.
			var exists bool
			err := tx.QueryRow(ctx,
				`SELECT true FROM amp_storage.branch_state WHERE branch_id = $1::uuid`,
				req.AncestorBranchID.String()).Scan(&exists)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: fork ancestor %s has no branch_state row",
					ErrNotFound, *req.AncestorBranchID)
			}
			if err != nil {
				return fmt.Errorf("probe fork ancestor %s: %w", *req.AncestorBranchID, err)
			}
			return fmt.Errorf("%w: fork point %s is below ancestor %s's GC cutoff (history already reclaimed)",
				ErrInvalid, req.StartLSN, *req.AncestorBranchID)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit record branch: %w", err)
	}
	return nil
}
