package service

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"meko/api_server/internal/repository"
)

func newProjectService(t *testing.T) (*ProjectService, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	svc := NewProjectService(
		repository.NewProjectRepo(db),
		repository.NewPlacementRepo(db),
		repository.NewDatapackRepo(db, "", repository.DeployEnvProd),
		repository.NewDatapackRollupRepo(db),
		// stats: nil, so the tests below exercise the tree and the feed
		// without the statistics panel's queries.
		nil,
		nil,
		nil,
	)
	return svc, mock, db
}

func TestValidateTreeName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"rejects empty", "", true},
		{"rejects whitespace only", "   ", true},
		{"rejects one character", "a", true},
		{"accepts the two-character floor, which datapack names disallow", "qa", false},
		{"accepts a normal name", "coding_standards", false},
		{"rejects HTML tags", "<script>alert(1)</script>", true},
		{"rejects the path separator, which would make a path ambiguous", "features/perf", true},
		{"trims before measuring", "  ok  ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTreeName("folder", tc.input)
			if tc.wantErr && err == nil {
				t.Fatalf("expected %q to be rejected", tc.input)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected %q to be accepted, got %v", tc.input, err)
			}
			if tc.wantErr && err != nil && !errors.Is(err, ErrInvalidProjectName) {
				t.Errorf("error should match ErrInvalidProjectName so the handler answers 400, got %v", err)
			}
		})
	}
}

func TestValidateTreeName_RejectsOverLongName(t *testing.T) {
	long := make([]rune, maxTreeNameLength+1)
	for i := range long {
		long[i] = 'a'
	}
	if err := validateTreeName("project", string(long)); err == nil {
		t.Fatal("expected a name over the length cap to be rejected")
	}
}

// Filing into a folder that belongs to someone else's project must be refused
// BEFORE anything is written. folder_id is client-supplied, so without this
// check a caller could place a datapack into another user's tree.
func TestPlace_RefusesFolderOwnedByAnotherUser(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()

	// Place is owner-only, so every case first resolves the datapack as owned.
	mock.ExpectQuery("FROM meko_system.datapacks").
		WithArgs("me", "datapack-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"datapack_id", "user_id", "datapack_name", "created_by_agent_id", "universe_id",
			"conn", "mcp_user", "created_at", "updated_at", "description", "owner_name", "owner_email",
		}).AddRow("datapack-1", "me", "dp", "", "u", "", "", time.Now(), time.Now(), "", "", ""))
	mock.ExpectQuery("FROM +meko_system.project_folders").
		WithArgs("folder-1").
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "user_id"}).
			AddRow("project-1", "someone-else"))

	err := svc.Place("me", "datapack-1", "folder-1")
	if !errors.Is(err, ErrFolderNotInProject) {
		t.Fatalf("expected ErrFolderNotInProject, got %v", err)
	}
	// No INSERT was expected, so an unfulfilled expectation here would mean a
	// write slipped through.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected statements: %v", err)
	}
}

// A folder_id that does not exist is reported as not-found rather than as a
// server error, and must not be distinguishable from "owned by someone else" —
// otherwise the endpoint becomes a probe for folder ids in other users' trees.
func TestPlace_UnknownFolderReportsNotInProject(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()

	// Place is owner-only, so every case first resolves the datapack as owned.
	mock.ExpectQuery("FROM meko_system.datapacks").
		WithArgs("me", "datapack-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"datapack_id", "user_id", "datapack_name", "created_by_agent_id", "universe_id",
			"conn", "mcp_user", "created_at", "updated_at", "description", "owner_name", "owner_email",
		}).AddRow("datapack-1", "me", "dp", "", "u", "", "", time.Now(), time.Now(), "", "", ""))
	mock.ExpectQuery("FROM +meko_system.project_folders").
		WithArgs("folder-missing").
		WillReturnError(sql.ErrNoRows)

	if err := svc.Place("me", "datapack-1", "folder-missing"); !errors.Is(err, ErrFolderNotInProject) {
		t.Fatalf("expected ErrFolderNotInProject, got %v", err)
	}
}

func TestPlace_RequiresFolderID(t *testing.T) {
	svc, _, db := newProjectService(t)
	defer db.Close()

	if err := svc.Place("me", "datapack-1", "  "); !errors.Is(err, ErrInvalidProjectName) {
		t.Fatalf("expected a validation error for a blank folder_id, got %v", err)
	}
}

