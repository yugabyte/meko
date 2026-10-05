package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/repository"
)

// A nil Go slice marshals to `null`, not `[]`. Every array in the collections API
// is declared as a required array on the wire, and the UI maps and spreads
// over them directly, so a nil one is not a degraded response — it is a
// TypeError that blanks the page.
//
// This has now happened three times in this feature (the Blank theme's
// folders, a collection whose folders were all deleted, and a collection with no
// datapacks yet), which is why the invariant is asserted rather than
// remembered. Each test below covers a path a user reaches in normal use.

// The Blank theme seeds no folders on purpose. That must be an EMPTY list, not
// a missing one: the create dialog maps over it to show the layout.
func TestCollectionThemes_FoldersNeverMarshalToNull(t *testing.T) {
	for _, theme := range CollectionThemes() {
		raw, err := json.Marshal(theme)
		if err != nil {
			t.Fatalf("marshal theme %q: %v", theme.Key, err)
		}
		if strings.Contains(string(raw), `"folders":null`) {
			t.Errorf("theme %q marshals folders as null; the picker maps over it: %s",
				theme.Key, raw)
		}
	}
}

// The state right after creating a collection: no datapacks in it yet, so the
// feed has nothing to read. The response must still carry an empty array.
func TestListActivityLogForDatapacks_EmptySetReturnsEmptyItems(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	svc := NewKnowledgeBaseService(repository.NewKnowledgeBaseRepo(db, db), "bucket")
	resp, err := svc.ListActivityLogForDatapacks(nil, 10, "", nil)
	if err != nil {
		t.Fatalf("ListActivityLogForDatapacks: %v", err)
	}
	if resp.Items == nil {
		t.Fatal("items is nil, which marshals to null; the feed spreads over it")
	}
	raw, mErr := json.Marshal(resp)
	if mErr != nil {
		t.Fatalf("marshal response: %v", mErr)
	}
	if strings.Contains(string(raw), `"items":null`) {
		t.Errorf("items marshaled as null: %s", raw)
	}
	// No datapacks means no query at all, not a query with an empty array.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected statements: %v", err)
	}
}

// A collection whose folders have all been deleted still renders, so folders must
// come back as an empty array rather than null.
func TestFolders_EmptyCollectionReturnsEmptySlice(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM +meko_system.collection_folders").
		WithArgs("proj-1").
		WillReturnRows(sqlmock.NewRows(
			[]string{"folder_id", "collection_id", "folder_name", "folder_description", "created_at", "updated_at"}))

	folders, err := repository.NewCollectionRepo(db).Folders("proj-1")
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if folders == nil {
		t.Fatal("folders is nil, which marshals to null; the detail page reads .length")
	}
	raw, mErr := json.Marshal(map[string]any{"folders": folders})
	if mErr != nil {
		t.Fatalf("marshal folders: %v", mErr)
	}
	if string(raw) != `{"folders":[]}` {
		t.Errorf("got %s, want an empty array", raw)
	}
}
