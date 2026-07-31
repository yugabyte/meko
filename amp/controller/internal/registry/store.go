package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/controller/internal/db"
)

// Sentinel errors the handlers map onto HTTP status codes.
var (
	// ErrNotFound is returned when an addressed row does not exist.
	ErrNotFound = errors.New("registry: not found")
	// ErrConflict is returned when a request collides with existing state.
	ErrConflict = errors.New("registry: conflict")
	// ErrInvalid is returned when a request is semantically invalid
	// against current state. Retrying the same request cannot succeed.
	ErrInvalid = errors.New("registry: invalid request")
)

// Store persists the storage-layer control state (the branch and endpoint
// registries) in the `amp_registry` schema of the shared yb_storage database. All statements are schema-qualified so they resolve
// regardless of the pool's search_path (`controller`).
type Store struct {
	pool *db.Pool
}

// NewStore returns a Store backed by pool.
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool}
}

// Ready reports whether the backing database is reachable.
func (s *Store) Ready(ctx context.Context) error {
	return s.pool.Healthy(ctx)
}

// RegistryCounts returns the live inventory size the metrics collector exports
// as a gauge: the number of branches known to the registry. Queried on each
// scrape, so it must stay cheap (count(*) over one small table).
func (s *Store) RegistryCounts(ctx context.Context) (branches int64, err error) {
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM amp_registry.branches`).Scan(&branches); err != nil {
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

// --- timeline / branch-name registry --------------------------------------

// SetBranchName sets or clears a branch's name alias.
func (s *Store) SetBranchName(ctx context.Context, branchID string, req SetBranchNameRequest) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE amp_registry.branches SET branch_name = $2 WHERE branch_id = $1`,
		branchID, req.BranchName); err != nil {
		return fmt.Errorf("set branch name: %w", err)
	}
	return nil
}