// Filing into a folder the caller DOES own writes the placement, with the
// project_id resolved from the folder rather than trusted from the client.
func TestPlace_WritesPlacementForOwnedFolder(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()

	// Place is owner-only, so every case first resolves the datapack as owned.
	mock.ExpectQuery("FROM meko_system.datapacks").
		WithArgs("me", "datapack-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"datapack_id", "user_id", "datapack_name", "created_by_agent_id", "universe_id",
			"conn", "mcp_user", "created_at", "updated_at", "description", "owner_name", "owner_email",
		}).AddRow("datapack-1", "me", "dp", "", "u", "", "", time.Now(), time.Now(), "", "", ""))
	mock.ExpectQuery("FROM +meko_system.project_folders").
		WithArgs("folder-1").
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "user_id"}).
			AddRow("project-1", "me"))
	// No user_id: a datapack has one placement, owned by its owner.
	mock.ExpectExec("INSERT INTO meko_system.datapack_placements").
		WithArgs("datapack-1", "project-1", "folder-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := svc.Place("me", "datapack-1", "folder-1"); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A project must end up with at least one folder however it was requested:
// a datapack has to be created inside one, so a folderless project is a dead
// end the user could not recover from without adding a folder first.
func TestResolveInitialFolders(t *testing.T) {
	theme := []string{"coding_standards", "features"}

	t.Run("falls back to the theme when the caller names none", func(t *testing.T) {
		got, err := resolveInitialFolders(nil, theme)
		if err != nil {
			t.Fatalf("resolveInitialFolders: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %v, want the theme's folders", got)
		}
	})

	t.Run("the caller's folders win over the theme's", func(t *testing.T) {
		got, err := resolveInitialFolders([]string{"standards"}, theme)
		if err != nil {
			t.Fatalf("resolveInitialFolders: %v", err)
		}
		if len(got) != 1 || got[0] != "standards" {
			t.Errorf("got %v, want [standards]", got)
		}
	})

	t.Run("a themeless layout with no caller folders is refused", func(t *testing.T) {
		if _, err := resolveInitialFolders(nil, nil); !errors.Is(err, ErrInvalidProjectName) {
			t.Fatalf("expected a validation error for a folderless project, got %v", err)
		}
	})

	t.Run("blank entries do not count toward the minimum", func(t *testing.T) {
		if _, err := resolveInitialFolders([]string{"  ", ""}, nil); !errors.Is(err, ErrInvalidProjectName) {
			t.Fatalf("expected whitespace-only folders to be refused, got %v", err)
		}
	})

	t.Run("duplicates are refused before the unique index sees them", func(t *testing.T) {
		if _, err := resolveInitialFolders([]string{"docs", "docs"}, nil); !errors.Is(err, ErrInvalidProjectName) {
			t.Fatalf("expected a duplicate folder to be refused, got %v", err)
		}
	})

	t.Run("folder names are validated, not passed through", func(t *testing.T) {
		if _, err := resolveInitialFolders([]string{"a/b"}, nil); !errors.Is(err, ErrInvalidProjectName) {
			t.Fatalf("expected the path separator to be refused, got %v", err)
		}
	})
}

// The create-datapack guard. It runs before the datapack exists, so these
// cases decide whether a create is attempted at all.

// Creating a datapack never requires a folder, whether or not the caller has
// projects. Filing is opt-in: making it a precondition would break every
// existing caller the moment they created their first project, agents
// included. An earlier revision did require one, which is what this asserts
// is gone.
func TestValidateNewDatapackFolder_NeverRequiresAFolder(t *testing.T) {
	for _, name := range []string{"empty", "whitespace"} {
		t.Run(name, func(t *testing.T) {
			svc, mock, db := newProjectService(t)
			defer db.Close()

			folderID := ""
			if name == "whitespace" {
				folderID = "   "
			}
			if err := svc.ValidateNewDatapackFolder("me", folderID); err != nil {
				t.Fatalf("creating unfiled must be allowed, got %v", err)
			}
			// No folder named means nothing to look up: the guard must not
			// ask the database whether the user has projects.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unexpected query: %v", err)
			}
		})
	}
}

// Naming someone else's folder is refused before anything is created.
func TestValidateNewDatapackFolder_RefusesAnotherUsersFolder(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()

	mock.ExpectQuery("FROM meko_system.project_folders").
		WithArgs("folder-1").
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "user_id"}).
			AddRow("project-1", "someone-else"))

	if err := svc.ValidateNewDatapackFolder("me", "folder-1"); !errors.Is(err, ErrFolderNotInProject) {
		t.Fatalf("expected ErrFolderNotInProject, got %v", err)
	}
}

