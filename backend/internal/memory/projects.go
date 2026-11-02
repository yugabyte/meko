package memory

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/yugabyte/meko/backend/internal/store"
)

// Projects file this Meko's datapacks in a two-level tree, project then folder, as Meko Cloud's do:
// the same names, rules and layouts, so a project means the same thing on both.

// Project errors. ErrNameTaken's message says which name.
var (
	ErrNoProject = errors.New("project not found")
	ErrNoFolder  = errors.New("folder not found")
	ErrNameTaken = errors.New("name taken")
)

// Name limits, Meko Cloud's.
const (
	minTreeName = 2
	maxTreeName = 60
)

var htmlTag = regexp.MustCompile(`<[^>]*>`)

// Theme is a layout a project starts from: the folders it's made with.
type Theme struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Version     string   `json:"version"`
	Folders     []string `json:"folders"`
}

// Themes are Meko Cloud's layouts. A project's folders are copied from one when it's made, and
// never read from it again.
var Themes = []Theme{
	{Key: "adlc", Label: "Software delivery", Description: "Standards, features and tests for a codebase.",
		Version: "1.0", Folders: []string{"coding_standards", "features", "integration_tests"}},
	{Key: "support", Label: "Customer support", Description: "What support agents need to answer customers.",
		Version: "1.0", Folders: []string{"product_knowledge", "playbooks", "escalations"}},
	{Key: "custom", Label: "Custom", Description: "Start with one folder you name.", Version: "1.0",
		Folders: []string{}},
}

// checkTreeName checks a project's or folder's name, already trimmed.
func checkTreeName(kind, name string) error {
	n := utf8.RuneCountInString(name)
	switch {
	case n < minTreeName || n > maxTreeName:
		return fmt.Errorf("%w: a %s name is %d to %d characters", ErrBadRequest, kind, minTreeName, maxTreeName)
	case htmlTag.MatchString(name) || strings.ContainsAny(name, "<>"):
		return fmt.Errorf("%w: a %s name can't contain HTML", ErrBadRequest, kind)
	case strings.Contains(name, "/"):
		return fmt.Errorf("%w: a %s name can't contain /", ErrBadRequest, kind)
	}
	return nil
}

// trimName trims a name being changed (nil, left as it is) and checks it.
func trimName(kind string, name *string) (*string, error) {
	if name == nil {
		return nil, nil
	}
	n := strings.TrimSpace(*name)
	return &n, checkTreeName(kind, n)
}

func projectError(err error) error {
	switch {
	case errors.Is(err, store.ErrNoProject):
		return ErrNoProject
	case errors.Is(err, store.ErrNoFolder):
		return ErrNoFolder
	case errors.Is(err, store.ErrNoDatapack):
		return ErrNoDatapack
	case errors.Is(err, store.ErrProjectNameUsed), errors.Is(err, store.ErrFolderNameUsed):
		return fmt.Errorf("%w: %v", ErrNameTaken, err) //nolint:errorlint // the store's message, not its error
	}
	return err
}

// Projects lists the projects, newest first.
func (s *Service) Projects(ctx context.Context) ([]store.Project, error) {
	return s.st.Projects(ctx)
}

// Project is a project, or ErrNoProject.
func (s *Service) Project(ctx context.Context, id string) (store.Project, error) {
	p, err := s.st.Project(ctx, id)
	return p, projectError(err)
}

// NewProject is a project to make. Folders, when given, replace the theme's; a project starts with
// one folder at least.
type NewProject struct {
	Name        string
	Description string
	ThemeKey    string
	Folders     []string
}

// CreateProject makes a project with its first folders, or returns ErrBadRequest or ErrNameTaken.
func (s *Service) CreateProject(ctx context.Context, p NewProject) (store.Project, error) {
	p.Name = strings.TrimSpace(p.Name)
	if err := checkTreeName("project", p.Name); err != nil {
		return store.Project{}, err
	}
	if p.ThemeKey == "" {
		p.ThemeKey = "custom"
	}
	folders := p.Folders
	found := false
	for _, t := range Themes {
		if t.Key == p.ThemeKey {
			found = true
			if folders == nil {
				folders = t.Folders
			}
		}
	}
	if !found {
		return store.Project{}, fmt.Errorf("%w: unknown layout %q", ErrBadRequest, p.ThemeKey)
	}
	if len(folders) == 0 {
		return store.Project{}, fmt.Errorf("%w: a project starts with one folder at least", ErrBadRequest)
	}
	news := make([]store.NewFolder, 0, len(folders))
	seen := map[string]bool{}
	for _, f := range folders {
		f = strings.TrimSpace(f)
		if err := checkTreeName("folder", f); err != nil {
			return store.Project{}, err
		}
		if seen[f] {
			return store.Project{}, fmt.Errorf("%w: folder %q is named twice", ErrBadRequest, f)
		}
		seen[f] = true
		news = append(news, store.NewFolder{Name: f})
	}
	out, err := s.st.CreateProject(ctx, p.Name, strings.TrimSpace(p.Description), p.ThemeKey, news)
	return out, projectError(err)
}

