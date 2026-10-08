package repository

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestRagRepo_QueueSourceDocuments_CallsQueueFunctionWithIndexID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM dist_rag\.vector_indexes`).
		WithArgs("datapacks", "knowledge_bases").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("index-1"))
	mock.ExpectExec(`SELECT dist_rag\._queue_source_documents\(r_source_id := \$1::UUID, r_index_id := \$2::UUID\)`).
		WithArgs("source-1", "index-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := NewRagRepo(db, "datapacks", "knowledge_bases").QueueSourceDocuments("source-1"); err != nil {
		t.Fatalf("QueueSourceDocuments: unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