func TestValidateNewDatapackFolder_AcceptsAnOwnedFolder(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()

	mock.ExpectQuery("FROM meko_system.project_folders").
		WithArgs("folder-1").
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "user_id"}).
			AddRow("project-1", "me"))

	if err := svc.ValidateNewDatapackFolder("me", "folder-1"); err != nil {
		t.Fatalf("expected an owned folder to be accepted, got %v", err)
	}
}

// datapackRows builds the row set the datapack list queries scan, so an access
// query can be stubbed without restating 13 column names per test.
func datapackRows(owned map[string]string, sharedWith string) *sqlmock.Rows {
	cols := []string{
		"datapack_id", "user_id", "datapack_name", "created_by_agent_id", "universe_id",
		"conn", "mcp_user", "created_at", "updated_at", "description",
		"owner_name", "owner_email", "grant",
	}
	rows := sqlmock.NewRows(cols)
	now := time.Now()
	for id, owner := range owned {
		grant := "owner"
		if owner != sharedWith {
			grant = "viewer"
		}
		rows.AddRow(id, owner, id, "", "u", "", "", now, now, "", "", "", grant)
	}
	return rows
}

func placementRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"datapack_id", "project_id", "folder_id", "placed_at", "updated_at"})
}

// expectVisibleByIDs stubs DatapackRepo.VisibleByIDs: the owned query, the
// owner lookup when anything is owned, then the shared query.
func expectVisibleByIDs(mock sqlmock.Sqlmock, userID string, ids []string, owned, shared *sqlmock.Rows, anyOwned bool) {
	mock.ExpectQuery(`FROM meko_system.datapacks d\s+WHERE d.user_id = \$1 AND d.datapack_id = ANY`).
		WithArgs(userID, pq.Array(ids)).
		WillReturnRows(owned)
	if anyOwned {
		mock.ExpectQuery("FROM meko_system.users WHERE user_id").
			WithArgs(userID).
			WillReturnRows(sqlmock.NewRows([]string{"name", "email"}).AddRow("Me", "me@example.com"))
	}
	mock.ExpectQuery(`WITH shared AS`).
		WithArgs(userID, pq.Array(ids)).
		WillReturnRows(shared)
}

// expectSharedIDs stubs DatapackRepo.SharedIDs.
func expectSharedIDs(mock sqlmock.Sqlmock, userID string, ids ...string) {
	rows := sqlmock.NewRows([]string{"datapack_id"})
	for _, id := range ids {
		rows.AddRow(id)
	}
	mock.ExpectQuery(`WITH shared AS`).WithArgs(userID).WillReturnRows(rows)
}

// expectOwnedPlacements stubs PlacementRepo.OwnedByUser.
func expectOwnedPlacements(mock sqlmock.Sqlmock, rows *sqlmock.Rows, args ...driver.Value) {
	mock.ExpectQuery(`FROM +meko_system.projects p\s+JOIN +meko_system.datapack_placements`).
		WithArgs(args...).
		WillReturnRows(rows)
}

func folderRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"folder_id", "project_id", "folder_name", "folder_description", "created_at", "updated_at",
	})
}

func projectRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"project_id", "user_id", "project_name", "theme_key", "theme_version",
		"description", "owner_name", "owner_email", "created_at", "updated_at",
	})
}

// DatapackPaths assembles the label server-side, always as project/folder.
func TestDatapackPaths_ResolvesProjectAndFolderNames(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()

	now := time.Now()
	// The caller's own placements, via their projects, then what is shared.
	expectOwnedPlacements(mock, placementRows().
		AddRow("dp-root", "proj-1", "folder-root", now, now).
		AddRow("dp-child", "proj-1", "folder-child", now, now), "me")
	expectSharedIDs(mock, "me")

	mock.ExpectQuery("FROM +meko_system.projects").
		WillReturnRows(projectRows().
			AddRow("proj-1", "me", "ADLC", "adlc", "1.0", "", "Me", "me@example.com", now, now))

	mock.ExpectQuery("FROM +meko_system.project_folders").
		WillReturnRows(folderRows().
			AddRow("folder-root", "proj-1", "coding_standards", "", now, now).
			AddRow("folder-child", "proj-1", "features", "", now, now))

	paths, err := svc.DatapackPaths("me", nil)
	if err != nil {
		t.Fatalf("DatapackPaths: %v", err)
	}
	// A path is always exactly project/folder: folders do not nest.
	if got := paths["dp-root"].Path; got != "ADLC/coding_standards" {
		t.Errorf("path = %q, want %q", got, "ADLC/coding_standards")
	}
	if got := paths["dp-child"].Path; got != "ADLC/features" {
		t.Errorf("path = %q, want %q", got, "ADLC/features")
	}
	if got := paths["dp-child"].FolderName; got != "features" {
		t.Errorf("folder_name = %q, want %q", got, "features")
	}
}

