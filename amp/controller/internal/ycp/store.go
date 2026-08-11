package ycp

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Identity is the controller's persisted YCP enrollment: who it enrolled as
// (EnvName), the environment id the control plane assigned, and the long-lived
// status token it reports with. Persisting it is what lets a pod restart resume
// without re-enrolling (the enrollment token is short-lived).
type Identity struct {
	EnvName       string
	EnvironmentID string
	StatusToken   string
}

// Store persists the YCP identity in the controller's own schema (the ycp_identity
// table from migration 0004), reached over the same pool as the task log — the
// pool's search_path is pinned to that schema, so the unqualified table resolves
// there.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore constructs the store over the controller pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Load returns the persisted identity for envName, or (nil, nil) when none has
// been stored yet (a first-ever start, or ENV_NAME changed).
func (s *Store) Load(ctx context.Context, envName string) (*Identity, error) {
	var id Identity
	err := s.pool.QueryRow(ctx,
		`SELECT env_name, environment_id, status_token FROM ycp_identity WHERE env_name = $1`,
		envName).Scan(&id.EnvName, &id.EnvironmentID, &id.StatusToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load ycp identity: %w", err)
	}
	return &id, nil
}

// Save upserts the identity, keyed by env_name. Called once after a successful
// enroll; idempotent on restart.
func (s *Store) Save(ctx context.Context, id Identity) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO ycp_identity (env_name, environment_id, status_token)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (env_name) DO UPDATE SET
		     environment_id = EXCLUDED.environment_id,
		     status_token   = EXCLUDED.status_token,
		     updated_at     = now()`,
		id.EnvName, id.EnvironmentID, id.StatusToken)
	if err != nil {
		return fmt.Errorf("save ycp identity: %w", err)
	}
	return nil
}
