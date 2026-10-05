package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// postCutoff is any timestamp after Mem0V2CutoffTS, so the date gate passes.
var postCutoff = Mem0V2CutoffTS.Add(24 * time.Hour)

func newMemoriesRepo(t *testing.T) (*MemoriesRepo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return &MemoriesRepo{db: db}, mock, func() { _ = db.Close() }
}

// A delete must take the memory row and its entity links in one transaction:
// entities left pointing at a deleted memory are the bug this guards.
func TestDeleteByIDForUser_PrunesEntityLinksInSameTx(t *testing.T) {
	repo, mock, done := newMemoriesRepo(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT meko_agent_id, meko_created_at FROM datapacks\.meko_mem0_collection`).
		WithArgs("mem-1", "dp-1", "user-1").
		WillReturnRows(sqlmock.NewRows([]string{"meko_agent_id", "meko_created_at"}).
			AddRow("ag-1", postCutoff))
	mock.ExpectExec(`DELETE FROM datapacks\.meko_mem0_collection`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Entities whose only link was this memory.
	mock.ExpectExec(`DELETE FROM datapacks\.meko_mem0_collection_entities`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Survivors keep the row and lose just this id.
	mock.ExpectExec(`UPDATE datapacks\.meko_mem0_collection_entities`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	ok, err := repo.DeleteByIDForUser(context.Background(), "mem-1", "dp-1", "user-1")
	if err != nil {
		t.Fatalf("DeleteByIDForUser: %v", err)
	}
	if !ok {
		t.Fatal("expected deleted=true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Both entity statements must carry the full partition key. The table is
// hash-partitioned on (datapack, user, agent); with fewer, YB opens all 128.
func TestPruneEntityLinks_ScopedToFullPartitionKey(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	scoped := `meko_datapack_id = \$1::uuid\s+AND meko_user_id\s+= \$2::uuid\s+AND meko_agent_id\s+= \$3`
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM datapacks\.meko_mem0_collection_entities\s+WHERE ` + scoped).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)UPDATE datapacks\.meko_mem0_collection_entities.*WHERE ` + scoped).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := pruneEntityLinks(context.Background(), tx, "dp-1", "user-1", "ag-1",
		[]string{"mem-1"}); err != nil {
		t.Fatalf("pruneEntityLinks: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// An empty id list must touch nothing rather than emit a predicate that
// matches every entity in the tenant.
func TestPruneEntityLinks_NoMemoryIDsIsNoOp(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectCommit()

	tx, _ := db.Begin()
	if err := pruneEntityLinks(context.Background(), tx, "dp-1", "user-1", "ag-1", nil); err != nil {
		t.Fatalf("pruneEntityLinks: %v", err)
	}
	_ = tx.Commit()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A pre-cutoff row is refused before anything is written, and the tx rolls back.
func TestDeleteByIDForUser_PreCutoffRollsBack(t *testing.T) {
	repo, mock, done := newMemoriesRepo(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT meko_agent_id, meko_created_at FROM datapacks\.meko_mem0_collection`).
		WithArgs("mem-1", "dp-1", "user-1").
		WillReturnRows(sqlmock.NewRows([]string{"meko_agent_id", "meko_created_at"}).
			AddRow("ag-1", Mem0V2CutoffTS.Add(-time.Hour)))
	mock.ExpectRollback()

	_, err := repo.DeleteByIDForUser(context.Background(), "mem-1", "dp-1", "user-1")
	if err != ErrPreMem0V2Memory {
		t.Fatalf("expected ErrPreMem0V2Memory, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A miss (wrong owner, or already deleted) writes nothing and reports 404.
func TestDeleteByIDForUser_NotFoundSkipsEntityWork(t *testing.T) {
	repo, mock, done := newMemoriesRepo(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT meko_agent_id, meko_created_at FROM datapacks\.meko_mem0_collection`).
		WithArgs("mem-1", "dp-1", "other-user").
		WillReturnRows(sqlmock.NewRows([]string{"meko_agent_id", "meko_created_at"}))
	mock.ExpectRollback()

	ok, err := repo.DeleteByIDForUser(context.Background(), "mem-1", "dp-1", "other-user")
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