// UpdateProject changes what's given of a project's name and description.
func (s *Service) UpdateProject(ctx context.Context, id string, name, description *string) error {
	if name == nil && description == nil {
		return fmt.Errorf("%w: nothing to update", ErrBadRequest)
	}
	name, err := trimName("project", name)
	if err != nil {
		return err
	}
	return projectError(s.st.UpdateProject(ctx, id, name, description))
}

// DeleteProject deletes a project and its folders. Its datapacks are unfiled, never deleted.
func (s *Service) DeleteProject(ctx context.Context, id string) error {
	return projectError(s.st.DeleteProject(ctx, id))
}

// ProjectTree is a project's folders and the datapacks it files.
func (s *Service) ProjectTree(ctx context.Context, id string) ([]store.Folder, []store.FiledDatapack, error) {
	if _, err := s.Project(ctx, id); err != nil {
		return nil, nil, err
	}
	folders, err := s.st.Folders(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	filed, err := s.st.FiledDatapacks(ctx, id)
	return folders, filed, err
}

// CreateFolder adds a folder to a project.
func (s *Service) CreateFolder(ctx context.Context, project, name, description string) (store.Folder, error) {
	name = strings.TrimSpace(name)
	if err := checkTreeName("folder", name); err != nil {
		return store.Folder{}, err
	}
	f, err := s.st.CreateFolder(ctx, project, store.NewFolder{Name: name, Description: strings.TrimSpace(description)})
	return f, projectError(err)
}

// UpdateFolder changes what's given of a project's folder's name and description.
func (s *Service) UpdateFolder(ctx context.Context, project, id string, name, description *string) error {
	if name == nil && description == nil {
		return fmt.Errorf("%w: nothing to update", ErrBadRequest)
	}
	name, err := trimName("folder", name)
	if err != nil {
		return err
	}
	return projectError(s.st.UpdateFolder(ctx, project, id, name, description))
}

// DeleteFolder deletes a project's folder. Its datapacks are unfiled, never deleted.
func (s *Service) DeleteFolder(ctx context.Context, project, id string) error {
	return projectError(s.st.DeleteFolder(ctx, project, id))
}

// PlaceDatapack files one of this Meko's datapacks in a folder, moving it from where it was.
func (s *Service) PlaceDatapack(ctx context.Context, datapack, folder string) error {
	if strings.TrimSpace(folder) == "" {
		return fmt.Errorf("%w: folder_id is required", ErrBadRequest)
	}
	if err := s.needDatapack(ctx, datapack); err != nil {
		return err
	}
	return projectError(s.st.PlaceDatapack(ctx, datapack, folder))
}

// CreateDatapackIn makes a datapack filed in a folder ("" for none), in one transaction: a wrong
// folder makes nothing.
func (s *Service) CreateDatapackIn(ctx context.Context, name, description, folder string) (store.Datapack, error) {
	name = strings.TrimSpace(name)
	if err := checkName(name); err != nil {
		return store.Datapack{}, err
	}
	d, err := s.st.CreateDatapackIn(ctx, name, description, strings.TrimSpace(folder))
	if errors.Is(err, store.ErrDuplicateName) {
		return d, ErrDuplicateName
	}
	return d, projectError(err)
}

// UnplaceDatapack unfiles one of this Meko's datapacks.
func (s *Service) UnplaceDatapack(ctx context.Context, datapack string) error {
	if err := s.needDatapack(ctx, datapack); err != nil {
		return err
	}
	return s.st.UnplaceDatapack(ctx, datapack)
}

// Placements is where each datapack is filed (every filed one, for nil).
func (s *Service) Placements(ctx context.Context, datapacks []string) (map[string]store.Placement, error) {
	return s.st.Placements(ctx, datapacks)
}

// ProjectActivity is a page of a project's events, newest first, and the next page's cursor ("" for
// none), as a datapack's Activity pages.
func (s *Service) ProjectActivity(ctx context.Context, project, cursor string, limit int) (
	[]store.Activity, string, error) {
	if _, err := s.Project(ctx, project); err != nil {
		return nil, "", err
	}
	return activityPage(cursor, limit, func(after *store.ActivityCursor) ([]store.Activity, *store.ActivityCursor,
		error) {
		return s.st.ProjectActivity(ctx, project, after, limit)
	})
}

// ProjectStats is what a project's datapacks hold in all.
func (s *Service) ProjectStats(ctx context.Context, id string) (store.ProjectStats, error) {
	if _, err := s.Project(ctx, id); err != nil {
		return store.ProjectStats{}, err
	}
	return s.st.ProjectStats(ctx, id)
}
