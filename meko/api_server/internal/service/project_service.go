package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

const (
	minTreeNameLength = 2
	maxTreeNameLength = 60
	// folderPathSeparator joins a folder path ("features/perf") and a full path
	// ("ADLC/features/perf"). Assembled server-side so every client renders the
	// same string.
	folderPathSeparator = "/"
)

// ErrInvalidProjectName covers both project and folder names — they share one
// rule set, so a caller distinguishes them from the message, not the sentinel.
var ErrInvalidProjectName = errors.New("invalid name")

// ErrUnknownTheme is returned for a theme_key no ProjectTheme declares.
// Deliberately not a fallback to the default: silently substituting a layout
// gives the user a project that isn't the one they picked.
var ErrUnknownTheme = errors.New("unknown project theme")

// ErrFolderNotInProject is returned when a folder exists but belongs to a
// project the caller does not own. Reported as a not-found rather than a
// forbidden so a caller can't probe for folder ids in other users' trees.
var ErrFolderNotInProject = errors.New("folder not found in this project")

type invalidProjectNameError struct{ message string }

func (e invalidProjectNameError) Error() string { return e.message }
func (e invalidProjectNameError) Is(target error) bool {
	return target == ErrInvalidProjectName
}

// validateTreeName applies one rule set to project and folder names. Reuses
// htmlTagPattern from datapack_service.go — these names are rendered as text
// in the UI, so the same injection guard applies.
//
// Unlike a datapack name these are not mirrored to Langfuse, so the floor is 2
// rather than 3: short folder names ("qa", "db") are legitimate.
func validateTreeName(kind, name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return invalidProjectNameError{message: kind + " name is required"}
	}
	length := utf8.RuneCountInString(trimmed)
	if length < minTreeNameLength {
		return invalidProjectNameError{
			message: fmt.Sprintf("%s name must have at least %d characters", kind, minTreeNameLength),
		}
	}
	if length > maxTreeNameLength {
		return invalidProjectNameError{
			message: fmt.Sprintf("%s name must have at most %d characters", kind, maxTreeNameLength),
		}
	}
	if htmlTagPattern.MatchString(trimmed) {
		return invalidProjectNameError{message: kind + " name cannot contain HTML tags"}
	}
	if strings.Contains(trimmed, folderPathSeparator) {
		// A name containing the separator would make a path ambiguous to parse
		// back into its segments.
		return invalidProjectNameError{
			message: fmt.Sprintf("%s name cannot contain %q", kind, folderPathSeparator),
		}
	}
	return nil
}

// ProjectService owns the project tree: projects, their folders, and where
// datapacks sit inside them.
//
// Read methods are written to cost a FIXED number of queries rather than one
// per project or per datapack — see ListWithCounts and DatapackPaths. That is
// the whole reason placement counts are kept in meko_system rather than being
// asked of each datapack's own database.
type ProjectService struct {
	projects   *repository.ProjectRepo
	placements *repository.PlacementRepo
	// datapacks answers "what may this caller see?". Every shared-project read
	// is scoped through it, so a recipient can never learn about a datapack or
	// a folder that was not shared with them.
	datapacks *repository.DatapackRepo
	// rollups reads the DATA database for per-datapack counts. Optional: when
	// it is nil, or fails, the detail page still renders with zeros the same
	// way DatapackHandler.Describe degrades.
	rollups *repository.DatapackRollupRepo
	// stats reads the project-wide statistics panel. Optional. Each metric
	// degrades to zero on its own (see ProjectStatsRepo.Stats); the
	// endpoint fails only when the repo is absent or every metric failed, so
	// a stats outage costs the panel rather than the page.
	stats *repository.ProjectStatsRepo
	// activity is the datapack activity feed, widened to a set of datapacks.
	// An interface rather than *KnowledgeBaseService so this file depends on
	// the one method it uses.
	activity activityLogger
	// datapackDeleter deletes a datapack through the full teardown path (Langfuse
	// project, AMP clusters, universe slot), which deleting a project now
	// cascades into. An interface for the same reason as activity.
	datapackDeleter datapackDeleter
}

// datapackDeleter is the slice of DatapackService that deleting a project
// needs.
type datapackDeleter interface {
	Delete(userID, datapackID string) error
}

// activityLogger is the slice of KnowledgeBaseService the project feed needs.
type activityLogger interface {
	ListActivityLogForDatapacks(datapackIDs []string, limit int, cursor string,
		structural *repository.StructuralActivity) (ActivityLogResponse, error)
}

