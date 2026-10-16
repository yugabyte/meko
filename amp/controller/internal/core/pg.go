package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/controller/internal/db"
)

// PgBackend is the YBDB-backed Backend. Inserts generate the row id in the
// controller (uuid.New) instead of relying on the DB default, and use
// INSERT ... ON CONFLICT (id) DO NOTHING followed by a read-back, so
// re-executing the *same* statement (same id) converges instead of erroring.
// That is defensive: the pgxpool path does not auto-retry, and request-level
// idempotency (a client-supplied idempotency key) is out of scope for #92 —
// a retried API call generates a fresh id and creates a distinct row. UUID
// columns are projected ::text and parsed, the same convention the task store
// uses, to avoid a pgx uuid codec.
type PgBackend struct {
	pool *db.Pool
}

// NewPgBackend returns a Backend backed by pool.
func NewPgBackend(pool *db.Pool) *PgBackend {
	return &PgBackend{pool: pool}
}

func (b *PgBackend) Healthy(ctx context.Context) error {
	return b.pool.Healthy(ctx)
}

func (b *PgBackend) CreateUser(ctx context.Context, isAdmin bool) (User, error) {
	return b.EnsureUser(ctx, uuid.New(), isAdmin)
}

func (b *PgBackend) EnsureUser(ctx context.Context, id uuid.UUID, isAdmin bool) (User, error) {
	if _, err := b.pool.Exec(ctx,
		`INSERT INTO amp_registry.users (id, is_admin) VALUES ($1::uuid, $2)
		 ON CONFLICT (id) DO NOTHING`,
		id.String(), isAdmin); err != nil {
		return User{}, fmt.Errorf("insert user: %w", err)
	}
	// The row is durable from here, so a failed read-back is not the API's to
	// report as retryable (db.AfterWrite). Conservative for a caller that passed
	// its own id — that replay would converge — but this cannot tell it apart
	// from CreateUser's generated one, which would mint a second user.
	u, err := b.GetUser(ctx, id)
	if err != nil {
		return User{}, db.AfterWrite(err)
	}
	return u, nil
}

func (b *PgBackend) GetUser(ctx context.Context, id uuid.UUID) (User, error) {
	row := b.pool.QueryRow(ctx,
		`SELECT id::text, is_admin, created_at FROM amp_registry.users WHERE id = $1::uuid`,
		id.String())
	u, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// poolLive excludes tombstoned (dropped) pools. A pool is tombstoned rather
// than deleted so its own tombstoned clusters keep a valid storage_pool_id (see
// PgBackend.DeleteStoragePool), which means every read has to filter them out
// explicitly. Unqualified, so it only composes into the pool-only statements
// below -- clusterSelect and branchSelect alias the table `sp` and would need
// their own qualified predicate. They do not carry one: a live cluster or branch
// under a tombstoned pool is the state the EnsureCluster interlock exists to
// prevent, not one the read paths are expected to filter out.
const poolLive = `dropped_at IS NULL`

func (b *PgBackend) CreateStoragePool(ctx context.Context, owner uuid.UUID, name string) (StoragePool, error) {
	return b.CreateStoragePoolWithID(ctx, uuid.New(), owner, name)
}

func (b *PgBackend) CreateStoragePoolWithID(ctx context.Context, id, owner uuid.UUID, name string) (StoragePool, error) {
	_, err := b.pool.Exec(ctx,
		`INSERT INTO amp_registry.storage_pools (id, name, owner_user_id) VALUES ($1::uuid, $2, $3::uuid)
		 ON CONFLICT (id) DO NOTHING`,
		id.String(), name, owner.String())
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23505": // unique_violation: the pool name is already taken
				return StoragePool{}, ErrConflict
			case "23503": // foreign_key_violation: the owner user does not exist
				return StoragePool{}, ErrNotFound
			}
		}
		return StoragePool{}, fmt.Errorf("insert storage pool: %w", err)
	}
	pool, err := b.getStoragePoolByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		// The insert was a no-op and no live pool has the id, so a tombstoned pool
		// holds it. Ids are never reused (the tombstone keeps the PK), so this is a
		// conflict rather than the idempotent re-create the caller asked for.
		return StoragePool{}, ErrConflict
	}
	if err != nil {
		return StoragePool{}, db.AfterWrite(err)
	}
	return pool, nil
}

func (b *PgBackend) ListUsers(ctx context.Context, limit, offset int) ([]User, int, error) {
	total, err := b.count(ctx, `SELECT count(*) FROM amp_registry.users`)
	if err != nil {
		return nil, 0, fmt.Errorf("count users: %w", err)
	}
	rows, err := b.pool.Query(ctx,
		`SELECT id::text, is_admin, created_at
		 FROM amp_registry.users ORDER BY created_at, id LIMIT $1 OFFSET $2`,
		limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list users: %w", err)
	}
	return users, total, nil
}

func (b *PgBackend) ListStoragePools(ctx context.Context, limit, offset int) ([]StoragePool, int, error) {
	total, err := b.count(ctx, `SELECT count(*) FROM amp_registry.storage_pools WHERE `+poolLive)
	if err != nil {
		return nil, 0, fmt.Errorf("count storage pools: %w", err)
	}
	rows, err := b.pool.Query(ctx,
		`SELECT id::text, name, owner_user_id::text, created_at
		 FROM amp_registry.storage_pools WHERE `+poolLive+`
		 ORDER BY created_at, id LIMIT $1 OFFSET $2`,
		limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list storage pools: %w", err)
	}
	defer rows.Close()

	var pools []StoragePool
	for rows.Next() {
		p, err := scanPool(rows)
		if err != nil {
			return nil, 0, err
		}
		pools = append(pools, p)
	}
	// rows.Next returns false on both end-of-rows and error; check Err so an
	// iteration failure isn't mistaken for a complete (but truncated) result.
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list storage pools: %w", err)
	}
	return pools, total, nil
}

