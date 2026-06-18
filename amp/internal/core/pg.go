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

func (b *PgBackend) CreateTimeline(ctx context.Context, poolID uuid.UUID, name string, pgVersion int) (Timeline, error) {
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
// and a missing pool (foreign key on tenant.storage_pool_id) -> ErrNotFound.
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

func (b *PgBackend) ListTimelines(ctx context.Context, poolID uuid.UUID, limit, offset int) ([]Timeline, int, error) {
	// A missing pool 404s rather than returning an empty page.
	if _, err := b.GetStoragePool(ctx, poolID); err != nil {
		return nil, 0, err
	}
	total, err := b.count(ctx,
		`SELECT count(*) FROM pg_cluster c
		 JOIN tenant t ON c.tenant_id = t.id
		 WHERE t.storage_pool_id = $1::uuid`,
		poolID.String())
	if err != nil {
		return nil, 0, fmt.Errorf("count timelines: %w", err)
	}
	rows, err := b.pool.Query(ctx,
		`SELECT c.tenant_id::text, c.timeline_id::text, c.name, c.pg_version, c.is_main, c.created_at
		 FROM pg_cluster c
		 JOIN tenant t ON c.tenant_id = t.id
		 WHERE t.storage_pool_id = $1::uuid
		 ORDER BY c.created_at, c.timeline_id LIMIT $2 OFFSET $3`,
		poolID.String(), limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list timelines: %w", err)
	}
	defer rows.Close()

	var timelines []Timeline
	for rows.Next() {
		t, err := scanTimeline(rows)
		if err != nil {
			return nil, 0, err
		}
		timelines = append(timelines, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list timelines: %w", err)
	}
	return timelines, total, nil
}

// timelineSelect joins pg_cluster -> tenant -> storage_pool so a timeline is
// only visible through the pool its tenant belongs to. The API addresses a
// timeline by id alone (no tenant in the path); timeline_id is a random 16-byte
// storage id, so it is treated as globally unique. Pools are top-level, so this
// is not owner-scoped.
const timelineSelect = `SELECT c.tenant_id::text, c.timeline_id::text, c.name, c.pg_version, c.is_main, c.created_at
	FROM pg_cluster c
	JOIN tenant t ON t.id = c.tenant_id
	JOIN storage_pool sp ON sp.id = t.storage_pool_id`

func (b *PgBackend) GetTimeline(ctx context.Context, poolID, timelineID uuid.UUID) (Timeline, error) {
	row := b.pool.QueryRow(ctx,
		timelineSelect+` WHERE c.timeline_id = $1::uuid AND sp.id = $2::uuid`,
		timelineID.String(), poolID.String())
	tl, err := scanTimeline(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Timeline{}, ErrNotFound
	}
	return tl, err
}

func (b *PgBackend) CreateEndpoint(ctx context.Context, tenantID, timelineID uuid.UUID, role string, config []byte) (Endpoint, error) {
	id := uuid.New()
	var cfg any
	if len(config) > 0 {
		cfg = config
	}
	_, err := b.pool.Exec(ctx,
		`INSERT INTO endpoint (id, tenant_id, timeline_id, role, config)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, $4::endpoint_role, $5::jsonb)
		 ON CONFLICT (id) DO NOTHING`,
		id.String(), tenantID.String(), timelineID.String(), role, cfg)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23505": // unique_violation: a read_write endpoint already exists (one_writer_per_timeline)
				return Endpoint{}, ErrConflict
			case "23503": // foreign_key_violation: the (tenant_id, timeline_id) timeline does not exist
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
const endpointSelect = `SELECT e.id::text, e.tenant_id::text, e.timeline_id::text, e.role::text, COALESCE(e.address, ''), e.created_at
	FROM endpoint e`

func (b *PgBackend) getEndpointByID(ctx context.Context, id uuid.UUID) (Endpoint, error) {
	row := b.pool.QueryRow(ctx, endpointSelect+` WHERE e.id = $1::uuid`, id.String())
	return scanEndpoint(row)
}

func (b *PgBackend) GetEndpoint(ctx context.Context, poolID, timelineID, endpointID uuid.UUID) (Endpoint, error) {
	row := b.pool.QueryRow(ctx,
		endpointSelect+`
		 JOIN pg_cluster c ON c.tenant_id = e.tenant_id AND c.timeline_id = e.timeline_id
		 JOIN tenant t ON t.id = c.tenant_id
		 JOIN storage_pool sp ON sp.id = t.storage_pool_id
		 WHERE e.id = $1::uuid AND c.timeline_id = $2::uuid AND sp.id = $3::uuid`,
		endpointID.String(), timelineID.String(), poolID.String())
	ep, err := scanEndpoint(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Endpoint{}, ErrNotFound
	}
	return ep, err
}

func (b *PgBackend) ListEndpoints(ctx context.Context, poolID, timelineID uuid.UUID, limit, offset int) ([]Endpoint, int, error) {
	// Resolve the timeline within the pool (it's addressed by id under the pool,
	// but the storage layer keys it by (tenant_id, timeline_id)). No row means
	// the pool or the timeline does not exist -> 404.
	tenantID, err := b.timelineTenant(ctx, poolID, timelineID)
	if err != nil {
		return nil, 0, err
	}
	total, err := b.count(ctx,
		`SELECT count(*) FROM endpoint WHERE tenant_id = $1::uuid AND timeline_id = $2::uuid`,
		tenantID.String(), timelineID.String())
	if err != nil {
		return nil, 0, fmt.Errorf("count endpoints: %w", err)
	}
	rows, err := b.pool.Query(ctx,
		endpointSelect+` WHERE e.tenant_id = $1::uuid AND e.timeline_id = $2::uuid
		 ORDER BY e.created_at, e.id LIMIT $3 OFFSET $4`,
		tenantID.String(), timelineID.String(), limit, offset)
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

func (b *PgBackend) SetEndpointAddress(ctx context.Context, endpointID uuid.UUID, address string) error {
	_, err := b.pool.Exec(ctx,
		`UPDATE endpoint SET address = $2 WHERE id = $1::uuid`,
		endpointID.String(), address)
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

// timelineTenant resolves the storage-layer tenant id of the timeline addressed
// by timelineID within poolID, or ErrNotFound if the pool or the timeline does
// not exist. timeline_id is a generated UUID, so it is unique within a pool.
func (b *PgBackend) timelineTenant(ctx context.Context, poolID, timelineID uuid.UUID) (uuid.UUID, error) {
	var tenantStr string
	err := b.pool.QueryRow(ctx,
		`SELECT c.tenant_id::text
		 FROM pg_cluster c
		 JOIN tenant t ON c.tenant_id = t.id
		 WHERE t.storage_pool_id = $1::uuid AND c.timeline_id = $2::uuid`,
		poolID.String(), timelineID.String()).Scan(&tenantStr)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve timeline tenant: %w", err)
	}
	tenantID, err := uuid.Parse(tenantStr)
	if err != nil {
		return uuid.Nil, fmt.Errorf("parse tenant id: %w", err)
	}
	return tenantID, nil
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
		ep          Endpoint
	)
	if err := r.Scan(&idStr, &tenantStr, &timelineStr, &ep.Role, &ep.Address, &ep.CreatedAt); err != nil {
		return Endpoint{}, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse endpoint id: %w", err)
	}
	tenant, err := uuid.Parse(tenantStr)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse tenant id: %w", err)
	}
	timeline, err := uuid.Parse(timelineStr)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse timeline id: %w", err)
	}
	ep.ID = id
	ep.TenantID = tenant
	ep.TimelineID = timeline
	return ep, nil
}
