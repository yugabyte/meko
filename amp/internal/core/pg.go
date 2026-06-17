package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/yb-light/amp/internal/db"
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
	id := uuid.New()
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
	id := uuid.New()
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

func (b *PgBackend) ListStoragePools(ctx context.Context, owner uuid.UUID) ([]StoragePool, error) {
	if _, err := b.GetUser(ctx, owner); err != nil {
		return nil, err // ErrNotFound when the user is unknown
	}
	rows, err := b.pool.Query(ctx,
		`SELECT id::text, name, owner_user_id::text, created_at
		 FROM storage_pool WHERE owner_user_id = $1::uuid ORDER BY created_at, id`,
		owner.String())
	if err != nil {
		return nil, fmt.Errorf("list storage pools: %w", err)
	}
	defer rows.Close()

	var pools []StoragePool
	for rows.Next() {
		p, err := scanPool(rows)
		if err != nil {
			return nil, err
		}
		pools = append(pools, p)
	}
	// rows.Next returns false on both end-of-rows and error; check Err so an
	// iteration failure isn't mistaken for a complete (but truncated) result.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list storage pools: %w", err)
	}
	return pools, nil
}

func (b *PgBackend) GetStoragePool(ctx context.Context, owner, poolID uuid.UUID) (StoragePool, error) {
	row := b.pool.QueryRow(ctx,
		`SELECT id::text, name, owner_user_id::text, created_at
		 FROM storage_pool WHERE id = $1::uuid AND owner_user_id = $2::uuid`,
		poolID.String(), owner.String())
	p, err := scanPool(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoragePool{}, ErrNotFound
	}
	return p, err
}

func (b *PgBackend) CreateTimeline(ctx context.Context, owner, poolID uuid.UUID, name string, pgVersion int) (Timeline, error) {
	// Owner-scoped pool check: a pool the caller doesn't own 404s rather than
	// leaking its existence, matching GetStoragePool.
	if _, err := b.GetStoragePool(ctx, owner, poolID); err != nil {
		return Timeline{}, err
	}

	tenantID := uuid.New()
	timelineID := uuid.New()

	// The tenant and its main cluster are written together: a tenant with no
	// timeline is not a valid resource. Both inserts are ON CONFLICT (pk) DO
	// NOTHING so re-executing the same statement (same ids) converges; the
	// tenant-name collision surfaces as 23505 on the (storage_pool_id, name)
	// unique constraint, which is not the conflict target and so still errors.
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return Timeline{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`INSERT INTO tenant (id, storage_pool_id, name) VALUES ($1::uuid, $2::uuid, $3)
		 ON CONFLICT (id) DO NOTHING`,
		tenantID.String(), poolID.String(), name); err != nil {
		return Timeline{}, mapTimelineErr("insert tenant", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO pg_cluster (tenant_id, timeline_id, name, pg_version, is_main)
		 VALUES ($1::uuid, $2::uuid, $3, $4, true)
		 ON CONFLICT (tenant_id, timeline_id) DO NOTHING`,
		tenantID.String(), timelineID.String(), name, pgVersion); err != nil {
		return Timeline{}, mapTimelineErr("insert pg_cluster", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Timeline{}, fmt.Errorf("commit create timeline: %w", err)
	}
	return b.getTimeline(ctx, tenantID, timelineID)
}

func (b *PgBackend) CreateEndpoint(ctx context.Context, tenantID, timelineID uuid.UUID, role, address string) (Endpoint, error) {
	id := uuid.New()
	_, err := b.pool.Exec(ctx,
		`INSERT INTO endpoint (id, tenant_id, timeline_id, role, address, config)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, $4::endpoint_role, $5, '{}'::jsonb)
		 ON CONFLICT (id) DO NOTHING`,
		id.String(), tenantID.String(), timelineID.String(), role, address)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23505": // one_writer_per_timeline: a writer already exists
				return Endpoint{}, ErrConflict
			case "23503": // foreign_key_violation: the timeline does not exist
				return Endpoint{}, ErrNotFound
			}
		}
		return Endpoint{}, fmt.Errorf("insert endpoint: %w", err)
	}
	return b.getEndpoint(ctx, id)
}

func (b *PgBackend) getEndpoint(ctx context.Context, id uuid.UUID) (Endpoint, error) {
	row := b.pool.QueryRow(ctx,
		`SELECT id::text, tenant_id::text, timeline_id::text, role::text, COALESCE(address, ''), created_at
		 FROM endpoint WHERE id = $1::uuid`,
		id.String())
	e, err := scanEndpoint(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Endpoint{}, ErrNotFound
	}
	return e, err
}

func (b *PgBackend) getTimeline(ctx context.Context, tenantID, timelineID uuid.UUID) (Timeline, error) {
	row := b.pool.QueryRow(ctx,
		`SELECT tenant_id::text, timeline_id::text, name, pg_version, is_main, created_at
		 FROM pg_cluster WHERE tenant_id = $1::uuid AND timeline_id = $2::uuid`,
		tenantID.String(), timelineID.String())
	t, err := scanTimeline(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Timeline{}, ErrNotFound
	}
	return t, err
}

// mapTimelineErr translates the constraint violations the timeline writes can
// raise: a duplicate tenant name (unique storage_pool_id, name) -> ErrConflict,
// and a missing pool (foreign key) -> ErrNotFound (it was deleted between the
// ownership check and the insert).
func mapTimelineErr(what string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation: the tenant name is already taken in the pool
			return ErrConflict
		case "23503": // foreign_key_violation: the pool no longer exists
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

func scanTimeline(r scanRow) (Timeline, error) {
	var (
		tenantStr   string
		timelineStr string
		t           Timeline
	)
	if err := r.Scan(&tenantStr, &timelineStr, &t.Name, &t.PgVersion, &t.IsMain, &t.CreatedAt); err != nil {
		return Timeline{}, err
	}
	tenantID, err := uuid.Parse(tenantStr)
	if err != nil {
		return Timeline{}, fmt.Errorf("parse tenant id: %w", err)
	}
	timelineID, err := uuid.Parse(timelineStr)
	if err != nil {
		return Timeline{}, fmt.Errorf("parse timeline id: %w", err)
	}
	t.TenantID = tenantID
	t.TimelineID = timelineID
	return t, nil
}

func scanEndpoint(r scanRow) (Endpoint, error) {
	var (
		idStr       string
		tenantStr   string
		timelineStr string
		e           Endpoint
	)
	if err := r.Scan(&idStr, &tenantStr, &timelineStr, &e.Role, &e.Address, &e.CreatedAt); err != nil {
		return Endpoint{}, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse endpoint id: %w", err)
	}
	tenantID, err := uuid.Parse(tenantStr)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse tenant id: %w", err)
	}
	timelineID, err := uuid.Parse(timelineStr)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse timeline id: %w", err)
	}
	e.ID = id
	e.TenantID = tenantID
	e.TimelineID = timelineID
	return e, nil
}
