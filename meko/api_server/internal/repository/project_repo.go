package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/lib/pq"

	"meko/api_server/internal/models"
)

// ErrProjectNotFound is returned when a project_id resolves to no row, or to
// a row owned by a different user.
var ErrProjectNotFound = errors.New("project not found")

// ErrFolderNotFound is returned when a folder_id resolves to no row.
var ErrFolderNotFound = errors.New("folder not found")

// ErrProjectNameTaken maps the uq_projects_user_name violation. Surfaced
// distinctly so the handler can answer 409 instead of 500.
var ErrProjectNameTaken = errors.New("a project with that name already exists")

// ErrFolderNameTaken maps the uq_project_folders_name violation.
var ErrFolderNameTaken = errors.New("a folder with that name already exists in this project")

// ProjectRepo wraps meko_system.projects and meko_system.project_folders —
// the user-owned tree that organizes datapacks. Where a datapack sits in that
// tree lives in meko_system.datapack_placements; see PlacementRepo.
type ProjectRepo struct {
	db *sql.DB
}

func NewProjectRepo(db *sql.DB) *ProjectRepo {
	return &ProjectRepo{db: db}
}

// CreateProjectParams is the input to Create. Folders is the theme's folder
// list, stamped into project_folders in the same transaction — see
// service.ProjectTheme.
type CreateProjectParams struct {
	UserID       string
	Name         string
	Description  string
	ThemeKey     string
	ThemeVersion string
	Folders      []string
}

// Create inserts a project and its theme folders in one transaction, so a
// project never exists without the layout the user picked.
func (r *ProjectRepo) Create(p CreateProjectParams) (*models.Project, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("projects: begin transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			slog.Warn("projects: rollback error", "user_id", p.UserID, "error", rbErr)
		}
	}()

	var out models.Project
	err = tx.QueryRow(
		`INSERT INTO meko_system.projects
		     (user_id, project_name, theme_key, theme_version, project_details)
		 VALUES ($1, $2, $3, $4, jsonb_build_object('description', $5::text))
		 RETURNING project_id, user_id, project_name, theme_key, theme_version,
		           COALESCE(project_details->>'description', ''),
		           created_at, updated_at`,
		p.UserID, p.Name, p.ThemeKey, p.ThemeVersion, p.Description,
	).Scan(
		&out.ID, &out.UserID, &out.Name, &out.ThemeKey, &out.ThemeVersion,
		&out.Description, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		if isUniqueViolation23505(err) {
			return nil, ErrProjectNameTaken
		}
		return nil, fmt.Errorf("projects: insert %q: %w", p.Name, err)
	}

	// One statement for every folder, not one per folder. Each round trip here
	// is a distributed write, so a four-folder theme cost five of them before
	// this and the create latency scaled with the theme's size.
	if len(p.Folders) > 0 {
		if _, fErr := tx.Exec(
			`INSERT INTO meko_system.project_folders (project_id, folder_name)
			 SELECT $1, folder_name
			 FROM   unnest($2::text[]) AS folder_name`,
			out.ID, pq.Array(p.Folders),
		); fErr != nil {
			return nil, fmt.Errorf("projects: stamp folders %v: %w", p.Folders, fErr)
		}
	}
	out.FolderCount = len(p.Folders)

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("projects: commit: %w", err)
	}
	return &out, nil
}

