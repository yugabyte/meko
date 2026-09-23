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
}

func (c *capturingActivity) ListActivityLogForDatapacks(
	datapackIDs []string, limit int, cursor string,
) (ActivityLogResponse, error) {
	c.askedFor = append([]string{}, datapackIDs...)
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

	if _, err := svc.ActivityLog("bob", "proj-1", 10, ""); err != nil {
		t.Fatalf("ActivityLog: %v", err)
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

	if _, err := svc.ActivityLog("me", "proj-1", 10, ""); err != nil {
		t.Fatalf("ActivityLog: %v", err)
	}
	if len(activity.askedFor) != 3 {
		t.Fatalf("feed asked about %v, want all three", activity.askedFor)
	}
}
