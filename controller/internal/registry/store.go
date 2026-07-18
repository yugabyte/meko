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
	// ErrConflict is returned when a request collides with existing state (e.g.
	// re-registering a node id with different listen addresses).
	ErrConflict = errors.New("registry: conflict")
)

// Store persists the storage-layer control state (generation authority + the
// branch/node/endpoint/pending-init registries) in the `registry` schema of the
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

// RegistryCounts returns the live inventory sizes the metrics collector exports
// as gauges: the number of branches known to the registry and the number of
// registered pageserver nodes. Queried on each scrape, so it must stay cheap
// (count(*) over two small tables).
func (s *Store) RegistryCounts(ctx context.Context) (branches, nodes int64, err error) {
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM registry.branches`).Scan(&branches); err != nil {
		return 0, 0, fmt.Errorf("count branches: %w", err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM registry.nodes`).Scan(&nodes); err != nil {
		return 0, 0, fmt.Errorf("count nodes: %w", err)
	}
	return branches, nodes, nil
}

// --- generation authority -------------------------------------------------

// AttachHook issues a branch's next generation. With a node id it attaches the
// branch to that pageserver and returns the incremented generation, creating the
// branch row on first attach; without one it detaches the branch and returns
// nil. The generation is a monotonic per-branch counter — the fencing primitive.
func (s *Store) AttachHook(ctx context.Context, req AttachHookRequest) (*uint32, error) {
	if req.NodeID == nil {
		// Detach clears the attachment but PRESERVES the generation counter: it
		// must stay monotonic across the branch's lifecycle so a later reattach
		// fences any stale pageserver from a prior generation.
		if _, err := s.pool.Exec(ctx,
			`UPDATE registry.branches
			   SET generation_pageserver = NULL, placement_policy = '"Detached"'
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
		   (branch_id, generation, generation_pageserver, placement_policy, config, scheduling_policy)
		 VALUES ($1, $2, NULL, '{"Attached":0}', $3::jsonb, '"Active"')
		 ON CONFLICT (branch_id) DO NOTHING`,
		req.BranchID, initialGen, []byte(config)); err != nil {
		return nil, fmt.Errorf("insert branch: %w", err)
	}

	var gen *int32
	err = tx.QueryRow(ctx,
		`UPDATE registry.branches
		    SET generation = generation + 1, generation_pageserver = $2, placement_policy = '{"Attached":0}'
		  WHERE branch_id = $1
		  RETURNING generation`,
		req.BranchID, *req.NodeID).Scan(&gen)
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

// Inspect returns a branch's current (generation, node) attachment, or nil when
// the branch is unattached or unknown.
func (s *Store) Inspect(ctx context.Context, branchID string) (*[2]uint64, error) {
	var gen *int32
	var node *int64
	err := s.pool.QueryRow(ctx,
		`SELECT generation, generation_pageserver FROM registry.branches WHERE branch_id = $1`,
		branchID).Scan(&gen, &node)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect branch: %w", err)
	}
	if gen == nil || node == nil {
		return nil, nil
	}
	return &[2]uint64{uint64(uint32(*gen)), uint64(*node)}, nil
}

// BranchImport returns the branch's current attachment as a single shard. The
// branch must already be attached (via AttachHook).
func (s *Store) BranchImport(ctx context.Context, branchID string) (BranchCreateResponse, error) {
	att, err := s.Inspect(ctx, branchID)
	if err != nil {
		return BranchCreateResponse{}, err
	}
	if att == nil {
		return BranchCreateResponse{}, ErrNotFound
	}
	return BranchCreateResponse{Shards: []BranchCreateResponseShard{{
		BranchID:   branchID,
		NodeID:     int64(att[1]),
		Generation: uint32(att[0]),
	}}}, nil
}

// --- node registry --------------------------------------------------------

// NodeRegister records a pageserver in the node registry. Re-registering the
// same id with the same listen addresses is idempotent; a different address set
// is a conflict.
func (s *Store) NodeRegister(ctx context.Context, req NodeRegisterRequest) error {
	// Insert first: `ON CONFLICT DO NOTHING` makes concurrent registrations
	// race-free (a lost insert affects zero rows) and saves a round-trip on the
	// fresh-registration path.
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO registry.nodes
		   (node_id, listen_http_addr, listen_http_port, listen_pg_addr, listen_pg_port,
		    listen_https_port, listen_grpc_addr, listen_grpc_port)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (node_id) DO NOTHING`,
		req.NodeID, req.ListenHTTPAddr, int32(req.ListenHTTPPort), req.ListenPgAddr, int32(req.ListenPgPort),
		u16ToInt(req.ListenHTTPSPort), req.ListenGrpcAddr, u16ToInt(req.ListenGrpcPort))
	if err != nil {
		return fmt.Errorf("insert node: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil // freshly registered
	}

	// The row already existed: idempotent only if the listen addresses match;
	// a different address set for the same id is a conflict.
	var hAddr, pAddr string
	var hPort, pPort int32
	if err := s.pool.QueryRow(ctx,
		`SELECT listen_http_addr, listen_http_port, listen_pg_addr, listen_pg_port
		   FROM registry.nodes WHERE node_id = $1`,
		req.NodeID).Scan(&hAddr, &hPort, &pAddr, &pPort); err != nil {
		return fmt.Errorf("lookup node: %w", err)
	}
	if hAddr != req.ListenHTTPAddr || hPort != int32(req.ListenHTTPPort) ||
		pAddr != req.ListenPgAddr || pPort != int32(req.ListenPgPort) {
		return ErrConflict
	}
	return nil
}

// NodeList returns every registered node. Availability is reported as "Active":
// the registry is bookkeeping only, with no liveness tracking.
func (s *Store) NodeList(ctx context.Context) ([]NodeDescribeResponse, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT node_id, listen_http_addr, listen_http_port, listen_https_port,
		        listen_pg_addr, listen_pg_port, listen_grpc_addr, listen_grpc_port
		   FROM registry.nodes ORDER BY node_id`)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()

	out := []NodeDescribeResponse{}
	for rows.Next() {
		var n NodeDescribeResponse
		var httpsPort, grpcPort *int32
		if err := rows.Scan(&n.ID, &n.ListenHTTPAddr, &n.ListenHTTPPort, &httpsPort,
			&n.ListenPgAddr, &n.ListenPgPort, &n.ListenGrpcAddr, &grpcPort); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		n.Availability = "Active"
		n.ListenHTTPSPort = intToU16(httpsPort)
		n.ListenGrpcPort = intToU16(grpcPort)
		out = append(out, n)
	}
	return out, rows.Err()
}

// NodeConfigure accepts an availability update. Availability is not tracked
// (no liveness heartbeat), so this is a no-op retained for wire compatibility.
func (s *Store) NodeConfigure(ctx context.Context, req NodeConfigureRequest) error {
	return nil
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

// u16ToInt widens an optional uint16 to an optional int32 for the INTEGER port
// columns (nil stays NULL).
func u16ToInt(p *uint16) *int32 {
	if p == nil {
		return nil
	}
	v := int32(*p)
	return &v
}

// intToU16 narrows an optional INTEGER port column back to an optional uint16.
func intToU16(p *int32) *uint16 {
	if p == nil {
		return nil
	}
	v := uint16(*p)
	return &v
}
