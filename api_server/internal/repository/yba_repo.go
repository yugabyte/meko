package repository

import (
	"database/sql"
	"fmt"
)

// YBARepo encapsulates database operations for YBA universe management.
type YBARepo struct {
	db *sql.DB
}

// NewYBARepo creates a new YBARepo.
func NewYBARepo(db *sql.DB) *YBARepo {
	return &YBARepo{db: db}
}

const maxDatapacksPerUniverse = 100

// UniverseAssignment holds the universe selected for a new datapack.
type UniverseAssignment struct {
	UniverseID string
	Hostname   string
	Port       int
	AdminConnStr string
}

// AssignUniverse atomically picks a universe with capacity and increments its
// counter. Returns an error if no universe has room. Backed by
// meko_system.yb_universes.
func (r *YBARepo) AssignUniverse() (*UniverseAssignment, error) {
	var a UniverseAssignment
	err := r.db.QueryRow(
		`UPDATE meko_system.yb_universes
		 SET num_datapacks_created = num_datapacks_created + 1,
		     updated_at = NOW()
		 WHERE universe_id = (
		     SELECT universe_id FROM meko_system.yb_universes
		     WHERE num_datapacks_created < $1
		     ORDER BY num_datapacks_created ASC
		     LIMIT 1
		     FOR UPDATE SKIP LOCKED
		 )
		 RETURNING universe_id, ysql_hostname, ysql_port, ysql_admin_connection_string`,
		maxDatapacksPerUniverse,
	).Scan(&a.UniverseID, &a.Hostname, &a.Port, &a.AdminConnStr)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no available universe with capacity (all have >= %d datapacks)", maxDatapacksPerUniverse)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to assign universe: %w", err)
	}
	return &a, nil
}

// GetAdminConnStr returns the admin connection string for a universe.
func (r *YBARepo) GetAdminConnStr(universeID string) (string, error) {
	var connStr string
	err := r.db.QueryRow(
		`SELECT ysql_admin_connection_string
		 FROM meko_system.yb_universes
		 WHERE universe_id = $1`, universeID,
	).Scan(&connStr)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("universe %s not found", universeID)
	}
	if err != nil {
		return "", fmt.Errorf("failed to get admin conn str for universe %s: %w", universeID, err)
	}
	return connStr, nil
}

// ReleaseUniverse decrements the counter when a datapack is removed.
func (r *YBARepo) ReleaseUniverse(universeID string) error {
	_, err := r.db.Exec(
		`UPDATE meko_system.yb_universes
		 SET num_datapacks_created = GREATEST(num_datapacks_created - 1, 0),
		     updated_at = NOW()
		 WHERE universe_id = $1`,
		universeID,
	)
	if err != nil {
		return fmt.Errorf("failed to release universe %s: %w", universeID, err)
	}
	return nil
}
