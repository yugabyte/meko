package service

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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

// ErrInvalidCollectionName covers both collection and folder names — they share one
// rule set, so a caller distinguishes them from the message, not the sentinel.
var ErrInvalidCollectionName = errors.New("invalid name")

// ErrUnknownTheme is returned for a theme_key no CollectionTheme declares.
// Deliberately not a fallback to the default: silently substituting a layout
// gives the user a collection that isn't the one they picked.
var ErrUnknownTheme = errors.New("unknown collection theme")

// ErrFolderNotInCollection is returned when a folder exists but belongs to a
// collection the caller does not own. Reported as a not-found rather than a
// forbidden so a caller can't probe for folder ids in other users' trees.
var ErrFolderNotInCollection = errors.New("folder not found in this collection")

type invalidCollectionNameError struct{ message string }

func (e invalidCollectionNameError) Error() string { return e.message }
func (e invalidCollectionNameError) Is(target error) bool {
	return target == ErrInvalidCollectionName
}

// validateTreeName applies one rule set to collection and folder names. Reuses
// htmlTagPattern from datapack_service.go — these names are rendered as text
// in the UI, so the same injection guard applies.
//
// Unlike a datapack name these are not mirrored to Langfuse, so the floor is 2
// rather than 3: short folder names ("qa", "db") are legitimate.
func validateTreeName(kind, name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return invalidCollectionNameError{message: kind + " name is required"}
	}
	length := utf8.RuneCountInString(trimmed)
	if length < minTreeNameLength {
		return invalidCollectionNameError{
			message: fmt.Sprintf("%s name must have at least %d characters", kind, minTreeNameLength),
		}
	}
	if length > maxTreeNameLength {
		return invalidCollectionNameError{
			message: fmt.Sprintf("%s name must have at most %d characters", kind, maxTreeNameLength),
		}
	}
	if htmlTagPattern.MatchString(trimmed) {
		return invalidCollectionNameError{message: kind + " name cannot contain HTML tags"}
	}
	if strings.Contains(trimmed, folderPathSeparator) {
		// A name containing the separator would make a path ambiguous to parse
		// back into its segments.
		return invalidCollectionNameError{
			message: fmt.Sprintf("%s name cannot contain %q", kind, folderPathSeparator),
		}
	}
	return nil
}

// CollectionService owns the collection tree: collections, their folders, and where
// datapacks sit inside them.
//
// Read methods are written to cost a FIXED number of queries rather than one
// per collection or per datapack — see ListWithCounts and DatapackPaths. That is
// the whole reason placement counts are kept in meko_system rather than being
// asked of each datapack's own database.
type CollectionService struct {
	collections *repository.CollectionRepo
	placements  *repository.PlacementRepo
	// datapacks answers "what may this caller see?". Every shared-collection read
	// is scoped through it, so a recipient can never learn about a datapack or
	// a folder that was not shared with them.
	datapacks *repository.DatapackRepo
	// rollups reads the DATA database for per-datapack counts. Optional: when
	// it is nil, or fails, the detail page still renders with zeros the same
	// way DatapackHandler.Describe degrades.
	rollups *repository.DatapackRollupRepo
	// stats reads the collection-wide statistics panel. Optional. Each metric
	// degrades to zero on its own (see CollectionStatsRepo.Stats); the
	// endpoint fails only when the repo is absent or every metric failed, so
	// a stats outage costs the panel rather than the page.
	stats *repository.CollectionStatsRepo
	// activity is the datapack activity feed, widened to a set of datapacks.
	// An interface rather than *KnowledgeBaseService so this file depends on
	// the one method it uses.
	activity activityLogger
	// datapackDeleter deletes a datapack through the full teardown path (Langfuse
	// collection, AMP clusters, universe slot), which deleting a collection now
	// cascades into. An interface for the same reason as activity.
	datapackDeleter datapackDeleter
}

// datapackDeleter is the slice of DatapackService that deleting a collection
// needs.
type datapackDeleter interface {
	Delete(userID, datapackID string) error
}

// activityLogger is the slice of KnowledgeBaseService the collection feed needs.
type activityLogger interface {
	ListActivityLogForDatapacks(datapackIDs []string, limit int, cursor string,
		structural *repository.StructuralActivity) (ActivityLogResponse, error)
}