func (b *PgBackend) GetStoragePool(ctx context.Context, poolID uuid.UUID) (StoragePool, error) {
	p, err := b.getStoragePoolByID(ctx, poolID)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoragePool{}, ErrNotFound
	}
	return p, err
}

func (b *PgBackend) RenameStoragePool(ctx context.Context, poolID uuid.UUID, name string) (StoragePool, error) {
	row := b.pool.QueryRow(ctx,
		`UPDATE amp_registry.storage_pools SET name = $2 WHERE id = $1::uuid AND `+poolLive+`
		 RETURNING id::text, name, owner_user_id::text, created_at`,
		poolID.String(), name)
	p, err := scanPool(row)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return StoragePool{}, ErrNotFound
	case err != nil:
		var pgErr *pgconn.PgError
		// unique_violation on storage_pools_name_live: a live pool holds the name.
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return StoragePool{}, ErrConflict
		}
		return StoragePool{}, fmt.Errorf("rename storage pool: %w", err)
	}
	return p, nil
}

// The tombstone UPDATE follows other statements in its transaction, so YSQL's
// query-layer retry cannot absorb a write-write conflict on the pool row and the
// 40001 reaches us. The writer that conflicts by design is EnsureCluster's pool
// interlock; when the cluster wins, the re-run sees it and answers ErrConflict.
// The transaction rolled back in full, so it is re-run here rather than answered
// as a 500 (#1849).
func (b *PgBackend) DeleteStoragePool(ctx context.Context, poolID uuid.UUID) error {
	return db.RetryAborted(ctx, func() error { return b.deleteStoragePool(ctx, poolID) })
}

func (b *PgBackend) deleteStoragePool(ctx context.Context, poolID uuid.UUID) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	id := poolID.String()
	// Only LIVE clusters block the delete. A tombstoned cluster is unrecoverable
	// history whose data the branch reaper has already reclaimed, and its row is
	// kept forever — waiting for it to go away would make the pool undeletable.
	var hasClusters bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM amp_registry.clusters
		     WHERE storage_pool_id = $1::uuid AND dropped_at IS NULL)`,
		id).Scan(&hasClusters); err != nil {
		return fmt.Errorf("check pool clusters: %w", err)
	}
	if hasClusters {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM amp_registry.user_pool_grants WHERE storage_pool_id = $1::uuid`, id); err != nil {
		return fmt.Errorf("delete pool grants: %w", err)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE amp_registry.storage_pools SET dropped_at = now() WHERE id = $1::uuid AND `+poolLive,
		id)
	if err != nil {
		return fmt.Errorf("tombstone storage pool: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delete storage pool: %w", err)
	}
	return nil
}

func (b *PgBackend) CreateCluster(ctx context.Context, poolID uuid.UUID, name string, pgVersion int) (Cluster, error) {
	clusterID := uuid.New()
	branchID := uuid.New()

	// The cluster and its default branch are written together: a cluster with no
	// branch is not a valid resource. Both inserts are ON CONFLICT (pk) DO
	// NOTHING so re-executing the same statement (same ids) converges; the
	// cluster-name collision surfaces as 23505 on the (storage_pool_id, name)
	// unique constraint, which is not the conflict target and so still errors.
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return Cluster{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`INSERT INTO amp_registry.clusters (id, storage_pool_id, name) VALUES ($1::uuid, $2::uuid, $3)
		 ON CONFLICT (id) DO NOTHING`,
		clusterID.String(), poolID.String(), name); err != nil {
		return Cluster{}, mapClusterErr("insert cluster", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO amp_registry.branches (cluster_id, branch_id, name, pg_version, is_default)
		 VALUES ($1::uuid, $2::uuid, $3, $4, true)
		 ON CONFLICT (branch_id) DO NOTHING`,
		clusterID.String(), branchID.String(), DefaultBranchName, pgVersion); err != nil {
		return Cluster{}, mapClusterErr("insert default branch", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Cluster{}, fmt.Errorf("commit create cluster: %w", err)
	}
	return b.getCluster(ctx, poolID, clusterID)
}

// clusterSelect joins cluster -> storage_pools and its default branch so a
// cluster is only visible through the pool it belongs to, and always carries
// its default branch. Pools are top-level, so this is not owner-scoped.
//
// The default-branch join filters tombstones inline; the cluster's own tombstone
// filter is clusterLive, which callers append alongside their WHERE.
const clusterSelect = `SELECT p.id::text, p.storage_pool_id::text, p.name, p.created_at, p.engine, p.tags,
		b.cluster_id::text, b.branch_id::text, b.name, b.pg_version, b.is_default, b.parent_branch_id::text, b.fork_lsn::text, b.extensions, b.created_at
	FROM amp_registry.clusters p
	JOIN amp_registry.storage_pools sp ON sp.id = p.storage_pool_id
	JOIN amp_registry.branches b ON b.cluster_id = p.id AND b.is_default AND b.dropped_at IS NULL`

// clusterLive excludes tombstoned (dropped) clusters. Clusters are tombstoned
// rather than deleted so their tombstoned branches keep a valid parent (see
// PgBackend.DeleteCluster), which means every read has to filter them out
// explicitly. Assumes the cluster table is aliased `p`.
const clusterLive = ` AND p.dropped_at IS NULL`

// clusterCreateNotFailed excludes a cluster whose create_cluster task failed.
// The cluster row is written mid-provisioning (the create_cluster step's
// EnsureCluster) before the later storage steps run, so a task that fails
// afterward leaves an orphan cluster that should not surface as a real cluster.
// Each cluster has exactly one create_cluster task — a retry mints a fresh
// cluster id — so a single failed task is definitive. The failed task itself is
// left intact so it still shows on the tasks page. Assumes the cluster table is
// aliased `p`.
const clusterCreateNotFailed = ` AND NOT EXISTS (
		SELECT 1 FROM tasks t
		WHERE t.cluster_id = p.id AND t.kind = 'create_cluster' AND t.status = 'failed')`

