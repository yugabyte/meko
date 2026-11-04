package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Project errors.
var (
	ErrNoProject       = errors.New("no such project")
	ErrNoFolder        = errors.New("no such folder")
	ErrProjectNameUsed = errors.New("a project with that name already exists")
	ErrFolderNameUsed  = errors.New("a folder with that name is already in the project")
)

// Project is a project, with its folders' names (by name) and how many datapacks it files.
type Project struct {
	ID            string
	Name          string
	Description   string
	ThemeKey      string
	FolderNames   []string
	DatapackCount int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Folder is a project's folder and how many datapacks it files.
type Folder struct {
	ID            string
	ProjectID     string
	Name          string
	Description   string
	DatapackCount int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// FiledDatapack is a datapack a project files, with what it holds.
type FiledDatapack struct {
	DatapackID     string
	DatapackName   string
	FolderID       string
	UpdatedAt      time.Time
	PlacedAt       time.Time
	MemoryCount    int
	LearningCount  int // memories waiting to be kept or promoted
	PromotedCount  int
	KnowledgeCount int
}

// Placement is where a datapack is filed.
type Placement struct {
	DatapackID  string
	ProjectID   string
	ProjectName string
	FolderID    string
	FolderName  string
}

// NewFolder is a folder to make.
type NewFolder struct{ Name, Description string }

func parseTimes(dst []*time.Time, src ...string) error {
	for i, s := range src {
		t, err := time.Parse(timeLayout, s)
		if err != nil {
			return err
		}
		*dst[i] = t
	}
	return nil
}

// Projects lists the projects, newest first.
func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	return s.projects(ctx, "")
}

// Project is one project, or ErrNoProject.
func (s *Store) Project(ctx context.Context, id string) (Project, error) {
	ps, err := s.projects(ctx, id)
	if err != nil {
		return Project{}, err
	}
	if len(ps) == 0 {
		return Project{}, ErrNoProject
	}
	return ps[0], nil
}

func (s *Store) projects(ctx context.Context, id string) ([]Project, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT p.id, p.name, p.description, p.theme_key, p.created_at, p.updated_at, "+
		"(SELECT count(*) FROM datapack_placements d WHERE d.project_id = p.id), "+
		"(SELECT coalesce(json_group_array(name), '[]') FROM "+
		"(SELECT f.name FROM project_folders f WHERE f.project_id = p.id ORDER BY f.name)) "+
		"FROM projects p WHERE ? = '' OR p.id = ? ORDER BY p.created_at DESC, p.id", id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var p Project
		var created, updated, folders string
		if err = rows.Scan(&p.ID, &p.Name, &p.Description, &p.ThemeKey, &created, &updated, &p.DatapackCount,
			&folders); err != nil {
			return nil, err
		}
		if err = parseTimes([]*time.Time{&p.CreatedAt, &p.UpdatedAt}, created, updated); err != nil {
			return nil, err
		}
		if p.FolderNames, err = jsonStrings(folders); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CreateProject makes a project and its first folders in one transaction, or returns
// ErrProjectNameUsed or ErrFolderNameUsed.
func (s *Store) CreateProject(ctx context.Context, name, description, theme string, folders []NewFolder) (
	Project, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return Project{}, err
	}
	defer func() { _ = tx.Rollback() }()
	id := uuid.NewString()
	now := time.Now().UTC().Format(timeLayout)
	_, err = tx.ExecContext(ctx, "INSERT INTO projects(id, name, description, theme_key, created_at, updated_at) "+
		"VALUES (?, ?, ?, ?, ?, ?)", id, name, description, theme, now, now)
	if isUniqueViolation(err) {
		return Project{}, ErrProjectNameUsed
	}
	if err != nil {
		return Project{}, err
	}
	for _, f := range folders {
		if _, err = insertFolder(ctx, tx, id, f, now); err != nil {
			return Project{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return Project{}, err
	}
	return s.Project(ctx, id)
}

func insertFolder(ctx context.Context, tx *sql.Tx, project string, f NewFolder, now string) (string, error) {
	id := uuid.NewString()
	_, err := tx.ExecContext(ctx, "INSERT INTO project_folders(id, project_id, name, description, created_at, "+
		"updated_at) VALUES (?, ?, ?, ?, ?, ?)", id, project, f.Name, f.Description, now, now)
	if isUniqueViolation(err) {
		return "", ErrFolderNameUsed
	}
	if isForeignKeyViolation(err) {
		return "", ErrNoProject
	}
	return id, err
}

// UpdateProject sets a project's name or description (nil leaves it), or returns ErrNoProject or
// ErrProjectNameUsed.
func (s *Store) UpdateProject(ctx context.Context, id string, name, description *string) error {
	res, err := s.w.ExecContext(ctx, "UPDATE projects SET name = COALESCE(?, name), "+
		"description = COALESCE(?, description), updated_at = ? WHERE id = ?",
		name, description, time.Now().UTC().Format(timeLayout), id)
	if isUniqueViolation(err) {
		return ErrProjectNameUsed
	}
	return affected(res, err, ErrNoProject)
}

// DeleteProject deletes a project and its folders, unfiling their datapacks, or returns ErrNoProject.
func (s *Store) DeleteProject(ctx context.Context, id string) error {
	res, err := s.w.ExecContext(ctx, "DELETE FROM projects WHERE id = ?", id)
	return affected(res, err, ErrNoProject)
}

// Folders is a project's folders, by name.
func (s *Store) Folders(ctx context.Context, project string) ([]Folder, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT f.id, f.project_id, f.name, f.description, f.created_at, f.updated_at, "+
		"(SELECT count(*) FROM datapack_placements d WHERE d.folder_id = f.id) "+
		"FROM project_folders f WHERE f.project_id = ? ORDER BY f.name, f.id", project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Folder{}
	for rows.Next() {
		var f Folder
		var created, updated string
		if err = rows.Scan(&f.ID, &f.ProjectID, &f.Name, &f.Description, &created, &updated,
			&f.DatapackCount); err != nil {
			return nil, err
		}
		if err = parseTimes([]*time.Time{&f.CreatedAt, &f.UpdatedAt}, created, updated); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// CreateFolder adds a folder to a project, or returns ErrNoProject or ErrFolderNameUsed.
func (s *Store) CreateFolder(ctx context.Context, project string, f NewFolder) (Folder, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return Folder{}, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(timeLayout)
	id, err := insertFolder(ctx, tx, project, f, now)
	if err != nil {
		return Folder{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE projects SET updated_at = ? WHERE id = ?", now, project); err != nil {
		return Folder{}, err
	}
	if err = tx.Commit(); err != nil {
		return Folder{}, err
	}
	fs, err := s.Folders(ctx, project)
	if err != nil {
		return Folder{}, err
	}
	for _, x := range fs {
		if x.ID == id {
			return x, nil
		}
	}
	return Folder{}, ErrNoFolder
}

// UpdateFolder sets a project's folder's name or description (nil leaves it), or returns
// ErrNoFolder or ErrFolderNameUsed.
func (s *Store) UpdateFolder(ctx context.Context, project, id string, name, description *string) error {
	res, err := s.w.ExecContext(ctx, "UPDATE project_folders SET name = COALESCE(?, name), "+
		"description = COALESCE(?, description), updated_at = ? WHERE id = ? AND project_id = ?",
		name, description, time.Now().UTC().Format(timeLayout), id, project)
	if isUniqueViolation(err) {
		return ErrFolderNameUsed
	}
	return affected(res, err, ErrNoFolder)
}

// DeleteFolder deletes a project's folder, unfiling its datapacks, or returns ErrNoFolder.
func (s *Store) DeleteFolder(ctx context.Context, project, id string) error {
	res, err := s.w.ExecContext(ctx, "DELETE FROM project_folders WHERE id = ? AND project_id = ?", id, project)
	return affected(res, err, ErrNoFolder)
}

// FiledDatapacks is the datapacks a project files, by name.
func (s *Store) FiledDatapacks(ctx context.Context, project string) ([]FiledDatapack, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT d.id, d.name, p.folder_id, d.updated_at, p.placed_at, "+
		"(SELECT count(*) FROM memories m WHERE m.datapack_id = d.id), "+
		"(SELECT count(*) FROM memories m WHERE m.datapack_id = d.id AND m.promotion_action = ''), "+
		"(SELECT count(*) FROM promoted_memories m WHERE m.datapack_id = d.id), "+
		"(SELECT count(*) FROM kb_documents k WHERE k.datapack_id = d.id) "+
		"FROM datapack_placements p JOIN datapacks d ON d.id = p.datapack_id WHERE p.project_id = ? "+
		"ORDER BY d.name, d.id", project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FiledDatapack{}
	for rows.Next() {
		var f FiledDatapack
		var updated, placed string
		if err = rows.Scan(&f.DatapackID, &f.DatapackName, &f.FolderID, &updated, &placed, &f.MemoryCount,
			&f.LearningCount, &f.PromotedCount, &f.KnowledgeCount); err != nil {
			return nil, err
		}
		if err = parseTimes([]*time.Time{&f.UpdatedAt, &f.PlacedAt}, updated, placed); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// PlaceDatapack files a datapack in a folder, moving it from where it was, or returns
// ErrNoDatapack or ErrNoFolder.
func (s *Store) PlaceDatapack(ctx context.Context, datapack, folder string) error {
	return placeIn(ctx, s.w, datapack, folder, time.Now().UTC().Format(timeLayout))
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// placeIn files a datapack in a folder, or returns ErrNoDatapack or ErrNoFolder.
func placeIn(ctx context.Context, db execer, datapack, folder, now string) error {
	res, err := db.ExecContext(ctx, "INSERT INTO datapack_placements(datapack_id, project_id, folder_id, "+
		"placed_at, updated_at) SELECT ?, f.project_id, f.id, ?, ? FROM project_folders f WHERE f.id = ? "+
		"ON CONFLICT(datapack_id) DO UPDATE SET project_id = excluded.project_id, folder_id = excluded.folder_id, "+
		"placed_at = excluded.placed_at, updated_at = excluded.updated_at", datapack, now, now, folder)
	if isForeignKeyViolation(err) {
		return ErrNoDatapack
	}
	return affected(res, err, ErrNoFolder)
}

// UnplaceDatapack unfiles a datapack; one that isn't filed is left as it is.
func (s *Store) UnplaceDatapack(ctx context.Context, datapack string) error {
	_, err := s.w.ExecContext(ctx, "DELETE FROM datapack_placements WHERE datapack_id = ?", datapack)
	return err
}

// Placements is where each of the datapacks (all of them, for nil) is filed; one that isn't filed
// has no entry.
func (s *Store) Placements(ctx context.Context, datapacks []string) (map[string]Placement, error) {
	q := "SELECT p.datapack_id, p.project_id, j.name, p.folder_id, f.name FROM datapack_placements p " +
		"JOIN projects j ON j.id = p.project_id JOIN project_folders f ON f.id = p.folder_id"
	args := make([]any, len(datapacks))
	if datapacks != nil {
		for i, d := range datapacks {
			args[i] = d
		}
		q += " WHERE p.datapack_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(datapacks)), ",") + ")"
		if len(datapacks) == 0 {
			return map[string]Placement{}, nil
		}
	}
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Placement{}
	for rows.Next() {
		var p Placement
		if err = rows.Scan(&p.DatapackID, &p.ProjectID, &p.ProjectName, &p.FolderID, &p.FolderName); err != nil {
			return nil, err
		}
		out[p.DatapackID] = p
	}
	return out, rows.Err()
}

// ProjectActivity is a page of a project's events, newest first, starting after the cursor (nil for
// the newest), and the cursor of its last event: its folders made, its datapacks filed, and their
// file uploads and promotions. Like a datapack's knowledge activity, it's read from the rows that
// exist, so a deleted file or a moved datapack's earlier filing isn't there.
func (s *Store) ProjectActivity(ctx context.Context, project string, after *ActivityCursor, limit int) (
	[]Activity, *ActivityCursor, error) {
	ts, id := "~", ""
	if after != nil {
		ts, id = after.Timestamp, after.ID
	}
	rows, err := s.r.QueryContext(ctx, "SELECT * FROM ("+
		"SELECT 'folder_created' AS type, f.created_at AS ts, f.id AS id, '', '', 0, 0, '', '', '', '', '', "+
		"f.id, f.name FROM project_folders f WHERE f.project_id = ? "+
		"UNION ALL SELECT 'datapack_filed', p.placed_at, p.datapack_id, '', '', 0, 0, '', '', '', d.id, d.name, "+
		"f.id, f.name FROM datapack_placements p JOIN datapacks d ON d.id = p.datapack_id "+
		"JOIN project_folders f ON f.id = p.folder_id WHERE p.project_id = ? "+
		"UNION ALL SELECT 'file_upload', k.uploaded_at, k.id, k.document_key, k.document_type, "+
		"(SELECT count(*) FROM kb_chunks c WHERE c.document_rowid = k.rowid AND c.embedded = 1), k.size_bytes, "+
		"'', '', '', d.id, d.name, f.id, f.name FROM kb_documents k JOIN datapack_placements p "+
		"ON p.datapack_id = k.datapack_id JOIN datapacks d ON d.id = k.datapack_id "+
		"JOIN project_folders f ON f.id = p.folder_id WHERE p.project_id = ? "+
		"UNION ALL SELECT 'memory_promotion', m.promoted_at, m.id, '', '', 0, 0, m.text, m.agent_id, "+
		"m.conversation_id, d.id, d.name, f.id, f.name FROM promoted_memories m JOIN datapack_placements p "+
		"ON p.datapack_id = m.datapack_id JOIN datapacks d ON d.id = m.datapack_id "+
		"JOIN project_folders f ON f.id = p.folder_id WHERE p.project_id = ?) "+
		"WHERE ts < ? OR (ts = ? AND id < ?) ORDER BY ts DESC, id DESC LIMIT ?",
		project, project, project, project, ts, ts, id, min(max(limit, 1), maxListLimit))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []Activity{}
	var last *ActivityCursor
	for rows.Next() {
		var a Activity
		var at string
		if err = rows.Scan(&a.Type, &at, &a.ID, &a.DocumentKey, &a.DocumentType, &a.ChunksProcessed, &a.SizeBytes,
			&a.Content, &a.AgentID, &a.ConversationID, &a.DatapackID, &a.DatapackName, &a.FolderID,
			&a.FolderName); err != nil {
			return nil, nil, err
		}
		if a.Timestamp, err = time.Parse(timeLayout, at); err != nil {
			return nil, nil, err
		}
		out = append(out, a)
		last = &ActivityCursor{Timestamp: at, ID: a.ID}
	}
	return out, last, rows.Err()
}

func jsonStrings(s string) ([]string, error) {
	out := []string{}
	err := json.Unmarshal([]byte(s), &out)
	return out, err
}

// affected is err, or else missing when the statement changed no row.
func affected(res sql.Result, err, missing error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return err
		}
		return missing
	}
	return nil
}

// ProjectStats is what a project's datapacks hold in all, as Meko Cloud's project stats: searches by
// tool (the call log's, which keeps 90 days), conversations, knowledge files (by MIME type) and their
// embedded chunks (not promoted memories'), and artifacts.
type ProjectStats struct {
	KBFilesByType  map[string]int64
	SearchesHybrid int64
	SearchesKB     int64
	SearchesMemory int64
	Conversations  int64
	KBFiles        int64
	KBChunks       int64
	Artifacts      int64
	ArtifactBytes  int64
}

// ProjectStats sums what a project's datapacks hold.
func (s *Store) ProjectStats(ctx context.Context, project string) (ProjectStats, error) {
	const in = "IN (SELECT datapack_id FROM datapack_placements WHERE project_id = ?)"
	out := ProjectStats{KBFilesByType: map[string]int64{}}
	if err := s.r.QueryRowContext(ctx, "SELECT "+
		"(SELECT count(*) FROM call_log WHERE ok = 1 AND tool = 'context_search' AND datapack_id "+in+"), "+
		"(SELECT count(*) FROM call_log WHERE ok = 1 AND tool = 'knowledgebase_search' AND datapack_id "+in+"), "+
		"(SELECT count(*) FROM call_log WHERE ok = 1 AND tool = 'memory_search' AND datapack_id "+in+"), "+
		"(SELECT count(*) FROM conversations WHERE datapack_id "+in+"), "+
		"(SELECT count(*) FROM kb_chunks WHERE embedded = 1 AND document_rowid IS NOT NULL AND datapack_id "+in+"), "+
		"(SELECT count(*) FROM artifacts WHERE datapack_id "+in+"), "+
		"(SELECT coalesce(sum(size_bytes), 0) FROM artifacts WHERE datapack_id "+in+")",
		project, project, project, project, project, project, project).Scan(&out.SearchesHybrid, &out.SearchesKB,
		&out.SearchesMemory, &out.Conversations, &out.KBChunks, &out.Artifacts, &out.ArtifactBytes); err != nil {
		return out, err
	}
	rows, err := s.r.QueryContext(ctx, "SELECT document_type, count(*) FROM kb_documents WHERE datapack_id "+in+
		" GROUP BY document_type", project)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var typ string
		var n int64
		if err = rows.Scan(&typ, &n); err != nil {
			return out, err
		}
		out.KBFilesByType[typ] = n
		out.KBFiles += n
	}
	return out, rows.Err()
}
