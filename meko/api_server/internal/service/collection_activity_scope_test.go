package service

import (
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"meko/api_server/internal/repository"
)

// capturingActivity records which datapacks the feed was asked about, which
// is the whole privacy question: a recipient must never see activity from a
// datapack that was not shared with them, even though the collection holds more.
type capturingActivity struct {
	askedFor []string
	// The folder ids the structural side was scoped to, so a test can assert
	// a recipient is not handed the owner's whole tree.
	structural *repository.StructuralActivity
}

func (c *capturingActivity) ListActivityLogForDatapacks(
	datapackIDs []string, limit int, cursor string,
	structural *repository.StructuralActivity,
) (ActivityLogResponse, error) {
	c.askedFor = append([]string{}, datapackIDs...)
	c.structural = structural
	return ActivityLogResponse{Items: []repository.ActivityLogItem{}, Limit: limit}, nil
}

// Alice owns a collection holding four datapacks and shares two with Bob. Bob's
// activity feed must cover exactly those two.
func TestCollectionActivityLog_RecipientSeesOnlyTheSharedDatapacks(t *testing.T) {
	svc, mock, db := newCollectionService(t)
	defer db.Close()
	activity := &capturingActivity{}
	svc.activity = activity

	now := time.Now()

	// Bob does not own the collection.
	mock.ExpectQuery("FROM +meko_system.collections").
		WithArgs("proj-1", "bob").
		WillReturnError(sql.ErrNoRows)

	// His datapack list is already grant-checked: it holds the two Alice
	// shared and one of his own. The other two in Alice's collection never
	// appear here, which is what makes the rest of this safe.
	mock.ExpectQuery("FROM meko_system.datapacks").
		WithArgs("bob").
		WillReturnRows(sqlmock.NewRows([]string{
			"datapack_id", "user_id", "datapack_name", "created_by_agent_id", "universe_id",
			"conn", "mcp_user", "created_at", "updated_at", "description",
			"owner_name", "owner_email", "grant",
		}).
			AddRow("shared-1", "alice", "perf", "", "u", "", "", now, now, "", "Alice", "", "read").
			AddRow("shared-2", "alice", "wal", "", "u", "", "", now, now, "", "Alice", "", "read").
			AddRow("bobs-own", "bob", "mine", "", "u", "", "", now, now, "", "", "", "owner"))

	// Placements for those three. Bob's own datapack sits elsewhere.
	mock.ExpectQuery("FROM +meko_system.datapack_placements").
		WithArgs(pq.Array([]string{"shared-1", "shared-2", "bobs-own"})).
		WillReturnRows(sqlmock.NewRows(
			[]string{"datapack_id", "collection_id", "folder_id", "placed_at", "updated_at"}).
			AddRow("shared-1", "proj-1", "f1", now, now).
			AddRow("shared-2", "proj-1", "f1", now, now).
			AddRow("bobs-own", "proj-other", "f9", now, now))

	mock.ExpectQuery("FROM +meko_system.collections").
		WithArgs(pq.Array([]string{"proj-1"})).
		WillReturnRows(sqlmock.NewRows(
			[]string{"collection_id", "user_id", "collection_name", "theme_key", "theme_version",
				"description", "owner_name", "created_at", "updated_at"}).
			AddRow("proj-1", "alice", "adlc", "adlc", "1.0", "", "Alice", now, now))

	// The structural sides. Bob is not the owner, so folders come from his
	// OWN placements: f1 only. f9 is his other collection's, and Alice's
	// other folders never enter the query at all.
	mock.ExpectQuery("FROM +meko_system.collection_folders").
		WithArgs(pq.Array([]string{"f1"}), 11).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "folder_id", "folder_name"}).
			AddRow(now, "f1", "perf_baselines"))
	mock.ExpectQuery("FROM +meko_system.datapack_placements p").
		WithArgs(pq.Array([]string{"shared-1", "shared-2"}), 11).
		WillReturnRows(sqlmock.NewRows(
			[]string{"updated_at", "datapack_id", "folder_name", "folder_id"}).
			AddRow(now, "shared-1", "perf_baselines", "f1"))

	if _, err := svc.ActivityLog("bob", "proj-1", 10, ""); err != nil {
		t.Fatalf("ActivityLog: %v", err)
	}

	// The folder side is the one that could leak an owner's structure past a
	// share, so assert it was handed the scoped set rather than trusting the
	// datapack assertion below to cover it.
	if activity.structural == nil {
		t.Fatal("structural sides missing: the collection feed must carry them")
	}
	if n := len(activity.structural.Folders); n != 1 {
		t.Errorf("folder events for %d folders, want only the shared one", n)
	}

	want := map[string]bool{"shared-1": true, "shared-2": true}
	if len(activity.askedFor) != len(want) {
		t.Fatalf("feed asked about %v, want exactly the two shared datapacks", activity.askedFor)
	}
	for _, id := range activity.askedFor {
		if !want[id] {
			t.Errorf("feed asked about %q, which was not shared with this caller", id)
		}
	}
}

// The same collection opened by its owner covers everything in it, so the test
// above is measuring scoping rather than a feed that is narrow for everyone.
func TestCollectionActivityLog_OwnerSeesEveryDatapackInTheCollection(t *testing.T) {
	svc, mock, db := newCollectionService(t)
	defer db.Close()
	activity := &capturingActivity{}
	svc.activity = activity

	collectionDeleteMocks(mock, []string{"dp-a", "dp-b", "dp-c"})

	now := time.Now()
	// The owner's folder list is the whole collection, including folders
	// holding nothing: an empty folder they created is still their event.
	mock.ExpectQuery("FROM +meko_system.collection_folders\n\t\t WHERE +collection_id").
		WithArgs("proj-1").
		WillReturnRows(sqlmock.NewRows(
			[]string{"folder_id", "collection_id", "folder_name", "created_at", "updated_at"}).
			AddRow("f1", "proj-1", "standards", now, now).
			AddRow("f2", "proj-1", "empty_one", now, now))
	mock.ExpectQuery("FROM +meko_system.collection_folders").
		WithArgs(pq.Array([]string{"f1", "f2"}), 11).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "folder_id", "folder_name"}).
			AddRow(now, "f2", "empty_one"))
	mock.ExpectQuery("FROM +meko_system.datapack_placements p").
		WithArgs(pq.Array([]string{"dp-a", "dp-b", "dp-c"}), 11).
		WillReturnRows(sqlmock.NewRows(
			[]string{"updated_at", "datapack_id", "folder_name", "folder_id"}).
			AddRow(now, "dp-a", "standards", "f1"))

	if _, err := svc.ActivityLog("me", "proj-1", 10, ""); err != nil {
		t.Fatalf("ActivityLog: %v", err)
	}
	if len(activity.askedFor) != 3 {
		t.Fatalf("feed asked about %v, want all three", activity.askedFor)
	}
	// An empty folder still produces an event for its owner, which is the
	// case the recipient path above cannot reach.
	if activity.structural == nil || len(activity.structural.Folders) != 1 {
		t.Error("owner feed should carry the folder event, including for empty folders")
	}
}
