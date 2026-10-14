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
// datapack that was not shared with them, even though the project holds more.
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

// Alice owns a project holding four datapacks and shares two with Bob. Bob's
// activity feed must cover exactly those two.
func TestProjectActivityLog_RecipientSeesOnlyTheSharedDatapacks(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()
	activity := &capturingActivity{}
	svc.activity = activity

	now := time.Now()

	// Bob does not own the project.
	mock.ExpectQuery("FROM +meko_system.projects").
		WithArgs("proj-1", "bob").
		WillReturnError(sql.ErrNoRows)

	// The project holds four datapacks. Access is checked on those ids, and
	// only the two shared with Bob come back.
	all := []string{"shared-1", "shared-2", "secret-1", "secret-2"}
	pl := placementRows()
	for _, id := range all {
		pl.AddRow(id, "proj-1", "f1", now, now)
	}
	mock.ExpectQuery("FROM +meko_system.datapack_placements").
		WithArgs("proj-1").
		WillReturnRows(pl)
	expectVisibleByIDs(mock, "bob", all, datapackRows(nil, "bob"),
		datapackRows(map[string]string{"shared-1": "alice", "shared-2": "alice"}, "bob"), false)

	mock.ExpectQuery("FROM +meko_system.projects").
		WithArgs(pq.Array([]string{"proj-1"})).
		WillReturnRows(sqlmock.NewRows(
			[]string{"project_id", "user_id", "project_name", "theme_key", "theme_version",
				"description", "owner_name", "owner_email", "created_at", "updated_at"}).
			AddRow("proj-1", "alice", "adlc", "adlc", "1.0", "", "Alice", "alice@example.com", now, now))

	// The structural sides. Bob is not the owner, so folders come from the
	// placements he can see: f1 only. Alice's other folders never enter the
	// query.
	mock.ExpectQuery("FROM +meko_system.project_folders").
		WithArgs(pq.Array([]string{"f1"}), 11).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "folder_id", "folder_name"}).
			AddRow(now, "f1", "perf_baselines"))
	mock.ExpectQuery("FROM +meko_system.datapack_placements p").
		WithArgs(pq.Array([]string{"shared-1", "shared-2"}), 11).
		WillReturnRows(sqlmock.NewRows(
			[]string{"updated_at", "datapack_id", "folder_name", "folder_id"}).
			AddRow(now, "shared-1", "perf_baselines", "f1"))

	if _, err := svc.ActivityLog("bob", "bob@x.com", "proj-1", 10, ""); err != nil {
		t.Fatalf("ActivityLog: %v", err)
	}

	// The folder side is the one that could leak an owner's structure past a
	// share, so assert it was handed the scoped set rather than trusting the
	// datapack assertion below to cover it.
	if activity.structural == nil {
		t.Fatal("structural sides missing: the project feed must carry them")
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

// The same project opened by its owner covers everything in it, so the test
// above is measuring scoping rather than a feed that is narrow for everyone.
func TestProjectActivityLog_OwnerSeesEveryDatapackInTheProject(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()
	activity := &capturingActivity{}
	svc.activity = activity

	projectDeleteMocks(mock, []string{"dp-a", "dp-b", "dp-c"})

	now := time.Now()
	// The owner's folder list is the whole project, including folders
	// holding nothing: an empty folder they created is still their event.
	mock.ExpectQuery("FROM +meko_system.project_folders\n\t\t WHERE +project_id").
		WithArgs("proj-1").
		WillReturnRows(sqlmock.NewRows(
			[]string{"folder_id", "project_id", "folder_name", "folder_description", "created_at", "updated_at"}).
			AddRow("f1", "proj-1", "standards", "", now, now).
			AddRow("f2", "proj-1", "empty_one", "", now, now))
	mock.ExpectQuery("FROM +meko_system.project_folders").
		WithArgs(pq.Array([]string{"f1", "f2"}), 11).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "folder_id", "folder_name"}).
			AddRow(now, "f2", "empty_one"))
	mock.ExpectQuery("FROM +meko_system.datapack_placements p").
		WithArgs(pq.Array([]string{"dp-a", "dp-b", "dp-c"}), 11).
		WillReturnRows(sqlmock.NewRows(
			[]string{"updated_at", "datapack_id", "folder_name", "folder_id"}).
			AddRow(now, "dp-a", "standards", "f1"))

	if _, err := svc.ActivityLog("me", "me@x.com", "proj-1", 10, ""); err != nil {
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