func NewCollectionService(
	collections *repository.CollectionRepo,
	placements *repository.PlacementRepo,
	datapacks *repository.DatapackRepo,
	rollups *repository.DatapackRollupRepo,
	stats *repository.CollectionStatsRepo,
	activity activityLogger,
	datapackDeleter datapackDeleter,
) *CollectionService {
	return &CollectionService{
		collections: collections,
		placements:  placements,
		datapacks:   datapacks,
		rollups:     rollups,
		stats:       stats,
		activity:    activity,

		datapackDeleter: datapackDeleter,
	}
}

// collectionScope is what a caller may see inside one collection: the collection row,
// the datapacks of theirs that sit in it, and every datapack they can see
// keyed by id.
//
// Extracted so the detail page and the activity feed cannot drift on who is
// allowed to see what. Both start from the grant-checked datapack list and
// narrow by placement, which is why neither can reach a datapack that was
// not shared.
type collectionScope struct {
	collection *models.Collection
	// owned is nil when the caller does not own the collection.
	owned    *models.Collection
	byID     map[string]models.Datapack
	byFolder map[string]int
	// Last: a slice header carries 16 bytes of non-pointer data, and govet's
	// fieldalignment wants that after the pointer-only fields.
	placements []models.DatapackPlacement
}

func (s *CollectionService) scopeFor(userID, collectionID string) (*collectionScope, error) {
	owned, err := s.collections.Get(userID, collectionID)
	if err != nil && !errors.Is(err, repository.ErrCollectionNotFound) {
		return nil, err
	}

	visible, err := s.datapacks.List(userID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]models.Datapack, len(visible))
	visibleIDs := make([]string, 0, len(visible))
	for _, dp := range visible {
		byID[dp.ID] = dp
		visibleIDs = append(visibleIDs, dp.ID)
	}

	all, err := s.placements.ForDatapacks(visibleIDs)
	if err != nil {
		return nil, err
	}
	inCollection := make([]models.DatapackPlacement, 0, len(all))
	byFolder := map[string]int{}
	for _, pl := range all {
		if pl.CollectionID != collectionID {
			continue
		}
		inCollection = append(inCollection, pl)
		byFolder[pl.FolderID]++
	}

	collection := owned
	if collection == nil {
		// Someone else's collection. It exists for this caller only if something
		// inside it was shared with them.
		if len(inCollection) == 0 {
			return nil, repository.ErrCollectionNotFound
		}
		found, fErr := s.collections.ByIDs([]string{collectionID})
		if fErr != nil {
			return nil, fErr
		}
		if len(found) == 0 {
			return nil, repository.ErrCollectionNotFound
		}
		collection = &found[0]
	}

	return &collectionScope{
		collection: collection,
		owned:      owned,
		placements: inCollection,
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
func (s *CollectionService) ActivityLog(
	userID, collectionID string, limit int, cursor string,
) (ActivityLogResponse, error) {
	if s.activity == nil {
		return ActivityLogResponse{}, fmt.Errorf("activity log is unavailable")
	}
	scope, err := s.scopeFor(userID, collectionID)
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
		}
	}
	return resp, nil
}

