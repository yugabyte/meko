package service

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"meko/api_server/internal/repository"
)

func newCollectionService(t *testing.T) (*CollectionService, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	svc := NewCollectionService(
		repository.NewCollectionRepo(db),
		repository.NewPlacementRepo(db),
		repository.NewDatapackRepo(db, "", repository.DeployEnvProd),
		repository.NewDatapackRollupRepo(db),
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
			if tc.wantErr && err != nil && !errors.Is(err, ErrInvalidCollectionName) {
				t.Errorf("error should match ErrInvalidCollectionName so the handler answers 400, got %v", err)
			}
		})
	}
}

func TestValidateTreeName_RejectsOverLongName(t *testing.T) {
	long := make([]rune, maxTreeNameLength+1)
	for i := range long {
		long[i] = 'a'
	}
	if err := validateTreeName("collection", string(long)); err == nil {
		t.Fatal("expected a name over the length cap to be rejected")
	}
}

// Filing into a folder that belongs to someone else's collection must be refused
// BEFORE anything is written. folder_id is client-supplied, so without this
// check a caller could place a datapack into another user's tree.
func TestPlace_RefusesFolderOwnedByAnotherUser(t *testing.T) {
	svc, mock, db := newCollectionService(t)
	defer db.Close()

	// Place is owner-only, so every case first resolves the datapack as owned.
	mock.ExpectQuery("FROM meko_system.datapacks").
		WithArgs("me", "datapack-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"datapack_id", "user_id", "datapack_name", "created_by_agent_id", "universe_id",
			"conn", "mcp_user", "created_at", "updated_at", "description", "owner_name", "owner_email",
		}).AddRow("datapack-1", "me", "dp", "", "u", "", "", time.Now(), time.Now(), "", "", ""))
	mock.ExpectQuery("FROM +meko_system.collection_folders").
		WithArgs("folder-1").
		WillReturnRows(sqlmock.NewRows([]string{"collection_id", "user_id"}).
			AddRow("collection-1", "someone-else"))

	err := svc.Place("me", "datapack-1", "folder-1")
	if !errors.Is(err, ErrFolderNotInCollection) {
		t.Fatalf("expected ErrFolderNotInCollection, got %v", err)
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
func TestPlace_UnknownFolderReportsNotInCollection(t *testing.T) {
	svc, mock, db := newCollectionService(t)
	defer db.Close()

	// Place is owner-only, so every case first resolves the datapack as owned.
	mock.ExpectQuery("FROM meko_system.datapacks").
		WithArgs("me", "datapack-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"datapack_id", "user_id", "datapack_name", "created_by_agent_id", "universe_id",
			"conn", "mcp_user", "created_at", "updated_at", "description", "owner_name", "owner_email",
		}).AddRow("datapack-1", "me", "dp", "", "u", "", "", time.Now(), time.Now(), "", "", ""))
	mock.ExpectQuery("FROM +meko_system.collection_folders").
		WithArgs("folder-missing").
		WillReturnError(sql.ErrNoRows)

	if err := svc.Place("me", "datapack-1", "folder-missing"); !errors.Is(err, ErrFolderNotInCollection) {
		t.Fatalf("expected ErrFolderNotInCollection, got %v", err)
	}
}

func TestPlace_RequiresFolderID(t *testing.T) {
	svc, _, db := newCollectionService(t)
	defer db.Close()

	if err := svc.Place("me", "datapack-1", "  "); !errors.Is(err, ErrInvalidCollectionName) {
		t.Fatalf("expected a validation error for a blank folder_id, got %v", err)
	}
}