// List returns every project owned by userID, newest first. Served by
// idx_projects_user_id.
//
// Counts are NOT joined here: the caller fills FolderCount/DatapackCount from
// PlacementRepo.CountsByFolder and FolderCountsByProject, which are one query
// each for the whole page rather than one per project.
func (r *ProjectRepo) List(userID string) ([]models.Project, error) {
	rows, err := r.db.Query(
		`SELECT project_id, user_id, project_name, theme_key, theme_version,
		        COALESCE(project_details->>'description', ''),
		        created_at, updated_at
		 FROM   meko_system.projects
		 WHERE  user_id = $1
		 ORDER  BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("projects: list for %s: %w", userID, err)
	}
	defer rows.Close()

	var out []models.Project
	for rows.Next() {
		var p models.Project
		if sErr := rows.Scan(
			&p.ID, &p.UserID, &p.Name, &p.ThemeKey, &p.ThemeVersion,
			&p.Description, &p.CreatedAt, &p.UpdatedAt,
		); sErr != nil {
			return nil, fmt.Errorf("projects: scan: %w", sErr)
		}
		out = append(out, p)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("projects: iterate: %w", err)
	}
	return out, nil
}

// Get reads one project and checks ownership in the same statement, so a
// caller cannot read another user's project by guessing an id.
func (r *ProjectRepo) Get(userID, projectID string) (*models.Project, error) {
	var p models.Project
	err := r.db.QueryRow(
		`SELECT project_id, user_id, project_name, theme_key, theme_version,
		        COALESCE(project_details->>'description', ''),
		        created_at, updated_at
		 FROM   meko_system.projects
		 WHERE  project_id = $1 AND user_id = $2`,
		projectID, userID,
	).Scan(
		&p.ID, &p.UserID, &p.Name, &p.ThemeKey, &p.ThemeVersion,
		&p.Description, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("projects: get %s: %w", projectID, err)
	}
	return &p, nil
}

// Rename updates a project's name and description. Ownership is part of the
// WHERE clause for the same reason as Get.
func (r *ProjectRepo) Rename(userID, projectID, name, description string) error {
	res, err := r.db.Exec(
		`UPDATE meko_system.projects
		 SET    project_name    = $3,
		        project_details = jsonb_build_object('description', $4::text),
		        updated_at      = NOW()
		 WHERE  project_id = $1 AND user_id = $2`,
		projectID, userID, name, description,
	)
	if err != nil {
		if isUniqueViolation23505(err) {
			return ErrProjectNameTaken
		}
		return fmt.Errorf("projects: rename %s: %w", projectID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("projects: rename %s: rows affected: %w", projectID, err)
	}
	if n == 0 {
		return ErrProjectNotFound
	}
	return nil
}

// Delete removes the project row. Its folders and placements go with it via
// ON DELETE CASCADE.
//
// The DATAPACKS are NOT removed here, but that does not make deleting a
// project non-destructive: ProjectService.Delete tears each one down first,
// through the datapack teardown path, and only then calls this. Anything
// calling this repo method directly leaves the datapacks behind, unfiled.
func (r *ProjectRepo) Delete(userID, projectID string) error {
	res, err := r.db.Exec(
		`DELETE FROM meko_system.projects WHERE project_id = $1 AND user_id = $2`,
		projectID, userID,
	)
	if err != nil {
		return fmt.Errorf("projects: delete %s: %w", projectID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("projects: delete %s: rows affected: %w", projectID, err)
	}
	if n == 0 {
		return ErrProjectNotFound
	}
	return nil
}

// Folders returns a project's folders, from one covering-index scan on
// idx_project_folders_project. Sorted by name in Go: a folder list is tens of
// rows, so the sort is free and the index stays a pure lookup.
func (r *ProjectRepo) Folders(projectID string) ([]models.ProjectFolder, error) {
	rows, err := r.db.Query(
		`SELECT folder_id, project_id, folder_name, COALESCE(folder_description, ''), created_at, updated_at
		 FROM   meko_system.project_folders
		 WHERE  project_id = $1`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("project_folders: list for %s: %w", projectID, err)
	}
	defer rows.Close()

	// Non-nil even when empty: this reaches the wire as ProjectDetail.folders,
	// a required array, and a nil slice would marshal to `null`.
	out := make([]models.ProjectFolder, 0)
	for rows.Next() {
		var f models.ProjectFolder
		if sErr := rows.Scan(
			&f.ID, &f.ProjectID, &f.Name, &f.Description, &f.CreatedAt, &f.UpdatedAt,
		); sErr != nil {
			return nil, fmt.Errorf("project_folders: scan: %w", sErr)
		}
		out = append(out, f)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("project_folders: iterate: %w", err)
	}
	return out, nil
}

// ByIDs returns the given projects with their owner's display name and email, for the
// "Shared projects" section where the owner is the point.
//
// Takes ids rather than a user, because the caller has already worked out
// which projects it may see by following its own datapacks outward. Passing a
// user here would be asking a different, wider question.
func (r *ProjectRepo) ByIDs(projectIDs []string) ([]models.Project, error) {
	if len(projectIDs) == 0 {
		return nil, nil
	}
	rows, err := r.db.Query(
		`SELECT p.project_id, p.user_id, p.project_name, p.theme_key, p.theme_version,
		        COALESCE(p.project_details->>'description', ''),
		        COALESCE(NULLIF(TRIM(CONCAT(u.user_first_name, ' ', u.user_last_name)), ''), u.user_email, ''),
		        COALESCE(u.user_email, ''),
		        p.created_at, p.updated_at
		 FROM   meko_system.projects p
		 JOIN   meko_system.users u ON u.user_id = p.user_id
		 WHERE  p.project_id = ANY($1)`,
		pq.Array(projectIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("projects: by ids: %w", err)
	}
	defer rows.Close()

	var out []models.Project
	for rows.Next() {
		var p models.Project
		if sErr := rows.Scan(
			&p.ID, &p.UserID, &p.Name, &p.ThemeKey, &p.ThemeVersion,
			&p.Description, &p.OwnerName, &p.OwnerEmail, &p.CreatedAt, &p.UpdatedAt,
		); sErr != nil {
			return nil, fmt.Errorf("projects: scan: %w", sErr)
		}
		out = append(out, p)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("projects: iterate: %w", err)
	}
	return out, nil
}

// FoldersByProjects returns every folder in the given projects. Callers
// showing a SHARED project must filter the result down to folders the caller
// can actually reach before returning it.
func (r *ProjectRepo) FoldersByProjects(projectIDs []string) ([]models.ProjectFolder, error) {
	if len(projectIDs) == 0 {
		return nil, nil
	}
	rows, err := r.db.Query(
		`SELECT folder_id, project_id, folder_name, COALESCE(folder_description, ''), created_at, updated_at
		 FROM   meko_system.project_folders
		 WHERE  project_id = ANY($1)`,
		pq.Array(projectIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("project_folders: by projects: %w", err)
	}
	defer rows.Close()

	var out []models.ProjectFolder
	for rows.Next() {
		var f models.ProjectFolder
		if sErr := rows.Scan(
			&f.ID, &f.ProjectID, &f.Name, &f.Description, &f.CreatedAt, &f.UpdatedAt,
		); sErr != nil {
			return nil, fmt.Errorf("project_folders: scan by project: %w", sErr)
		}
		out = append(out, f)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("project_folders: iterate by projects: %w", err)
	}
	return out, nil
}

// FolderOwner reads the project a folder belongs to and that project's owner,
// in one indexed lookup.
//
// This is the authorization check behind filing a datapack: a folder_id is a
// client-supplied UUID, so without it a caller could file into a folder in
// someone else's tree. Returns ErrFolderNotFound when the folder doesn't
// exist.
func (r *ProjectRepo) FolderOwner(folderID string) (projectID, ownerUserID string, err error) {
	err = r.db.QueryRow(
		`SELECT f.project_id, p.user_id
		 FROM   meko_system.project_folders f
		 JOIN   meko_system.projects p ON p.project_id = f.project_id
		 WHERE  f.folder_id = $1`,
		folderID,
	).Scan(&projectID, &ownerUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrFolderNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("project_folders: owner of %s: %w", folderID, err)
	}
	return projectID, ownerUserID, nil
}

// SortedFolders is Folders with a stable, name-ordered result. There is no
// tree to assemble: folders do not nest.
func (r *ProjectRepo) SortedFolders(projectID string) ([]models.ProjectFolder, error) {
	folders, err := r.Folders(projectID)
	if err != nil {
		return nil, err
	}
	sort.Slice(folders, func(i, j int) bool { return folders[i].Name < folders[j].Name })
	return folders, nil
}

// CreateFolder adds a folder to a project. Folders do not nest, so there is
// no parent to resolve and no depth to check.
func (r *ProjectRepo) CreateFolder(
	projectID, name, description string,
) (*models.ProjectFolder, error) {
	var f models.ProjectFolder
	err := r.db.QueryRow(
		`INSERT INTO meko_system.project_folders
		     (project_id, folder_name, folder_description)
		 VALUES ($1, $2, NULLIF($3, ''))
		 RETURNING folder_id, project_id, folder_name, COALESCE(folder_description, ''), created_at, updated_at`,
		projectID, name, description,
	).Scan(&f.ID, &f.ProjectID, &f.Name, &f.Description, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		if isUniqueViolation23505(err) {
			return nil, ErrFolderNameTaken
		}
		return nil, fmt.Errorf("project_folders: insert %q: %w", name, err)
	}
	return &f, nil
}

// RenameFolder renames a folder, and replaces its description only when the
// caller sent one.
//
// A nil description leaves the column alone, so a rename-only PATCH cannot
// wipe a description it never mentioned; an empty string clears it. Without
// that distinction an older client sending just folder_name would silently
// drop the stored text.
func (r *ProjectRepo) RenameFolder(projectID, folderID, name string, description *string) error {
	var desc string
	if description != nil {
		desc = *description
	}
	res, err := r.db.Exec(
		`UPDATE meko_system.project_folders
		 SET    folder_name        = $3,
		        folder_description = CASE WHEN $4::boolean
		                                  THEN NULLIF($5, '')
		                                  ELSE folder_description END,
		        updated_at         = NOW()
		 WHERE  folder_id = $1 AND project_id = $2`,
		folderID, projectID, name, description != nil, desc,
	)
	if err != nil {
		if isUniqueViolation23505(err) {
			return ErrFolderNameTaken
		}
		return fmt.Errorf("project_folders: rename %s: %w", folderID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("project_folders: rename %s: rows affected: %w", folderID, err)
	}
	if n == 0 {
		return ErrFolderNotFound
	}
	return nil
}

// DeleteFolder removes a folder. Placements inside it cascade away, so the
// datapacks become unfiled rather than being deleted.
func (r *ProjectRepo) DeleteFolder(projectID, folderID string) error {
	res, err := r.db.Exec(
		`DELETE FROM meko_system.project_folders
		 WHERE folder_id = $1 AND project_id = $2`,
		folderID, projectID,
	)
	if err != nil {
		return fmt.Errorf("project_folders: delete %s: %w", folderID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("project_folders: delete %s: rows affected: %w", folderID, err)
	}
	if n == 0 {
		return ErrFolderNotFound
	}
	return nil
}

// ProjectMembers is who can reach a project's datapacks, and how many agents
// are actually working in them.
type ProjectMembers struct {
	// People is the owner plus everyone who can reach at least one datapack
	// in the project. Filled by PeopleWithAccess.
	People int `json:"people"`
	// Agents is distinct agents that have WRITTEN into those datapacks, not
	// agents with an access row. Filled from DatapackRollupRepo, which reads
	// the data database — see ProjectRollups.ActiveAgents for why the access
	// table is the wrong source.
	Agents int `json:"agents"`
}

// PeopleWithAccess counts the distinct people who can reach the given
// datapacks. ONE query whatever the number of datapacks, and both legs probe
// an index on datapack_id (agent_access_pkey, idx_invites_datapack_id) rather
// than scanning.
//
// "Can reach" is the same rule as sharedInvitesCTE for a non-owner: an
// agent_access row AND an accepted invite. Counting accepted
// invites alone would be wrong, because unlinking an agent removes the access
// but leaves the invite behind — that person would keep being counted after
// the datapack had stopped appearing in their list.
//
// ownerUserID is counted unconditionally, since the owner reaches their own
// datapacks without an invite, and excluded from the join so a self-invite
// could not double-count them.
//
// The caller passes the datapacks the VIEWER can see, so on a shared project
// this answers "who else reaches the part shared with me", not "who is in the
// owner's project" — which is the answer a recipient is allowed to have.
func (r *ProjectRepo) PeopleWithAccess(ownerUserID string, datapackIDs []string) (int, error) {
	if len(datapackIDs) == 0 {
		return 1, nil
	}
	var others int
	// The `i.datapack_id = ANY($1)` inside EXISTS is redundant against the
	// correlated equality beside it, and it is what keeps this off a
	// sequential scan: with only the correlated predicate the planner
	// materializes the whole of datapack_invites, whereas the constant array
	// gives it an Index Cond on idx_invites_datapack_id. Verified with
	// EXPLAIN — both legs index-scan, neither table is scanned.
	if err := r.db.QueryRow(
		`SELECT COUNT(DISTINCT aa.user_id)
		 FROM   meko_system.agent_access aa
		 WHERE  aa.datapack_id = ANY($1)
		   AND  aa.user_id <> $2
		   AND  EXISTS (
		          SELECT 1
		          FROM   meko_system.datapack_invites i
		          WHERE  i.datapack_id       = ANY($1)
		            AND  i.datapack_id       = aa.datapack_id
		            AND  i.recipient_user_id = aa.user_id
		            AND  i.status            = 'accepted')`,
		pq.Array(datapackIDs), ownerUserID,
	).Scan(&others); err != nil {
		return 1, fmt.Errorf("project members: %w", err)
	}
	return 1 + others, nil
}

// ActivityFolders returns "folder created" rows for a project, newest
// first, keyset-paged like every other side of the activity feed.
//
// Lives here rather than beside the other activity sides because this is the
// project repo's own table; KnowledgeBaseRepo could reach meko_system (it
// holds that pool too) but has no business knowing about folders. The caller
// merges the two.
//
// folderIDs is the SCOPED set: a recipient passes only the folders holding a
// datapack shared with them, so this cannot leak the rest of an owner's tree.
func (r *ProjectRepo) ActivityFolders(
	folderIDs []string, cursorTs, cursorID string, cap int,
) ([]ActivityLogRow, error) {
	if len(folderIDs) == 0 {
		return nil, nil
	}
	const baseQ = `SELECT created_at, folder_id::text, folder_name
		 FROM   meko_system.project_folders
		 WHERE  folder_id = ANY($1) `

	var (
		rows *sql.Rows
		err  error
	)
	if cursorTs == "" || cursorID == "" {
		rows, err = r.db.Query(
			baseQ+` ORDER BY created_at DESC, folder_id DESC LIMIT $2`,
			pq.Array(folderIDs), cap,
		)
	} else {
		rows, err = r.db.Query(
			baseQ+`   AND (created_at, folder_id) < ($2::timestamptz, $3::uuid)
				 ORDER BY created_at DESC, folder_id DESC LIMIT $4`,
			pq.Array(folderIDs), cursorTs, cursorID, cap,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("activity log folders: %w", err)
	}
	defer rows.Close()

	out := []ActivityLogRow{}
	for rows.Next() {
		var (
			ts   sql.NullTime
			id   sql.NullString
			name sql.NullString
		)
		if err := rows.Scan(&ts, &id, &name); err != nil {
			return nil, fmt.Errorf("scan folder activity row: %w", err)
		}
		it := ActivityLogItem{Type: ActivityFolderCreated}
		if ts.Valid {
			it.Timestamp = ts.Time.UTC().Format(time.RFC3339Nano)
		}
		if id.Valid {
			v := id.String
			it.FolderID = &v
		}
		if name.Valid {
			v := name.String
			it.FolderName = &v
		}
		out = append(out, ActivityLogRow{Ts: it.Timestamp, ID: id.String, Item: it})
	}
	return out, rows.Err()
}

// ActivityPlacements returns "datapack filed into a folder" rows.
//
// Keyed on updated_at, NOT the datapack's own created_at: a datapack created
// in March and filed here in September is a September event, and created_at
// would sort it into the wrong place entirely. updated_at rather than
// placed_at because placement is an upsert, so a move is the same row with a
// new folder, and the move is the thing that happened.
//
// datapackIDs is the caller's already-scoped visible set.
func (r *ProjectRepo) ActivityPlacements(
	datapackIDs []string, cursorTs, cursorID string, cap int,
) ([]ActivityLogRow, error) {
	if len(datapackIDs) == 0 {
		return nil, nil
	}
	const baseQ = `SELECT p.updated_at, p.datapack_id::text, f.folder_name, f.folder_id::text
		 FROM   meko_system.datapack_placements p
		 JOIN   meko_system.project_folders f ON f.folder_id = p.folder_id
		 WHERE  p.datapack_id = ANY($1) `

	var (
		rows *sql.Rows
		err  error
	)
	if cursorTs == "" || cursorID == "" {
		rows, err = r.db.Query(
			baseQ+` ORDER BY p.updated_at DESC, p.datapack_id DESC LIMIT $2`,
			pq.Array(datapackIDs), cap,
		)
	} else {
		rows, err = r.db.Query(
			baseQ+`   AND (p.updated_at, p.datapack_id) < ($2::timestamptz, $3::uuid)
				 ORDER BY p.updated_at DESC, p.datapack_id DESC LIMIT $4`,
			pq.Array(datapackIDs), cursorTs, cursorID, cap,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("activity log placements: %w", err)
	}
	defer rows.Close()

	out := []ActivityLogRow{}
	for rows.Next() {
		var (
			ts         sql.NullTime
			datapackID sql.NullString
			folderName sql.NullString
			folderID   sql.NullString
		)
		if err := rows.Scan(&ts, &datapackID, &folderName, &folderID); err != nil {
			return nil, fmt.Errorf("scan placement activity row: %w", err)
		}
		it := ActivityLogItem{Type: ActivityDatapackFiled}
		if ts.Valid {
			it.Timestamp = ts.Time.UTC().Format(time.RFC3339Nano)
		}
		if datapackID.Valid {
			it.DatapackID = datapackID.String
		}
		if folderName.Valid {
			v := folderName.String
			it.FolderName = &v
		}
		if folderID.Valid {
			v := folderID.String
			it.FolderID = &v
		}
		out = append(out, ActivityLogRow{Ts: it.Timestamp, ID: datapackID.String, Item: it})
	}
	return out, rows.Err()
}
