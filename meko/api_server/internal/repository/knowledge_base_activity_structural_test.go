package repository

import (
	"testing"
)

// A project with folders but no datapacks is the ordinary state right
// after creating one. The datapack-scoped sides have nothing to query, but
// the structural rows the caller already fetched must still be served —
// returning early here once made a new project report "No activity yet"
// while holding folder_created rows.
func TestListActivityLog_NoDatapacks_StillServesStructural(t *testing.T) {
	repo := &KnowledgeBaseRepo{} // no DB: the datapack sides must not be queried

	structural := &StructuralActivity{
		Folders: []ActivityLogRow{{
			Ts:   "2026-10-01T10:00:00Z",
			ID:   "folder-1",
			Item: ActivityLogItem{Type: ActivityFolderCreated, Timestamp: "2026-10-01T10:00:00Z"},
		}},
	}

	items, _, err := repo.ListActivityLog(nil, "datapacks", "knowledge_bases", 10, ActivityLogCursor{}, structural)
	if err != nil {
		t.Fatalf("ListActivityLog: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 structural item, got %d", len(items))
	}
	if items[0].Type != ActivityFolderCreated {
		t.Fatalf("want %s, got %s", ActivityFolderCreated, items[0].Type)
	}
}

// Empty, not nil: `items` is a required array on the wire.
func TestListActivityLog_NoDatapacksNoStructural_EmptyNotNil(t *testing.T) {
	repo := &KnowledgeBaseRepo{}

	items, _, err := repo.ListActivityLog(nil, "datapacks", "knowledge_bases", 10, ActivityLogCursor{}, nil)
	if err != nil {
		t.Fatalf("ListActivityLog: %v", err)
	}
	if items == nil {
		t.Fatal("items must be an empty slice, not nil")
	}
	if len(items) != 0 {
		t.Fatalf("want 0 items, got %d", len(items))
	}
}

// Structural rows interleave with nothing else here, but the cursor must
// still carry the folder side forward so page 2 does not restart from the
// top and re-emit rows already shown.
func TestListActivityLog_StructuralCarriesCursor(t *testing.T) {
	repo := &KnowledgeBaseRepo{}

	structural := &StructuralActivity{
		Folders: []ActivityLogRow{
			{Ts: "2026-10-01T12:00:00Z", ID: "f2",
				Item: ActivityLogItem{Type: ActivityFolderCreated, Timestamp: "2026-10-01T12:00:00Z"}},
			{Ts: "2026-10-01T11:00:00Z", ID: "f1",
				Item: ActivityLogItem{Type: ActivityFolderCreated, Timestamp: "2026-10-01T11:00:00Z"}},
		},
	}

	items, next, err := repo.ListActivityLog(nil, "datapacks", "knowledge_bases", 1, ActivityLogCursor{}, structural)
	if err != nil {
		t.Fatalf("ListActivityLog: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 item on a limit-1 page, got %d", len(items))
	}
	// Newest first: f2 is kept, and the cursor must point at it.
	if next.FolderID != "f2" {
		t.Fatalf("want folder cursor f2, got %q", next.FolderID)
	}
	if next.FolderTs != "2026-10-01T12:00:00Z" {
		t.Fatalf("want folder ts 2026-10-01T12:00:00Z, got %q", next.FolderTs)
	}
}
