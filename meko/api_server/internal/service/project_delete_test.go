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

// recordingDeleter stands in for DatapackService, so these tests assert the
// ORDER and the SET of datapacks a project delete destroys without needing
// Langfuse, AMP or a universe.
type recordingDeleter struct {
	failOn  string
	deleted []string
}

func (d *recordingDeleter) Delete(_, datapackID string) error {
	if datapackID == d.failOn {
		return errors.New("teardown exploded")
	}
	d.deleted = append(d.deleted, datapackID)
	return nil
}

// projectDeleteMocks sets up the three reads scopeFor performs: the project,
// the caller's datapacks, and their placements.
func projectDeleteMocks(mock sqlmock.Sqlmock, dpIDs []string) {
	now := time.Now()
	mock.ExpectQuery("FROM +meko_system.projects").
		WithArgs("proj-1", "me").
		WillReturnRows(sqlmock.NewRows(
			[]string{"project_id", "user_id", "project_name", "theme_key", "theme_version",
				"description", "created_at", "updated_at"}).
			AddRow("proj-1", "me", "adlc", "adlc", "1.0", "", now, now))

	dpRows := sqlmock.NewRows([]string{
		"datapack_id", "user_id", "datapack_name", "created_by_agent_id", "universe_id",
		"conn", "mcp_user", "created_at", "updated_at", "description",
		"owner_name", "owner_email", "grant",
	})
	for _, id := range dpIDs {
		dpRows.AddRow(id, "me", id, "", "u", "", "", now, now, "", "", "", "owner")
	}
	mock.ExpectQuery("FROM meko_system.datapacks").WithArgs("me").WillReturnRows(dpRows)

	plRows := sqlmock.NewRows([]string{"datapack_id", "project_id", "folder_id", "placed_at", "updated_at"})
	for _, id := range dpIDs {
		plRows.AddRow(id, "proj-1", "f1", now, now)
	}
	mock.ExpectQuery("FROM +meko_system.datapack_placements").
		WithArgs(pq.Array(dpIDs)).
		WillReturnRows(plRows)
}

// Deleting a project destroys the datapacks filed in it. This is the most
// destructive call in the feature, so the set is asserted explicitly.
func TestDeleteProject_DeletesTheDatapacksInsideThenTheProject(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()
	deleter := &recordingDeleter{}
	svc.datapackDeleter = deleter

	projectDeleteMocks(mock, []string{"dp-a", "dp-b"})
	mock.ExpectExec("DELETE FROM meko_system.projects").
		WithArgs("proj-1", "me").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := svc.Delete("me", "proj-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(deleter.deleted) != 2 {
		t.Fatalf("deleted %v, want both datapacks", deleter.deleted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// If a datapack cannot be torn down, the project must survive. Deleting it
// anyway would leave the user with orphaned datapacks and no handle on them.
func TestDeleteProject_KeepsTheProjectWhenADatapackFails(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()
	svc.datapackDeleter = &recordingDeleter{failOn: "dp-a"}

	projectDeleteMocks(mock, []string{"dp-a"})
	// No DELETE expectation: the project row must not be touched.

	if err := svc.Delete("me", "proj-1"); err == nil {
		t.Fatal("expected the failure to propagate")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the project was deleted despite a failed datapack: %v", err)
	}
}

// A project shared with you is visible but not yours to delete.
func TestDeleteProject_RecipientCannotDelete(t *testing.T) {
	svc, mock, db := newProjectService(t)
	defer db.Close()
	deleter := &recordingDeleter{}
	svc.datapackDeleter = deleter

	now := time.Now()
	// Not the owner: Get finds nothing.
	mock.ExpectQuery("FROM +meko_system.projects").
		WithArgs("proj-1", "me").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("FROM meko_system.datapacks").
		WithArgs("me").
		WillReturnRows(sqlmock.NewRows([]string{
			"datapack_id", "user_id", "datapack_name", "created_by_agent_id", "universe_id",
			"conn", "mcp_user", "created_at", "updated_at", "description",
			"owner_name", "owner_email", "grant",
		}).AddRow("dp-a", "someone-else", "dp", "", "u", "", "", now, now, "", "", "", "read"))
	mock.ExpectQuery("FROM +meko_system.datapack_placements").
		WithArgs(pq.Array([]string{"dp-a"})).
		WillReturnRows(sqlmock.NewRows(
			[]string{"datapack_id", "project_id", "folder_id", "placed_at", "updated_at"}).
			AddRow("dp-a", "proj-1", "f1", now, now))
	mock.ExpectQuery("FROM +meko_system.projects").
		WithArgs(pq.Array([]string{"proj-1"})).
		WillReturnRows(sqlmock.NewRows(
			[]string{"project_id", "user_id", "project_name", "theme_key", "theme_version",
				"description", "owner_name", "created_at", "updated_at"}).
			AddRow("proj-1", "someone-else", "adlc", "adlc", "1.0", "", "Alice", now, now))

	err := svc.Delete("me", "proj-1")
	if !errors.Is(err, repository.ErrProjectNotFound) {
		t.Fatalf("expected ErrProjectNotFound, got %v", err)
	}
	if len(deleter.deleted) != 0 {
		t.Errorf("deleted %v; a recipient must destroy nothing", deleter.deleted)
	}
}
