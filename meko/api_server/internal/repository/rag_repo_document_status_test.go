package repository

import (
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// GetDocumentStatus carries the old PipelineDetail along with the narrower
// progress, so document-status can keep returning the fields it used to.
func TestRagRepo_GetDocumentStatus_FillsLegacyDetail(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM dist_rag.vector_index_pipeline_details`).
		WillReturnRows(sqlmock.NewRows([]string{
			"schema_name", "index_name", "source_uri", "document_name", "raw_document_status",
			"document_status", "chunks_processed", "uploaded_at", "size_bytes",
		}).AddRow("datapacks", "knowledge_bases", "s3://b/u/dp/", "u/dp/h/a.pdf", "COMPLETED",
			"COMPLETED", 4, time.Now(), int64(2048)))

	d, err := NewRagRepo(db, "datapacks", "knowledge_bases").GetDocumentStatus([]string{"s3://b/u/dp/"}, "u/dp/h/a.pdf")
	if err != nil {
		t.Fatalf("GetDocumentStatus: %v", err)
	}
	if d == nil {
		t.Fatal("GetDocumentStatus: got nil")
	}
	if d.Legacy.IndexName != "datapacks.knowledge_bases" || d.Legacy.SourceURI != "s3://b/u/dp/" ||
		d.Legacy.DocumentName != "u/dp/h/a.pdf" || d.Legacy.DocumentStatus != "COMPLETED" {
		t.Errorf("legacy detail = %+v", d.Legacy)
	}
	if d.Legacy.ChunksProcessed == nil || *d.Legacy.ChunksProcessed != 4 {
		t.Errorf("legacy chunks_processed = %v, want 4", d.Legacy.ChunksProcessed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}