func (b *PgBackend) getCluster(ctx context.Context, poolID, clusterID uuid.UUID) (Cluster, error) {
	row := b.pool.QueryRow(ctx,
		clusterSelect+` WHERE p.id = $1::uuid AND sp.id = $2::uuid`+clusterLive,
		clusterID.String(), poolID.String())
	p, err := scanCluster(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cluster{}, ErrNotFound
	}
	return p, err
}

func (b *PgBackend) GetCluster(ctx context.Context, poolID, clusterID uuid.UUID) (Cluster, error) {
	return b.getCluster(ctx, poolID, clusterID)
}

func (b *PgBackend) ListClusters(ctx context.Context, poolID uuid.UUID, limit, offset int, includeFailed bool) ([]Cluster, int, error) {
	// A missing pool 404s rather than returning an empty page.
	if _, err := b.GetStoragePool(ctx, poolID); err != nil {
		return nil, 0, err
	}
	// Scope to usable clusters unless the caller opts in: exclude failed-creation
	// orphans from both the page and its total. Included only when includeFailed
	// is set (an operator's fleet view), so they can be seen and deleted.
	filter := ""
	if !includeFailed {
		filter = clusterCreateNotFailed
	}
	// The count must match the page: clusterSelect INNER-JOINs the default branch,
	// so a cluster with no is_default branch never appears in the page — require
	// one in the count too, or total would over-report and a cluster would be
	// "counted but never returned", breaking pagination.
	total, err := b.count(ctx,
		`SELECT count(*) FROM amp_registry.clusters p
		 WHERE p.storage_pool_id = $1::uuid
		   AND EXISTS (SELECT 1 FROM amp_registry.branches b
		               WHERE b.cluster_id = p.id AND b.is_default AND b.dropped_at IS NULL)`+clusterLive+filter,
		poolID.String())
	if err != nil {
		return nil, 0, fmt.Errorf("count clusters: %w", err)
	}
	rows, err := b.pool.Query(ctx,
		clusterSelect+` WHERE sp.id = $1::uuid`+clusterLive+filter+`
		 ORDER BY p.created_at, p.id LIMIT $2 OFFSET $3`,
		poolID.String(), limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list clusters: %w", err)
	}
	defer rows.Close()

	var clusters []Cluster
	for rows.Next() {
		p, err := scanCluster(rows)
		if err != nil {
			return nil, 0, err
		}
		clusters = append(clusters, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list clusters: %w", err)
	}
	return clusters, total, nil
}

func (b *PgBackend) CreateBranch(ctx context.Context, poolID, clusterID, parentBranchID uuid.UUID, name, forkLSN string) (Branch, error) {
	// Validate the cluster within the pool first (a cluster not in the pool 404s).
	if err := b.clusterInPool(ctx, poolID, clusterID); err != nil {
		return Branch{}, err
	}

	branchID := uuid.New()
	// is_default = false; record the fork ancestry. pg_version is inherited from
	// the parent (the SELECT source) so a branch and its ancestor never drift.
	// The SELECT is keyed by the parent within the cluster; a missing parent
	// inserts nothing, so the read-back 404s. ON CONFLICT (pk) DO NOTHING makes a
	// same-id retry converge; a duplicate name surfaces as 23505 on
	// branches_name_per_cluster -> ErrConflict. forkLSN is validated to be a
	// well-formed LSN by the caller before it reaches the pg_lsn cast. A
	// tombstoned parent selects nothing, so forking a dropped branch 404s
	// rather than resurrecting its lineage. Nor does a parent whose floor is
	// above forkLSN, which is ErrForkBelowFloor.
	tag, err := b.pool.Exec(ctx,
		`INSERT INTO amp_registry.branches (cluster_id, branch_id, name, pg_version, is_default, parent_branch_id, fork_lsn)
		 SELECT $1::uuid, $2::uuid, $3, p.pg_version, false, $4::uuid, $5::pg_lsn
		 FROM amp_registry.branches p
		 WHERE p.cluster_id = $1::uuid AND p.branch_id = $4::uuid AND p.dropped_at IS NULL
		   AND (p.fork_lsn IS NULL OR p.fork_lsn <= $5::pg_lsn)
		 ON CONFLICT (branch_id) DO NOTHING`,
		clusterID.String(), branchID.String(), name, parentBranchID.String(), forkLSN)
	if err != nil {
		return Branch{}, mapBranchErr("insert branch", err)
	}
	if tag.RowsAffected() == 0 {
		if err := forkBelowFloor(ctx, b.pool, clusterID, parentBranchID, forkLSN); err != nil {
			return Branch{}, err
		}
	}
	return b.getBranchByID(ctx, clusterID, branchID)
}

func (b *PgBackend) getBranchByID(ctx context.Context, clusterID, branchID uuid.UUID) (Branch, error) {
	row := b.pool.QueryRow(ctx,
		`SELECT `+branchColumns+`
		 FROM amp_registry.branches b
		 WHERE b.cluster_id = $1::uuid AND b.branch_id = $2::uuid AND b.dropped_at IS NULL`,
		clusterID.String(), branchID.String())
	br, err := scanBranch(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Branch{}, ErrNotFound
	}
	return br, err
}

// branchColumns is the column list scanBranch reads, in order. Every branch
// query builds on this ONE constant, and every one of them aliases the table
// `b`, so a column added to the projection cannot reach one scanBranch caller
// and miss another — which is exactly how a query and its scanner drift out of
// step and start answering 500.
const branchColumns = `b.cluster_id::text, b.branch_id::text, b.name, b.pg_version, b.is_default, b.parent_branch_id::text, b.fork_lsn::text, b.pg_settings, b.write_token_settings, b.extensions, b.ycp_generation, b.created_at`

// branchSelect joins branch -> cluster -> storage_pools so a branch is only
// visible through the cluster (and pool) it belongs to. Tombstoned clusters are
// filtered in the join; the branch's own tombstone filter is branchLive.
const branchSelect = `SELECT ` + branchColumns + `
	FROM amp_registry.branches b
	JOIN amp_registry.clusters p ON p.id = b.cluster_id AND p.dropped_at IS NULL
	JOIN amp_registry.storage_pools sp ON sp.id = p.storage_pool_id`

// branchLive excludes tombstoned (dropped) branches. Assumes `b` is the branch.
const branchLive = ` AND b.dropped_at IS NULL`

func (b *PgBackend) GetBranch(ctx context.Context, poolID, clusterID, branchID uuid.UUID) (Branch, error) {
	row := b.pool.QueryRow(ctx,
		branchSelect+` WHERE b.branch_id = $1::uuid AND b.cluster_id = $2::uuid AND sp.id = $3::uuid`+branchLive,
		branchID.String(), clusterID.String(), poolID.String())
	br, err := scanBranch(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Branch{}, ErrNotFound
	}
	return br, err
}

func (b *PgBackend) ListBranches(ctx context.Context, poolID, clusterID uuid.UUID, limit, offset int) ([]Branch, int, error) {
	// A missing pool or cluster 404s rather than returning an empty page.
	if err := b.clusterInPool(ctx, poolID, clusterID); err != nil {
		return nil, 0, err
	}
	total, err := b.count(ctx,
		`SELECT count(*) FROM amp_registry.branches WHERE cluster_id = $1::uuid AND dropped_at IS NULL`,
		clusterID.String())
	if err != nil {
		return nil, 0, fmt.Errorf("count branches: %w", err)
	}
	rows, err := b.pool.Query(ctx,
		`SELECT `+branchColumns+`
		 FROM amp_registry.branches b
		 WHERE b.cluster_id = $1::uuid AND b.dropped_at IS NULL
		 ORDER BY b.created_at, b.branch_id LIMIT $2 OFFSET $3`,
		clusterID.String(), limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list branches: %w", err)
	}
	defer rows.Close()

	var branches []Branch
	for rows.Next() {
		br, err := scanBranch(rows)
		if err != nil {
			return nil, 0, err
		}
		branches = append(branches, br)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list branches: %w", err)
	}
	return branches, total, nil
}

// clusterInPool returns ErrNotFound unless a live clusterID exists under poolID.
func (b *PgBackend) clusterInPool(ctx context.Context, poolID, clusterID uuid.UUID) error {
	var x int
	err := b.pool.QueryRow(ctx,
		`SELECT 1 FROM amp_registry.clusters
		  WHERE id = $1::uuid AND storage_pool_id = $2::uuid AND dropped_at IS NULL`,
		clusterID.String(), poolID.String()).Scan(&x)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("resolve cluster in pool: %w", err)
	}
	return nil
}

