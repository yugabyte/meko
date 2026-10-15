package service

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// A caller polling one file sees only these states, so the mapping is the whole
// contract: a live row must never read as terminal (it would stop polling an
// upload that is about to succeed) and a terminal one must carry the recorded
// reason, which is the only explanation it ever gets.
func TestPendingUploadState(t *testing.T) {
	tests := []struct {
		name      string
		wantState string
		wantMsg   string
		pending   repository.PendingUpload
	}{
		{
			name:      "awaiting the bytes",
			pending:   repository.PendingUpload{State: repository.PendingUploadAwaiting},
			wantState: docStateAwaitingUpload,
		},
		{
			// The watcher holding a lease is not a different answer for the
			// caller: still waiting.
			name:      "watcher is looking",
			pending:   repository.PendingUpload{State: repository.PendingUploadInProgress},
			wantState: docStateAwaitingUpload,
		},
		{
			// A live row's last_error is a transient S3 complaint, not the
			// outcome, so it must not be reported as the reason.
			name: "transient error while awaiting",
			pending: repository.PendingUpload{
				State:     repository.PendingUploadAwaiting,
				LastError: "connection reset by peer",
			},
			wantState: docStateAwaitingUpload,
		},
		{
			// The bytes already arrived; this is dist_rag's own worker not
			// having created the document yet, not the upload itself.
			name:      "handed off to dist_rag's own discovery",
			pending:   repository.PendingUpload{State: repository.PendingUploadAwaitingDiscovery},
			wantState: docStateAwaitingUpload,
			wantMsg:   "The file has arrived and is waiting to be picked up for indexing.",
		},
		{
			name: "rejected for its contents",
			pending: repository.PendingUpload{
				State:     repository.PendingUploadAbandoned,
				LastError: "file is not a PDF",
			},
			wantState: docStateAbandoned,
			wantMsg:   "file is not a PDF",
		},
		{
			name:      "expired with no reason recorded",
			pending:   repository.PendingUpload{State: repository.PendingUploadAbandoned},
			wantState: docStateAbandoned,
		},
		{
			name: "over its cap",
			pending: repository.PendingUpload{
				State:     repository.PendingUploadOversized,
				LastError: "object is 99 bytes, over the 10-byte cap it was authorized under",
			},
			wantState: docStateOversized,
			wantMsg:   "object is 99 bytes, over the 10-byte cap it was authorized under",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, message := pendingUploadState(tt.pending)
			if state != tt.wantState {
				t.Errorf("state = %q, want %q", state, tt.wantState)
			}
			if tt.wantMsg != "" && message != tt.wantMsg {
				t.Errorf("message = %q, want %q", message, tt.wantMsg)
			}
			if message == "" {
				t.Error("message is empty; every state has something to say")
			}
		})
	}
}

// Only COMPLETED and FAILED are terminal in dist_rag. NOT_STARTED and RETRY are
// the trap: an allow-list of QUEUED/PROCESSING would report them as finished.
func TestDocumentIndexingState(t *testing.T) {
	for status, want := range map[string]string{
		"COMPLETED":   docStateCompleted,
		"FAILED":      docStateFailed,
		"QUEUED":      docStateIndexing,
		"PROCESSING":  docStateIndexing,
		"NOT_STARTED": docStateIndexing,
		"RETRY":       docStateIndexing,
		"":            docStateIndexing,
	} {
		if got, _ := documentIndexingState(status); got != want {
			t.Errorf("documentIndexingState(%q) = %q, want %q", status, got, want)
		}
	}
}