func NewProjectService(
	projects *repository.ProjectRepo,
	placements *repository.PlacementRepo,
	datapacks *repository.DatapackRepo,
	rollups *repository.DatapackRollupRepo,
	stats *repository.ProjectStatsRepo,
	activity activityLogger,
	datapackDeleter datapackDeleter,
) *ProjectService {
	return &ProjectService{
		projects:   projects,
		placements: placements,
		datapacks:  datapacks,
		rollups:    rollups,
		stats:      stats,
		activity:   activity,

		datapackDeleter: datapackDeleter,
	}
}

// projectScope is what a caller may see inside one project: the project row,
// the placements in it they can see, and those datapacks keyed by id.
//
// Extracted so the detail page and the activity feed cannot drift on who is
// allowed to see what. Both start from the project's placements and then
// check access on those ids, which is why neither can reach a datapack that
// was not shared.
type projectScope struct {
	project *models.Project
	// owned is nil when the caller does not own the project.
	owned    *models.Project
	byID     map[string]models.Datapack
	byFolder map[string]int
	// Last: a slice header carries 16 bytes of non-pointer data, and govet's
	// fieldalignment wants that after the pointer-only fields.
	placements []models.DatapackPlacement
}

func (s *ProjectService) scopeFor(userID, projectID string) (*projectScope, error) {
	owned, err := s.projects.Get(userID, projectID)
	if err != nil && !errors.Is(err, repository.ErrProjectNotFound) {
		return nil, err
	}

	// Placements first, then only those datapacks' access: the caller may see
	// far more datapacks than one project holds.
	all, err := s.placements.InProject(projectID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(all))
	for _, pl := range all {
		ids = append(ids, pl.DatapackID)
	}
	visible, err := s.datapacks.VisibleByIDs(context.Background(), userID, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]models.Datapack, len(visible))
	for _, dp := range visible {
		byID[dp.ID] = dp
	}

	inProject := make([]models.DatapackPlacement, 0, len(all))
	byFolder := map[string]int{}
	for _, pl := range all {
		if _, found := byID[pl.DatapackID]; !found {
			continue
		}
		inProject = append(inProject, pl)
		byFolder[pl.FolderID]++
	}

	project := owned
	if project == nil {
		// Someone else's project. It exists for this caller only if something
		// inside it was shared with them.
		if len(inProject) == 0 {
			return nil, repository.ErrProjectNotFound
		}
		found, fErr := s.projects.ByIDs([]string{projectID})
		if fErr != nil {
			return nil, fErr
		}
		if len(found) == 0 {
			return nil, repository.ErrProjectNotFound
		}
		project = &found[0]
	}

	return &projectScope{
		project:    project,
		owned:      owned,
		placements: inProject,
		byID:       byID,
		byFolder:   byFolder,
	}, nil
}

// ActivityLog returns file uploads, memory promotions, folder creations and
// datapack filings as one time-ordered feed, scoped to what the caller can see.
//
// Still derived from rows that exist rather than from an event log, so a
// deleted file's upload disappears from history and a renamed folder reports
// nothing. A MOVE does appear, as a fresh filing: placement is an upsert and
// ActivityPlacements keys on its updated_at. See
// KnowledgeBaseService.ListActivityLogForDatapacks for the cost.
func (s *ProjectService) ActivityLog(
	userID, callerEmail, projectID string, limit int, cursor string,
) (ActivityLogResponse, error) {
	if s.activity == nil {
		return ActivityLogResponse{}, fmt.Errorf("activity log is unavailable")
	}
	scope, err := s.scopeFor(userID, projectID)
	if err != nil {
		return ActivityLogResponse{}, err
	}

	datapackIDs := make([]string, 0, len(scope.placements))
	for _, pl := range scope.placements {
		datapackIDs = append(datapackIDs, pl.DatapackID)
	}

	structural := s.structuralActivity(scope, limit, cursor)
	resp, err := s.activity.ListActivityLogForDatapacks(datapackIDs, limit, cursor, structural)
	if err != nil {
		return ActivityLogResponse{}, err
	}

	// The repo knows which datapack a row came from but not its name; this
	// layer already holds the names, so it labels the rows.
	for i := range resp.Items {
		if dp, found := scope.byID[resp.Items[i].DatapackID]; found {
			resp.Items[i].DatapackName = dp.Name
			if models.IsViewerGrant(dp.Grant) {
				redactOthersUploaderEmail(&resp.Items[i], callerEmail)
			}
		}
	}
	return resp, nil
}

