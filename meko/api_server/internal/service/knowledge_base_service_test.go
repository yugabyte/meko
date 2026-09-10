package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// expectGetDocumentIDForURI arms the two queries GetDocumentIDForURI issues,
// resolving documentURI to documentID.
func expectGetDocumentIDForURI(mock sqlmock.Sqlmock, documentID string) {
	mock.ExpectQuery(`SELECT rag_source_id\s+FROM meko_system\.knowledge_bases`).
		WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).AddRow("rag-1"))
	mock.ExpectQuery(`SELECT document_id\s+FROM\s+dist_rag\.documents`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"document_id"}).AddRow(documentID))
}

// expectGetDocumentRef arms the two queries GetDocumentRef issues, reporting
// the given pipeline status for documentID.
func expectGetDocumentRef(mock sqlmock.Sqlmock, status string) {
	mock.ExpectQuery(`SELECT rag_source_id\s+FROM meko_system\.knowledge_bases`).
		WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).AddRow("rag-1"))
	mock.ExpectQuery(`SELECT d\.document_details`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "document_uri", "status"}).
			AddRow(nil, "s3://bucket/dp-1/hash/f.pdf", status))
}

// expectDocumentExistsForURI arms the two queries DocumentExistsForURI issues
// (source ids, then the dist_rag lookup) to report tracked/untracked.
func expectDocumentExistsForURI(mock sqlmock.Sqlmock, tracked bool) {
	mock.ExpectQuery(`SELECT rag_source_id\s+FROM meko_system\.knowledge_bases`).
		WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).AddRow("rag-1"))
	mock.ExpectQuery(`SELECT EXISTS \(\s+SELECT 1\s+FROM\s+dist_rag\.documents`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(tracked))
}

// S3KeyIsSafeToDelete's callers all switch on this: a live dist_rag
// document is the only thing that makes a key unsafe to delete. Artifacts
// don't need checking — UploadArtifactObject writes under a disjoint
// "artifacts/" prefix.
func TestKnowledgeBaseService_S3KeyIsSafeToDelete_DocumentTracked_NotSafe(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	expectDocumentExistsForURI(mock, true)

	svc := service.NewKnowledgeBaseService(repository.NewKnowledgeBaseRepo(db, db), "bucket")

	safe, err := svc.S3KeyIsSafeToDelete(&models.Datapack{ID: "dp-1"}, "u/dp-1/hash/f.pdf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if safe {
		t.Error("a key backing a registered document must not be reported safe to delete")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseService_S3KeyIsSafeToDelete_NotTracked_Safe(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	expectDocumentExistsForURI(mock, false)

	svc := service.NewKnowledgeBaseService(repository.NewKnowledgeBaseRepo(db, db), "bucket")

	safe, err := svc.S3KeyIsSafeToDelete(&models.Datapack{ID: "dp-1"}, "u/dp-1/hash/f.pdf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !safe {
		t.Error("a key referenced by no document should be safe to delete")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A lookup failure must fail closed ("not safe") — the delete this guards
// can't be undone.
func TestKnowledgeBaseService_S3KeyIsSafeToDelete_LookupErrors_NotSafe(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT rag_source_id\s+FROM meko_system\.knowledge_bases`).
		WithArgs("dp-1").
		WillReturnError(context.DeadlineExceeded)

	svc := service.NewKnowledgeBaseService(repository.NewKnowledgeBaseRepo(db, db), "bucket")

	safe, err := svc.S3KeyIsSafeToDelete(&models.Datapack{ID: "dp-1"}, "u/dp-1/hash/f.pdf")
	if err == nil {
		t.Fatal("expected an error to propagate")
	}
	if safe {
		t.Error("a lookup failure must not be reported safe to delete")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// DeleteDocumentByURI must refuse a document whose pipeline hasn't reached a
// terminal status yet -- the rag_agent worker chunking it holds its own
// work_queue task and keeps writing chunks after a mid-flight delete, same
// as the bug DeleteDocument's handler-level guard exists to prevent.
func TestKnowledgeBaseService_DeleteDocumentByURI_InFlight_ReturnsErrDocumentInFlight(t *testing.T) {
	for _, status := range []string{"NOT_STARTED", "QUEUED", "PROCESSING", "RETRY"} {
		t.Run(status, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()
			expectGetDocumentIDForURI(mock, "doc-1")
			expectGetDocumentRef(mock, status)

			svc := service.NewKnowledgeBaseService(repository.NewKnowledgeBaseRepo(db, db), "bucket")
			found, err := svc.DeleteDocumentByURI(&models.Datapack{ID: "dp-1"}, "s3://bucket/dp-1/hash/f.pdf")
			if !errors.Is(err, service.ErrDocumentInFlight) {
				t.Fatalf("expected ErrDocumentInFlight for status %s, got %v", status, err)
			}
			if found {
				t.Error("in-flight document must not be reported as deleted")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet expectations: %v", err)
			}
		})
	}
}

// No document row at the given URI is not an error — not every caller's key
// necessarily has one registered.
func TestKnowledgeBaseService_DeleteDocumentByURI_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT rag_source_id\s+FROM meko_system\.knowledge_bases`).
		WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}))

	svc := service.NewKnowledgeBaseService(repository.NewKnowledgeBaseRepo(db, db), "bucket")
	found, err := svc.DeleteDocumentByURI(&models.Datapack{ID: "dp-1"}, "s3://bucket/dp-1/hash/missing.pdf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Error("expected found=false for a URI with no document row")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