// A caller with nothing filed and nothing shared pays for the two access
// queries and nothing else.
func TestDatapackPaths_SkipsFollowUpQueriesWhenNothingIsVisible(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()

	expectOwnedPlacements(mock, placementRows(), "me")
	expectSharedIDs(mock, "me")

	paths, err := svc.DatapackPaths("me", nil)
	if err != nil {
		t.Fatalf("DatapackPaths: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("expected an empty map, got %d entries", len(paths))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected follow-up queries: %v", err)
	}
}

// datapack_ids narrows both halves: the owned placements in SQL, the shared ids
// before their placements are read. A shared datapack outside the filter is
// never looked up.
func TestDatapackPaths_FiltersToTheRequestedDatapacks(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()

	now := time.Now()
	want := []string{"dp-mine", "dp-shared"}
	expectOwnedPlacements(mock, placementRows().AddRow("dp-mine", "proj-1", "f1", now, now),
		"me", pq.Array(want))
	expectSharedIDs(mock, "me", "dp-shared", "dp-shared-other")
	mock.ExpectQuery("FROM +meko_system.datapack_placements").
		WithArgs(pq.Array([]string{"dp-shared"})).
		WillReturnRows(placementRows().AddRow("dp-shared", "proj-2", "f2", now, now))
	mock.ExpectQuery("FROM +meko_system.projects").
		WillReturnRows(projectRows().
			AddRow("proj-1", "me", "Mine", "adlc", "1.0", "", "Me", "me@example.com", now, now).
			AddRow("proj-2", "alice", "Hers", "adlc", "1.0", "", "Alice", "alice@example.com", now, now))
	mock.ExpectQuery("FROM +meko_system.project_folders").
		WillReturnRows(folderRows().
			AddRow("f1", "proj-1", "a", "", now, now).
			AddRow("f2", "proj-2", "b", "", now, now))

	paths, err := svc.DatapackPaths("me", want)
	if err != nil {
		t.Fatalf("DatapackPaths: %v", err)
	}
	if len(paths) != 2 || paths["dp-mine"].Path != "Mine/a" || paths["dp-shared"].Path != "Hers/b" {
		t.Errorf("paths = %+v", paths)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// SharedIDs returns lowercase UUIDs. An uppercase filter must still select
// that shared datapack. ::uuid would match it on the owned half either way.
func TestDatapackPaths_UppercaseIDStillMatchesShared(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()

	id := "aaaaaaaa-0000-0000-0000-000000000001"
	now := time.Now()
	expectOwnedPlacements(mock, placementRows(), "me", pq.Array([]string{id}))
	expectSharedIDs(mock, "me", id)
	mock.ExpectQuery("FROM +meko_system.datapack_placements").
		WithArgs(pq.Array([]string{id})).
		WillReturnRows(placementRows().AddRow(id, "proj-2", "f2", now, now))
	mock.ExpectQuery("FROM +meko_system.projects").
		WillReturnRows(projectRows().
			AddRow("proj-2", "alice", "Hers", "adlc", "1.0", "", "Alice", "alice@example.com", now, now))
	mock.ExpectQuery("FROM +meko_system.project_folders").
		WillReturnRows(folderRows().
			AddRow("f2", "proj-2", "b", "", now, now))

	paths, err := svc.DatapackPaths("me", []string{strings.ToUpper(id)})
	if err != nil {
		t.Fatalf("DatapackPaths: %v", err)
	}
	if len(paths) != 1 || paths[id].Path != "Hers/b" {
		t.Errorf("paths = %+v", paths)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// An empty datapack_ids asks about nothing, so nothing is queried.
func TestDatapackPaths_EmptyFilterQueriesNothing(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()

	paths, err := svc.DatapackPaths("me", []string{})
	if err != nil || len(paths) != 0 {
		t.Fatalf("paths = %v, err = %v", paths, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected queries: %v", err)
	}
}

// THE privacy test. Alice owns a project holding two datapacks and shares only
// one with Bob. Bob must see the project, the one datapack, and the one folder
// it sits in — never the sibling folder or the datapack he was not given.
//
// The guarantee is structural: the placement lookup is keyed by the datapacks
// Bob may see, so Alice's other datapack is never in a result set at all.
func TestListSharedWithCounts_ScopesToWhatTheRecipientCanSee(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()

	now := time.Now()
	// Bob can see exactly one datapack, owned by Alice.
	expectSharedIDs(mock, "bob", "dp-shared")

	// Only that datapack's placement comes back — "dp-secret" is never asked for.
	mock.ExpectQuery("FROM +meko_system.datapack_placements").
		WillReturnRows(sqlmock.NewRows(
			[]string{"datapack_id", "project_id", "folder_id", "placed_at", "updated_at"}).
			AddRow("dp-shared", "proj-alice", "folder-features", now, now))

	mock.ExpectQuery("FROM +meko_system.projects").
		WillReturnRows(projectRows().
			AddRow("proj-alice", "alice", "ADLC", "adlc", "1.0", "", "Alice", "alice@example.com", now, now))

	// Alice's project really has two folders; only one holds something Bob can see.
	mock.ExpectQuery("FROM +meko_system.project_folders").
		WillReturnRows(folderRows().
			AddRow("folder-features", "proj-alice", "features", "", now, now).
			AddRow("folder-compliance", "proj-alice", "compliance", "", now, now))

	shared, err := svc.ListSharedWithCounts("bob")
	if err != nil {
		t.Fatalf("ListSharedWithCounts: %v", err)
	}
	if len(shared) != 1 {
		t.Fatalf("got %d shared projects, want 1", len(shared))
	}
	p := shared[0]
	if p.OwnerName != "Alice" {
		t.Errorf("owner_name = %q, want Alice so the recipient knows whose project it is", p.OwnerName)
	}
	if p.OwnerEmail != "alice@example.com" {
		t.Errorf("owner_email = %q, want alice@example.com for the card's Shared by line", p.OwnerEmail)
	}
	if p.DatapackCount != 1 {
		t.Errorf("datapack_count = %d, want 1 (only what Bob can see)", p.DatapackCount)
	}
	if len(p.FolderNames) != 1 || p.FolderNames[0] != "features" {
		t.Errorf("folder_names = %v, want only [features]; %q must not leak", p.FolderNames, "compliance")
	}
}

// A project the caller owns is not "shared with" them, even when a datapack
// inside it was shared out and came back through the same access list.
func TestListSharedWithCounts_ExcludesTheCallersOwnProjects(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()

	// Owned datapacks are not in the shared set, so nothing else is queried.
	expectSharedIDs(mock, "me")

	shared, err := svc.ListSharedWithCounts("me")
	if err != nil {
		t.Fatalf("ListSharedWithCounts: %v", err)
	}
	if len(shared) != 0 {
		t.Errorf("got %d shared projects, want 0", len(shared))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected queries: %v", err)
	}
}

// ListWithCounts must not issue a query per project — the counts come from two
// whole-tree reads. Asserting the exact statement set is what stops a later
// change reintroducing a per-project loop.
func TestListWithCounts_UsesThreeQueriesForAnyNumberOfProjects(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()

	now := time.Now()
	mock.ExpectQuery("FROM +meko_system.projects").
		WithArgs("me").
		WillReturnRows(sqlmock.NewRows(
			[]string{"project_id", "user_id", "project_name", "theme_key", "theme_version",
				"description", "created_at", "updated_at"}).
			AddRow("proj-1", "me", "ADLC", "adlc", "1.0", "", now, now).
			AddRow("proj-2", "me", "support", "support", "1.0", "", now, now))

	// By project id, not by user: the join form plans as a full scan of
	// project_folders, so ListWithCounts passes the ids it already has.
	mock.ExpectQuery("FROM +meko_system.project_folders").
		WithArgs(pq.Array([]string{"proj-1", "proj-2"})).
		WillReturnRows(folderRows().
			AddRow("f1", "proj-1", "coding_standards", "", now, now).
			AddRow("f2", "proj-1", "features", "", now, now).
			AddRow("f3", "proj-2", "playbooks", "", now, now))

	mock.ExpectQuery("FROM +meko_system.datapack_placements").
		WithArgs("me").
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "folder_id", "count"}).
			AddRow("proj-1", "f1", 4).
			AddRow("proj-1", "f2", 3).
			AddRow("proj-2", "f3", 9))

	projects, err := svc.ListWithCounts("me")
	if err != nil {
		t.Fatalf("ListWithCounts: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("got %d projects, want 2", len(projects))
	}
	if projects[0].FolderCount != 2 || projects[0].DatapackCount != 7 {
		t.Errorf("proj-1 counts = %d folders / %d datapacks, want 2 / 7",
			projects[0].FolderCount, projects[0].DatapackCount)
	}
	if projects[1].FolderCount != 1 || projects[1].DatapackCount != 9 {
		t.Errorf("proj-2 counts = %d folders / %d datapacks, want 1 / 9",
			projects[1].FolderCount, projects[1].DatapackCount)
	}
	// Three expectations declared, three met — a per-project query would show
	// up here as an unexpected statement.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("query count changed: %v", err)
	}
}