// mapClusterErr translates the constraint violations the cluster writes can
// raise: a duplicate cluster name among live clusters (clusters_name_per_pool)
// -> ErrConflict, and a missing pool (foreign key on cluster.storage_pool_id)
// -> ErrNotFound.
func mapClusterErr(what string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation: the cluster name is already taken in the pool
			return ErrConflict
		case "23503": // foreign_key_violation: the pool no longer exists
			return ErrNotFound
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

// mapBranchErr translates the constraint violations a branch write can raise: a
// duplicate branch name among live branches (branches_name_per_cluster) ->
// ErrConflict, and a missing in-cluster parent (foreign key) -> ErrNotFound.
func mapBranchErr(what string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation: the branch name is already taken in the cluster
			return ErrConflict
		case "23503": // foreign_key_violation: the in-cluster parent does not exist
			return ErrNotFound
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

func (b *PgBackend) getStoragePoolByID(ctx context.Context, id uuid.UUID) (StoragePool, error) {
	row := b.pool.QueryRow(ctx,
		`SELECT id::text, name, owner_user_id::text, created_at
		 FROM amp_registry.storage_pools WHERE id = $1::uuid AND `+poolLive,
		id.String())
	return scanPool(row)
}

func (b *PgBackend) CreateEndpoint(ctx context.Context, clusterID, branchID uuid.UUID, role string, config []byte) (Endpoint, error) {
	id := uuid.New()
	var cfg any
	if len(config) > 0 {
		cfg = config
	}
	_, err := b.pool.Exec(ctx,
		`INSERT INTO amp_registry.endpoints (id, branch_id, role, config)
		 VALUES ($1::uuid, $2::uuid, $3::amp_registry.endpoint_role, $4::jsonb)
		 ON CONFLICT (id) DO NOTHING`,
		id.String(), branchID.String(), role, cfg)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23505": // unique_violation: a read_write endpoint already exists (endpoints_one_writer_per_branch)
				return Endpoint{}, ErrConflict
			case "23503": // foreign_key_violation: the branch does not exist
				return Endpoint{}, ErrNotFound
			}
		}
		return Endpoint{}, fmt.Errorf("insert endpoint: %w", err)
	}
	return b.getEndpointByID(ctx, id)
}

