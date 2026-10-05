package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newPromoteRepo(t *testing.T) (*PromoteMemoriesRepo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return &PromoteMemoriesRepo{db: db}, mock, func() { _ = db.Close() }
}

func promotedRow(userID, agentID string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"meko_user_id", "meko_agent_id", "meko_created_at"}).
		AddRow(userID, agentID, Mem0V2CutoffTS.Add(24*time.Hour))
}

// Deleting a promoted memory is the first point the memory stops existing —
// promotion moved it here keeping the same id — so this is where its entities
// are reaped.
func TestDeleteByIDForDatapack_PrunesEntityLinks(t *testing.T) {
	repo, mock, done := newPromoteRepo(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT meko_user_id::text, meko_agent_id, meko_created_at`).
		WithArgs("mem-1", "dp-1").
		WillReturnRows(promotedRow("user-1", "ag-1"))
	mock.ExpectExec(`DELETE FROM datapacks\.promoted_collection`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM datapacks\.knowledge_bases`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM datapacks\.meko_mem0_collection_entities`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE datapacks\.meko_mem0_collection_entities`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	ok, err := repo.DeleteByIDForDatapack(context.Background(), "mem-1", "dp-1")
	if err != nil {
		t.Fatalf("DeleteByIDForDatapack: %v", err)
	}
	if !ok {
		t.Fatal("expected deleted=true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// meko_user_id/meko_agent_id are nullable on promoted_collection but NOT NULL
// on the entities table. An empty one cannot scope the prune, so the delete
// still commits and the skip is logged rather than running a predicate that
// silently matches nothing.
func TestDeleteByIDForDatapack_SkipsPruneOnIncompleteScope(t *testing.T) {
	for _, tc := range []struct{ name, userID, agentID string }{
		{"empty agent", "user-1", ""},
		{"empty user", "", "ag-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, mock, done := newPromoteRepo(t)
			defer done()

			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT meko_user_id::text, meko_agent_id, meko_created_at`).
				WithArgs("mem-1", "dp-1").
				WillReturnRows(promotedRow(tc.userID, tc.agentID))
			mock.ExpectExec(`DELETE FROM datapacks\.promoted_collection`).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`DELETE FROM datapacks\.knowledge_bases`).
				WillReturnResult(sqlmock.NewResult(0, 1))
			// No entity statements.
			mock.ExpectCommit()

			ok, err := repo.DeleteByIDForDatapack(context.Background(), "mem-1", "dp-1")
			if err != nil {
				t.Fatalf("DeleteByIDForDatapack: %v", err)
			}
			if !ok {
				t.Fatal("expected deleted=true: the delete must still succeed")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet expectations: %v", err)
			}
		})
	}
}

// A miss touches neither the KB nor the entity store.
func TestDeleteByIDForDatapack_NotFoundSkipsEntityWork(t *testing.T) {
	repo, mock, done := newPromoteRepo(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT meko_user_id::text, meko_agent_id, meko_created_at`).
		WithArgs("mem-1", "dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"meko_user_id", "meko_agent_id", "meko_created_at"}))
	mock.ExpectRollback()

	ok, err := repo.DeleteByIDForDatapack(context.Background(), "mem-1", "dp-1")
	if err != nil {
		t.Fatalf("expected nil error for 404, got %v", err)
	}
	if ok {
		t.Fatal("expected deleted=false")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A pre-cutoff row is refused before anything is written.
func TestDeleteByIDForDatapack_PreCutoffRollsBack(t *testing.T) {
	repo, mock, done := newPromoteRepo(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT meko_user_id::text, meko_agent_id, meko_created_at`).
		WithArgs("mem-1", "dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"meko_user_id", "meko_agent_id", "meko_created_at"}).
			AddRow("user-1", "ag-1", Mem0V2CutoffTS.Add(-time.Hour)))
	mock.ExpectRollback()

	_, err := repo.DeleteByIDForDatapack(context.Background(), "mem-1", "dp-1")
	if err != ErrPreMem0V2Memory {
		t.Fatalf("expected ErrPreMem0V2Memory, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
