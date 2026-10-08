package repository

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"
)

// YBARepo encapsulates database operations for YBA universe management.
type YBARepo struct {
	db *sql.DB
}

// NewYBARepo creates a new YBARepo.
func NewYBARepo(db *sql.DB) *YBARepo {
	return &YBARepo{db: db}
}

// UniverseAssignment holds the universe selected for a new datapack.
type UniverseAssignment struct {
	UniverseID   string
	Hostname     string
	Port         int
	AdminConnStr string
}

// assignAttempts bounds how often AssignUniverse retries after the universe it
// waited on filled up, or a claim hit a serialization failure, while some
// universe still had room.
const assignAttempts = 3

var (
	// ErrNoUniverseCapacity means every universe has reached max_datapacks.
	ErrNoUniverseCapacity = errors.New("no available universe with capacity (all have reached their configured max_datapacks)")
	// ErrUniverseContention means some universe still had room, but every
	// attempt lost the race for it. Retrying the create can succeed.
	ErrUniverseContention = errors.New("no universe assigned: concurrent creates took every attempt while a universe still had room")
)

// AssignUniverse atomically picks a universe with capacity and increments its
// counter. Returns ErrNoUniverseCapacity if no universe has room. Backed by
// meko_system.yb_universes.
//
// It first skips locked rows, which spreads concurrent creates across
// universes. If every universe with room is locked (always the case with a
// single universe), it waits for the least-loaded one instead of reporting the
// pool full. A waiter re-checks only the row it waited on, so if that universe
// filled up meanwhile and another still has room, it tries again. A
// serialization failure (SQLSTATE 40001), which a waiter gets under snapshot
// isolation when the holder commits, is retried the same way.
func (r *YBARepo) AssignUniverse() (*UniverseAssignment, error) {
	for attempt := 0; attempt < assignAttempts; attempt++ {
		for _, lock := range []string{"FOR UPDATE SKIP LOCKED", "FOR UPDATE"} {
			a, err := r.assignUniverse(lock)
			if err != sql.ErrNoRows && !isSerializationFailure(err) {
				return a, err
			}
		}
		var room bool
		if err := r.db.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM meko_system.yb_universes
			                WHERE num_datapacks_created < max_datapacks)`,
		).Scan(&room); err != nil {
			return nil, fmt.Errorf("failed to assign universe: %w", err)
		}
		if !room {
			return nil, ErrNoUniverseCapacity
		}
	}
	return nil, ErrUniverseContention
}

func isSerializationFailure(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "40001"
}

// assignUniverse runs one claim with the given row-lock clause. It returns
// sql.ErrNoRows when no universe was claimed.
func (r *YBARepo) assignUniverse(lock string) (*UniverseAssignment, error) {
	var a UniverseAssignment
	err := r.db.QueryRow(
		`UPDATE meko_system.yb_universes
		 SET num_datapacks_created = num_datapacks_created + 1,
		     updated_at = NOW()
		 WHERE universe_id = (
		     SELECT universe_id FROM meko_system.yb_universes
		     WHERE num_datapacks_created < max_datapacks
		     ORDER BY num_datapacks_created ASC
		     LIMIT 1
		     `+lock+`
		 )
		 RETURNING universe_id, ysql_hostname, ysql_port, ysql_admin_connection_string`,
	).Scan(&a.UniverseID, &a.Hostname, &a.Port, &a.AdminConnStr)
	if err == sql.ErrNoRows {
		return nil, err
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