// endpointSelect projects an endpoint row; address is NULL until the endpoint
// is deployed (SetEndpointAddress), so it is coalesced to the empty string to
// match Endpoint.Address being a plain string.
// An endpoint has no cluster_id of its own — it reaches its cluster through its
// branch — so every projection joins `branches` to supply it. That join is also
// what scopes a query to a cluster. The ingress name is LEFT JOINed from the live
// allocation, as in endpointDeploySelect; at most one is live per endpoint.
const endpointSelect = `SELECT e.id::text, eb.cluster_id::text, e.branch_id::text, e.role::text, COALESCE(e.name, ''), COALESCE(e.address, ''), COALESCE(e.tailnet_address, ''), COALESCE(e.config->>'cpu', ''), COALESCE(e.config->>'memory', ''), COALESCE(e.version, ''), COALESCE(e.target_version, ''), COALESCE(e.config->>'connectivity', ''), e.config->'allowed_cidrs', e.pg_settings, e.pageserver_settings, e.safekeeper_settings, e.config->'extended', e.tags, e.ycp_generation, COALESCE(ia.dns_name, ''), e.created_at, e.config->'tolerations', e.config->'topology_spread_constraints'
	FROM amp_registry.endpoints e
	JOIN amp_registry.branches eb ON eb.branch_id = e.branch_id
	LEFT JOIN amp_registry.ingress_allocations ia
	       ON ia.endpoint_id = e.id AND ia.released_at IS NULL`

func (b *PgBackend) getEndpointByID(ctx context.Context, id uuid.UUID) (Endpoint, error) {
	row := b.pool.QueryRow(ctx, endpointSelect+` WHERE e.id = $1::uuid`, id.String())
	return scanEndpoint(row)
}

func (b *PgBackend) GetEndpoint(ctx context.Context, poolID, clusterID, branchID, endpointID uuid.UUID) (Endpoint, error) {
	row := b.pool.QueryRow(ctx,
		endpointSelect+`
		 JOIN amp_registry.clusters p ON p.id = eb.cluster_id AND p.dropped_at IS NULL
		 JOIN amp_registry.storage_pools sp ON sp.id = p.storage_pool_id
		 WHERE e.id = $1::uuid AND eb.branch_id = $2::uuid AND eb.cluster_id = $3::uuid
		   AND eb.dropped_at IS NULL AND sp.id = $4::uuid`,
		endpointID.String(), branchID.String(), clusterID.String(), poolID.String())
	ep, err := scanEndpoint(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Endpoint{}, ErrNotFound
	}
	return ep, err
}

func (b *PgBackend) ListEndpoints(ctx context.Context, poolID, clusterID, branchID uuid.UUID, limit, offset int) ([]Endpoint, int, error) {
	// Resolve the branch within the cluster and pool first. No row means the
	// pool, cluster, or branch does not exist -> 404.
	if _, err := b.GetBranch(ctx, poolID, clusterID, branchID); err != nil {
		return nil, 0, err
	}
	total, err := b.count(ctx,
		`SELECT count(*) FROM amp_registry.endpoints e
		  JOIN amp_registry.branches b ON b.branch_id = e.branch_id
		 WHERE b.cluster_id = $1::uuid AND e.branch_id = $2::uuid`,
		clusterID.String(), branchID.String())
	if err != nil {
		return nil, 0, fmt.Errorf("count endpoints: %w", err)
	}
	rows, err := b.pool.Query(ctx,
		endpointSelect+` WHERE eb.cluster_id = $1::uuid AND e.branch_id = $2::uuid
		 ORDER BY e.created_at, e.id LIMIT $3 OFFSET $4`,
		clusterID.String(), branchID.String(), limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list endpoints: %w", err)
	}
	defer rows.Close()

	var endpoints []Endpoint
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, 0, err
		}
		endpoints = append(endpoints, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list endpoints: %w", err)
	}
	return endpoints, total, nil
}

func (b *PgBackend) SetEndpointAddress(ctx context.Context, endpointID uuid.UUID, address, tailnetAddress string) error {
	_, err := b.pool.Exec(ctx,
		`UPDATE amp_registry.endpoints SET address = $2, tailnet_address = $3 WHERE id = $1::uuid`,
		endpointID.String(), address, tailnetAddress)
	if err != nil {
		return fmt.Errorf("set endpoint address: %w", err)
	}
	return nil
}

func (b *PgBackend) DeleteEndpoint(ctx context.Context, endpointID uuid.UUID) error {
	_, err := b.pool.Exec(ctx, `DELETE FROM amp_registry.endpoints WHERE id = $1::uuid`, endpointID.String())
	if err != nil {
		return fmt.Errorf("delete endpoint: %w", err)
	}
	return nil
}

// UpdateEndpointResources merges the new cpu/memory into the endpoint's config
// jsonb (the spec used to (re)create it), leaving the other config fields intact.
// This is the durable, DB-side half of a resize: a controller-driven recreate
// re-renders from this config, so the new size survives a redeploy. Idempotent;
// an absent row is a no-op.
func (b *PgBackend) UpdateEndpointResources(ctx context.Context, endpointID uuid.UUID, cpu, memory string) error {
	_, err := b.pool.Exec(ctx,
		`UPDATE amp_registry.endpoints
		    SET config = COALESCE(config, '{}'::jsonb) || jsonb_build_object('cpu', $2::text, 'memory', $3::text)
		  WHERE id = $1::uuid`,
		endpointID.String(), cpu, memory)
	if err != nil {
		return fmt.Errorf("update endpoint resources: %w", err)
	}
	return nil
}

// UpdateEndpointAllowedCIDRs replaces config.allowed_cidrs, leaving the other
// config fields intact. An empty list is stored as [], which reads back as no
// restriction. Idempotent; an absent row is a no-op.
func (b *PgBackend) UpdateEndpointAllowedCIDRs(ctx context.Context, endpointID uuid.UUID, cidrs []string) error {
	if cidrs == nil {
		cidrs = []string{}
	}
	payload, err := json.Marshal(cidrs)
	if err != nil {
		return fmt.Errorf("marshal allowed cidrs: %w", err)
	}
	if _, err := b.pool.Exec(ctx,
		`UPDATE amp_registry.endpoints
		    SET config = COALESCE(config, '{}'::jsonb) || jsonb_build_object('allowed_cidrs', $2::jsonb)
		  WHERE id = $1::uuid`,
		endpointID.String(), payload); err != nil {
		return fmt.Errorf("update endpoint allowed cidrs: %w", err)
	}
	return nil
}

