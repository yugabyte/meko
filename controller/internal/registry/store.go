package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/yugabyte/amp/controller/internal/db"
)

// Sentinel errors the handlers map onto HTTP status codes.
var (
	// ErrNotFound is returned when an addressed row does not exist.
	ErrNotFound = errors.New("registry: not found")
	// ErrConflict is returned when a request collides with existing state.
	ErrConflict = errors.New("registry: conflict")
)

// Store persists the storage-layer control state (generation authority + the
// branch/endpoint/pending-init registries) in the `amp_registry` schema of the
// shared yb_storage database. All statements are schema-qualified so they resolve
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

// --- generation authority -------------------------------------------------

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

// AttachHook issues a branch's next generation. On attach it increments and
// returns the generation, creating the branch row on first attach; on detach it
// clears the attachment and returns nil. The generation is a monotonic per-branch
// counter — the fencing primitive.
func (s *Store) AttachHook(ctx context.Context, req AttachHookRequest) (*uint32, error) {
	if !req.Attach {
		// Detach clears the attachment but PRESERVES the generation counter: it
		// must stay monotonic across the branch's lifecycle so a later reattach
		// fences any stale pageserver from a prior generation.
		if _, err := s.pool.Exec(ctx,
			`UPDATE amp_registry.branches
			   SET placement_policy = '"Detached"'
			 WHERE branch_id = $1`, req.BranchID); err != nil {
			return nil, fmt.Errorf("detach branch: %w", err)
		}
		return nil, nil
	}

	initialGen := int32(0)
	if req.GenerationOverride != nil {
		initialGen = *req.GenerationOverride
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// The branch row is normally created at branch-create time (see
	// RecordBranch), before any pageserver/safekeeper starts. This insert is a
	// safety net for a branch that reaches attach without having been created;
	// a concurrent creator is a no-op. It fills only the fencing group — this
	// path has nothing but a branch id to go on, so the creation group stays
	// NULL and the branch reads as "not properly created".
	if _, err := tx.Exec(ctx,
		`INSERT INTO amp_registry.branches
		   (branch_id, generation, placement_policy)
		 VALUES ($1, $2, '{"Attached":0}')
		 ON CONFLICT (branch_id) DO NOTHING`,
		req.BranchID, initialGen); err != nil {
		return nil, fmt.Errorf("insert branch: %w", err)
	}

	var gen *int32
	err = tx.QueryRow(ctx,
		// COALESCE: a branch created by RecordBranch starts with
		// generation NULL ("never attached"), and NULL + 1 would stay NULL.
		// First attach therefore issues generation 1, as it always has.
		`UPDATE amp_registry.branches
		    SET generation = COALESCE(generation, 0) + 1, placement_policy = '{"Attached":0}'
		  WHERE branch_id = $1 AND dropped_at IS NULL
		  RETURNING generation`,
		req.BranchID).Scan(&gen)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("increment generation: %w", err)
	}
	if gen == nil {
		return nil, fmt.Errorf("generation is NULL after increment for branch %s", req.BranchID)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit attach: %w", err)
	}
	g := uint32(*gen)
	return &g, nil
}

// Inspect returns a branch's current generation, or nil when the branch is
// unattached (detached or never attached) or unknown.
func (s *Store) Inspect(ctx context.Context, branchID string) (*uint32, error) {
	var gen *int32
	var policy string
	err := s.pool.QueryRow(ctx,
		`SELECT generation, placement_policy FROM amp_registry.branches
		  WHERE branch_id = $1 AND dropped_at IS NULL`,
		branchID).Scan(&gen, &policy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect branch: %w", err)
	}
	if gen == nil || policy == `"Detached"` {
		return nil, nil
	}
	g := uint32(*gen)
	return &g, nil
}

// BranchImport returns the branch's current attachment as a single shard. The
// branch must already be attached (via AttachHook).
func (s *Store) BranchImport(ctx context.Context, branchID string) (BranchCreateResponse, error) {
	gen, err := s.Inspect(ctx, branchID)
	if err != nil {
		return BranchCreateResponse{}, err
	}
	if gen == nil {
		return BranchCreateResponse{}, ErrNotFound
	}
	return BranchCreateResponse{Shards: []BranchCreateResponseShard{{
		BranchID:   branchID,
		Generation: *gen,
	}}}, nil
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
// never reused, so keeping it preserves the generation counter (and the fencing
// history) and leaves a record of the branch. Every "is this live?" reader
// filters `dropped_at IS NULL`, so the name stops resolving either way.
func (s *Store) BranchDrop(ctx context.Context, branchID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	for _, stmt := range []string{
		`DELETE FROM amp_registry.endpoints WHERE branch_id = $1`,
		`UPDATE amp_registry.branches
		    SET dropped_at = now(), placement_policy = '"Detached"'
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
//   - amp_registry.branches — the branch's identity (name + how to create it)
//     and the generation authority. generation stays NULL ("never attached")
//     until the first attach-hook bumps it to 1.
//   - amp_storage.branch_state — the per-branch state row the pageserver and
//     safekeeper share. Both daemons only ever validate that it exists and
//     UPDATE their own column group; neither creates it, so it has to be here
//     before either starts.
//
// Re-recording is idempotent. The branches upsert deliberately fills the
// creation group ON CONFLICT rather than doing nothing: a row can already exist
// without ever having carried a creation record — the attach-hook's safety-net
// insert creates a bare row — and a plain DO NOTHING
// would leave that row's creation group NULL forever while reporting success,
// making the branch invisible to ListBranches. The `pg_version IS NULL` guard
// keeps a genuine re-record a no-op, so an existing record is never rewritten.
func (s *Store) RecordBranch(ctx context.Context, branchID string, req BranchCreateRecord) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin record branch: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`INSERT INTO amp_registry.branches
		   (branch_id, branch_name, pg_version, ancestor_branch_id, start_lsn,
		    generation, placement_policy)
		 VALUES ($1, $2, $3, $4, $5::pg_lsn, NULL, '"Detached"')
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
	if _, err := tx.Exec(ctx,
		`INSERT INTO amp_storage.branch_state
		   (branch_id, pg_version, ancestor_branch_id, ancestor_lsn)
		 VALUES ($1, $2, $3, ($4::pg_lsn - '0/0'::pg_lsn)::bigint)
		 ON CONFLICT (branch_id) DO NOTHING`,
		branchID, pgVersion, req.AncestorBranchID, ancestorLSN); err != nil {
		return fmt.Errorf("record branch state: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit record branch: %w", err)
	}
	return nil
}

// ListBranches returns every live, properly created branch — the ones the
// launcher can materialize. A row whose `pg_version` is NULL was created by the
// attach-hook's safety net and carries no creation record, so it is skipped, as
// is a tombstoned (dropped) branch.
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
