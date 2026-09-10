package repository

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

func TestKnowledgeBaseRepo_Insert_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO meko_system\.knowledge_bases`).
		WithArgs("dp-1", "s3://bucket/u/dp-1/", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).
			AddRow("rag-uuid-1"))

	repo := NewKnowledgeBaseRepo(db, db)
	id, err := repo.Insert(
		"dp-1", "s3://bucket/u/dp-1/", json.RawMessage(`{"k":"v"}`),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "rag-uuid-1" {
		t.Errorf("expected rag-uuid-1, got %q", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_Insert_DefaultDetails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO meko_system\.knowledge_bases`).
		WithArgs("dp-1", "s3://bucket/", []byte(`{}`)).
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).
			AddRow("rag-uuid-2"))

	repo := NewKnowledgeBaseRepo(db, db)
	if _, err := repo.Insert("dp-1", "s3://bucket/", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_Insert_ConflictReturnsErrExists(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO meko_system\.knowledge_bases`).
		WithArgs("dp-1", "s3://bucket/", sqlmock.AnyArg()).
		WillReturnError(&pq.Error{Code: "23505"})

	repo := NewKnowledgeBaseRepo(db, db)
	_, err = repo.Insert("dp-1", "s3://bucket/", nil)
	if err == nil {
		t.Fatal("expected ErrKnowledgeBaseExists, got nil")
	}
	if !errors.Is(err, ErrKnowledgeBaseExists) {
		t.Errorf("expected ErrKnowledgeBaseExists, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_InsertWithID_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO meko_system\.knowledge_bases`).
		WithArgs("dp-1", "rag-1", "s3://bucket/", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewKnowledgeBaseRepo(db, db)
	if err := repo.InsertWithID(
		"dp-1", "rag-1", "s3://bucket/", json.RawMessage(`{}`),
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_InsertWithID_Conflict(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO meko_system\.knowledge_bases`).
		WithArgs("dp-1", "rag-1", "s3://bucket/", sqlmock.AnyArg()).
		WillReturnError(&pq.Error{Code: "23505"})

	repo := NewKnowledgeBaseRepo(db, db)
	err = repo.InsertWithID("dp-1", "rag-1", "s3://bucket/", nil)
	if !errors.Is(err, ErrKnowledgeBaseExists) {
		t.Errorf("expected ErrKnowledgeBaseExists, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_Get_Found(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	now := time.Now()
	mock.ExpectQuery(`FROM meko_system\.knowledge_bases\s+WHERE datapack_id`).
		WithArgs("dp-1", "rag-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"datapack_id", "rag_source_id", "knowledge_base_url",
			"knowledge_base_details", "created_at", "updated_at",
		}).AddRow(
			"dp-1", "rag-1", "s3://bucket/u/dp-1/",
			`{"k":"v"}`, now, now,
		))

	repo := NewKnowledgeBaseRepo(db, db)
	kb, err := repo.Get("dp-1", "rag-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kb == nil || kb.RagSourceID != "rag-1" {
		t.Fatalf("got %+v", kb)
	}
	if string(kb.Details) != `{"k":"v"}` {
		t.Errorf("expected details to round-trip, got %s", kb.Details)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_Get_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM meko_system\.knowledge_bases\s+WHERE datapack_id`).
		WithArgs("dp-1", "missing").
		WillReturnRows(sqlmock.NewRows([]string{
			"datapack_id", "rag_source_id", "knowledge_base_url",
			"knowledge_base_details", "created_at", "updated_at",
		}))

	repo := NewKnowledgeBaseRepo(db, db)
	_, err = repo.Get("dp-1", "missing")
	if !errors.Is(err, ErrKnowledgeBaseNotFound) {
		t.Errorf("expected ErrKnowledgeBaseNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_ListByDatapack(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	now := time.Now()
	mock.ExpectQuery(`FROM meko_system\.knowledge_bases\s+WHERE datapack_id`).
		WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"datapack_id", "rag_source_id", "knowledge_base_url",
			"knowledge_base_details", "created_at", "updated_at",
		}).
			AddRow("dp-1", "rag-1", "s3://b/u/dp-1/", `{}`, now, now).
			AddRow("dp-1", "rag-2", "s3://b/u/dp-1/", `{}`, now, now))

	repo := NewKnowledgeBaseRepo(db, db)
	kbs, err := repo.ListByDatapack("dp-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(kbs) != 2 {
		t.Errorf("expected 2 rows, got %d", len(kbs))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_LookupByURL_Found(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	now := time.Now()
	mock.ExpectQuery(`FROM meko_system\.knowledge_bases`).
		WithArgs("dp-1", "s3://b/u/dp-1/").
		WillReturnRows(sqlmock.NewRows([]string{
			"datapack_id", "rag_source_id", "knowledge_base_url",
			"knowledge_base_details", "created_at", "updated_at",
		}).AddRow(
			"dp-1", "rag-1", "s3://b/u/dp-1/",
			`{}`, now, now,
		))

	repo := NewKnowledgeBaseRepo(db, db)
	kb, err := repo.LookupByURL("dp-1", "s3://b/u/dp-1/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kb == nil {
		t.Fatal("expected non-nil kb")
	}
	if kb.RagSourceID != "rag-1" {
		t.Errorf("expected rag-1, got %q", kb.RagSourceID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_LookupByURL_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM meko_system\.knowledge_bases`).
		WithArgs("dp-1", "s3://missing/").
		WillReturnRows(sqlmock.NewRows([]string{
			"datapack_id", "rag_source_id", "knowledge_base_url",
			"knowledge_base_details", "created_at", "updated_at",
		}))

	repo := NewKnowledgeBaseRepo(db, db)
	kb, err := repo.LookupByURL("dp-1", "s3://missing/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kb != nil {
		t.Errorf("expected nil, got %+v", kb)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_Delete_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`DELETE FROM meko_system\.knowledge_bases`).
		WithArgs("dp-1", "rag-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewKnowledgeBaseRepo(db, db)
	if err := repo.Delete("dp-1", "rag-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_Delete_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`DELETE FROM meko_system\.knowledge_bases`).
		WithArgs("dp-1", "ghost").
		WillReturnResult(sqlmock.NewResult(0, 0))

	repo := NewKnowledgeBaseRepo(db, db)
	err = repo.Delete("dp-1", "ghost")
	if !errors.Is(err, ErrKnowledgeBaseNotFound) {
		t.Errorf("expected ErrKnowledgeBaseNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_DeleteByDatapack(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`DELETE FROM meko_system\.knowledge_bases\s+WHERE datapack_id`).
		WithArgs("dp-1").
		WillReturnResult(sqlmock.NewResult(0, 3))

	repo := NewKnowledgeBaseRepo(db, db)
	if err := repo.DeleteByDatapack("dp-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_GetSourceIDs(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT rag_source_id\s+FROM meko_system\.knowledge_bases`).
		WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).
			AddRow("rag-1").
			AddRow("rag-2"))

	repo := NewKnowledgeBaseRepo(db, db)
	ids, err := repo.GetSourceIDs("dp-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 2 || ids[0] != "rag-1" {
		t.Errorf("expected [rag-1, rag-2], got %v", ids)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_DocumentExistsForURI(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT rag_source_id\s+FROM meko_system\.knowledge_bases`).
		WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).
			AddRow("rag-1").
			AddRow("rag-2"))
	// Both sources are searched in one query, so a document registered under
	// the older one still counts as tracked.
	mock.ExpectQuery(`SELECT EXISTS \(\s+SELECT 1\s+FROM\s+dist_rag\.documents`).
		WithArgs(sqlmock.AnyArg(), "s3://bucket/dp-1/file.pdf").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	repo := NewKnowledgeBaseRepo(db, db)
	exists, err := repo.DocumentExistsForURI("dp-1", "s3://bucket/dp-1/file.pdf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Error("expected the document to be reported as registered")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A datapack with no sources can hold no documents, so the dist_rag query is
// skipped entirely -- sqlmock fails the test if it is issued.
func TestKnowledgeBaseRepo_DocumentExistsForURI_NoSources(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT rag_source_id\s+FROM meko_system\.knowledge_bases`).
		WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}))

	repo := NewKnowledgeBaseRepo(db, db)
	exists, err := repo.DocumentExistsForURI("dp-1", "s3://bucket/dp-1/file.pdf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Error("expected no document for a datapack with no sources")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_GetDocumentIDForURI_Found(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT rag_source_id\s+FROM meko_system\.knowledge_bases`).
		WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).AddRow("rag-1"))
	mock.ExpectQuery(`SELECT document_id\s+FROM\s+dist_rag\.documents`).
		WithArgs(sqlmock.AnyArg(), "s3://bucket/dp-1/file.pdf").
		WillReturnRows(sqlmock.NewRows([]string{"document_id"}).AddRow("doc-1"))

	repo := NewKnowledgeBaseRepo(db, db)
	id, ok, err := repo.GetDocumentIDForURI("dp-1", "s3://bucket/dp-1/file.pdf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || id != "doc-1" {
		t.Errorf("expected (doc-1, true), got (%q, %v)", id, ok)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_GetDocumentIDForURI_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT rag_source_id\s+FROM meko_system\.knowledge_bases`).
		WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).AddRow("rag-1"))
	mock.ExpectQuery(`SELECT document_id\s+FROM\s+dist_rag\.documents`).
		WithArgs(sqlmock.AnyArg(), "s3://bucket/dp-1/missing.pdf").
		WillReturnRows(sqlmock.NewRows([]string{"document_id"}))

	repo := NewKnowledgeBaseRepo(db, db)
	id, ok, err := repo.GetDocumentIDForURI("dp-1", "s3://bucket/dp-1/missing.pdf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok || id != "" {
		t.Errorf("expected (\"\", false), got (%q, %v)", id, ok)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_GetSourceURIs(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT knowledge_base_url\s+FROM meko_system\.knowledge_bases`).
		WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"knowledge_base_url"}).
			AddRow("s3://b/u/dp-1/").
			AddRow("s3://b/u/dp-1/v2/"))

	repo := NewKnowledgeBaseRepo(db, db)
	uris, err := repo.GetSourceURIs("dp-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(uris) != 2 {
		t.Errorf("expected 2 URIs, got %d", len(uris))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_Upsert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO meko_system\.knowledge_bases`).
		WithArgs("dp-1", "rag-1", "s3://b/u/dp-1/", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewKnowledgeBaseRepo(db, db)
	if err := repo.Upsert(
		"dp-1", "rag-1", "s3://b/u/dp-1/", json.RawMessage(`{"v":1}`),
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