// UpdateEndpointScheduling replaces the scheduling lists that are non-nil,
// leaving the other config fields — and a nil list — intact. An empty list
// removes its key, so a cleared endpoint reads back exactly like one created
// without it. Idempotent; an absent row is a no-op.
func (b *PgBackend) UpdateEndpointScheduling(ctx context.Context, endpointID uuid.UUID, sch Scheduling) error {
	drop, set, err := schedulingConfigPatch(sch)
	if err != nil {
		return err
	}
	if _, err := b.pool.Exec(ctx,
		`UPDATE amp_registry.endpoints
		    SET config = (COALESCE(config, '{}'::jsonb) - $2::text[]) || $3::jsonb
		  WHERE id = $1::uuid`,
		endpointID.String(), drop, set); err != nil {
		return fmt.Errorf("update endpoint scheduling: %w", err)
	}
	return nil
}

// SetEndpointYCPGeneration records the YCP desired-state generation fully applied
// to an endpoint (meko#985). Called by the task worker once every step of the
// task carrying it succeeded, so the value means "the work landed" rather than
// "the controller accepted a 202" — which is what lets the reconcile agent
// re-issue an update whose task failed instead of latching it as applied.
//
// MONOTONIC, not an assignment: GREATEST keeps the higher value, so a task
// carrying an older generation that completes after a newer one cannot drag the
// column backwards and make the agent re-apply what already landed. Idempotent;
// an absent row is a no-op.
func (b *PgBackend) SetEndpointYCPGeneration(ctx context.Context, endpointID uuid.UUID, gen int64) error {
	_, err := b.pool.Exec(ctx,
		`UPDATE amp_registry.endpoints
		    SET ycp_generation = GREATEST(COALESCE(ycp_generation, 0), $2::bigint)
		  WHERE id = $1::uuid`,
		endpointID.String(), gen)
	if err != nil {
		return fmt.Errorf("set endpoint ycp generation: %w", err)
	}
	return nil
}

// SetBranchYCPGeneration is SetEndpointYCPGeneration for a branch. A tombstoned
// branch is skipped: its row survives the drop forever (a branch id is never
// reused) and nothing will reconcile it again.
func (b *PgBackend) SetBranchYCPGeneration(ctx context.Context, branchID uuid.UUID, gen int64) error {
	_, err := b.pool.Exec(ctx,
		`UPDATE amp_registry.branches
		    SET ycp_generation = GREATEST(COALESCE(ycp_generation, 0), $2::bigint)
		  WHERE branch_id = $1::uuid AND dropped_at IS NULL`,
		branchID.String(), gen)
	if err != nil {
		return fmt.Errorf("set branch ycp generation: %w", err)
	}
	return nil
}

// SetEndpointVersion records the version the endpoint runs — an absolute UPDATE
// of the row's own column, the same shape as SetEndpointAddress and the DB-side
// half of an upgrade. Idempotent; an absent row is a no-op.
//
// The column is the desired state every controller-driven redeploy renders from
// (BranchEndpointDeploys reads it), so writing it is what stops the next redeploy
// reverting the endpoint to the deployer's configured default.
func (b *PgBackend) SetEndpointVersion(ctx context.Context, endpointID uuid.UUID, version string) error {
	_, err := b.pool.Exec(ctx,
		`UPDATE amp_registry.endpoints SET version = $2 WHERE id = $1::uuid`,
		endpointID.String(), version)
	if err != nil {
		return fmt.Errorf("set endpoint version: %w", err)
	}
	return nil
}

// SetEndpointTargetVersion records the version the endpoint SHOULD run — the
// upgrade's intent, written before the deploy. It differing from `version` is an
// upgrade that has not converged. Idempotent; an absent row is a no-op.
func (b *PgBackend) SetEndpointTargetVersion(ctx context.Context, endpointID uuid.UUID, version string) error {
	_, err := b.pool.Exec(ctx,
		`UPDATE amp_registry.endpoints SET target_version = $2 WHERE id = $1::uuid`,
		endpointID.String(), version)
	if err != nil {
		return fmt.Errorf("set endpoint target version: %w", err)
	}
	return nil
}

// UpdateBranchSettings writes the branch-wide settings the caller named, and
// stamps the YCP generation the write applies.
//
// One statement with a COALESCE per column rather than a built-up SET list: an
// omitted surface passes NULL and keeps what is stored, so the shape of the
// query does not depend on which fields the caller happened to send. Idempotent;
// an absent row is a no-op.
//
// The generation rides that same statement so the settings and the record of
// which generation they satisfy cannot land apart. Unlike the task path, where
// the worker stamps after every step succeeded, this verb IS the work — it
// returns once the row is written — so the handler is the right place for it.
func (b *PgBackend) UpdateBranchSettings(ctx context.Context, branchID uuid.UUID, pg *map[string]string, writeToken *map[string]any, ycpGeneration *int64) error {
	pgJSON, err := settingsJSON(pg)
	if err != nil {
		return fmt.Errorf("encode branch pg_settings: %w", err)
	}
	wtJSON, err := settingsJSON(writeToken)
	if err != nil {
		return fmt.Errorf("encode branch write_token_settings: %w", err)
	}
	stamp, args := ycpGenerationSet("$4", ycpGeneration)
	_, err = b.pool.Exec(ctx,
		`UPDATE amp_registry.branches
		    SET pg_settings          = COALESCE($2::jsonb, pg_settings),
		        write_token_settings = COALESCE($3::jsonb, write_token_settings)`+stamp+`
		  WHERE branch_id = $1::uuid`,
		append([]any{branchID.String(), pgJSON, wtJSON}, args...)...)
	if err != nil {
		return fmt.Errorf("update branch settings: %w", err)
	}
	return nil
}

