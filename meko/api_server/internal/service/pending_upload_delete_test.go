package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// newDeleteGuardService wires a watcher whose S3 is an httptest server counting
// DeleteObject calls, and whose registration lookups go to sqlmock.
func newDeleteGuardService(t *testing.T) (*PendingUploadService, sqlmock.Sqlmock, *int32) {
	t.Helper()
	var deletes int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&deletes, 1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("k", "s", ""),
		UsePathStyle: true,
		BaseEndpoint: aws.String(srv.URL),
	})
	s3Svc := &S3Service{client: client, bucket: "test-bucket"}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	kbSvc := NewKnowledgeBaseService(repository.NewKnowledgeBaseRepo(db, db), "test-bucket")

	return NewPendingUploadService(repository.NewPendingUploadRepo(db), s3Svc, nil, kbSvc, nil), mock, &deletes
}

func TestPendingUploadService_DeleteObject_Guard(t *testing.T) {
	const key = "user-1/dp-1/abc/notes.txt"
	dp := &models.Datapack{ID: "dp-1", UserID: "user-1"}

	cases := []struct {
		expect      func(sqlmock.Sqlmock)
		name        string
		wantDeletes int32
	}{
		{
			name: "unregistered key is deleted",
			expect: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(`FROM meko_system.knowledge_bases`).
					WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).AddRow("src-1"))
				m.ExpectQuery(`FROM\s+dist_rag.documents`).
					WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
			},
			wantDeletes: 1,
		},
		{
			// A client's upload-complete/create registered the key after
			// resolve's own check: deleting now would strand the document.
			name: "key registered since resolve's check is kept",
			expect: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(`FROM meko_system.knowledge_bases`).
					WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).AddRow("src-1"))
				m.ExpectQuery(`FROM\s+dist_rag.documents`).
					WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
			},
			wantDeletes: 0,
		},
		{
			name: "lookup failure keeps the object",
			expect: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(`FROM meko_system.knowledge_bases`).
					WillReturnError(errors.New("connection reset"))
			},
			wantDeletes: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, mock, deletes := newDeleteGuardService(t)
			tc.expect(mock)

			svc.deleteObject(context.Background(), dp, key)

			if got := atomic.LoadInt32(deletes); got != tc.wantDeletes {
				t.Errorf("DeleteObject calls = %d, want %d", got, tc.wantDeletes)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("sql expectations: %v", err)
			}
		})
	}
}
