package repository

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"

	"meko/api_server/internal/models"
)

// ErrPlacementNotFound is returned when a datapack has no placement — i.e. it
// is unfiled.
var ErrPlacementNotFound = errors.New("datapack is not filed in a project")

// PlacementRepo wraps meko_system.datapack_placements: where a datapack lives.
//
// EXACTLY ONE row per datapack. Sharing does not duplicate it: a recipient
// sees the owner's project read-only rather than getting a copy in a tree of
// their own, so there is one home per datapack and one truth about its path.
// Access scoping happens by filtering to the datapacks a caller can see, never
// by giving them their own placement.
type PlacementRepo struct {
	db *sql.DB
}

func NewPlacementRepo(db *sql.DB) *PlacementRepo {
	return &PlacementRepo{db: db}
}

// Place files a datapack into a folder, or moves it if already filed. One
// statement either way: the primary key is datapack_id, so re-filing is an
// upsert rather than a delete-then-insert that could leave it unfiled on a
// crash.
func (r *PlacementRepo) Place(datapackID, projectID, folderID string) error {
	_, err := r.db.Exec(
		`INSERT INTO meko_system.datapack_placements (datapack_id, project_id, folder_id)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (datapack_id) DO UPDATE
		     SET project_id = EXCLUDED.project_id,
		         folder_id  = EXCLUDED.folder_id,
		         updated_at = NOW()`,
		datapackID, projectID, folderID,
	)
	if err != nil {
		return fmt.Errorf("datapack_placements: place %s: %w", datapackID, err)
	}
	return nil
}

// Unplace makes a datapack unfiled again.
func (r *PlacementRepo) Unplace(datapackID string) error {
	_, err := r.db.Exec(
		`DELETE FROM meko_system.datapack_placements WHERE datapack_id = $1`,
		datapackID,
	)
	if err != nil {
		return fmt.Errorf("datapack_placements: unplace %s: %w", datapackID, err)
	}
	return nil
}

// Get reads one placement by primary key. ErrPlacementNotFound means unfiled,
// which is an ordinary state rather than an error for most callers.
func (r *PlacementRepo) Get(datapackID string) (*models.DatapackPlacement, error) {
	var p models.DatapackPlacement
	err := r.db.QueryRow(
		`SELECT datapack_id, project_id, folder_id, placed_at, updated_at
		 FROM   meko_system.datapack_placements
		 WHERE  datapack_id = $1`,
		datapackID,
	).Scan(&p.DatapackID, &p.ProjectID, &p.FolderID, &p.PlacedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlacementNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("datapack_placements: get %s: %w", datapackID, err)
	}
	return &p, nil
}

// CountsByFolderForOwner returns the per-folder datapack count across every
// project userID OWNS, in one query.
//
// Joined through projects rather than filtered on the placement row, because a
// placement no longer carries a user: the owner of the project is the owner of
// everything filed in it. Served by idx_projects_user_id then
// idx_placements_project, so neither side is scanned.
func (r *PlacementRepo) CountsByFolderForOwner(userID string) ([]models.FolderDatapackCount, error) {
	rows, err := r.db.Query(
		`SELECT dp.project_id, dp.folder_id, COUNT(*)
		 FROM   meko_system.datapack_placements dp
		 JOIN   meko_system.projects p ON p.project_id = dp.project_id
		 WHERE  p.user_id = $1
		 GROUP  BY dp.project_id, dp.folder_id`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("datapack_placements: owner counts for %s: %w", userID, err)
	}
	defer rows.Close()

	var out []models.FolderDatapackCount
	for rows.Next() {
		var c models.FolderDatapackCount
		if sErr := rows.Scan(&c.ProjectID, &c.FolderID, &c.Count); sErr != nil {
			return nil, fmt.Errorf("datapack_placements: scan owner count: %w", sErr)
		}
		out = append(out, c)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("datapack_placements: iterate owner counts: %w", err)
	}
	return out, nil
}

// ForDatapacks returns the placements of the given datapacks, skipping any
// that are unfiled.
//
// This is THE access-scoping primitive: callers pass the datapacks the user is
// allowed to see, and nothing outside that set can come back. Every lookup is
// on the primary key, so it stays an index probe per id rather than a scan.
func (r *PlacementRepo) ForDatapacks(datapackIDs []string) ([]models.DatapackPlacement, error) {
	if len(datapackIDs) == 0 {
		return nil, nil
	}
	rows, err := r.db.Query(
		`SELECT datapack_id, project_id, folder_id, placed_at, updated_at
		 FROM   meko_system.datapack_placements
		 WHERE  datapack_id = ANY($1)`,
		pq.Array(datapackIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("datapack_placements: for datapacks: %w", err)
	}
	defer rows.Close()

	var out []models.DatapackPlacement
	for rows.Next() {
		var p models.DatapackPlacement
		if sErr := rows.Scan(
			&p.DatapackID, &p.ProjectID, &p.FolderID, &p.PlacedAt, &p.UpdatedAt,
		); sErr != nil {
			return nil, fmt.Errorf("datapack_placements: scan: %w", sErr)
		}
		out = append(out, p)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("datapack_placements: iterate: %w", err)
	}
	return out, nil
}