// UpdateEndpointSettings writes the endpoint settings the caller named, on the
// same terms as UpdateBranchSettings, generation included.
func (b *PgBackend) UpdateEndpointSettings(ctx context.Context, endpointID uuid.UUID, pg *map[string]string, pageserver, safekeeper *map[string]any, ycpGeneration *int64) error {
	pgJSON, err := settingsJSON(pg)
	if err != nil {
		return fmt.Errorf("encode endpoint pg_settings: %w", err)
	}
	psJSON, err := settingsJSON(pageserver)
	if err != nil {
		return fmt.Errorf("encode endpoint pageserver_settings: %w", err)
	}
	skJSON, err := settingsJSON(safekeeper)
	if err != nil {
		return fmt.Errorf("encode endpoint safekeeper_settings: %w", err)
	}
	stamp, args := ycpGenerationSet("$5", ycpGeneration)
	_, err = b.pool.Exec(ctx,
		`UPDATE amp_registry.endpoints
		    SET pg_settings         = COALESCE($2::jsonb, pg_settings),
		        pageserver_settings = COALESCE($3::jsonb, pageserver_settings),
		        safekeeper_settings = COALESCE($4::jsonb, safekeeper_settings)`+stamp+`
		  WHERE id = $1::uuid`,
		append([]any{endpointID.String(), pgJSON, psJSON, skJSON}, args...)...)
	if err != nil {
		return fmt.Errorf("update endpoint settings: %w", err)
	}
	return nil
}

// ycpGenerationSet folds a generation stamp into a statement that is writing
// something else, returning the SET fragment to append and the arg to append with
// it. Both are empty when no generation was supplied.
//
// "Leave the column alone" is the ABSENCE of an assignment, not an expression that
// evaluates to the old value. That distinction is the whole reason this is built
// rather than inlined: the obvious `GREATEST(COALESCE(ycp_generation, 0), $n)`
// rewrites an unstamped row's NULL to 0 whenever $n is NULL, and guarding it with
// `CASE WHEN $n IS NULL THEN ycp_generation ELSE ... END` does not work on
// YugabyteDB — that form wrote NULL over a stamped row, so the column is simply
// left out of the statement instead. Nothing here depends on how the engine
// evaluates a NULL parameter.
//
// When it is present the expression is SetBranchYCPGeneration's verbatim: an older
// generation arriving late cannot drag the column backwards.
func ycpGenerationSet(param string, gen *int64) (string, []any) {
	if gen == nil {
		return "", nil
	}
	return `,
		        ycp_generation = GREATEST(COALESCE(ycp_generation, 0), ` + param + `::bigint)`,
		[]any{*gen}
}

