package core

import (
	"context"
	"database/sql"
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
		`INSERT INTO app_user (id, is_admin) VALUES ($1::uuid, $2)
		 ON CONFLICT (id) DO NOTHING`,
		id.String(), isAdmin); err != nil {
		return User{}, fmt.Errorf("insert user: %w", err)
	}
	return b.GetUser(ctx, id)
}

func (b *PgBackend) GetUser(ctx context.Context, id uuid.UUID) (User, error) {
	row := b.pool.QueryRow(ctx,
		`SELECT id::text, is_admin, created_at FROM app_user WHERE id = $1::uuid`,
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
		`INSERT INTO storage_pool (id, name, owner_user_id) VALUES ($1::uuid, $2, $3::uuid)
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
	total, err := b.count(ctx, `SELECT count(*) FROM app_user`)
	if err != nil {
		return nil, 0, fmt.Errorf("count users: %w", err)
	}
	rows, err := b.pool.Query(ctx,
		`SELECT id::text, is_admin, created_at
		 FROM app_user ORDER BY created_at, id LIMIT $1 OFFSET $2`,
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
	total, err := b.count(ctx, `SELECT count(*) FROM storage_pool`)
	if err != nil {
		return nil, 0, fmt.Errorf("count storage pools: %w", err)
	}
	rows, err := b.pool.Query(ctx,
		`SELECT id::text, name, owner_user_id::text, created_at
		 FROM storage_pool ORDER BY created_at, id LIMIT $1 OFFSET $2`,
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
		`INSERT INTO project (id, storage_pool_id, name) VALUES ($1::uuid, $2::uuid, $3)
		 ON CONFLICT (id) DO NOTHING`,
		projectID.String(), poolID.String(), name); err != nil {
		return Project{}, mapProjectErr("insert project", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO branch (project_id, branch_id, name, pg_version, is_default)
		 VALUES ($1::uuid, $2::uuid, $3, $4, true)
		 ON CONFLICT (project_id, branch_id) DO NOTHING`,
		projectID.String(), branchID.String(), DefaultBranchName, pgVersion); err != nil {
		return Project{}, mapProjectErr("insert default branch", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Project{}, fmt.Errorf("commit create project: %w", err)
	}
	return b.getProject(ctx, poolID, projectID)
}

// projectSelect joins project -> storage_pool and its default branch so a
// project is only visible through the pool it belongs to, and always carries
// its default branch. Pools are top-level, so this is not owner-scoped.
const projectSelect = `SELECT p.id::text, p.storage_pool_id::text, p.name, p.created_at,
		b.project_id::text, b.branch_id::text, b.name, b.pg_version, b.is_default, b.parent_branch_id::text, b.fork_lsn::text, b.created_at
	FROM project p
	JOIN storage_pool sp ON sp.id = p.storage_pool_id
	JOIN branch b ON b.project_id = p.id AND b.is_default`

func (b *PgBackend) getProject(ctx context.Context, poolID, projectID uuid.UUID) (Project, error) {
	row := b.pool.QueryRow(ctx,
		projectSelect+` WHERE p.id = $1::uuid AND sp.id = $2::uuid`,
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

func (b *PgBackend) ListProjects(ctx context.Context, poolID uuid.UUID, limit, offset int) ([]Project, int, error) {
	// A missing pool 404s rather than returning an empty page.
	if _, err := b.GetStoragePool(ctx, poolID); err != nil {
		return nil, 0, err
	}
	total, err := b.count(ctx,
		`SELECT count(*) FROM project WHERE storage_pool_id = $1::uuid`,
		poolID.String())
	if err != nil {
		return nil, 0, fmt.Errorf("count projects: %w", err)
	}
	rows, err := b.pool.Query(ctx,
		projectSelect+` WHERE sp.id = $1::uuid
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
	// UNIQUE (project_id, name) -> ErrConflict. forkLSN is validated to be a
	// well-formed LSN by the caller before it reaches the pg_lsn cast.
	if _, err := b.pool.Exec(ctx,
		`INSERT INTO branch (project_id, branch_id, name, pg_version, is_default, parent_branch_id, fork_lsn)
		 SELECT $1::uuid, $2::uuid, $3, p.pg_version, false, $4::uuid, $5::pg_lsn
		 FROM branch p WHERE p.project_id = $1::uuid AND p.branch_id = $4::uuid
		 ON CONFLICT (project_id, branch_id) DO NOTHING`,
		projectID.String(), branchID.String(), name, parentBranchID.String(), forkLSN); err != nil {
		return Branch{}, mapBranchErr("insert branch", err)
	}
	return b.getBranchByID(ctx, projectID, branchID)
}

func (b *PgBackend) getBranchByID(ctx context.Context, projectID, branchID uuid.UUID) (Branch, error) {
	row := b.pool.QueryRow(ctx,
		`SELECT project_id::text, branch_id::text, name, pg_version, is_default, parent_branch_id::text, fork_lsn::text, created_at
		 FROM branch WHERE project_id = $1::uuid AND branch_id = $2::uuid`,
		projectID.String(), branchID.String())
	br, err := scanBranch(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Branch{}, ErrNotFound
	}
	return br, err
}

// branchSelect joins branch -> project -> storage_pool so a branch is only
// visible through the project (and pool) it belongs to.
const branchSelect = `SELECT b.project_id::text, b.branch_id::text, b.name, b.pg_version, b.is_default, b.parent_branch_id::text, b.fork_lsn::text, b.created_at
	FROM branch b
	JOIN project p ON p.id = b.project_id
	JOIN storage_pool sp ON sp.id = p.storage_pool_id`

func (b *PgBackend) GetBranch(ctx context.Context, poolID, projectID, branchID uuid.UUID) (Branch, error) {
	row := b.pool.QueryRow(ctx,
		branchSelect+` WHERE b.branch_id = $1::uuid AND b.project_id = $2::uuid AND sp.id = $3::uuid`,
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
		`SELECT count(*) FROM branch WHERE project_id = $1::uuid`,
		projectID.String())
	if err != nil {
		return nil, 0, fmt.Errorf("count branches: %w", err)
	}
	rows, err := b.pool.Query(ctx,
		`SELECT project_id::text, branch_id::text, name, pg_version, is_default, parent_branch_id::text, fork_lsn::text, created_at
		 FROM branch WHERE project_id = $1::uuid
		 ORDER BY created_at, branch_id LIMIT $2 OFFSET $3`,
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

// projectInPool returns ErrNotFound unless projectID exists under poolID.
func (b *PgBackend) projectInPool(ctx context.Context, poolID, projectID uuid.UUID) error {
	var x int
	err := b.pool.QueryRow(ctx,
		`SELECT 1 FROM project WHERE id = $1::uuid AND storage_pool_id = $2::uuid`,
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
// raise: a duplicate project name (unique storage_pool_id, name) -> ErrConflict,
// and a missing pool (foreign key on project.storage_pool_id) -> ErrNotFound.
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
// duplicate branch name (unique project_id, name) -> ErrConflict, and a missing
// in-project parent (foreign key) -> ErrNotFound.
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
		 FROM storage_pool WHERE id = $1::uuid`,
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
		`INSERT INTO endpoint (id, project_id, branch_id, role, config)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, $4::endpoint_role, $5::jsonb)
		 ON CONFLICT (id) DO NOTHING`,
		id.String(), projectID.String(), branchID.String(), role, cfg)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23505": // unique_violation: a read_write endpoint already exists (one_writer_per_branch)
				return Endpoint{}, ErrConflict
			case "23503": // foreign_key_violation: the (project_id, branch_id) branch does not exist
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
const endpointSelect = `SELECT e.id::text, e.project_id::text, e.branch_id::text, e.role::text, COALESCE(e.address, ''), COALESCE(e.tailnet_address, ''), e.created_at
	FROM endpoint e`

func (b *PgBackend) getEndpointByID(ctx context.Context, id uuid.UUID) (Endpoint, error) {
	row := b.pool.QueryRow(ctx, endpointSelect+` WHERE e.id = $1::uuid`, id.String())
	return scanEndpoint(row)
}

func (b *PgBackend) GetEndpoint(ctx context.Context, poolID, projectID, branchID, endpointID uuid.UUID) (Endpoint, error) {
	row := b.pool.QueryRow(ctx,
		endpointSelect+`
		 JOIN branch b ON b.project_id = e.project_id AND b.branch_id = e.branch_id
		 JOIN project p ON p.id = b.project_id
		 JOIN storage_pool sp ON sp.id = p.storage_pool_id
		 WHERE e.id = $1::uuid AND b.branch_id = $2::uuid AND b.project_id = $3::uuid AND sp.id = $4::uuid`,
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
		`SELECT count(*) FROM endpoint WHERE project_id = $1::uuid AND branch_id = $2::uuid`,
		projectID.String(), branchID.String())
	if err != nil {
		return nil, 0, fmt.Errorf("count endpoints: %w", err)
	}
	rows, err := b.pool.Query(ctx,
		endpointSelect+` WHERE e.project_id = $1::uuid AND e.branch_id = $2::uuid
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
		`UPDATE endpoint SET address = $2, tailnet_address = $3 WHERE id = $1::uuid`,
		endpointID.String(), address, tailnetAddress)
	if err != nil {
		return fmt.Errorf("set endpoint address: %w", err)
	}
	return nil
}

func (b *PgBackend) DeleteEndpoint(ctx context.Context, endpointID uuid.UUID) error {
	_, err := b.pool.Exec(ctx, `DELETE FROM endpoint WHERE id = $1::uuid`, endpointID.String())
	if err != nil {
		return fmt.Errorf("delete endpoint: %w", err)
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

func scanBranch(r scanRow) (Branch, error) {
	var (
		projectStr string
		branchStr  string
		parentStr  sql.NullString // NULL for the default branch
		forkLSN    sql.NullString // NULL for the default branch
		br         Branch
	)
	if err := r.Scan(&projectStr, &branchStr, &br.Name, &br.PgVersion, &br.IsDefault, &parentStr, &forkLSN, &br.CreatedAt); err != nil {
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
	return br, nil
}

func scanEndpoint(r scanRow) (Endpoint, error) {
	var (
		idStr      string
		projectStr string
		branchStr  string
		ep         Endpoint
	)
	if err := r.Scan(&idStr, &projectStr, &branchStr, &ep.Role, &ep.Address, &ep.TailnetAddress, &ep.CreatedAt); err != nil {
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
	return ep, nil
}