// Filing into a folder the caller DOES own writes the placement, with the
// collection_id resolved from the folder rather than trusted from the client.
func TestPlace_WritesPlacementForOwnedFolder(t *testing.T) {
	svc, mock, db := newCollectionService(t)
	defer db.Close()

	// Place is owner-only, so every case first resolves the datapack as owned.
	mock.ExpectQuery("FROM meko_system.datapacks").
		WithArgs("me", "datapack-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"datapack_id", "user_id", "datapack_name", "created_by_agent_id", "universe_id",
			"conn", "mcp_user", "created_at", "updated_at", "description", "owner_name", "owner_email",
		}).AddRow("datapack-1", "me", "dp", "", "u", "", "", time.Now(), time.Now(), "", "", ""))
	mock.ExpectQuery("FROM +meko_system.collection_folders").
		WithArgs("folder-1").
		WillReturnRows(sqlmock.NewRows([]string{"collection_id", "user_id"}).
			AddRow("collection-1", "me"))
	// No user_id: a datapack has one placement, owned by its owner.
	mock.ExpectExec("INSERT INTO meko_system.datapack_placements").
		WithArgs("datapack-1", "collection-1", "folder-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := svc.Place("me", "datapack-1", "folder-1"); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A collection must end up with at least one folder however it was requested:
// a datapack has to be created inside one, so a folderless collection is a dead
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
		if _, err := resolveInitialFolders(nil, nil); !errors.Is(err, ErrInvalidCollectionName) {
			t.Fatalf("expected a validation error for a folderless collection, got %v", err)
		}
	})

	t.Run("blank entries do not count toward the minimum", func(t *testing.T) {
		if _, err := resolveInitialFolders([]string{"  ", ""}, nil); !errors.Is(err, ErrInvalidCollectionName) {
			t.Fatalf("expected whitespace-only folders to be refused, got %v", err)
		}
	})

	t.Run("duplicates are refused before the unique index sees them", func(t *testing.T) {
		if _, err := resolveInitialFolders([]string{"docs", "docs"}, nil); !errors.Is(err, ErrInvalidCollectionName) {
			t.Fatalf("expected a duplicate folder to be refused, got %v", err)
		}
	})

	t.Run("folder names are validated, not passed through", func(t *testing.T) {
		if _, err := resolveInitialFolders([]string{"a/b"}, nil); !errors.Is(err, ErrInvalidCollectionName) {
			t.Fatalf("expected the path separator to be refused, got %v", err)
		}
	})
}

// The create-datapack guard. It runs before the datapack exists, so these
// cases decide whether a create is attempted at all.

// Creating a datapack never requires a folder, whether or not the caller has
// collections. Filing is opt-in: making it a precondition would break every
// existing caller the moment they created their first collection, agents
// included. An earlier revision did require one, which is what this asserts
// is gone.
func TestValidateNewDatapackFolder_NeverRequiresAFolder(t *testing.T) {
	for _, name := range []string{"empty", "whitespace"} {
		t.Run(name, func(t *testing.T) {
			svc, mock, db := newCollectionService(t)
			defer db.Close()

			folderID := ""
			if name == "whitespace" {
				folderID = "   "
			}
			if err := svc.ValidateNewDatapackFolder("me", folderID); err != nil {
				t.Fatalf("creating unfiled must be allowed, got %v", err)
			}
			// No folder named means nothing to look up: the guard must not
			// ask the database whether the user has collections.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unexpected query: %v", err)
			}
		})
	}
}

// Naming someone else's folder is refused before anything is created.
func TestValidateNewDatapackFolder_RefusesAnotherUsersFolder(t *testing.T) {
	svc, mock, db := newCollectionService(t)
	defer db.Close()

	mock.ExpectQuery("FROM meko_system.collection_folders").
		WithArgs("folder-1").
		WillReturnRows(sqlmock.NewRows([]string{"collection_id", "user_id"}).
			AddRow("collection-1", "someone-else"))

	if err := svc.ValidateNewDatapackFolder("me", "folder-1"); !errors.Is(err, ErrFolderNotInCollection) {
		t.Fatalf("expected ErrFolderNotInCollection, got %v", err)
	}
}

func TestValidateNewDatapackFolder_AcceptsAnOwnedFolder(t *testing.T) {
	svc, mock, db := newCollectionService(t)
	defer db.Close()

	mock.ExpectQuery("FROM meko_system.collection_folders").
		WithArgs("folder-1").
		WillReturnRows(sqlmock.NewRows([]string{"collection_id", "user_id"}).
			AddRow("collection-1", "me"))

	if err := svc.ValidateNewDatapackFolder("me", "folder-1"); err != nil {
		t.Fatalf("expected an owned folder to be accepted, got %v", err)
	}
}