// structuralActivity fetches the two project-only sides of the feed:
// folders created, and datapacks filed into one.
//
// Both are scoped the same way the tree is. Folders come from the caller's
// VISIBLE placements rather than from the project, so a recipient sees
// only folders holding a datapack shared with them; an owner additionally
// sees their empty folders, which is the one case where the two differ.
// Never returns an error: both sides degrade to empty rather than failing.
func (s *ProjectService) structuralActivity(
	scope *projectScope, limit int, cursor string,
) *repository.StructuralActivity {
	c := repository.DecodeActivityLogCursor(cursor)
	// Same over-fetch the other sides use: enough to fill a page alone, plus
	// one so the merge can tell there is more without a COUNT.
	perSideCap := limit + 1

	// Each side degrades independently, the rule ListActivityLog already
	// applies to its own two sides: a meko_system hiccup costs the folder or
	// placement rows, not the uploads and promotions that would have loaded
	// fine. Returning the error here would take the whole tab down with it.
	folderIDs := s.visibleFolderIDs(scope)
	folders, err := s.projects.ActivityFolders(folderIDs, c.FolderTs, c.FolderID, perSideCap)
	if err != nil {
		slog.Warn("activity log: folder side failed; serving the rest", "error", err)
		folders = nil
	}

	datapackIDs := make([]string, 0, len(scope.placements))
	for _, pl := range scope.placements {
		datapackIDs = append(datapackIDs, pl.DatapackID)
	}
	placements, err := s.projects.ActivityPlacements(
		datapackIDs, c.PlaceTs, c.PlaceID, perSideCap,
	)
	if err != nil {
		slog.Warn("activity log: placement side failed; serving the rest", "error", err)
		placements = nil
	}

	// Placement rows carry a datapack id but not its name, same as every
	// other side; the caller labels them.
	return &repository.StructuralActivity{Folders: folders, Placements: placements}
}

// visibleFolderIDs is every folder this caller may know exists: all of them
// when they own the project, otherwise only the ones their visible
// datapacks sit in.
func (s *ProjectService) visibleFolderIDs(scope *projectScope) []string {
	if scope.owned != nil {
		folders, err := s.projects.Folders(scope.project.ID)
		if err != nil {
			// The feed degrades to its other sides rather than failing: a
			// missing folder list costs one row type, not the page.
			slog.Warn("activity log: folder list failed; omitting folder events",
				"project_id", scope.project.ID, "error", err)
			return nil
		}
		out := make([]string, 0, len(folders))
		for _, f := range folders {
			out = append(out, f.ID)
		}
		return out
	}

	seen := make(map[string]bool, len(scope.placements))
	out := make([]string, 0, len(scope.placements))
	for _, pl := range scope.placements {
		if pl.FolderID == "" || seen[pl.FolderID] {
			continue
		}
		seen[pl.FolderID] = true
		out = append(out, pl.FolderID)
	}
	return out
}

// CreateProjectRequest is the body of POST /projects.
type CreateProjectRequest struct {
	Name        string `json:"project_name"`
	Description string `json:"description"`
	ThemeKey    string `json:"theme_key"`
	// Folders replaces the theme's own folder list. The Blank theme exists for
	// exactly this: it ships no meaningful layout, so the caller names the
	// first folder instead. Omitted, the theme's folders are used.
	Folders []string `json:"folders,omitempty"`
}

// Create stamps a theme's folders into a new project. An unknown theme is an
// error, and an empty theme_key takes the documented default rather than
// failing — the UI always sends one, agents creating an implicit project do
// not.
func (s *ProjectService) Create(userID string, req CreateProjectRequest) (*models.Project, error) {
	if err := validateTreeName("project", req.Name); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(req.ThemeKey)
	if key == "" {
		key = DefaultProjectThemeKey
	}
	theme, found := ProjectThemeByKey(key)
	if !found {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTheme, key)
	}

	folders, err := resolveInitialFolders(req.Folders, theme.Folders)
	if err != nil {
		return nil, err
	}

	return s.projects.Create(repository.CreateProjectParams{
		UserID:       userID,
		Name:         strings.TrimSpace(req.Name),
		Description:  strings.TrimSpace(req.Description),
		ThemeKey:     theme.Key,
		ThemeVersion: theme.Version,
		Folders:      folders,
	})
}