// settingsJSON encodes a settings map for the COALESCE above: nil means "leave
// it", and an empty map means "clear it" — which is why nil and `{}` cannot be
// collapsed into one case here.
func settingsJSON[T any](m *map[string]T) (any, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(*m)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// count runs a scalar COUNT(*) query and returns its result.
func (b *PgBackend) count(ctx context.Context, sql string, args ...any) (int, error) {
	var n int
	if err := b.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// scanRow is satisfied by both pgx.Row and pgx.Rows.
type scanRow interface {
	Scan(dest ...any) error
}

func scanUser(r scanRow) (User, error) {
	var (
		idStr string
		u     User
	)
	if err := r.Scan(&idStr, &u.IsAdmin, &u.CreatedAt); err != nil {
		return User{}, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return User{}, fmt.Errorf("parse user id: %w", err)
	}
	u.ID = id
	return u, nil
}

func scanPool(r scanRow) (StoragePool, error) {
	var (
		idStr    string
		ownerStr string
		p        StoragePool
	)
	if err := r.Scan(&idStr, &p.Name, &ownerStr, &p.CreatedAt); err != nil {
		return StoragePool{}, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return StoragePool{}, fmt.Errorf("parse storage pool id: %w", err)
	}
	owner, err := uuid.Parse(ownerStr)
	if err != nil {
		return StoragePool{}, fmt.Errorf("parse owner id: %w", err)
	}
	p.ID = id
	p.OwnerUserID = owner
	return p, nil
}

// scanCluster scans a cluster row joined with its default branch (clusterSelect).
func scanCluster(r scanRow) (Cluster, error) {
	var (
		idStr   string
		poolStr string
		p       Cluster
		def     Branch
		// default-branch columns
		defClusterStr string
		defBranchStr  string
		defParentStr  sql.NullString
		defForkLSN    sql.NullString
		defExtRaw     []byte
		tagsRaw       []byte
	)
	if err := r.Scan(&idStr, &poolStr, &p.Name, &p.CreatedAt, &p.Engine, &tagsRaw,
		&defClusterStr, &defBranchStr, &def.Name, &def.PgVersion, &def.IsDefault, &defParentStr, &defForkLSN, &defExtRaw, &def.CreatedAt); err != nil {
		return Cluster{}, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return Cluster{}, fmt.Errorf("parse cluster id: %w", err)
	}
	poolID, err := uuid.Parse(poolStr)
	if err != nil {
		return Cluster{}, fmt.Errorf("parse storage pool id: %w", err)
	}
	defClusterID, err := uuid.Parse(defClusterStr)
	if err != nil {
		return Cluster{}, fmt.Errorf("parse default branch cluster id: %w", err)
	}
	defBranchID, err := uuid.Parse(defBranchStr)
	if err != nil {
		return Cluster{}, fmt.Errorf("parse default branch id: %w", err)
	}
	p.ID = id
	p.StoragePoolID = poolID
	if p.Tags, err = decodeTags(tagsRaw, "cluster"); err != nil {
		return Cluster{}, err
	}
	def.ClusterID = defClusterID
	def.BranchID = defBranchID
	if defParentStr.Valid {
		parentID, err := uuid.Parse(defParentStr.String)
		if err != nil {
			return Cluster{}, fmt.Errorf("parse default branch parent id: %w", err)
		}
		def.ParentBranchID = &parentID
	}
	def.ForkLSN = defForkLSN.String // "" when NULL
	// The cluster's externalized-extension set lives on this default branch row;
	// carry it so getCluster/listClusters can surface it on the cluster.
	if len(defExtRaw) > 0 {
		if err := json.Unmarshal(defExtRaw, &def.Extensions); err != nil {
			return Cluster{}, fmt.Errorf("decode cluster extensions: %w", err)
		}
	}
	p.DefaultBranch = def
	return p, nil
}

// decodePgSettings turns the stored jsonb object into a map, or nil when it is
// absent or empty. Nil rather than an empty map so "no settings" is one case.
func decodePgSettings(raw []byte, what string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode %s pg_settings: %w", what, err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func scanBranch(r scanRow) (Branch, error) {
	var (
		clusterStr  string
		branchStr   string
		parentStr   sql.NullString // NULL for the default branch
		forkLSN     sql.NullString // NULL for the default branch
		settingsRaw []byte
		leaseRaw    []byte
		extRaw      []byte
		ycpGen      sql.NullInt64 // NULL until a YCP update has been applied
		br          Branch
	)
	if err := r.Scan(&clusterStr, &branchStr, &br.Name, &br.PgVersion, &br.IsDefault, &parentStr, &forkLSN, &settingsRaw, &leaseRaw, &extRaw, &ycpGen, &br.CreatedAt); err != nil {
		return Branch{}, err
	}
	if ycpGen.Valid {
		br.YCPGeneration = &ycpGen.Int64
	}
	clusterID, err := uuid.Parse(clusterStr)
	if err != nil {
		return Branch{}, fmt.Errorf("parse cluster id: %w", err)
	}
	branchID, err := uuid.Parse(branchStr)
	if err != nil {
		return Branch{}, fmt.Errorf("parse branch id: %w", err)
	}
	br.ClusterID = clusterID
	br.BranchID = branchID
	if parentStr.Valid {
		parentID, err := uuid.Parse(parentStr.String)
		if err != nil {
			return Branch{}, fmt.Errorf("parse parent branch id: %w", err)
		}
		br.ParentBranchID = &parentID
	}
	br.ForkLSN = forkLSN.String // "" when NULL
	if br.PgSettings, err = decodePgSettings(settingsRaw, "branch"); err != nil {
		return Branch{}, err
	}
	if br.WriteTokenSettings, err = decodeDaemonSettings(leaseRaw, "branch write_token"); err != nil {
		return Branch{}, err
	}
	if len(extRaw) > 0 {
		if err := json.Unmarshal(extRaw, &br.Extensions); err != nil {
			return Branch{}, fmt.Errorf("decode branch extensions: %w", err)
		}
	}
	return br, nil
}

func scanEndpoint(r scanRow) (Endpoint, error) {
	var (
		idStr       string
		clusterStr  string
		branchStr   string
		settingsRaw []byte
		psRaw       []byte
		skRaw       []byte
		cidrsRaw    []byte
		tagsRaw     []byte
		ycpGen      sql.NullInt64 // NULL until a YCP update has been applied
		ep          Endpoint
	)
	if err := r.Scan(&idStr, &clusterStr, &branchStr, &ep.Role, &ep.Name, &ep.Address, &ep.TailnetAddress, &ep.CPU, &ep.Memory, &ep.Version, &ep.TargetVersion,
		&ep.Connectivity, &cidrsRaw, &settingsRaw, &psRaw, &skRaw, &ep.Extended, &tagsRaw, &ycpGen, &ep.IngressDNSName, &ep.CreatedAt,
		&ep.Scheduling.Tolerations, &ep.Scheduling.TopologySpreadConstraints); err != nil {
		return Endpoint{}, err
	}
	// A malformed allowed_cidrs is not worth failing the read over — the endpoint
	// and its address are still valid, and the source ranges that matter live on
	// the Service — but it is written by the controller from values ParseCIDR
	// already accepted, so it means the row was edited by hand or something is
	// wrong upstream. Never silent.
	if len(cidrsRaw) > 0 {
		if err := json.Unmarshal(cidrsRaw, &ep.AllowedCIDRs); err != nil {
			slog.Default().Warn("endpoint config.allowed_cidrs is not a JSON string array; ignoring it",
				"endpoint", idStr, "error", err, "raw", string(cidrsRaw))
		}
	}
	if ycpGen.Valid {
		ep.YCPGeneration = &ycpGen.Int64
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse endpoint id: %w", err)
	}
	cluster, err := uuid.Parse(clusterStr)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse cluster id: %w", err)
	}
	branch, err := uuid.Parse(branchStr)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse branch id: %w", err)
	}
	ep.ID = id
	ep.ClusterID = cluster
	ep.BranchID = branch
	if ep.PgSettings, err = decodePgSettings(settingsRaw, "endpoint"); err != nil {
		return Endpoint{}, err
	}
	if ep.PageserverSettings, err = decodeDaemonSettings(psRaw, "pageserver"); err != nil {
		return Endpoint{}, err
	}
	if ep.SafekeeperSettings, err = decodeDaemonSettings(skRaw, "safekeeper"); err != nil {
		return Endpoint{}, err
	}
	if ep.Tags, err = decodeTags(tagsRaw, "endpoint"); err != nil {
		return Endpoint{}, err
	}
	return ep, nil
}

// decodeTags turns a stored tags object into a map, or nil when it is absent or
// empty, so "no tags" is one case.
func decodeTags(raw []byte, what string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode %s tags: %w", what, err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// decodeDaemonSettings is decodePgSettings for the daemon override columns, whose
// values are mixed-type scalars. Absent or empty decodes to nil, so "no overrides"
// is one case.
func decodeDaemonSettings(raw []byte, what string) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode %s_settings: %w", what, err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