// structuralActivity fetches the two collection-only sides of the feed:
// folders created, and datapacks filed into one.
//
// Both are scoped the same way the tree is. Folders come from the caller's
// VISIBLE placements rather than from the collection, so a recipient sees
// only folders holding a datapack shared with them; an owner additionally
// sees their empty folders, which is the one case where the two differ.
// Never returns an error: both sides degrade to empty rather than failing.
func (s *CollectionService) structuralActivity(
	scope *collectionScope, limit int, cursor string,
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
	folders, err := s.collections.ActivityFolders(folderIDs, c.FolderTs, c.FolderID, perSideCap)
	if err != nil {
		slog.Warn("activity log: folder side failed; serving the rest", "error", err)
		folders = nil
	}

	datapackIDs := make([]string, 0, len(scope.placements))
	for _, pl := range scope.placements {
		datapackIDs = append(datapackIDs, pl.DatapackID)
	}
	placements, err := s.collections.ActivityPlacements(
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
// when they own the collection, otherwise only the ones their visible
// datapacks sit in.
func (s *CollectionService) visibleFolderIDs(scope *collectionScope) []string {
	if scope.owned != nil {
		folders, err := s.collections.Folders(scope.collection.ID)
		if err != nil {
			// The feed degrades to its other sides rather than failing: a
			// missing folder list costs one row type, not the page.
			slog.Warn("activity log: folder list failed; omitting folder events",
				"collection_id", scope.collection.ID, "error", err)
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

// visibleDatapackIDs returns the ids of every datapack the caller can see,
// split by whether they own it. The shared half is the access boundary for
// every "Shared collections" read below.
func (s *CollectionService) visibleDatapackIDs(userID string) (owned, shared []string, err error) {
	list, err := s.datapacks.List(userID)
	if err != nil {
		return nil, nil, err
	}
	for _, dp := range list {
		if dp.UserID == userID {
			owned = append(owned, dp.ID)
			continue
		}
		shared = append(shared, dp.ID)
	}
	return owned, shared, nil
}

// CreateCollectionRequest is the body of POST /collections.
type CreateCollectionRequest struct {
	Name        string `json:"collection_name"`
	Description string `json:"description"`
	ThemeKey    string `json:"theme_key"`
	// Folders replaces the theme's own folder list. The Blank theme exists for
	// exactly this: it ships no meaningful layout, so the caller names the
	// first folder instead. Omitted, the theme's folders are used.
	Folders []string `json:"folders,omitempty"`
}

// Create stamps a theme's folders into a new collection. An unknown theme is an
// error, and an empty theme_key takes the documented default rather than
// failing — the UI always sends one, agents creating an implicit collection do
// not.
func (s *CollectionService) Create(userID string, req CreateCollectionRequest) (*models.Collection, error) {
	if err := validateTreeName("collection", req.Name); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(req.ThemeKey)
	if key == "" {
		key = DefaultCollectionThemeKey
	}
	theme, found := CollectionThemeByKey(key)
	if !found {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTheme, key)
	}

	folders, err := resolveInitialFolders(req.Folders, theme.Folders)
	if err != nil {
		return nil, err
	}

	return s.collections.Create(repository.CreateCollectionParams{
		UserID:       userID,
		Name:         strings.TrimSpace(req.Name),
		Description:  strings.TrimSpace(req.Description),
		ThemeKey:     theme.Key,
		ThemeVersion: theme.Version,
		Folders:      folders,
	})
}

// resolveInitialFolders picks the folder list a new collection is stamped with:
// the caller's when they supplied one, the theme's otherwise.
//
// Always returns at least one folder. A collection with none is a dead end,
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
			return nil, invalidCollectionNameError{
				message: fmt.Sprintf("folder %q is listed twice", trimmed),
			}
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}

	if len(out) == 0 {
		return nil, invalidCollectionNameError{
			message: "a collection needs at least one folder",
		}
	}
	return out, nil
}

// ListWithCounts returns the caller's collections with folder and datapack counts
// filled in.
//
// THREE queries, whatever the tree size: the collections, every folder the user
// owns, and the per-folder placement counts. Folder counts are derived from
// the folder slice rather than asked for separately, and no per-collection query
// is issued — that is what stops this page degrading as a user accumulates
// collections.
func (s *CollectionService) ListWithCounts(userID string) ([]models.Collection, error) {
	collections, err := s.collections.List(userID)
	if err != nil {
		return nil, err
	}
	if len(collections) == 0 {
		return []models.Collection{}, nil
	}

	// By collection id, not by user. The join form ("folders whose collection
	// belongs to me") plans as a full index scan of collection_folders batched
	// against collections -- EXPLAIN drives it from the folders side with no
	// Index Cond at all, so one user's page reads every folder in the table.
	// The ids are already in hand, so passing them keeps it an index probe.
	collectionIDs := make([]string, 0, len(collections))
	for _, p := range collections {
		collectionIDs = append(collectionIDs, p.ID)
	}
	folders, err := s.collections.FoldersByCollections(collectionIDs)
	if err != nil {
		return nil, err
	}
	counts, err := s.placements.CountsByFolderForOwner(userID)
	if err != nil {
		return nil, err
	}

	foldersByCollection := make(map[string]int, len(collections))
	namesByCollection := make(map[string][]string, len(collections))
	for _, f := range folders {
		foldersByCollection[f.CollectionID]++
		namesByCollection[f.CollectionID] = append(namesByCollection[f.CollectionID], f.Name)
	}
	datapacksByCollection := make(map[string]int, len(collections))
	for _, c := range counts {
		datapacksByCollection[c.CollectionID] += c.Count
	}

	for i := range collections {
		id := collections[i].ID
		collections[i].FolderCount = foldersByCollection[id]
		collections[i].DatapackCount = datapacksByCollection[id]
		names := namesByCollection[id]
		sort.Strings(names)
		collections[i].FolderNames = names
	}
	return collections, nil
}

// CollectionDetail is a collection, its folders, and every datapack in it the caller
// can see, each datapack carrying the counts the detail page reports.
//
// ONE response rather than one per selection. The collection tile, a folder tile,
// a datapack tile and the collection-wide learnings rollup are all derivable from
// these rows, so clicking around the tree costs the client nothing.
type CollectionDetail struct {
	Collection models.Collection            `json:"collection"`
	Folders    []models.CollectionFolder    `json:"folders"`
	Datapacks  []CollectionDatapackNode     `json:"datapacks"`
	Members    repository.CollectionMembers `json:"members"`
}

// CollectionDatapackNode is one datapack inside a collection: where it sits, and
// what it holds.
//
// Conversations are deliberately absent. They live in Langfuse, reachable only
// one datapack at a time through a per-datapack collection mapping, so carrying
// them would cost one cross-database call per datapack, which is the one thing
// the rest of this file is built to avoid.
type CollectionDatapackNode struct {
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

// Detail returns one collection, its folders, and the datapacks inside it.
//
// A FIXED number of queries whatever the collection holds: the collection row, the
// caller's datapack list, their placements, the folders, three grouped
// rollups and one member count. Nothing here runs per datapack.
//
// Two paths share that body. The caller's OWN collection comes back whole. A
// collection merely SHARED with them is pruned to the folders and datapacks they
// can reach, because the datapack list it starts from is already grant
// checked, so opening a shared collection never reveals work that was not
// shared. A collection that is neither resolves to not-found rather than
// forbidden, so collection ids cannot be probed.
func (s *CollectionService) Detail(userID, collectionID string) (*CollectionDetail, error) {
	scope, err := s.scopeFor(userID, collectionID)
	if err != nil {
		return nil, err
	}
	collection := scope.collection

	folders, err := s.collections.SortedFolders(collectionID)
	if err != nil {
		return nil, err
	}
	if scope.owned == nil {
		folders = pruneToVisible(folders, scope.byFolder)
	}
	applyFolderCounts(folders, scope.byFolder, collection)

	datapackIDs := make([]string, 0, len(scope.placements))
	for _, pl := range scope.placements {
		datapackIDs = append(datapackIDs, pl.DatapackID)
	}

	// Counts are non-fatal, matching DatapackHandler.Describe: a data-database
	// hiccup should leave the tree usable rather than blanking the page.
	var rollups repository.CollectionRollups
	if s.rollups != nil {
		if rollups, err = s.rollups.Rollups(datapackIDs); err != nil {
			slog.Warn("collection detail: rollups unavailable", "collection_id", collectionID, "error", err)
			rollups = repository.CollectionRollups{}
		}
	}
	people, mErr := s.collections.PeopleWithAccess(collection.UserID, datapackIDs)
	if mErr != nil {
		slog.Warn("collection detail: member count unavailable", "collection_id", collectionID, "error", mErr)
	}
	// People come from meko_system (who may reach it), agents from the data
	// database (who has written in it). Two sources because they answer two
	// different questions and only one of them has a usable record.
	members := repository.CollectionMembers{People: people, Agents: rollups.ActiveAgents}

	nodes := make([]CollectionDatapackNode, 0, len(scope.placements))
	for _, pl := range scope.placements {
		dp := scope.byID[pl.DatapackID]
		r := rollups.ByDatapack[pl.DatapackID]
		nodes = append(nodes, CollectionDatapackNode{
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

	return &CollectionDetail{
		Collection: *collection,
		Folders:    folders,
		Datapacks:  nodes,
		Members:    members,
	}, nil
}

// applyFolderCounts stamps each folder with its count and rolls the totals onto
// the collection. For a shared collection the caller passes the pruned list, so the
// totals describe what the recipient can see rather than what exists.
func applyFolderCounts(folders []models.CollectionFolder, byFolder map[string]int, collection *models.Collection) {
	total := 0
	for i := range folders {
		folders[i].DatapackCount = byFolder[folders[i].ID]
		total += folders[i].DatapackCount
	}
	collection.DatapackCount = total
	collection.FolderCount = len(folders)
}

// pruneToVisible drops every folder holding nothing the caller can see, so a
// shared collection reveals the folders its shared datapacks sit in and no others.
func pruneToVisible(folders []models.CollectionFolder, byFolder map[string]int) []models.CollectionFolder {
	out := make([]models.CollectionFolder, 0, len(folders))
	for _, f := range folders {
		if byFolder[f.ID] > 0 {
			out = append(out, f)
		}
	}
	return out
}

// Rename updates a collection's name and description.
func (s *CollectionService) Rename(userID, collectionID, name, description string) error {
	if err := validateTreeName("collection", name); err != nil {
		return err
	}
	return s.collections.Rename(userID, collectionID, strings.TrimSpace(name), strings.TrimSpace(description))
}

// Delete removes a collection AND every datapack filed in it.
//
// Datapacks go first, each through the full DatapackService teardown so the
// Langfuse project, the AMP clusters and the universe slot are released
// rather than orphaned. The collection row goes last: if a datapack cannot be
// deleted the collection stays, so the user can see what is left and retry
// instead of losing the only handle on it.
//
// Owner-only, and every datapack here is the caller's by construction --
// filing is owner-only -- but the ownership filter stays as a second lock,
// because this is the most destructive call in the feature.
func (s *CollectionService) Delete(userID, collectionID string) error {
	scope, err := s.scopeFor(userID, collectionID)
	if err != nil {
		return err
	}
	// scopeFor resolves a collection a RECIPIENT can see too. Deleting is
	// owner-only, and owned is nil for anyone else.
	if scope.owned == nil {
		return repository.ErrCollectionNotFound
	}
	for _, pl := range scope.placements {
		dp, found := scope.byID[pl.DatapackID]
		if !found || dp.UserID != userID {
			continue
		}
		if s.datapackDeleter == nil {
			return fmt.Errorf("cannot delete collection: datapack deletion is unavailable")
		}
		if dErr := s.datapackDeleter.Delete(userID, pl.DatapackID); dErr != nil {
			return fmt.Errorf("deleting datapack %q in this collection: %w", dp.Name, dErr)
		}
		slog.Info("datapack deleted as part of collection delete",
			"collection_id", collectionID, "datapack_id", pl.DatapackID)
	}

	return s.collections.Delete(userID, collectionID)
}

// CreateFolder adds a folder to a collection the caller owns. Ownership is
// checked before the insert so a caller cannot grow another user's tree.
func (s *CollectionService) CreateFolder(
	userID, collectionID, name, description string,
) (*models.CollectionFolder, error) {
	if err := validateTreeName("folder", name); err != nil {
		return nil, err
	}
	if _, err := s.collections.Get(userID, collectionID); err != nil {
		return nil, err
	}
	return s.collections.CreateFolder(
		collectionID, strings.TrimSpace(name), strings.TrimSpace(description),
	)
}

// RenameFolder renames a folder in a collection the caller owns.
func (s *CollectionService) RenameFolder(
	userID, collectionID, folderID, name string, description *string,
) error {
	if err := validateTreeName("folder", name); err != nil {
		return err
	}
	if _, err := s.collections.Get(userID, collectionID); err != nil {
		return err
	}
	if description != nil {
		trimmed := strings.TrimSpace(*description)
		description = &trimmed
	}
	return s.collections.RenameFolder(
		collectionID, folderID, strings.TrimSpace(name), description,
	)
}

// DeleteFolder removes a folder and its children from a collection the caller
// owns. Datapacks filed inside become unfiled rather than being deleted.
func (s *CollectionService) DeleteFolder(userID, collectionID, folderID string) error {
	if _, err := s.collections.Get(userID, collectionID); err != nil {
		return err
	}
	return s.collections.DeleteFolder(collectionID, folderID)
}

// ValidateNewDatapackFolder checks a folder_id supplied at datapack-create
// time. An EMPTY folder_id is always fine: a new datapack is never forced
// into a collection.
//
// Filing is something a user opts into, and making it a precondition of
// creating a datapack would break every existing caller the moment they
// created their first collection — including agents, whose datapack_create
// sends no folder. Unfiled datapacks are a supported state, and the
// Datapacks page offers one-click filing for them.
//
// When a folder IS named it must exist and belong to a collection the caller
// owns, checked before the datapack is created so a bad id fails the request
// rather than leaving an unplaceable datapack behind.
func (s *CollectionService) ValidateNewDatapackFolder(userID, folderID string) error {
	folderID = strings.TrimSpace(folderID)
	if folderID == "" {
		return nil
	}
	_, ownerUserID, err := s.collections.FolderOwner(folderID)
	if err != nil {
		if errors.Is(err, repository.ErrFolderNotFound) {
			return ErrFolderNotInCollection
		}
		return err
	}
	if ownerUserID != userID {
		return ErrFolderNotInCollection
	}
	return nil
}

// Place files a datapack into a folder, or moves it between folders.
//
// A datapack has ONE home, in a collection its owner controls, so this is
// owner-only: RequireDatapackAccess upstream lets a recipient reach the route,
// and this check is what stops them relocating someone else's datapack. The
// folder must also belong to a collection the same user owns.
func (s *CollectionService) Place(userID, datapackID, folderID string) error {
	folderID = strings.TrimSpace(folderID)
	if folderID == "" {
		return invalidCollectionNameError{message: "folder_id is required"}
	}
	if err := s.requireDatapackOwner(userID, datapackID); err != nil {
		return err
	}
	collectionID, ownerUserID, err := s.collections.FolderOwner(folderID)
	if err != nil {
		if errors.Is(err, repository.ErrFolderNotFound) {
			return ErrFolderNotInCollection
		}
		return err
	}
	if ownerUserID != userID {
		return ErrFolderNotInCollection
	}
	return s.placements.Place(datapackID, collectionID, folderID)
}

// Unplace makes a datapack unfiled. Owner-only, for the same reason as Place.
func (s *CollectionService) Unplace(userID, datapackID string) error {
	if err := s.requireDatapackOwner(userID, datapackID); err != nil {
		return err
	}
	return s.placements.Unplace(datapackID)
}

// ErrNotDatapackOwner is returned when someone with shared access tries to
// change where a datapack lives. Reading a shared datapack's path is fine;
// moving it is the owner's call.
var ErrNotDatapackOwner = errors.New("only the datapack owner can change where it is filed")

func (s *CollectionService) requireDatapackOwner(userID, datapackID string) error {
	dp, err := s.datapacks.GetByID(userID, datapackID)
	if err != nil {
		return err
	}
	if dp == nil || dp.UserID != userID {
		return ErrNotDatapackOwner
	}
	return nil
}

// ListSharedWithCounts returns collections owned by OTHER users that hold at
// least one datapack the caller can access.
//
// Everything here is scoped to that access set:
//   - only collections reachable from a shared datapack appear,
//   - DatapackCount counts only the datapacks the caller can see, not the
//     collection's real total,
//   - RootFolders lists only folders on the path to one of those datapacks.
//
// So sharing one datapack out of a twenty-datapack collection reveals one
// datapack and one folder, not the shape of the owner's whole tree.
func (s *CollectionService) ListSharedWithCounts(userID string) ([]models.Collection, error) {
	_, sharedIDs, err := s.visibleDatapackIDs(userID)
	if err != nil {
		return nil, err
	}
	if len(sharedIDs) == 0 {
		return []models.Collection{}, nil
	}

	placements, err := s.placements.ForDatapacks(sharedIDs)
	if err != nil {
		return nil, err
	}
	if len(placements) == 0 {
		return []models.Collection{}, nil
	}

	collectionIDs := make([]string, 0, len(placements))
	seenCollection := make(map[string]bool, len(placements))
	for _, pl := range placements {
		if !seenCollection[pl.CollectionID] {
			seenCollection[pl.CollectionID] = true
			collectionIDs = append(collectionIDs, pl.CollectionID)
		}
	}

	collections, err := s.collections.ByIDs(collectionIDs)
	if err != nil {
		return nil, err
	}
	folders, err := s.collections.FoldersByCollections(collectionIDs)
	if err != nil {
		return nil, err
	}
	folderByID := make(map[string]models.CollectionFolder, len(folders))
	for _, f := range folders {
		folderByID[f.ID] = f
	}

	// Per collection: how many accessible datapacks, and which root folders those
	// sit in.
	countByCollection := make(map[string]int, len(collections))
	namesByCollection := make(map[string]map[string]bool, len(collections))
	for _, pl := range placements {
		countByCollection[pl.CollectionID]++
		folder, found := folderByID[pl.FolderID]
		if !found {
			continue
		}
		if namesByCollection[pl.CollectionID] == nil {
			namesByCollection[pl.CollectionID] = map[string]bool{}
		}
		namesByCollection[pl.CollectionID][folder.Name] = true
	}

	out := make([]models.Collection, 0, len(collections))
	for _, p := range collections {
		// A collection the caller happens to own is not "shared with" them, even
		// if a datapack inside it was shared out and back.
		if p.UserID == userID {
			continue
		}
		p.DatapackCount = countByCollection[p.ID]
		names := make([]string, 0, len(namesByCollection[p.ID]))
		for name := range namesByCollection[p.ID] {
			names = append(names, name)
		}
		sort.Strings(names)
		p.FolderNames = names
		// The visible folder count, not the collection's real one.
		p.FolderCount = len(names)
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// DatapackPaths returns where every datapack the caller can see is filed,
// keyed by datapack_id.
//
// Includes shared datapacks, resolved against their OWNER's collection, because
// that is the one place they live. Datapacks absent from the map are unfiled,
// which is why the response is a map rather than a list.
func (s *CollectionService) DatapackPaths(userID string) (map[string]models.DatapackPath, error) {
	owned, shared, err := s.visibleDatapackIDs(userID)
	if err != nil {
		return nil, err
	}
	// A fresh slice, not append(owned, shared...): appending to `owned` can
	// write into its backing array, which visibleDatapackIDs' caller still
	// holds.
	visible := make([]string, 0, len(owned)+len(shared))
	visible = append(visible, owned...)
	visible = append(visible, shared...)
	out := make(map[string]models.DatapackPath, len(visible))
	if len(visible) == 0 {
		return out, nil
	}

	placements, err := s.placements.ForDatapacks(visible)
	if err != nil {
		return nil, err
	}
	if len(placements) == 0 {
		return out, nil
	}

	collectionIDs := make([]string, 0, len(placements))
	seen := make(map[string]bool, len(placements))
	for _, pl := range placements {
		if !seen[pl.CollectionID] {
			seen[pl.CollectionID] = true
			collectionIDs = append(collectionIDs, pl.CollectionID)
		}
	}

	collections, err := s.collections.ByIDs(collectionIDs)
	if err != nil {
		return nil, err
	}
	folders, err := s.collections.FoldersByCollections(collectionIDs)
	if err != nil {
		return nil, err
	}

	collectionNameByID := make(map[string]string, len(collections))
	for _, p := range collections {
		collectionNameByID[p.ID] = p.Name
	}
	folderByID := make(map[string]models.CollectionFolder, len(folders))
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
		collectionName := collectionNameByID[pl.CollectionID]
		out[pl.DatapackID] = models.DatapackPath{
			DatapackID:     pl.DatapackID,
			CollectionID:   pl.CollectionID,
			CollectionName: collectionName,
			FolderID:       pl.FolderID,
			FolderName:     folder.Name,
			Path:           collectionName + folderPathSeparator + folder.Name,
		}
	}
	return out, nil
}

// Stats returns what a collection holds.
//
// Scoped exactly like the tree and the activity feed: the caller's visible
// datapacks, so a recipient's numbers cover only what was shared with them
// rather than the owner's whole collection.
func (s *CollectionService) Stats(userID, collectionID string) (repository.CollectionStats, error) {
	empty := repository.CollectionStats{KBFilesByType: map[string]int64{}}
	if s.stats == nil {
		return empty, fmt.Errorf("collection stats are unavailable")
	}
	scope, err := s.scopeFor(userID, collectionID)
	if err != nil {
		return empty, err
	}

	datapackIDs := make([]string, 0, len(scope.placements))
	for _, pl := range scope.placements {
		datapackIDs = append(datapackIDs, pl.DatapackID)
	}
	return s.stats.Stats(datapackIDs, DatapackIndexSchema, DatapackIndexName)
}