// resolveInitialFolders picks the folder list a new project is stamped with:
// the caller's when they supplied one, the theme's otherwise.
//
// Always returns at least one folder. A project with none is a dead end,
// because a datapack has to be created inside a folder — which is also why the
// Blank theme asks the user to name one rather than inventing a name for them.
//
// Duplicates are rejected here rather than at the unique index, so the caller
// gets a readable message instead of a failed transaction.
func resolveInitialFolders(requested, themeFolders []string) ([]string, error) {
	source := requested
	if len(source) == 0 {
		source = themeFolders
	}

	seen := make(map[string]bool, len(source))
	out := make([]string, 0, len(source))
	for _, name := range source {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if err := validateTreeName("folder", trimmed); err != nil {
			return nil, err
		}
		if seen[trimmed] {
			return nil, invalidProjectNameError{
				message: fmt.Sprintf("folder %q is listed twice", trimmed),
			}
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}

	if len(out) == 0 {
		return nil, invalidProjectNameError{
			message: "a project needs at least one folder",
		}
	}
	return out, nil
}

// ListWithCounts returns the caller's projects with folder and datapack counts
// filled in.
//
// THREE queries, whatever the tree size: the projects, every folder the user
// owns, and the per-folder placement counts. Folder counts are derived from
// the folder slice rather than asked for separately, and no per-project query
// is issued — that is what stops this page degrading as a user accumulates
// projects.
func (s *ProjectService) ListWithCounts(userID string) ([]models.Project, error) {
	projects, err := s.projects.List(userID)
	if err != nil {
		return nil, err
	}
	if len(projects) == 0 {
		return []models.Project{}, nil
	}

	// By project id, not by user. The join form ("folders whose project
	// belongs to me") plans as a full index scan of project_folders batched
	// against projects -- EXPLAIN drives it from the folders side with no
	// Index Cond at all, so one user's page reads every folder in the table.
	// The ids are already in hand, so passing them keeps it an index probe.
	projectIDs := make([]string, 0, len(projects))
	for _, p := range projects {
		projectIDs = append(projectIDs, p.ID)
	}
	folders, err := s.projects.FoldersByProjects(projectIDs)
	if err != nil {
		return nil, err
	}
	counts, err := s.placements.CountsByFolderForOwner(userID)
	if err != nil {
		return nil, err
	}

	foldersByProject := make(map[string]int, len(projects))
	namesByProject := make(map[string][]string, len(projects))
	for _, f := range folders {
		foldersByProject[f.ProjectID]++
		namesByProject[f.ProjectID] = append(namesByProject[f.ProjectID], f.Name)
	}
	datapacksByProject := make(map[string]int, len(projects))
	for _, c := range counts {
		datapacksByProject[c.ProjectID] += c.Count
	}

	for i := range projects {
		id := projects[i].ID
		projects[i].FolderCount = foldersByProject[id]
		projects[i].DatapackCount = datapacksByProject[id]
		names := namesByProject[id]
		sort.Strings(names)
		projects[i].FolderNames = names
	}
	return projects, nil
}

// ProjectDetail is a project, its folders, and every datapack in it the caller
// can see, each datapack carrying the counts the detail page reports.
//
// ONE response rather than one per selection. The project tile, a folder tile,
// a datapack tile and the project-wide learnings rollup are all derivable from
// these rows, so clicking around the tree costs the client nothing.
type ProjectDetail struct {
	Project   models.Project            `json:"project"`
	Folders   []models.ProjectFolder    `json:"folders"`
	Datapacks []ProjectDatapackNode     `json:"datapacks"`
	Members   repository.ProjectMembers `json:"members"`
}

// ProjectDatapackNode is one datapack inside a project: where it sits, and
// what it holds.
//
// Conversations are deliberately absent. They live in Langfuse, reachable only
// one datapack at a time through a per-datapack project mapping, so carrying
// them would cost one cross-database call per datapack, which is the one thing
// the rest of this file is built to avoid.
type ProjectDatapackNode struct {
	UpdatedAt        time.Time `json:"updated_at"`
	DatapackID       string    `json:"datapack_id"`
	Name             string    `json:"datapack_name"`
	FolderID         string    `json:"folder_id"`
	OwnerName        string    `json:"datapack_owner_name,omitempty"`
	CreatedByAgentID string    `json:"created_by_agent_id,omitempty"`
	// MemoryCount is private memories, AwaitingReview the subset with no
	// promotion decision yet, PromotedCount what reached the shared KB.
	MemoryCount    int `json:"memory_count"`
	AwaitingReview int `json:"awaiting_review_count"`
	PromotedCount  int `json:"promoted_count"`
	KnowledgeCount int `json:"knowledge_count"`
}

// Detail returns one project, its folders, and the datapacks inside it.
//
// A fixed number of queries whatever the project holds: the project's
// placements, an access check on those ids, the folders, three grouped
// rollups and one member count. Nothing here runs per datapack.
//
// Two paths share that body. The caller's own project comes back whole. A
// project merely shared with them is pruned to the folders and datapacks
// VisibleByIDs kept, so opening a shared project never reveals work that was
// not shared. A project that is neither resolves to not-found rather than
// forbidden, so project ids cannot be probed.
func (s *ProjectService) Detail(userID, projectID string) (*ProjectDetail, error) {
	scope, err := s.scopeFor(userID, projectID)
	if err != nil {
		return nil, err
	}
	project := scope.project

	folders, err := s.projects.SortedFolders(projectID)
	if err != nil {
		return nil, err
	}
	if scope.owned == nil {
		folders = pruneToVisible(folders, scope.byFolder)
	}
	applyFolderCounts(folders, scope.byFolder, project)

	datapackIDs := make([]string, 0, len(scope.placements))
	for _, pl := range scope.placements {
		datapackIDs = append(datapackIDs, pl.DatapackID)
	}

	// Counts are non-fatal, matching DatapackHandler.Describe: a data-database
	// hiccup should leave the tree usable rather than blanking the page.
	var rollups repository.ProjectRollups
	if s.rollups != nil {
		if rollups, err = s.rollups.Rollups(datapackIDs); err != nil {
			slog.Warn("project detail: rollups unavailable", "project_id", projectID, "error", err)
			rollups = repository.ProjectRollups{}
		}
	}
	people, mErr := s.projects.PeopleWithAccess(project.UserID, datapackIDs)
	if mErr != nil {
		slog.Warn("project detail: member count unavailable", "project_id", projectID, "error", mErr)
	}
	// People come from meko_system (who may reach it), agents from the data
	// database (who has written in it). Two sources because they answer two
	// different questions and only one of them has a usable record.
	members := repository.ProjectMembers{People: people, Agents: rollups.ActiveAgents}

	nodes := make([]ProjectDatapackNode, 0, len(scope.placements))
	for _, pl := range scope.placements {
		dp := scope.byID[pl.DatapackID]
		r := rollups.ByDatapack[pl.DatapackID]
		nodes = append(nodes, ProjectDatapackNode{
			UpdatedAt:        dp.UpdatedAt,
			DatapackID:       pl.DatapackID,
			Name:             dp.Name,
			FolderID:         pl.FolderID,
			OwnerName:        dp.OwnerName,
			CreatedByAgentID: dp.CreatedByAgentID,
			MemoryCount:      r.MemoryCount,
			AwaitingReview:   r.AwaitingReview,
			PromotedCount:    r.PromotedCount,
			KnowledgeCount:   r.KnowledgeDocs,
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	return &ProjectDetail{
		Project:   *project,
		Folders:   folders,
		Datapacks: nodes,
		Members:   members,
	}, nil
}

// applyFolderCounts stamps each folder with its count and rolls the totals onto
// the project. For a shared project the caller passes the pruned list, so the
// totals describe what the recipient can see rather than what exists.
func applyFolderCounts(folders []models.ProjectFolder, byFolder map[string]int, project *models.Project) {
	total := 0
	for i := range folders {
		folders[i].DatapackCount = byFolder[folders[i].ID]
		total += folders[i].DatapackCount
	}
	project.DatapackCount = total
	project.FolderCount = len(folders)
}

// pruneToVisible drops every folder holding nothing the caller can see, so a
// shared project reveals the folders its shared datapacks sit in and no others.
func pruneToVisible(folders []models.ProjectFolder, byFolder map[string]int) []models.ProjectFolder {
	out := make([]models.ProjectFolder, 0, len(folders))
	for _, f := range folders {
		if byFolder[f.ID] > 0 {
			out = append(out, f)
		}
	}
	return out
}

// Rename updates a project's name and description.
func (s *ProjectService) Rename(userID, projectID, name, description string) error {
	if err := validateTreeName("project", name); err != nil {
		return err
	}
	return s.projects.Rename(userID, projectID, strings.TrimSpace(name), strings.TrimSpace(description))
}

// Delete removes a project AND every datapack filed in it.
//
// Datapacks go first, each through the full DatapackService teardown so the
// Langfuse project, the AMP clusters and the universe slot are released
// rather than orphaned. The project row goes last: if a datapack cannot be
// deleted the project stays, so the user can see what is left and retry
// instead of losing the only handle on it.
//
// Owner-only, and every datapack here is the caller's by construction --
// filing is owner-only -- but the ownership filter stays as a second lock,
// because this is the most destructive call in the feature.
func (s *ProjectService) Delete(userID, projectID string) error {
	scope, err := s.scopeFor(userID, projectID)
	if err != nil {
		return err
	}
	// scopeFor resolves a project a RECIPIENT can see too. Deleting is
	// owner-only, and owned is nil for anyone else.
	if scope.owned == nil {
		return repository.ErrProjectNotFound
	}
	for _, pl := range scope.placements {
		dp, found := scope.byID[pl.DatapackID]
		if !found || dp.UserID != userID {
			continue
		}
		if s.datapackDeleter == nil {
			return fmt.Errorf("cannot delete project: datapack deletion is unavailable")
		}
		if dErr := s.datapackDeleter.Delete(userID, pl.DatapackID); dErr != nil {
			return fmt.Errorf("deleting datapack %q in this project: %w", dp.Name, dErr)
		}
		slog.Info("datapack deleted as part of project delete",
			"project_id", projectID, "datapack_id", pl.DatapackID)
	}

	return s.projects.Delete(userID, projectID)
}

// CreateFolder adds a folder to a project the caller owns. Ownership is
// checked before the insert so a caller cannot grow another user's tree.
func (s *ProjectService) CreateFolder(
	userID, projectID, name, description string,
) (*models.ProjectFolder, error) {
	if err := validateTreeName("folder", name); err != nil {
		return nil, err
	}
	if _, err := s.projects.Get(userID, projectID); err != nil {
		return nil, err
	}
	return s.projects.CreateFolder(
		projectID, strings.TrimSpace(name), strings.TrimSpace(description),
	)
}

// RenameFolder renames a folder in a project the caller owns.
func (s *ProjectService) RenameFolder(
	userID, projectID, folderID, name string, description *string,
) error {
	if err := validateTreeName("folder", name); err != nil {
		return err
	}
	if _, err := s.projects.Get(userID, projectID); err != nil {
		return err
	}
	if description != nil {
		trimmed := strings.TrimSpace(*description)
		description = &trimmed
	}
	return s.projects.RenameFolder(
		projectID, folderID, strings.TrimSpace(name), description,
	)
}

// DeleteFolder removes a folder and its children from a project the caller
// owns. Datapacks filed inside become unfiled rather than being deleted.
func (s *ProjectService) DeleteFolder(userID, projectID, folderID string) error {
	if _, err := s.projects.Get(userID, projectID); err != nil {
		return err
	}
	return s.projects.DeleteFolder(projectID, folderID)
}

// ValidateNewDatapackFolder checks a folder_id supplied at datapack-create
// time. An EMPTY folder_id is always fine: a new datapack is never forced
// into a project.
//
// Filing is something a user opts into, and making it a precondition of
// creating a datapack would break every existing caller the moment they
// created their first project — including agents, whose datapack_create
// sends no folder. Unfiled datapacks are a supported state, and the
// Datapacks page offers one-click filing for them.
//
// When a folder IS named it must exist and belong to a project the caller
// owns, checked before the datapack is created so a bad id fails the request
// rather than leaving an unplaceable datapack behind.
func (s *ProjectService) ValidateNewDatapackFolder(userID, folderID string) error {
	folderID = strings.TrimSpace(folderID)
	if folderID == "" {
		return nil
	}
	_, ownerUserID, err := s.projects.FolderOwner(folderID)
	if err != nil {
		if errors.Is(err, repository.ErrFolderNotFound) {
			return ErrFolderNotInProject
		}
		return err
	}
	if ownerUserID != userID {
		return ErrFolderNotInProject
	}
	return nil
}

// Place files a datapack into a folder, or moves it between folders.
//
// A datapack has ONE home, in a project its owner controls, so this is
// owner-only: RequireDatapackAccess upstream lets a recipient reach the route,
// and this check is what stops them relocating someone else's datapack. The
// folder must also belong to a project the same user owns.
func (s *ProjectService) Place(userID, datapackID, folderID string) error {
	folderID = strings.TrimSpace(folderID)
	if folderID == "" {
		return invalidProjectNameError{message: "folder_id is required"}
	}
	if err := s.requireDatapackOwner(userID, datapackID); err != nil {
		return err
	}
	projectID, ownerUserID, err := s.projects.FolderOwner(folderID)
	if err != nil {
		if errors.Is(err, repository.ErrFolderNotFound) {
			return ErrFolderNotInProject
		}
		return err
	}
	if ownerUserID != userID {
		return ErrFolderNotInProject
	}
	return s.placements.Place(datapackID, projectID, folderID)
}

// Unplace makes a datapack unfiled. Owner-only, for the same reason as Place.
func (s *ProjectService) Unplace(userID, datapackID string) error {
	if err := s.requireDatapackOwner(userID, datapackID); err != nil {
		return err
	}
	return s.placements.Unplace(datapackID)
}

// ErrNotDatapackOwner is returned when someone with shared access tries to
// change where a datapack lives. Reading a shared datapack's path is fine;
// moving it is the owner's call.
var ErrNotDatapackOwner = errors.New("only the datapack owner can change where it is filed")

func (s *ProjectService) requireDatapackOwner(userID, datapackID string) error {
	dp, err := s.datapacks.GetByID(userID, datapackID)
	if err != nil {
		return err
	}
	if dp == nil || dp.UserID != userID {
		return ErrNotDatapackOwner
	}
	return nil
}

// ListSharedWithCounts returns projects owned by OTHER users that hold at
// least one datapack the caller can access.
//
// Everything here is scoped to that access set:
//   - only projects reachable from a shared datapack appear,
//   - DatapackCount counts only the datapacks the caller can see, not the
//     project's real total,
//   - RootFolders lists only folders on the path to one of those datapacks.
//
// So sharing one datapack out of a twenty-datapack project reveals one
// datapack and one folder, not the shape of the owner's whole tree.
func (s *ProjectService) ListSharedWithCounts(userID string) ([]models.Project, error) {
	sharedIDs, err := s.datapacks.SharedIDs(context.Background(), userID)
	if err != nil {
		return nil, err
	}
	if len(sharedIDs) == 0 {
		return []models.Project{}, nil
	}

	placements, err := s.placements.ForDatapacks(sharedIDs)
	if err != nil {
		return nil, err
	}
	if len(placements) == 0 {
		return []models.Project{}, nil
	}

	projectIDs := make([]string, 0, len(placements))
	seenProject := make(map[string]bool, len(placements))
	for _, pl := range placements {
		if !seenProject[pl.ProjectID] {
			seenProject[pl.ProjectID] = true
			projectIDs = append(projectIDs, pl.ProjectID)
		}
	}

	projects, err := s.projects.ByIDs(projectIDs)
	if err != nil {
		return nil, err
	}
	folders, err := s.projects.FoldersByProjects(projectIDs)
	if err != nil {
		return nil, err
	}
	folderByID := make(map[string]models.ProjectFolder, len(folders))
	for _, f := range folders {
		folderByID[f.ID] = f
	}

	// Per project: how many accessible datapacks, and which root folders those
	// sit in.
	countByProject := make(map[string]int, len(projects))
	namesByProject := make(map[string]map[string]bool, len(projects))
	for _, pl := range placements {
		countByProject[pl.ProjectID]++
		folder, found := folderByID[pl.FolderID]
		if !found {
			continue
		}
		if namesByProject[pl.ProjectID] == nil {
			namesByProject[pl.ProjectID] = map[string]bool{}
		}
		namesByProject[pl.ProjectID][folder.Name] = true
	}

	out := make([]models.Project, 0, len(projects))
	for _, p := range projects {
		// A project the caller happens to own is not "shared with" them, even
		// if a datapack inside it was shared out and back.
		if p.UserID == userID {
			continue
		}
		p.DatapackCount = countByProject[p.ID]
		names := make([]string, 0, len(namesByProject[p.ID]))
		for name := range namesByProject[p.ID] {
			names = append(names, name)
		}
		sort.Strings(names)
		p.FolderNames = names
		// The visible folder count, not the project's real one.
		p.FolderCount = len(names)
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// DatapackPaths returns where every datapack the caller can see is filed,
// keyed by datapack_id.
//
// Includes shared datapacks, resolved against their OWNER's project, because
// that is the one place they live. Datapacks absent from the map are unfiled,
// which is why the response is a map rather than a list.
//
// datapackIDs, when non-nil, limits the answer to those datapacks, so a page
// showing some rows of a very large account need not fetch every path.
func (s *ProjectService) DatapackPaths(userID string, datapackIDs []string) (map[string]models.DatapackPath, error) {
	ctx := context.Background()
	out := map[string]models.DatapackPath{}
	if datapackIDs != nil && len(datapackIDs) == 0 {
		return out, nil
	}
	datapackIDs = canonicalDatapackIDs(datapackIDs)
	placements, err := s.placements.OwnedByUser(userID, datapackIDs)
	if err != nil {
		return nil, err
	}
	shared, err := s.datapacks.SharedIDs(ctx, userID)
	if err != nil {
		return nil, err
	}
	if datapackIDs != nil {
		shared = intersect(shared, datapackIDs)
	}
	if len(shared) > 0 {
		sharedPlacements, sErr := s.placements.ForDatapacks(shared)
		if sErr != nil {
			return nil, sErr
		}
		placements = append(placements, sharedPlacements...)
	}
	if len(placements) == 0 {
		return out, nil
	}

	projectIDs := make([]string, 0, len(placements))
	seen := make(map[string]bool, len(placements))
	for _, pl := range placements {
		if !seen[pl.ProjectID] {
			seen[pl.ProjectID] = true
			projectIDs = append(projectIDs, pl.ProjectID)
		}
	}

	projects, err := s.projects.ByIDs(projectIDs)
	if err != nil {
		return nil, err
	}
	folders, err := s.projects.FoldersByProjects(projectIDs)
	if err != nil {
		return nil, err
	}

	projectNameByID := make(map[string]string, len(projects))
	for _, p := range projects {
		projectNameByID[p.ID] = p.Name
	}
	folderByID := make(map[string]models.ProjectFolder, len(folders))
	for _, f := range folders {
		folderByID[f.ID] = f
	}

	for _, pl := range placements {
		folder, found := folderByID[pl.FolderID]
		if !found {
			// Folder removed under a concurrent delete; the placement row is on
			// its way out via cascade. Skip rather than emit a blank segment.
			continue
		}
		projectName := projectNameByID[pl.ProjectID]
		out[pl.DatapackID] = models.DatapackPath{
			DatapackID:  pl.DatapackID,
			ProjectID:   pl.ProjectID,
			ProjectName: projectName,
			FolderID:    pl.FolderID,
			FolderName:  folder.Name,
			Path:        projectName + folderPathSeparator + folder.Name,
		}
	}
	return out, nil
}

// Stats returns what a project holds.
//
// Scoped exactly like the tree and the activity feed: the caller's visible
// datapacks, so a recipient's numbers cover only what was shared with them
// rather than the owner's whole project.
func (s *ProjectService) Stats(userID, projectID string) (repository.ProjectStats, error) {
	empty := repository.ProjectStats{KBFilesByType: map[string]int64{}}
	if s.stats == nil {
		return empty, fmt.Errorf("project stats are unavailable")
	}
	scope, err := s.scopeFor(userID, projectID)
	if err != nil {
		return empty, err
	}

	datapackIDs := make([]string, 0, len(scope.placements))
	for _, pl := range scope.placements {
		datapackIDs = append(datapackIDs, pl.DatapackID)
	}
	return s.stats.Stats(datapackIDs, DatapackIndexSchema, DatapackIndexName)
}

// canonicalDatapackIDs rewrites UUID ids to the lowercase form the database
// returns. SharedIDs comes back that way. OwnedByUser compares with ::uuid,
// which ignores case, so without this an uppercase id matches an owned
// datapack and then intersect drops the same id from the shared half.
// A value that is not a UUID is left as given.
func canonicalDatapackIDs(ids []string) []string {
	if ids == nil {
		return nil
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		parsed, err := uuid.Parse(id)
		if err != nil {
			out[i] = id
			continue
		}
		out[i] = parsed.String()
	}
	return out
}

// intersect returns the members of a that are also in b, in a's order.
func intersect(a, b []string) []string {
	in := make(map[string]bool, len(b))
	for _, v := range b {
		in[v] = true
	}
	out := make([]string, 0, len(a))
	for _, v := range a {
		if in[v] {
			out = append(out, v)
		}
	}
	return out
}