// A dist_rag lookup that fails must surface as an error: reported as "not
// found", it becomes a 404 that a polling agent reads as "this upload is gone".
func TestDocumentStatus_LookupFailureIsAnError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	kbSvc := NewKnowledgeBaseService(repository.NewKnowledgeBaseRepo(db, db), "test-bucket")
	ragSvc := NewRagService(db, DatapackIndexSchema, DatapackIndexName)
	svc := NewPendingUploadService(repository.NewPendingUploadRepo(db), nil, ragSvc, kbSvc, nil)

	mock.ExpectQuery(`SELECT knowledge_base_url`).
		WillReturnRows(sqlmock.NewRows([]string{"knowledge_base_url"}).AddRow("s3://bucket/u/dp/"))
	mock.ExpectQuery(`FROM dist_rag.vector_index_pipeline_details`).
		WillReturnError(errors.New("connection reset"))

	_, found, err := svc.DocumentStatus(&models.Datapack{ID: "dp-1", UserID: "u"}, "u/dp/h/a.pdf")
	if err == nil || found {
		t.Fatalf("DocumentStatus = (found %v, err %v), want an error", found, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func newStatusService(t *testing.T) (*PendingUploadService, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	kbSvc := NewKnowledgeBaseService(repository.NewKnowledgeBaseRepo(db, db), "test-bucket")
	ragSvc := NewRagService(db, DatapackIndexSchema, DatapackIndexName)
	return NewPendingUploadService(repository.NewPendingUploadRepo(db), nil, ragSvc, kbSvc, nil), mock
}

// expectRegistered is one dist_rag read: the datapack's source URIs, then the
// pipeline row for the key — present (with status) or not.
func expectRegistered(m sqlmock.Sqlmock, status string) {
	m.ExpectQuery(`SELECT knowledge_base_url`).
		WillReturnRows(sqlmock.NewRows([]string{"knowledge_base_url"}).AddRow("s3://bucket/u/dp/"))
	rows := sqlmock.NewRows([]string{
		"schema_name", "index_name", "source_uri", "document_name", "raw_document_status",
		"document_status", "chunks_processed", "uploaded_at", "size_bytes",
	})
	if status != "" {
		rows.AddRow("datapacks", "knowledge_bases", "s3://bucket/u/dp/", "u/dp/h/a.pdf", status,
			status, nil, nil, int64(1164))
	}
	m.ExpectQuery(`FROM dist_rag.vector_index_pipeline_details`).WillReturnRows(rows)
}

func expectNoPendingRow(m sqlmock.Sqlmock) {
	m.ExpectQuery(`SELECT id, s3_key, datapack_id`).WillReturnError(sql.ErrNoRows)
}

// The watcher registers a document and then deletes its row. A poll whose
// dist_rag read ran before the registration and whose row read ran after the
// delete sees neither (seen live: a 404 for an upload that had just
// succeeded). The second dist_rag read must find the document.
func TestDocumentStatus_RegisteredBetweenTheTwoReads(t *testing.T) {
	svc, mock := newStatusService(t)
	expectRegistered(mock, "")       // before the watcher registers it
	expectNoPendingRow(mock)         // after the watcher deleted the row
	expectRegistered(mock, "QUEUED") // registered by now

	info, found, err := svc.DocumentStatus(&models.Datapack{ID: "dp-1", UserID: "u"}, "u/dp/h/a.pdf")
	if err != nil || !found {
		t.Fatalf("DocumentStatus = (found %v, err %v), want the registered document", found, err)
	}
	if info.State != docStateIndexing || info.Document == nil {
		t.Errorf("state = %q (document %v), want %q from dist_rag", info.State, info.Document != nil, docStateIndexing)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// A key nothing knows is still a 404 after the second look.
func TestDocumentStatus_UnknownKeyIsNotFound(t *testing.T) {
	svc, mock := newStatusService(t)
	expectRegistered(mock, "")
	expectNoPendingRow(mock)
	expectRegistered(mock, "")

	_, found, err := svc.DocumentStatus(&models.Datapack{ID: "dp-1", UserID: "u"}, "u/dp/h/nope.pdf")
	if err != nil || found {
		t.Fatalf("DocumentStatus = (found %v, err %v), want not found", found, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}
