package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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
	return b.GetUser(ctx, id)
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
	return b.getStoragePoolByID(ctx, id)
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
	total, err := b.count(ctx, `SELECT count(*) FROM amp_registry.storage_pools`)
	if err != nil {
		return nil, 0, fmt.Errorf("count storage pools: %w", err)
	}
	rows, err := b.pool.Query(ctx,
		`SELECT id::text, name, owner_user_id::text, created_at
		 FROM amp_registry.storage_pools ORDER BY created_at, id LIMIT $1 OFFSET $2`,
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

func (b *PgBackend) CreateProject(ctx context.Context, poolID uuid.UUID, name string, pgVersion int) (Project, error) {
	projectID := uuid.New()
	branchID := uuid.New()

	// The project and its default branch are written together: a project with no
	// branch is not a valid resource. Both inserts are ON CONFLICT (pk) DO
	// NOTHING so re-executing the same statement (same ids) converges; the
	// project-name collision surfaces as 23505 on the (storage_pool_id, name)
	// unique constraint, which is not the conflict target and so still errors.
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return Project{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`INSERT INTO amp_registry.projects (id, storage_pool_id, name) VALUES ($1::uuid, $2::uuid, $3)
		 ON CONFLICT (id) DO NOTHING`,
		projectID.String(), poolID.String(), name); err != nil {
		return Project{}, mapProjectErr("insert project", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO amp_registry.branches (project_id, branch_id, name, pg_version, is_default)
		 VALUES ($1::uuid, $2::uuid, $3, $4, true)
		 ON CONFLICT (branch_id) DO NOTHING`,
		projectID.String(), branchID.String(), DefaultBranchName, pgVersion); err != nil {
		return Project{}, mapProjectErr("insert default branch", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Project{}, fmt.Errorf("commit create project: %w", err)
	}
	return b.getProject(ctx, poolID, projectID)
}

// projectSelect joins project -> storage_pools and its default branch so a
// project is only visible through the pool it belongs to, and always carries
// its default branch. Pools are top-level, so this is not owner-scoped.
//
// The default-branch join filters tombstones inline; the project's own tombstone
// filter is projectLive, which callers append alongside their WHERE.
const projectSelect = `SELECT p.id::text, p.storage_pool_id::text, p.name, p.created_at,
		b.project_id::text, b.branch_id::text, b.name, b.pg_version, b.is_default, b.parent_branch_id::text, b.fork_lsn::text, b.created_at
	FROM amp_registry.projects p
	JOIN amp_registry.storage_pools sp ON sp.id = p.storage_pool_id
	JOIN amp_registry.branches b ON b.project_id = p.id AND b.is_default AND b.dropped_at IS NULL`

// projectLive excludes tombstoned (dropped) projects. Projects are tombstoned
// rather than deleted so their tombstoned branches keep a valid parent (see
// PgBackend.DeleteProject), which means every read has to filter them out
// explicitly. Assumes the project table is aliased `p`.
const projectLive = ` AND p.dropped_at IS NULL`

// projectCreateNotFailed excludes a project whose create_project task failed.
// The project row is written mid-provisioning (the create_project step's
// EnsureProject) before the later storage steps run, so a task that fails
// afterward leaves an orphan project that should not surface as a real cluster.
// Each project has exactly one create_project task — a retry mints a fresh
// project id — so a single failed task is definitive. The failed task itself is
// left intact so it still shows on the tasks page. Assumes the project table is
// aliased `p`.
const projectCreateNotFailed = ` AND NOT EXISTS (
		SELECT 1 FROM tasks t
		WHERE t.project_id = p.id AND t.kind = 'create_project' AND t.status = 'failed')`

func (b *PgBackend) getProject(ctx context.Context, poolID, projectID uuid.UUID) (Project, error) {
	row := b.pool.QueryRow(ctx,
		projectSelect+` WHERE p.id = $1::uuid AND sp.id = $2::uuid`+projectLive,
		projectID.String(), poolID.String())
	p, err := scanProject(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

func (b *PgBackend) GetProject(ctx context.Context, poolID, projectID uuid.UUID) (Project, error) {
	return b.getProject(ctx, poolID, projectID)
}

func (b *PgBackend) ListProjects(ctx context.Context, poolID uuid.UUID, limit, offset int, includeFailed bool) ([]Project, int, error) {
	// A missing pool 404s rather than returning an empty page.
	if _, err := b.GetStoragePool(ctx, poolID); err != nil {
		return nil, 0, err
	}
	// Scope to usable clusters unless the caller opts in: exclude failed-creation
	// orphans from both the page and its total. Included only when includeFailed
	// is set (an operator's fleet view), so they can be seen and deleted.
	filter := ""
	if !includeFailed {
		filter = projectCreateNotFailed
	}
	// The count must match the page: projectSelect INNER-JOINs the default branch,
	// so a project with no is_default branch never appears in the page — require
	// one in the count too, or total would over-report and a cluster would be
	// "counted but never returned", breaking pagination.
	total, err := b.count(ctx,
		`SELECT count(*) FROM amp_registry.projects p
		 WHERE p.storage_pool_id = $1::uuid
		   AND EXISTS (SELECT 1 FROM amp_registry.branches b
		               WHERE b.project_id = p.id AND b.is_default AND b.dropped_at IS NULL)`+projectLive+filter,
		poolID.String())
	if err != nil {
		return nil, 0, fmt.Errorf("count projects: %w", err)
	}
	rows, err := b.pool.Query(ctx,
		projectSelect+` WHERE sp.id = $1::uuid`+projectLive+filter+`
		 ORDER BY p.created_at, p.id LIMIT $2 OFFSET $3`,
		poolID.String(), limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var projects []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, 0, err
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list projects: %w", err)
	}
	return projects, total, nil
}

func (b *PgBackend) CreateBranch(ctx context.Context, poolID, projectID, parentBranchID uuid.UUID, name, forkLSN string) (Branch, error) {
	// Validate the project within the pool first (a project not in the pool 404s).
	if err := b.projectInPool(ctx, poolID, projectID); err != nil {
		return Branch{}, err
	}

	branchID := uuid.New()
	// is_default = false; record the fork ancestry. pg_version is inherited from
	// the parent (the SELECT source) so a branch and its ancestor never drift.
	// The SELECT is keyed by the parent within the project; a missing parent
	// inserts nothing, so the read-back 404s. ON CONFLICT (pk) DO NOTHING makes a
	// same-id retry converge; a duplicate name surfaces as 23505 on
	// branches_name_per_project -> ErrConflict. forkLSN is validated to be a
	// well-formed LSN by the caller before it reaches the pg_lsn cast. A
	// tombstoned parent selects nothing, so forking a dropped branch 404s
	// rather than resurrecting its lineage.
	if _, err := b.pool.Exec(ctx,
		`INSERT INTO amp_registry.branches (project_id, branch_id, name, pg_version, is_default, parent_branch_id, fork_lsn)
		 SELECT $1::uuid, $2::uuid, $3, p.pg_version, false, $4::uuid, $5::pg_lsn
		 FROM amp_registry.branches p
		 WHERE p.project_id = $1::uuid AND p.branch_id = $4::uuid AND p.dropped_at IS NULL
		 ON CONFLICT (branch_id) DO NOTHING`,
		projectID.String(), branchID.String(), name, parentBranchID.String(), forkLSN); err != nil {
		return Branch{}, mapBranchErr("insert branch", err)
	}
	return b.getBranchByID(ctx, projectID, branchID)
}

func (b *PgBackend) getBranchByID(ctx context.Context, projectID, branchID uuid.UUID) (Branch, error) {
	row := b.pool.QueryRow(ctx,
		`SELECT `+branchColumns+`
		 FROM amp_registry.branches b
		 WHERE b.project_id = $1::uuid AND b.branch_id = $2::uuid AND b.dropped_at IS NULL`,
		projectID.String(), branchID.String())
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
const branchColumns = `b.project_id::text, b.branch_id::text, b.name, b.pg_version, b.is_default, b.parent_branch_id::text, b.fork_lsn::text, b.pg_settings, b.created_at`

// branchSelect joins branch -> project -> storage_pools so a branch is only
// visible through the project (and pool) it belongs to. Tombstoned projects are
// filtered in the join; the branch's own tombstone filter is branchLive.
const branchSelect = `SELECT ` + branchColumns + `
	FROM amp_registry.branches b
	JOIN amp_registry.projects p ON p.id = b.project_id AND p.dropped_at IS NULL
	JOIN amp_registry.storage_pools sp ON sp.id = p.storage_pool_id`

// branchLive excludes tombstoned (dropped) branches. Assumes `b` is the branch.
const branchLive = ` AND b.dropped_at IS NULL`

func (b *PgBackend) GetBranch(ctx context.Context, poolID, projectID, branchID uuid.UUID) (Branch, error) {
	row := b.pool.QueryRow(ctx,
		branchSelect+` WHERE b.branch_id = $1::uuid AND b.project_id = $2::uuid AND sp.id = $3::uuid`+branchLive,
		branchID.String(), projectID.String(), poolID.String())
	br, err := scanBranch(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Branch{}, ErrNotFound
	}
	return br, err
}

func (b *PgBackend) ListBranches(ctx context.Context, poolID, projectID uuid.UUID, limit, offset int) ([]Branch, int, error) {
	// A missing pool or project 404s rather than returning an empty page.
	if err := b.projectInPool(ctx, poolID, projectID); err != nil {
		return nil, 0, err
	}
	total, err := b.count(ctx,
		`SELECT count(*) FROM amp_registry.branches WHERE project_id = $1::uuid AND dropped_at IS NULL`,
		projectID.String())
	if err != nil {
		return nil, 0, fmt.Errorf("count branches: %w", err)
	}
	rows, err := b.pool.Query(ctx,
		`SELECT `+branchColumns+`
		 FROM amp_registry.branches b
		 WHERE b.project_id = $1::uuid AND b.dropped_at IS NULL
		 ORDER BY b.created_at, b.branch_id LIMIT $2 OFFSET $3`,
		projectID.String(), limit, offset)
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

// projectInPool returns ErrNotFound unless a live projectID exists under poolID.
func (b *PgBackend) projectInPool(ctx context.Context, poolID, projectID uuid.UUID) error {
	var x int
	err := b.pool.QueryRow(ctx,
		`SELECT 1 FROM amp_registry.projects
		  WHERE id = $1::uuid AND storage_pool_id = $2::uuid AND dropped_at IS NULL`,
		projectID.String(), poolID.String()).Scan(&x)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("resolve project in pool: %w", err)
	}
	return nil
}

// mapProjectErr translates the constraint violations the project writes can
// raise: a duplicate project name among live projects (projects_name_per_pool)
// -> ErrConflict, and a missing pool (foreign key on project.storage_pool_id)
// -> ErrNotFound.
func mapProjectErr(what string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation: the project name is already taken in the pool
			return ErrConflict
		case "23503": // foreign_key_violation: the pool no longer exists
			return ErrNotFound
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

// mapBranchErr translates the constraint violations a branch write can raise: a
// duplicate branch name among live branches (branches_name_per_project) ->
// ErrConflict, and a missing in-project parent (foreign key) -> ErrNotFound.
func mapBranchErr(what string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation: the branch name is already taken in the project
			return ErrConflict
		case "23503": // foreign_key_violation: the in-project parent does not exist
			return ErrNotFound
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

func (b *PgBackend) getStoragePoolByID(ctx context.Context, id uuid.UUID) (StoragePool, error) {
	row := b.pool.QueryRow(ctx,
		`SELECT id::text, name, owner_user_id::text, created_at
		 FROM amp_registry.storage_pools WHERE id = $1::uuid`,
		id.String())
	return scanPool(row)
}

func (b *PgBackend) CreateEndpoint(ctx context.Context, projectID, branchID uuid.UUID, role string, config []byte) (Endpoint, error) {
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
// An endpoint has no project_id of its own — it reaches its project through its
// branch — so every projection joins `branches` to supply it. That join is also
// what scopes a query to a project.
const endpointSelect = `SELECT e.id::text, eb.project_id::text, e.branch_id::text, e.role::text, COALESCE(e.name, ''), COALESCE(e.address, ''), COALESCE(e.tailnet_address, ''), COALESCE(e.config->>'cpu', ''), COALESCE(e.config->>'memory', ''), e.pg_settings, e.config->'extended', e.created_at
	FROM amp_registry.endpoints e
	JOIN amp_registry.branches eb ON eb.branch_id = e.branch_id`

func (b *PgBackend) getEndpointByID(ctx context.Context, id uuid.UUID) (Endpoint, error) {
	row := b.pool.QueryRow(ctx, endpointSelect+` WHERE e.id = $1::uuid`, id.String())
	return scanEndpoint(row)
}

func (b *PgBackend) GetEndpoint(ctx context.Context, poolID, projectID, branchID, endpointID uuid.UUID) (Endpoint, error) {
	row := b.pool.QueryRow(ctx,
		endpointSelect+`
		 JOIN amp_registry.projects p ON p.id = eb.project_id AND p.dropped_at IS NULL
		 JOIN amp_registry.storage_pools sp ON sp.id = p.storage_pool_id
		 WHERE e.id = $1::uuid AND eb.branch_id = $2::uuid AND eb.project_id = $3::uuid
		   AND eb.dropped_at IS NULL AND sp.id = $4::uuid`,
		endpointID.String(), branchID.String(), projectID.String(), poolID.String())
	ep, err := scanEndpoint(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Endpoint{}, ErrNotFound
	}
	return ep, err
}

func (b *PgBackend) ListEndpoints(ctx context.Context, poolID, projectID, branchID uuid.UUID, limit, offset int) ([]Endpoint, int, error) {
	// Resolve the branch within the project and pool first. No row means the
	// pool, project, or branch does not exist -> 404.
	if _, err := b.GetBranch(ctx, poolID, projectID, branchID); err != nil {
		return nil, 0, err
	}
	total, err := b.count(ctx,
		`SELECT count(*) FROM amp_registry.endpoints e
		  JOIN amp_registry.branches b ON b.branch_id = e.branch_id
		 WHERE b.project_id = $1::uuid AND e.branch_id = $2::uuid`,
		projectID.String(), branchID.String())
	if err != nil {
		return nil, 0, fmt.Errorf("count endpoints: %w", err)
	}
	rows, err := b.pool.Query(ctx,
		endpointSelect+` WHERE eb.project_id = $1::uuid AND e.branch_id = $2::uuid
		 ORDER BY e.created_at, e.id LIMIT $3 OFFSET $4`,
		projectID.String(), branchID.String(), limit, offset)
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

// scanProject scans a project row joined with its default branch (projectSelect).
func scanProject(r scanRow) (Project, error) {
	var (
		idStr   string
		poolStr string
		p       Project
		def     Branch
		// default-branch columns
		defProjectStr string
		defBranchStr  string
		defParentStr  sql.NullString
		defForkLSN    sql.NullString
	)
	if err := r.Scan(&idStr, &poolStr, &p.Name, &p.CreatedAt,
		&defProjectStr, &defBranchStr, &def.Name, &def.PgVersion, &def.IsDefault, &defParentStr, &defForkLSN, &def.CreatedAt); err != nil {
		return Project{}, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return Project{}, fmt.Errorf("parse project id: %w", err)
	}
	poolID, err := uuid.Parse(poolStr)
	if err != nil {
		return Project{}, fmt.Errorf("parse storage pool id: %w", err)
	}
	defProjectID, err := uuid.Parse(defProjectStr)
	if err != nil {
		return Project{}, fmt.Errorf("parse default branch project id: %w", err)
	}
	defBranchID, err := uuid.Parse(defBranchStr)
	if err != nil {
		return Project{}, fmt.Errorf("parse default branch id: %w", err)
	}
	p.ID = id
	p.StoragePoolID = poolID
	def.ProjectID = defProjectID
	def.BranchID = defBranchID
	if defParentStr.Valid {
		parentID, err := uuid.Parse(defParentStr.String)
		if err != nil {
			return Project{}, fmt.Errorf("parse default branch parent id: %w", err)
		}
		def.ParentBranchID = &parentID
	}
	def.ForkLSN = defForkLSN.String // "" when NULL
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
		projectStr  string
		branchStr   string
		parentStr   sql.NullString // NULL for the default branch
		forkLSN     sql.NullString // NULL for the default branch
		settingsRaw []byte
		br          Branch
	)
	if err := r.Scan(&projectStr, &branchStr, &br.Name, &br.PgVersion, &br.IsDefault, &parentStr, &forkLSN, &settingsRaw, &br.CreatedAt); err != nil {
		return Branch{}, err
	}
	projectID, err := uuid.Parse(projectStr)
	if err != nil {
		return Branch{}, fmt.Errorf("parse project id: %w", err)
	}
	branchID, err := uuid.Parse(branchStr)
	if err != nil {
		return Branch{}, fmt.Errorf("parse branch id: %w", err)
	}
	br.ProjectID = projectID
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
	return br, nil
}

func scanEndpoint(r scanRow) (Endpoint, error) {
	var (
		idStr       string
		projectStr  string
		branchStr   string
		settingsRaw []byte
		ep          Endpoint
	)
	if err := r.Scan(&idStr, &projectStr, &branchStr, &ep.Role, &ep.Name, &ep.Address, &ep.TailnetAddress, &ep.CPU, &ep.Memory, &settingsRaw, &ep.Extended, &ep.CreatedAt); err != nil {
		return Endpoint{}, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse endpoint id: %w", err)
	}
	project, err := uuid.Parse(projectStr)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse project id: %w", err)
	}
	branch, err := uuid.Parse(branchStr)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse branch id: %w", err)
	}
	ep.ID = id
	ep.ProjectID = project
	ep.BranchID = branch
	if ep.PgSettings, err = decodePgSettings(settingsRaw, "endpoint"); err != nil {
		return Endpoint{}, err
	}
	return ep, nil
}
