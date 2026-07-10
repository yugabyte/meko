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

// Upsert records (or refreshes) the datapack→pool mapping. datapack_id is the
// primary key, so a repeat call updates status/updated_at rather than
// duplicating the row.
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
