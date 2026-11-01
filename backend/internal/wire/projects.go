package wire

import (
	"github.com/yugabyte/meko/backend/internal/store"
)

// Project is a project in Meko Cloud's shape. The owner is the local user, so user_id is empty
// and there's no owner name or email, which Cloud gives only for a shared project.
type Project struct {
	ProjectID     string   `json:"project_id"`
	UserID        string   `json:"user_id"`
	ProjectName   string   `json:"project_name"`
	ThemeKey      string   `json:"theme_key"`
	ThemeVersion  string   `json:"theme_version"`
	Description   string   `json:"description"`
	FolderNames   []string `json:"folder_names"`
	FolderCount   int      `json:"folder_count"`
	DatapackCount int      `json:"datapack_count"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

// ProjectOf is a project in Cloud's shape.
func ProjectOf(p store.Project) Project {
	return Project{ProjectID: p.ID, ProjectName: p.Name, ThemeKey: p.ThemeKey, ThemeVersion: "1.0",
		Description: p.Description, FolderNames: p.FolderNames, FolderCount: len(p.FolderNames),
		DatapackCount: p.DatapackCount, CreatedAt: Time(p.CreatedAt), UpdatedAt: Time(p.UpdatedAt)}
}

// Folder is a project's folder in Cloud's shape.
type Folder struct {
	FolderID          string `json:"folder_id"`
	ProjectID         string `json:"project_id"`
	FolderName        string `json:"folder_name"`
	FolderDescription string `json:"folder_description"`
	DatapackCount     int    `json:"datapack_count"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

// FolderOf is a folder in Cloud's shape.
func FolderOf(f store.Folder) Folder {
	return Folder{FolderID: f.ID, ProjectID: f.ProjectID, FolderName: f.Name, FolderDescription: f.Description,
		DatapackCount: f.DatapackCount, CreatedAt: Time(f.CreatedAt), UpdatedAt: Time(f.UpdatedAt)}
}

// FiledDatapack is a project's datapack in Cloud's shape: awaiting_review_count is its learnings.
type FiledDatapack struct {
	DatapackID          string `json:"datapack_id"`
	DatapackName        string `json:"datapack_name"`
	FolderID            string `json:"folder_id"`
	UpdatedAt           string `json:"updated_at"`
	PlacedAt            string `json:"placed_at"`
	MemoryCount         int    `json:"memory_count"`
	AwaitingReviewCount int    `json:"awaiting_review_count"`
	PromotedCount       int    `json:"promoted_count"`
	KnowledgeCount      int    `json:"knowledge_count"`
}

// FiledDatapackOf is a filed datapack in Cloud's shape.
func FiledDatapackOf(f store.FiledDatapack) FiledDatapack {
	return FiledDatapack{DatapackID: f.DatapackID, DatapackName: f.DatapackName, FolderID: f.FolderID,
		UpdatedAt: Time(f.UpdatedAt), PlacedAt: Time(f.PlacedAt), MemoryCount: f.MemoryCount,
		AwaitingReviewCount: f.LearningCount,
		PromotedCount:       f.PromotedCount, KnowledgeCount: f.KnowledgeCount}
}

// Placement is where a datapack is filed, in Cloud's shape: path is project/folder.
type Placement struct {
	DatapackID  string `json:"datapack_id"`
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	FolderID    string `json:"folder_id"`
	FolderName  string `json:"folder_name"`
	Path        string `json:"path"`
}

// PlacementOf is a placement in Cloud's shape.
func PlacementOf(p store.Placement) Placement {
	return Placement{DatapackID: p.DatapackID, ProjectID: p.ProjectID, ProjectName: p.ProjectName,
		FolderID: p.FolderID, FolderName: p.FolderName, Path: p.ProjectName + "/" + p.FolderName}
}

// ProjectStats is a project's totals in Cloud's shape.
type ProjectStats struct {
	KBFilesByType  map[string]int64 `json:"kb_files_by_type"`
	SearchesHybrid int64            `json:"searches_hybrid"`
	SearchesKB     int64            `json:"searches_kb"`
	SearchesMemory int64            `json:"searches_memory"`
	Conversations  int64            `json:"conversations"`
	KBFiles        int64            `json:"kb_files"`
	KBChunks       int64            `json:"kb_chunks"`
	Artifacts      int64            `json:"artifacts"`
	ArtifactBytes  int64            `json:"artifact_bytes"`
}

// ProjectStatsOf is a project's totals in Cloud's shape.
func ProjectStatsOf(s store.ProjectStats) ProjectStats { return ProjectStats(s) }

// ProjectDetail is a project with its folders and the datapacks it files, as Cloud's GET
// /projects/:id. members is the owner alone: this Meko doesn't count the agents that wrote to them.
type ProjectDetail struct {
	Project   Project         `json:"project"`
	Folders   []Folder        `json:"folders"`
	Datapacks []FiledDatapack `json:"datapacks"`
	Members   map[string]int  `json:"members"`
}

// ProjectDetailOf is a project's detail in Cloud's shape.
func ProjectDetailOf(p store.Project, folders []store.Folder, filed []store.FiledDatapack) ProjectDetail {
	out := ProjectDetail{Project: ProjectOf(p), Folders: make([]Folder, len(folders)),
		Datapacks: make([]FiledDatapack, len(filed)), Members: map[string]int{"people": 1, "agents": 0}}
	for i, f := range folders {
		out.Folders[i] = FolderOf(f)
	}
	for i, d := range filed {
		out.Datapacks[i] = FiledDatapackOf(d)
	}
	return out
}

// ProjectList is the projects as Cloud's GET /projects: every one here is the owner's, so shared
// is empty.
func ProjectList(ps []store.Project) map[string][]Project {
	owned := make([]Project, len(ps))
	for i, p := range ps {
		owned[i] = ProjectOf(p)
	}
	return map[string][]Project{"owned": owned, "shared": {}}
}

// FiledPath is where a datapack is filed, as datapack_describe adds it: each null when it's Unfiled.
type FiledPath struct {
	Project *string `json:"project"`
	Folder  *string `json:"folder"`
	Path    *string `json:"path"`
}

// FiledAs is a placement as datapack_describe gives it; the zero placement is Unfiled.
func FiledAs(p store.Placement) FiledPath {
	if p.DatapackID == "" {
		return FiledPath{}
	}
	path := PlacementOf(p).Path
	return FiledPath{Project: &p.ProjectName, Folder: &p.FolderName, Path: &path}
}
