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
// branch/endpoint/pending-init registries) in the `registry` schema of the
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
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM registry.branches`).Scan(&branches); err != nil {
		return 0, fmt.Errorf("count branches: %w", err)
	}
	return branches, nil
}

// --- generation authority -------------------------------------------------

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
			`UPDATE registry.branches
			   SET placement_policy = '"Detached"'
			 WHERE branch_id = $1`, req.BranchID); err != nil {
			return nil, fmt.Errorf("detach branch: %w", err)
		}
		return nil, nil
	}

	config := req.Config
	if len(config) == 0 {
		config = json.RawMessage("{}")
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

	// Create the branch on first attach; a concurrent creator is a no-op.
	if _, err := tx.Exec(ctx,
		`INSERT INTO registry.branches
		   (branch_id, generation, placement_policy, config, scheduling_policy)
		 VALUES ($1, $2, '{"Attached":0}', $3::jsonb, '"Active"')
		 ON CONFLICT (branch_id) DO NOTHING`,
		req.BranchID, initialGen, []byte(config)); err != nil {
		return nil, fmt.Errorf("insert branch: %w", err)
	}

	var gen *int32
	err = tx.QueryRow(ctx,
		`UPDATE registry.branches
		    SET generation = generation + 1, placement_policy = '{"Attached":0}'
		  WHERE branch_id = $1
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
		`SELECT generation, placement_policy FROM registry.branches WHERE branch_id = $1`,
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

// BranchRegister records a launcher-created branch's start LSN (and optional
// name alias) in the timelines registry. Idempotent: an existing row is left
// untouched.
func (s *Store) BranchRegister(ctx context.Context, branchID string, req BranchRegisterRequest) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO registry.timelines (branch_id, start_lsn, generation, branch_name)
		 VALUES ($1, $2::pg_lsn, 1, $3)
		 ON CONFLICT (branch_id) DO NOTHING`,
		branchID, req.StartLSN, req.BranchName); err != nil {
		return fmt.Errorf("register timeline: %w", err)
	}
	return nil
}

// SetBranchName sets or clears a branch's name alias.
func (s *Store) SetBranchName(ctx context.Context, branchID string, req SetBranchNameRequest) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE registry.timelines SET branch_name = $2 WHERE branch_id = $1`,
		branchID, req.BranchName); err != nil {
		return fmt.Errorf("set branch name: %w", err)
	}
	return nil
}

// ListBranchNames returns every live branch's (branch_id, name) mapping. Names
// are non-unique labels.
func (s *Store) ListBranchNames(ctx context.Context) ([]BranchNameEntry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT branch_id, branch_name FROM registry.timelines
		  WHERE branch_name IS NOT NULL AND deleted_at IS NULL ORDER BY branch_id`)
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

// BranchDrop drops a branch: retire its endpoint records, discard any pending
// init recipe, and mark the timeline row deleted. Idempotent.
func (s *Store) BranchDrop(ctx context.Context, branchID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	for _, stmt := range []string{
		`DELETE FROM registry.endpoints WHERE branch_id = $1`,
		`DELETE FROM registry.pending_timeline_inits WHERE branch_id = $1`,
		// A dropped branch is terminal (its id is never reused and its pageserver
		// is stopped), so its generation record can go too — no fencing concern.
		`DELETE FROM registry.branches WHERE branch_id = $1`,
		`UPDATE registry.timelines SET deleted_at = now() WHERE branch_id = $1`,
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

// --- pending-init recipe registry -----------------------------------------

// RecordPendingInit stores a branch's init recipe. Idempotent on branch_id.
func (s *Store) RecordPendingInit(ctx context.Context, branchID string, req PendingBranchInitRequest) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO registry.pending_timeline_inits
		   (branch_id, branch_name, pg_version, ancestor_branch_id, start_lsn)
		 VALUES ($1, $2, $3, $4, $5::pg_lsn)
		 ON CONFLICT (branch_id) DO NOTHING`,
		branchID, req.BranchName, req.PgVersion, req.AncestorBranchID, req.StartLSN); err != nil {
		return fmt.Errorf("record pending init: %w", err)
	}
	return nil
}

// ListPendingInits returns every recorded init recipe.
func (s *Store) ListPendingInits(ctx context.Context) ([]PendingBranchInit, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT branch_id, branch_name, pg_version, ancestor_branch_id, start_lsn::text
		   FROM registry.pending_timeline_inits ORDER BY branch_id`)
	if err != nil {
		return nil, fmt.Errorf("list pending inits: %w", err)
	}
	defer rows.Close()

	out := []PendingBranchInit{}
	for rows.Next() {
		var p PendingBranchInit
		if err := rows.Scan(&p.BranchID, &p.BranchName, &p.PgVersion, &p.AncestorBranchID, &p.StartLSN); err != nil {
			return nil, fmt.Errorf("scan pending init: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePendingInit drops a branch's init recipe. Idempotent: an absent recipe
// is a no-op.
func (s *Store) DeletePendingInit(ctx context.Context, branchID string) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM registry.pending_timeline_inits WHERE branch_id = $1`, branchID); err != nil {
		return fmt.Errorf("delete pending init: %w", err)
	}
	return nil
}

// --- endpoint registry ----------------------------------------------------

// EndpointCreate persists (or replaces) a compute-endpoint record.
func (s *Store) EndpointCreate(ctx context.Context, ep ComputeEndpoint) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO registry.endpoints (endpoint_id, branch_id, spec)
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
		`SELECT endpoint_id, branch_id, spec FROM registry.endpoints ORDER BY endpoint_id`)
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
		`DELETE FROM registry.endpoints WHERE endpoint_id = $1`, endpointID); err != nil {
		return fmt.Errorf("delete endpoint: %w", err)
	}
	return nil
}
