package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AMPRepo reads/writes meko_system.amp_datapack_pool — the 1:1 mapping from a
// Meko datapack to its AMP shared pool. shared_pool_id equals datapack_id by
// construction (Meko passes the datapack id as the pool id when provisioning).
type AMPRepo struct {
	db *sql.DB
}

func NewAMPRepo(db *sql.DB) *AMPRepo {
	return &AMPRepo{db: db}
}

// AMPDatapackPool is one mapping row.
type AMPDatapackPool struct {
	DatapackID   string
	SharedPoolID string
	OwnerUserID  string
	Status       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// GetByDatapack returns the mapping for a datapack, or (nil, nil) if none exists
// yet (no AMP pool has been provisioned for it).
func (r *AMPRepo) GetByDatapack(ctx context.Context, datapackID string) (*AMPDatapackPool, error) {
	var m AMPDatapackPool
	err := r.db.QueryRowContext(ctx,
		`SELECT datapack_id, shared_pool_id, owner_user_id, status, created_at, updated_at
		 FROM   meko_system.amp_datapack_pool
		 WHERE  datapack_id = $1`,
		datapackID,
	).Scan(&m.DatapackID, &m.SharedPoolID, &m.OwnerUserID, &m.Status, &m.CreatedAt, &m.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("amp_datapack_pool: get %s: %w", datapackID, err)
	}
	return &m, nil
}

// ListByOwner returns every ready pool mapping owned by a user, across all
// their datapacks. Used to sum a user's total AMP compute for the per-tier core
// limit.
//
// Only status='ready' is counted. That is sound because no code path downgrades
// a live 'ready' row: the failure path (MarkPoolFailed) carries a
// `WHERE status <> 'ready'` guard, and the success path (Upsert) is only ever
// called with "ready" (amp_service.go). EnsurePool re-runs on every create — even
// for a datapack that already has live compute — so a controller blip must not
// flip its pool to 'failed' and drop its endpoints out of this sum, which would
// let the owner slip past the cap. Conversely a 'failed' row never provisioned
// successfully, so its pool holds no live compute and its shared_pool_id may not
// even exist on the controller — which is exactly why we must NOT fan out to it.
func (r *AMPRepo) ListByOwner(ctx context.Context, ownerUserID string) ([]AMPDatapackPool, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT datapack_id, shared_pool_id, owner_user_id, status, created_at, updated_at
		 FROM   meko_system.amp_datapack_pool
		 WHERE  owner_user_id = $1 AND status = 'ready'`,
		ownerUserID,
	)
	if err != nil {
		return nil, fmt.Errorf("amp_datapack_pool: list by owner %s: %w", ownerUserID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []AMPDatapackPool
	for rows.Next() {
		var m AMPDatapackPool
		if err := rows.Scan(&m.DatapackID, &m.SharedPoolID, &m.OwnerUserID, &m.Status, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("amp_datapack_pool: scan owner row: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Upsert records (or refreshes) the datapack→pool mapping. datapack_id is the
// primary key, so a repeat call updates status/updated_at rather than
// duplicating the row.
//
// It has NO ready-guard: it overwrites status with whatever the caller passes.
// This is the SUCCESS path and must only be called with "ready" — the failure
// path is MarkPoolFailed, which does guard 'ready'. ListByOwner's ready-only sum
// depends on that split (see its doc); don't call Upsert with a non-ready status.
func (r *AMPRepo) Upsert(ctx context.Context, datapackID, sharedPoolID, ownerUserID, status string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO meko_system.amp_datapack_pool (datapack_id, shared_pool_id, owner_user_id, status)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (datapack_id) DO UPDATE
		   SET status = EXCLUDED.status, updated_at = now()`,
		datapackID, sharedPoolID, ownerUserID, status,
	)
	if err != nil {
		return fmt.Errorf("amp_datapack_pool: upsert %s: %w", datapackID, err)
	}
	return nil
}

// MarkPoolFailed records a failed AMP provisioning attempt WITHOUT downgrading a
// pool that is already 'ready'. EnsurePool runs on every create-cluster (see
// AMPService.CreateCluster*), so a transient controller error on a datapack that
// already has live compute must not flip its pool to 'failed' — that would drop
// its endpoints out of ListByOwner and let the owner exceed the per-tier vCPU
// cap. A brand-new datapack (no row yet) is inserted as 'failed'; an existing
// non-'ready' row is refreshed; a 'ready' row is left untouched (the WHERE makes
// the UPDATE a no-op, not an error).
func (r *AMPRepo) MarkPoolFailed(ctx context.Context, datapackID, sharedPoolID, ownerUserID string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO meko_system.amp_datapack_pool (datapack_id, shared_pool_id, owner_user_id, status)
		 VALUES ($1, $2, $3, 'failed')
		 ON CONFLICT (datapack_id) DO UPDATE
		   SET status = 'failed', updated_at = now()
		   WHERE amp_datapack_pool.status <> 'ready'`,
		datapackID, sharedPoolID, ownerUserID,
	)
	if err != nil {
		return fmt.Errorf("amp_datapack_pool: mark failed %s: %w", datapackID, err)
	}
	return nil
}

// DeleteByDatapack removes the datapack→pool mapping row, called when a datapack
// is deleted after its AMP clusters are torn down. No-op if the row is absent.
func (r *AMPRepo) DeleteByDatapack(ctx context.Context, datapackID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM meko_system.amp_datapack_pool WHERE datapack_id = $1`,
		datapackID,
	)
	if err != nil {
		return fmt.Errorf("amp_datapack_pool: delete %s: %w", datapackID, err)
	}
	return nil
}