// datapackRows builds the row set DatapackRepo.List scans, so the access
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

func folderRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"folder_id", "collection_id", "folder_name", "created_at", "updated_at",
	})
}

func collectionRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"collection_id", "user_id", "collection_name", "theme_key", "theme_version",
		"description", "owner_name", "created_at", "updated_at",
	})
}

// DatapackPaths assembles the label server-side, always as collection/folder.
func TestDatapackPaths_ResolvesCollectionAndFolderNames(t *testing.T) {
	svc, mock, db := newCollectionService(t)
	defer db.Close()

	now := time.Now()
	// Every read starts from what the caller may see.
	mock.ExpectQuery("FROM +meko_system.datapacks").
		WithArgs("me").
		WillReturnRows(datapackRows(map[string]string{"dp-root": "me", "dp-child": "me"}, "me"))

	mock.ExpectQuery("FROM +meko_system.datapack_placements").
		WillReturnRows(sqlmock.NewRows(
			[]string{"datapack_id", "collection_id", "folder_id", "placed_at", "updated_at"}).
			AddRow("dp-root", "proj-1", "folder-root", now, now).
			AddRow("dp-child", "proj-1", "folder-child", now, now))

	mock.ExpectQuery("FROM +meko_system.collections").
		WillReturnRows(collectionRows().
			AddRow("proj-1", "me", "ADLC", "adlc", "1.0", "", "Me", now, now))

	mock.ExpectQuery("FROM +meko_system.collection_folders").
		WillReturnRows(folderRows().
			AddRow("folder-root", "proj-1", "coding_standards", now, now).
			AddRow("folder-child", "proj-1", "features", now, now))

	paths, err := svc.DatapackPaths("me")
	if err != nil {
		t.Fatalf("DatapackPaths: %v", err)
	}
	// A path is always exactly collection/folder: folders do not nest.
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

// A caller who can see nothing pays for the access query and nothing else.
func TestDatapackPaths_SkipsFollowUpQueriesWhenNothingIsVisible(t *testing.T) {
	svc, mock, db := newCollectionService(t)
	defer db.Close()

	mock.ExpectQuery("FROM +meko_system.datapacks").
		WithArgs("me").
		WillReturnRows(datapackRows(nil, "me"))

	paths, err := svc.DatapackPaths("me")
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

// THE privacy test. Alice owns a collection holding two datapacks and shares only
// one with Bob. Bob must see the collection, the one datapack, and the one folder
// it sits in — never the sibling folder or the datapack he was not given.
//
// The guarantee is structural: the placement lookup is keyed by the datapacks
// Bob may see, so Alice's other datapack is never in a result set at all.
func TestListSharedWithCounts_ScopesToWhatTheRecipientCanSee(t *testing.T) {
	svc, mock, db := newCollectionService(t)
	defer db.Close()

	now := time.Now()
	// Bob can see exactly one datapack, owned by Alice.
	mock.ExpectQuery("FROM +meko_system.datapacks").
		WithArgs("bob").
		WillReturnRows(datapackRows(map[string]string{"dp-shared": "alice"}, "bob"))

	// Only that datapack's placement comes back — "dp-secret" is never asked for.
	mock.ExpectQuery("FROM +meko_system.datapack_placements").
		WillReturnRows(sqlmock.NewRows(
			[]string{"datapack_id", "collection_id", "folder_id", "placed_at", "updated_at"}).
			AddRow("dp-shared", "proj-alice", "folder-features", now, now))

	mock.ExpectQuery("FROM +meko_system.collections").
		WillReturnRows(collectionRows().
			AddRow("proj-alice", "alice", "ADLC", "adlc", "1.0", "", "Alice", now, now))

	// Alice's collection really has two folders; only one holds something Bob can see.
	mock.ExpectQuery("FROM +meko_system.collection_folders").
		WillReturnRows(folderRows().
			AddRow("folder-features", "proj-alice", "features", now, now).
			AddRow("folder-compliance", "proj-alice", "compliance", now, now))

	shared, err := svc.ListSharedWithCounts("bob")
	if err != nil {
		t.Fatalf("ListSharedWithCounts: %v", err)
	}
	if len(shared) != 1 {
		t.Fatalf("got %d shared collections, want 1", len(shared))
	}
	p := shared[0]
	if p.OwnerName != "Alice" {
		t.Errorf("owner_name = %q, want Alice so the recipient knows whose collection it is", p.OwnerName)
	}
	if p.DatapackCount != 1 {
		t.Errorf("datapack_count = %d, want 1 (only what Bob can see)", p.DatapackCount)
	}
	if len(p.FolderNames) != 1 || p.FolderNames[0] != "features" {
		t.Errorf("folder_names = %v, want only [features]; %q must not leak", p.FolderNames, "compliance")
	}
}

// A collection the caller owns is not "shared with" them, even when a datapack
// inside it was shared out and came back through the same access list.
func TestListSharedWithCounts_ExcludesTheCallersOwnCollections(t *testing.T) {
	svc, mock, db := newCollectionService(t)
	defer db.Close()

	mock.ExpectQuery("FROM +meko_system.datapacks").
		WithArgs("me").
		WillReturnRows(datapackRows(map[string]string{"dp-mine": "me"}, "me"))

	// Owned datapacks are not in the shared set, so nothing else is queried.
	shared, err := svc.ListSharedWithCounts("me")
	if err != nil {
		t.Fatalf("ListSharedWithCounts: %v", err)
	}
	if len(shared) != 0 {
		t.Errorf("got %d shared collections, want 0", len(shared))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected queries: %v", err)
	}
}

// ListWithCounts must not issue a query per collection — the counts come from two
// whole-tree reads. Asserting the exact statement set is what stops a later
// change reintroducing a per-collection loop.
func TestListWithCounts_UsesThreeQueriesForAnyNumberOfCollections(t *testing.T) {
	svc, mock, db := newCollectionService(t)
	defer db.Close()

	now := time.Now()
	mock.ExpectQuery("FROM +meko_system.collections").
		WithArgs("me").
		WillReturnRows(sqlmock.NewRows(
			[]string{"collection_id", "user_id", "collection_name", "theme_key", "theme_version",
				"description", "created_at", "updated_at"}).
			AddRow("proj-1", "me", "ADLC", "adlc", "1.0", "", now, now).
			AddRow("proj-2", "me", "support", "support", "1.0", "", now, now))

	// By collection id, not by user: the join form plans as a full scan of
	// collection_folders, so ListWithCounts passes the ids it already has.
	mock.ExpectQuery("FROM +meko_system.collection_folders").
		WithArgs(pq.Array([]string{"proj-1", "proj-2"})).
		WillReturnRows(folderRows().
			AddRow("f1", "proj-1", "coding_standards", now, now).
			AddRow("f2", "proj-1", "features", now, now).
			AddRow("f3", "proj-2", "playbooks", now, now))

	mock.ExpectQuery("FROM +meko_system.datapack_placements").
		WithArgs("me").
		WillReturnRows(sqlmock.NewRows([]string{"collection_id", "folder_id", "count"}).
			AddRow("proj-1", "f1", 4).
			AddRow("proj-1", "f2", 3).
			AddRow("proj-2", "f3", 9))

	collections, err := svc.ListWithCounts("me")
	if err != nil {
		t.Fatalf("ListWithCounts: %v", err)
	}
	if len(collections) != 2 {
		t.Fatalf("got %d collections, want 2", len(collections))
	}
	if collections[0].FolderCount != 2 || collections[0].DatapackCount != 7 {
		t.Errorf("proj-1 counts = %d folders / %d datapacks, want 2 / 7",
			collections[0].FolderCount, collections[0].DatapackCount)
	}
	if collections[1].FolderCount != 1 || collections[1].DatapackCount != 9 {
		t.Errorf("proj-2 counts = %d folders / %d datapacks, want 1 / 9",
			collections[1].FolderCount, collections[1].DatapackCount)
	}
	// Three expectations declared, three met — a per-collection query would show
	// up here as an unexpected statement.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("query count changed: %v", err)
	}
}