// ListBranchNames returns every live branch's (branch_id, name) mapping. Names
// are non-unique labels.
func (s *Store) ListBranchNames(ctx context.Context) ([]BranchNameEntry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT branch_id, branch_name FROM amp_registry.branches
		  WHERE branch_name IS NOT NULL AND dropped_at IS NULL ORDER BY branch_id`)
	if err != nil {
		return nil, fmt.Errorf("list branch names: %w", err)
	}
	defer rows.Close()

	out := []BranchNameEntry{}
	for rows.Next() {
		var t BranchNameEntry
		if err := rows.Scan(&t.BranchID, &t.BranchName); err != nil {
			return nil, fmt.Errorf("scan branch name: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// BranchDrop drops a branch: retire its endpoint records and tombstone the
// branch row. Idempotent.
//
// The row is kept with `dropped_at` stamped rather than deleted: a branch id is
// never reused, so the record of what the id was is worth keeping. Every
// "is this live?" reader filters `dropped_at IS NULL`, so the name stops
// resolving either way.
func (s *Store) BranchDrop(ctx context.Context, branchID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	for _, stmt := range []string{
		`DELETE FROM amp_registry.endpoints WHERE branch_id = $1`,
		`UPDATE amp_registry.branches
		    SET dropped_at = now()
		  WHERE branch_id = $1 AND dropped_at IS NULL`,
	} {
		if _, err := tx.Exec(ctx, stmt, branchID); err != nil {
			return fmt.Errorf("drop branch: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit drop: %w", err)
	}
	return nil
}

// --- branch creation -------------------------------------------------------

// RecordBranch stores a branch's creation record. Idempotent on branch_id.
// RecordBranch is where a branch id first reaches the database, and it is
// reached by every branch-creation path (the launcher's `branch create` and the
// controller's record_branch_recipe step). So it provisions both of the
// branch's rows in one transaction, before any pageserver, safekeeper, or
// endpoint supervisor for the branch is started:
//
//   - amp_registry.branches — the branch's identity: its name and how to
//     create it.
//   - amp_storage.branch_state — the per-branch state row the pageserver and
//     safekeeper share. Both daemons only ever validate that it exists and
//     UPDATE their own column group; neither creates it, so it has to be here
//     before either starts.
//
// Re-recording is idempotent. This is the only creator of a branches row, so
// the ON CONFLICT arm should never fire in practice; it fills the creation
// group rather than doing nothing so that a row which somehow exists without
// one still becomes visible to ListBranches instead of reporting success and
// staying invisible. The `pg_version IS NULL` guard keeps a genuine re-record a
// no-op, so an existing record is never rewritten.
//
// A fork (AncestorBranchID set) is additionally validated against the
// ancestor's GC cutoff — ErrInvalid if the fork point's history is already
// reclaimed — via a write on the ancestor's row that serializes with the
// ancestor's GC cutoff advance (see the interlock comment in the body).
func (s *Store) RecordBranch(ctx context.Context, branchID string, req BranchCreateRecord) error {
	err := s.recordBranch(ctx, branchID, req)
	// The fork interlock write-conflicts with the ancestor's GC cutoff
	// advance by design; surface the losing side legibly (409, retryable)
	// instead of a raw serialization abort at HTTP 500.
	if isSerializationAbort(err) {
		return fmt.Errorf(
			"%w: branch registration conflicted with a concurrent GC cutoff advance on the ancestor — retry: %w",
			ErrConflict, err)
	}
	return err
}

// isSerializationAbort reports SQLSTATE 40001 (serialization_failure).
func isSerializationAbort(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40001"
}

func (s *Store) recordBranch(ctx context.Context, branchID string, req BranchCreateRecord) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin record branch: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`INSERT INTO amp_registry.branches
		   (branch_id, branch_name, pg_version, ancestor_branch_id, start_lsn)
		 VALUES ($1, $2, $3, $4, $5::pg_lsn)
		 ON CONFLICT (branch_id) DO UPDATE
		    SET branch_name        = EXCLUDED.branch_name,
		        pg_version         = EXCLUDED.pg_version,
		        ancestor_branch_id = EXCLUDED.ancestor_branch_id,
		        start_lsn          = EXCLUDED.start_lsn
		  WHERE amp_registry.branches.pg_version IS NULL
		    AND amp_registry.branches.dropped_at IS NULL`,
		branchID, req.BranchName, req.PgVersion, req.AncestorBranchID, req.StartLSN); err != nil {
		return fmt.Errorf("record branch: %w", err)
	}

	// The seed the pageserver reads at attach to decide how to create the
	// timeline: a NULL ancestor means bootstrap (initdb + import at pg_version),
	// a set one means branch off that ancestor at ancestor_lsn. This is why the
	// row has to exist before any pageserver starts.
	//
	// `pg_version` holds a full PgVersionId; the creation record carries only the
	// major version, so map it to the full id of the PostgreSQL we ship. This is the
	// column's only writer — both daemons just read it — so a major we can't map
	// has to fail here, before the branch exists, rather than seed a version no
	// build can serve.
	pgVersion, err := FullPgVersionID(req.PgVersion)
	if err != nil {
		return fmt.Errorf("record branch state: %w", err)
	}
	var ancestorLSN *string
	if req.AncestorBranchID != nil {
		ancestorLSN = &req.StartLSN
	}
	stateRow, err := tx.Exec(ctx,
		`INSERT INTO amp_storage.branch_state
		   (branch_id, pg_version, ancestor_branch_id, ancestor_lsn)
		 VALUES ($1, $2, $3, ($4::pg_lsn - '0/0'::pg_lsn)::bigint)
		 ON CONFLICT (branch_id) DO NOTHING`,
		branchID, pgVersion, req.AncestorBranchID, ancestorLSN)
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
	if req.AncestorBranchID != nil && stateRow.RowsAffected() == 1 {
		touched, err := tx.Exec(ctx,
			`UPDATE amp_storage.branch_state
			    SET applied_gc_cutoff_lsn = applied_gc_cutoff_lsn
			  WHERE branch_id = $1
			    AND COALESCE(applied_gc_cutoff_lsn, 0) <= ($2::pg_lsn - '0/0'::pg_lsn)::bigint`,
			*req.AncestorBranchID, req.StartLSN)
		if err != nil {
			return fmt.Errorf("validate fork point: %w", err)
		}
		if touched.RowsAffected() == 0 {
			// Rolled back with the txn: the child row above never lands.
			var exists bool
			err := tx.QueryRow(ctx,
				`SELECT true FROM amp_storage.branch_state WHERE branch_id = $1`,
				*req.AncestorBranchID).Scan(&exists)
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

// ListBranches returns every live, properly created branch — the ones the
// launcher can materialize. A row whose `pg_version` is NULL carries no creation
// record, so it is skipped, as is a tombstoned (dropped) branch.
func (s *Store) ListBranches(ctx context.Context) ([]BranchRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT branch_id, branch_name, pg_version, ancestor_branch_id, start_lsn::text
		   FROM amp_registry.branches
		  WHERE pg_version IS NOT NULL AND dropped_at IS NULL ORDER BY branch_id`)
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	defer rows.Close()

	out := []BranchRecord{}
	for rows.Next() {
		var p BranchRecord
		if err := rows.Scan(&p.BranchID, &p.BranchName, &p.PgVersion, &p.AncestorBranchID, &p.StartLSN); err != nil {
			return nil, fmt.Errorf("scan branch: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- endpoint registry ----------------------------------------------------

// EndpointCreate persists (or replaces) a compute-endpoint record.
func (s *Store) EndpointCreate(ctx context.Context, ep ComputeEndpoint) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO amp_registry.endpoints (endpoint_id, branch_id, spec)
		 VALUES ($1, $2, $3::jsonb)
		 ON CONFLICT (endpoint_id) DO UPDATE SET branch_id = EXCLUDED.branch_id, spec = EXCLUDED.spec`,
		ep.EndpointID, ep.BranchID, []byte(ep.Spec)); err != nil {
		return fmt.Errorf("create endpoint: %w", err)
	}
	return nil
}

// EndpointList returns every persisted compute-endpoint record.
func (s *Store) EndpointList(ctx context.Context) ([]ComputeEndpoint, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT endpoint_id, branch_id, spec FROM amp_registry.endpoints ORDER BY endpoint_id`)
	if err != nil {
		return nil, fmt.Errorf("list endpoints: %w", err)
	}
	defer rows.Close()

	out := []ComputeEndpoint{}
	for rows.Next() {
		var ep ComputeEndpoint
		var spec []byte
		if err := rows.Scan(&ep.EndpointID, &ep.BranchID, &spec); err != nil {
			return nil, fmt.Errorf("scan endpoint: %w", err)
		}
		ep.Spec = json.RawMessage(spec)
		out = append(out, ep)
	}
	return out, rows.Err()
}

// EndpointDelete removes a compute-endpoint record. Idempotent.
func (s *Store) EndpointDelete(ctx context.Context, endpointID string) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM amp_registry.endpoints WHERE endpoint_id = $1`, endpointID); err != nil {
		return fmt.Errorf("delete endpoint: %w", err)
	}
	return nil
}
