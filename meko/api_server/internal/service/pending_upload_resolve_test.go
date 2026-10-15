package service

import (
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// fakeObject is what the fake bucket serves for the one key a test resolves.
// A nil body is a missing object; status overrides HEAD's answer.
type fakeObject struct {
	// checksum is what HEAD reports as x-amz-checksum-sha256; defaults to the
	// body's real checksum.
	checksum string
	// listKey, when set, is an object already stored under the presigned
	// content's hash prefix, so the dedup lookup finds it.
	listKey    string
	body       []byte
	headStatus int
	getStatus  int
	// noChecksum makes HEAD return no checksum, as a backend that doesn't
	// store one would.
	noChecksum bool
}

// newResolveHarness wires a watcher against a fake S3 bucket holding obj and a
// sqlmock database standing in for both meko_system and dist_rag. It returns
// the service, the mock, and a counter of DeleteObject calls.
func newResolveHarness(t *testing.T, obj fakeObject) (*PendingUploadService, sqlmock.Sqlmock, *int32) {
	t.Helper()
	var deletes int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			atomic.AddInt32(&deletes, 1)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodHead:
			if obj.headStatus != 0 {
				w.WriteHeader(obj.headStatus)
				return
			}
			if obj.body == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			sum := obj.checksum
			if sum == "" {
				h := sha256.Sum256(obj.body)
				sum = base64.StdEncoding.EncodeToString(h[:])
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(obj.body)))
			w.Header().Set("ETag", `"etag-1"`)
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			if !obj.noChecksum {
				w.Header().Set("x-amz-checksum-sha256", sum)
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if r.URL.Query().Get("list-type") == "2" {
				// PresignUpload's dedup lookup.
				listing := `<ListBucketResult><KeyCount>0</KeyCount></ListBucketResult>`
				if obj.listKey != "" {
					listing = `<ListBucketResult><KeyCount>1</KeyCount><Contents><Key>` +
						obj.listKey + `</Key></Contents></ListBucketResult>`
				}
				w.Header().Set("Content-Type", "application/xml")
				if _, err := w.Write([]byte(listing)); err != nil {
					t.Error(err)
				}
				return
			}
			if obj.getStatus != 0 {
				w.WriteHeader(obj.getStatus)
				return
			}
			head := obj.body
			if len(head) > MIMESniffLen {
				head = head[:MIMESniffLen]
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(head)))
			w.WriteHeader(http.StatusPartialContent)
			if _, err := w.Write(head); err != nil {
				t.Error(err)
			}
		}
	}))
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("k", "s", ""),
		UsePathStyle: true,
		BaseEndpoint: aws.String(srv.URL),
		// The fake bucket never sends response checksums to validate, and a
		// 500 should reach resolve on the first try rather than after retries.
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		RetryMaxAttempts:           1,
	})
	s3Svc := &S3Service{client: client, presignClient: s3.NewPresignClient(client), bucket: "test-bucket"}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	kbSvc := NewKnowledgeBaseService(repository.NewKnowledgeBaseRepo(db, db), "test-bucket")
	ragSvc := NewRagService(db, DatapackIndexSchema, DatapackIndexName)
	svc := NewPendingUploadService(
		repository.NewPendingUploadRepo(db), s3Svc, ragSvc, kbSvc, NewSourceResolver(kbSvc, ragSvc))
	return svc, mock, &deletes
}

// claimedRow is a row as Claim hands it to resolve for "notes.txt": signed
// for body's exact length and keyed by body's real hash, in the given
// pre-claim state.
func claimedRow(body []byte, state string) repository.PendingUpload {
	const filename = "notes.txt"
	h := sha256.Sum256(body)
	return repository.PendingUpload{
		ID:             "pu-1",
		S3Key:          "owner-1/dp-1/" + hex.EncodeToString(h[:]) + "/" + filename,
		DatapackID:     "dp-1",
		OwnerUserID:    "owner-1",
		UploaderID:     "uploader-1",
		Filename:       filename,
		State:          state,
		DeclaredSize:   int64(len(body)),
		MaxSizeBytes:   5 << 20,
		Attempts:       1,
		ExpiresAt:      time.Now().Add(10 * time.Minute),
		LeaseExpiresAt: time.Now().Add(2 * time.Minute),
	}
}

var textBody = []byte("meeting notes: ship the watcher\n")

// Each expectation below names one statement resolve issues, in order.

func expectNotRegistered(m sqlmock.Sqlmock) {
	m.ExpectQuery(`SELECT rag_source_id\s+FROM meko_system.knowledge_bases`).
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).AddRow("src-1"))
	m.ExpectQuery(`FROM\s+dist_rag.documents`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
}

// expectNoDocument is the watcher's registration check finding no document at
// the row's key.
func expectNoDocument(m sqlmock.Sqlmock) {
	m.ExpectQuery(`SELECT rag_source_id\s+FROM meko_system.knowledge_bases`).
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).AddRow("src-1"))
	m.ExpectQuery(`FROM\s+dist_rag.documents d\s+LEFT JOIN LATERAL`).
		WillReturnRows(sqlmock.NewRows([]string{"source_id", "status", "has_meko_details"}))
}

// expectDocument is the registration check finding a document in src-1.
// hasMekoDetails is false for one dist_rag's own worker created.
func expectDocument(m sqlmock.Sqlmock, status string, hasMekoDetails bool) {
	m.ExpectQuery(`SELECT rag_source_id\s+FROM meko_system.knowledge_bases`).
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).AddRow("src-1"))
	m.ExpectQuery(`FROM\s+dist_rag.documents d\s+LEFT JOIN LATERAL`).
		WillReturnRows(sqlmock.NewRows([]string{"source_id", "status", "has_meko_details"}).
			AddRow("src-1", status, hasMekoDetails))
}

// expectResolveExistingSource is SourceResolver.Resolve finding the datapack's
// cached source, followed by the source-status read.
func expectResolveExistingSource(m sqlmock.Sqlmock, status string) {
	expectResolveCachedSource(m)
	m.ExpectQuery(`SELECT status FROM dist_rag.sources`).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(status))
}

// expectResolveCachedSource is Resolve's fast path: the cached source still
// exists, so no datapack lock is taken.
func expectResolveCachedSource(m sqlmock.Sqlmock) {
	m.ExpectQuery(`SELECT rag_source_id\s+FROM meko_system.knowledge_bases`).
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).AddRow("src-1"))
	m.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM dist_rag.sources`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
}

func expectInsertDocument(m sqlmock.Sqlmock) {
	m.ExpectBegin()
	m.ExpectQuery(`SELECT id FROM dist_rag.sources WHERE id = \$1::UUID FOR NO KEY UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("src-1"))
	m.ExpectExec(`INSERT INTO dist_rag.documents`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
}

func expectWrite(m sqlmock.Sqlmock, statement string, args ...driver.Value) {
	e := m.ExpectExec(statement)
	if len(args) > 0 {
		e = e.WithArgs(args...)
	}
	e.WillReturnResult(sqlmock.NewResult(0, 1))
}

const (
	retireStmt          = `DELETE FROM meko_system.pending_uploads`
	failOrRecheckStmt   = `UPDATE meko_system.pending_uploads`
	recheckLeaseFence   = `(?s)UPDATE meko_system.pending_uploads.*state = 'AWAITING_UPLOAD'.*lease_expires_at = \$3`
	discoveryLeaseFence = `(?s)UPDATE meko_system.pending_uploads.*state = 'AWAITING_DISCOVERY'.*lease_expires_at = \$2`
	markRegisteredStmt  = `(?s)UPDATE meko_system.pending_uploads\s+SET registered_at = NOW\(\)`
)

func TestResolve(t *testing.T) {
	pdf := []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\n")

	cases := []struct {
		row    func() repository.PendingUpload
		expect func(m sqlmock.Sqlmock, p repository.PendingUpload)
		name   string
		obj    fakeObject
		// wantBuild is whether resolve returns a source owed a queue call,
		// leaving the row for pass to retire.
		wantBuild   bool
		wantDeletes int32
	}{
		{
			name: "missing object before the deadline is looked at again",
			obj:  fakeObject{},
			row: func() repository.PendingUpload {
				return claimedRow(textBody, repository.PendingUploadAwaiting)
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectWrite(m, recheckLeaseFence, p.ID, "", p.LeaseExpiresAt)
			},
		},
		{
			name: "missing object after the deadline is abandoned",
			obj:  fakeObject{},
			row: func() repository.PendingUpload {
				p := claimedRow(textBody, repository.PendingUploadAwaiting)
				p.ExpiresAt = time.Now().Add(-time.Minute)
				return p
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectWrite(m, failOrRecheckStmt, p.ID, repository.PendingUploadAbandoned,
					"upload never arrived before the authorization expired", p.LeaseExpiresAt)
			},
		},
		{
			// A failure of ours (S3 erroring) is retried past ExpiresAt...
			name: "S3 error inside the retry window is retried",
			obj:  fakeObject{headStatus: http.StatusInternalServerError},
			row: func() repository.PendingUpload {
				p := claimedRow(textBody, repository.PendingUploadAwaiting)
				p.ExpiresAt = time.Now().Add(-time.Hour)
				return p
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				m.ExpectExec(recheckLeaseFence).WillReturnResult(sqlmock.NewResult(0, 1))
			},
		},
		{
			// ...but not forever: the row would hold quota indefinitely.
			name: "S3 error past the retry window is abandoned",
			obj:  fakeObject{headStatus: http.StatusInternalServerError},
			row: func() repository.PendingUpload {
				p := claimedRow(textBody, repository.PendingUploadAwaiting)
				p.ExpiresAt = time.Now().Add(-watcherRetryWindow - time.Minute)
				return p
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectWrite(m, failOrRecheckStmt, p.ID, repository.PendingUploadAbandoned,
					"the upload could not be registered; request a new upload URL and try again", p.LeaseExpiresAt)
			},
		},
		{
			name: "document the client registered and queued itself retires the row",
			obj:  fakeObject{body: textBody},
			row: func() repository.PendingUpload {
				return claimedRow(textBody, repository.PendingUploadAwaiting)
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectDocument(m, "QUEUED", true)
				expectWrite(m, retireStmt, p.ID, p.LeaseExpiresAt)
			},
		},
		{
			// A registration whose queue call never landed: the client's own
			// create failed it, or an earlier pass's did. The row is owed the
			// call and stays held for pass to retire after it.
			name: "registered document never queued is owed its queue call",
			obj:  fakeObject{body: textBody},
			row: func() repository.PendingUpload {
				return claimedRow(textBody, repository.PendingUploadAwaiting)
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectDocument(m, documentStatusNotStarted, true)
				expectWrite(m, markRegisteredStmt, p.ID, p.LeaseExpiresAt)
			},
			wantBuild: true,
		},
		{
			// A re-presign of content whose indexing failed is a retry.
			name: "failed document is queued again",
			obj:  fakeObject{body: textBody},
			row: func() repository.PendingUpload {
				return claimedRow(textBody, repository.PendingUploadAwaiting)
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectDocument(m, documentStatusFailed, true)
				expectWrite(m, markRegisteredStmt, p.ID, p.LeaseExpiresAt)
			},
			wantBuild: true,
		},
		{
			// dist_rag's own worker listed the agent's object first: its
			// document has no size or uploader, so retiring the row here
			// would leave the file counted as 0 bytes and unattributed.
			name: "document dist_rag created without meko details is registered to write them",
			obj:  fakeObject{body: textBody},
			row: func() repository.PendingUpload {
				return claimedRow(textBody, repository.PendingUploadAwaiting)
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectDocument(m, "QUEUED", false)
				expectResolveExistingSource(m, SourceStatusCompleted)
				expectInsertDocument(m)
				expectWrite(m, markRegisteredStmt, p.ID, p.LeaseExpiresAt)
			},
			wantBuild: true,
		},
		{
			name: "object over its signed cap is deleted and marked oversized",
			obj:  fakeObject{body: textBody},
			row: func() repository.PendingUpload {
				p := claimedRow(textBody, repository.PendingUploadAwaiting)
				p.MaxSizeBytes = 4
				return p
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectNoDocument(m)
				expectNotRegistered(m) // deleteObject's own S3KeyIsSafeToDelete
				m.ExpectExec(failOrRecheckStmt).
					WithArgs(p.ID, repository.PendingUploadOversized, sqlmock.AnyArg(), p.LeaseExpiresAt).
					WillReturnResult(sqlmock.NewResult(0, 1))
			},
			wantDeletes: 1,
		},
		{
			name: "object without a checksum whose size differs from the declaration is deleted",
			obj:  fakeObject{body: textBody, noChecksum: true},
			row: func() repository.PendingUpload {
				p := claimedRow(textBody, repository.PendingUploadAwaiting)
				p.DeclaredSize = 1
				return p
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectNoDocument(m)
				expectNotRegistered(m)
				m.ExpectExec(failOrRecheckStmt).
					WithArgs(p.ID, repository.PendingUploadAbandoned, sqlmock.AnyArg(), p.LeaseExpiresAt).
					WillReturnResult(sqlmock.NewResult(0, 1))
			},
			wantDeletes: 1,
		},
		{
			// S3 confirmed the content, which has one size, so the row's
			// declared size is the stale one: a re-presign kept the larger
			// size while the URL this object came through was still valid.
			name: "verified object whose size differs from the declaration is registered",
			obj:  fakeObject{body: textBody},
			row: func() repository.PendingUpload {
				p := claimedRow(textBody, repository.PendingUploadAwaiting)
				p.DeclaredSize = int64(len(textBody)) + 100
				return p
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectNoDocument(m)
				expectResolveExistingSource(m, SourceStatusCompleted)
				expectInsertDocument(m)
				expectWrite(m, markRegisteredStmt, p.ID, p.LeaseExpiresAt)
			},
			wantBuild: true,
		},
		{
			// Nothing was signed for an AlreadyUploaded row: the bytes were
			// already there, so a wrong declared size must not delete them.
			// The GET failing stops resolve right after the size checks.
			name: "unsigned row with a wrong declared size keeps the object",
			obj:  fakeObject{body: textBody, getStatus: http.StatusInternalServerError},
			row: func() repository.PendingUpload {
				p := claimedRow(textBody, repository.PendingUploadAwaiting)
				p.MaxSizeBytes = 0
				p.DeclaredSize = 1
				return p
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectNoDocument(m)
				m.ExpectExec(recheckLeaseFence).WillReturnResult(sqlmock.NewResult(0, 1))
			},
		},
		{
			name: "content that doesn't hash to the key is deleted",
			obj:  fakeObject{body: textBody, checksum: base64.StdEncoding.EncodeToString(make([]byte, 32))},
			row: func() repository.PendingUpload {
				return claimedRow(textBody, repository.PendingUploadAwaiting)
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectNoDocument(m)
				expectNotRegistered(m)
				m.ExpectExec(failOrRecheckStmt).
					WithArgs(p.ID, repository.PendingUploadAbandoned, sqlmock.AnyArg(), p.LeaseExpiresAt).
					WillReturnResult(sqlmock.NewResult(0, 1))
			},
			wantDeletes: 1,
		},
		{
			name: "PDF bytes named .txt are deleted",
			obj:  fakeObject{body: pdf},
			row:  func() repository.PendingUpload { return claimedRow(pdf, repository.PendingUploadAwaiting) },
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectNoDocument(m)
				expectNotRegistered(m)
				m.ExpectExec(failOrRecheckStmt).
					WithArgs(p.ID, repository.PendingUploadAbandoned, sqlmock.AnyArg(), p.LeaseExpiresAt).
					WillReturnResult(sqlmock.NewResult(0, 1))
			},
			wantDeletes: 1,
		},
		{
			name: "verified object in a settled source is registered",
			obj:  fakeObject{body: textBody},
			row: func() repository.PendingUpload {
				return claimedRow(textBody, repository.PendingUploadAwaiting)
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectNoDocument(m)
				expectResolveExistingSource(m, SourceStatusCompleted)
				expectInsertDocument(m)
				expectWrite(m, markRegisteredStmt, p.ID, p.LeaseExpiresAt)
			},
			wantBuild: true,
		},
		{
			name: "verified object in a source dist_rag is still listing waits for it",
			obj:  fakeObject{body: textBody},
			row: func() repository.PendingUpload {
				return claimedRow(textBody, repository.PendingUploadAwaiting)
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectNoDocument(m)
				expectResolveExistingSource(m, SourceStatusInProgress)
				expectWrite(m, discoveryLeaseFence, p.ID, p.LeaseExpiresAt)
			},
		},
		{
			// Registering on a status we couldn't read could insert alongside
			// dist_rag's worker and duplicate the document; waiting is bounded
			// by the retry window.
			name: "unreadable source status waits rather than registering",
			obj:  fakeObject{body: textBody},
			row:  func() repository.PendingUpload { return claimedRow(textBody, repository.PendingUploadAwaiting) },
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectNoDocument(m)
				expectResolveCachedSource(m)
				m.ExpectQuery(`SELECT status FROM dist_rag.sources`).WillReturnError(errors.New("connection reset"))
				expectWrite(m, discoveryLeaseFence, p.ID, p.LeaseExpiresAt)
			},
		},
		{
			name: "discovery hand-off keeps waiting while the source is still listing",
			obj:  fakeObject{body: textBody},
			row: func() repository.PendingUpload {
				return claimedRow(textBody, repository.PendingUploadAwaitingDiscovery)
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectResolveExistingSource(m, SourceStatusQueued)
				expectWrite(m, discoveryLeaseFence, p.ID, p.LeaseExpiresAt)
			},
		},
		{
			// Once dist_rag is done, the row registers even though dist_rag
			// may already have created the document: that write is what adds
			// the size and uploader dist_rag's own row lacks. No "already
			// registered" shortcut, so no dist_rag.documents lookup first.
			name: "discovery hand-off registers once the source has settled",
			obj:  fakeObject{body: textBody},
			row: func() repository.PendingUpload {
				return claimedRow(textBody, repository.PendingUploadAwaitingDiscovery)
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectResolveExistingSource(m, SourceStatusCompleted) // once, not again to register
				expectInsertDocument(m)
				expectWrite(m, markRegisteredStmt, p.ID, p.LeaseExpiresAt)
			},
			wantBuild: true,
		},
		{
			// Put back as AWAITING_UPLOAD, the row's next look would find
			// dist_rag's document and retire it without the meko details.
			name: "temporary failure after a discovery hand-off keeps the row waiting on discovery",
			obj:  fakeObject{body: textBody, getStatus: http.StatusInternalServerError},
			row: func() repository.PendingUpload {
				return claimedRow(textBody, repository.PendingUploadAwaitingDiscovery)
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectResolveExistingSource(m, SourceStatusCompleted)
				expectWrite(m, discoveryLeaseFence, p.ID, p.LeaseExpiresAt)
			},
		},
		{
			name: "discovery hand-off that never settles is abandoned past the retry window",
			obj:  fakeObject{body: textBody},
			row: func() repository.PendingUpload {
				p := claimedRow(textBody, repository.PendingUploadAwaitingDiscovery)
				p.ExpiresAt = time.Now().Add(-watcherRetryWindow - time.Minute)
				return p
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				expectResolveExistingSource(m, SourceStatusInProgress)
				expectWrite(m, failOrRecheckStmt, p.ID, repository.PendingUploadAbandoned,
					"the upload could not be indexed; request a new upload URL and try again", p.LeaseExpiresAt)
			},
		},
		{
			// Retried instead, the row would be looked at every few seconds
			// for a day while counting against the owner's quota.
			name: "upload into a deleted datapack is abandoned",
			obj:  fakeObject{body: textBody},
			row: func() repository.PendingUpload {
				return claimedRow(textBody, repository.PendingUploadAwaiting)
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				noSources := func() {
					m.ExpectQuery(`SELECT rag_source_id\s+FROM meko_system.knowledge_bases`).
						WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}))
				}
				noSources() // the registration check
				noSources() // Resolve's cached source
				m.ExpectBegin()
				m.ExpectQuery(`FROM meko_system.datapacks WHERE datapack_id = \$1::UUID FOR NO KEY UPDATE`).
					WillReturnRows(sqlmock.NewRows([]string{"datapack_id"}))
				m.ExpectRollback()
				expectWrite(m, failOrRecheckStmt, p.ID, repository.PendingUploadAbandoned,
					datapackDeletedReason, p.LeaseExpiresAt)
			},
		},
		{
			// Another pod reclaimed the row: the fenced write matches nothing,
			// and resolve simply drops it.
			name: "a lost lease is dropped quietly",
			obj:  fakeObject{},
			row: func() repository.PendingUpload {
				return claimedRow(textBody, repository.PendingUploadAwaiting)
			},
			expect: func(m sqlmock.Sqlmock, p repository.PendingUpload) {
				m.ExpectExec(recheckLeaseFence).WillReturnResult(sqlmock.NewResult(0, 0))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, mock, deletes := newResolveHarness(t, tc.obj)
			p := tc.row()
			tc.expect(mock, p)

			if got := svc.resolve(context.Background(), p); (got != "") != tc.wantBuild {
				t.Errorf("resolve = %q, want a source owed: %v", got, tc.wantBuild)
			}
			if got := atomic.LoadInt32(deletes); got != tc.wantDeletes {
				t.Errorf("DeleteObject calls = %d, want %d", got, tc.wantDeletes)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("sql expectations: %v", err)
			}
		})
	}
}

// expectStorageUsed is the quota read: registered bytes from dist_rag, then
// in-flight bytes from pending_uploads: other files' rows, and what the
// presigned key's own row holds.
func expectStorageUsed(m sqlmock.Sqlmock, registered, others, thisKey int64) {
	m.ExpectQuery(`SELECT kb.rag_source_id`).
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).AddRow("src-1"))
	m.ExpectQuery(`FROM\s+dist_rag.documents d`).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(registered))
	m.ExpectQuery(`SUM\(declared_size\) FILTER \(WHERE s3_key <> \$2\)`).
		WithArgs("owner-1", UploadKey("owner-1", "dp-1", strings.Repeat("ab", 32), "notes.txt")).
		WillReturnRows(sqlmock.NewRows([]string{"others", "this_key"}).AddRow(others, thisKey))
}

func expectUserLock(m sqlmock.Sqlmock) {
	m.ExpectBegin()
	m.ExpectQuery(`FROM meko_system.users WHERE user_id = \$1::UUID FOR NO KEY UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("owner-1"))
}

func presignRequest(size int64, accountType string) PresignRequest {
	return PresignRequest{
		Datapack:    &models.Datapack{ID: "dp-1", UserID: "owner-1"},
		UploaderID:  "uploader-1",
		AccountType: accountType,
		Filename:    "notes.txt",
		ContentHash: strings.Repeat("ab", 32),
		Size:        size,
		PerFileCap:  FreeTierMaxKBFileBytes,
	}
}

// signedPolicy decodes the POST policy's conditions out of the form fields.
func signedPolicy(t *testing.T, up *PresignedUpload) string {
	t.Helper()
	for k, v := range up.Fields {
		if strings.EqualFold(k, "policy") {
			raw, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				t.Fatalf("decode policy: %v", err)
			}
			return string(raw)
		}
	}
	t.Fatalf("no policy field in %v", up.Fields)
	return ""
}

// A gated presign signs exactly the declared size, records the row with the
// cap it was signed under (lowered to what the quota has left), and does both
// under the owner's row lock with in-flight bytes counted.
func TestPresignUpload_GatedTier_SignsExactSizeWithinQuota(t *testing.T) {
	svc, mock, _ := newResolveHarness(t, fakeObject{})
	const (
		size      = 1024
		pending   = 512 << 10
		remaining = 2 << 20 // under the 5 MiB per-file cap, so it becomes the signed cap
	)

	expectUserLock(mock)
	expectStorageUsed(mock, FreeTierMaxKBStorageBytes-remaining-pending, pending, 0)
	mock.ExpectExec(`INSERT INTO meko_system\.pending_uploads`).
		WithArgs(sqlmock.AnyArg(), "dp-1", "owner-1", "uploader-1", "notes.txt",
			int64(size), int64(remaining), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	up, err := svc.PresignUpload(context.Background(), presignRequest(size, "free"))
	if err != nil {
		t.Fatalf("PresignUpload: %v", err)
	}
	if policy := signedPolicy(t, up); !strings.Contains(policy, `["content-length-range",1024,1024]`) {
		t.Errorf("policy does not pin the exact size: %s", policy)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// No room for the declared size: refused before anything is signed or
// recorded, as the typed error the handler maps to 402.
func TestPresignUpload_GatedTier_RefusesOverQuota(t *testing.T) {
	svc, mock, _ := newResolveHarness(t, fakeObject{})

	expectUserLock(mock)
	expectStorageUsed(mock, FreeTierMaxKBStorageBytes-4096, 2048, 0)
	mock.ExpectRollback()

	_, err := svc.PresignUpload(context.Background(), presignRequest(4096, "free"))
	var quotaErr *KBQuotaExceededError
	if !errors.As(err, &quotaErr) {
		t.Fatalf("error = %v, want KBQuotaExceededError", err)
	}
	if quotaErr.Response != KBStorageLimitResponse("free") {
		t.Errorf("response = %+v, want the free tier's KB storage limit", quotaErr.Response)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// A re-presign of a file already pending counts it once. Its own row is not
// charged on top of the new request, so a user near the limit can retry it.
func TestPresignUpload_GatedTier_RepeatRequestCountsTheFileOnce(t *testing.T) {
	svc, mock, _ := newResolveHarness(t, fakeObject{})

	expectUserLock(mock)
	// 6000 bytes left besides this file, whose own row already holds 4096:
	// charged twice it would need 8192 and be refused.
	expectStorageUsed(mock, FreeTierMaxKBStorageBytes-6000, 0, 4096)
	mock.ExpectExec(`INSERT INTO meko_system\.pending_uploads`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if _, err := svc.PresignUpload(context.Background(), presignRequest(4096, "free")); err != nil {
		t.Fatalf("PresignUpload: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// Re-presigning a pending file with a smaller size can't free quota while its
// earlier URL may still land the larger file: the row keeps the larger size,
// and that is what is checked.
func TestPresignUpload_GatedTier_SmallerRepresignFreesNoQuota(t *testing.T) {
	svc, mock, _ := newResolveHarness(t, fakeObject{})

	expectUserLock(mock)
	expectStorageUsed(mock, FreeTierMaxKBStorageBytes-4000, 0, 5000)
	mock.ExpectRollback()

	_, err := svc.PresignUpload(context.Background(), presignRequest(10, "free"))
	var quotaErr *KBQuotaExceededError
	if !errors.As(err, &quotaErr) {
		t.Fatalf("error = %v, want KBQuotaExceededError", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// Content already stored sends no bytes, so it is answered AlreadyUploaded
// without reading the quota: a user at their limit can still re-request it.
func TestPresignUpload_AlreadyStoredContentSkipsTheQuota(t *testing.T) {
	existing := UploadKey("owner-1", "dp-1", strings.Repeat("ab", 32), "first-name.txt")
	svc, mock, _ := newResolveHarness(t, fakeObject{listKey: existing})

	expectUserLock(mock)
	mock.ExpectExec(`INSERT INTO meko_system\.pending_uploads`).
		WithArgs(existing, "dp-1", "owner-1", "uploader-1", "notes.txt",
			int64(4096), int64(0), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	up, err := svc.PresignUpload(context.Background(), presignRequest(4096, "free"))
	if err != nil {
		t.Fatalf("PresignUpload: %v", err)
	}
	if !up.AlreadyUploaded || up.Key != existing {
		t.Errorf("PresignUpload = %+v, want AlreadyUploaded for %s", up, existing)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// A promise that can't be written fails the presign: for an agent the row is
// the only registration path, so a URL without one is a file nobody indexes.
func TestPresignUpload_PromiseFailureFailsThePresign(t *testing.T) {
	svc, mock, _ := newResolveHarness(t, fakeObject{})

	mock.ExpectExec(`INSERT INTO meko_system\.pending_uploads`).
		WillReturnError(errors.New("connection reset"))

	if _, err := svc.PresignUpload(context.Background(), presignRequest(1024, "pro")); err == nil {
		t.Fatal("PresignUpload succeeded with no promise row recorded")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// The watcher retires a registered row only once its source's documents are
// queued: retired first, a failed queue call would leave the document
// registered, unqueued and with nothing left to notice.
func TestQueueAndRetire(t *testing.T) {
	for _, tc := range []struct {
		queueErr error
		name     string
	}{
		{name: "queued source retires its rows"},
		{name: "failed queue call puts its rows back", queueErr: errors.New("connection reset")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, mock, _ := newResolveHarness(t, fakeObject{})
			a := claimedRow(textBody, repository.PendingUploadAwaiting)
			b := claimedRow([]byte("second file\n"), repository.PendingUploadAwaiting)
			b.ID = "pu-2"

			mock.ExpectQuery(`SELECT id FROM dist_rag.vector_indexes`).
				WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("idx-1"))
			queue := mock.ExpectExec(`dist_rag._queue_source_documents`).WithArgs("src-1", "idx-1")
			if tc.queueErr != nil {
				queue.WillReturnError(tc.queueErr)
				expectWrite(mock, recheckLeaseFence, a.ID,
					"queue the source's documents: _queue_source_documents("+DatapackIndexSchema+"."+
						DatapackIndexName+", source=src-1): connection reset", a.LeaseExpiresAt)
				expectWrite(mock, recheckLeaseFence, b.ID, sqlmock.AnyArg(), b.LeaseExpiresAt)
			} else {
				queue.WillReturnResult(sqlmock.NewResult(0, 1))
				expectWrite(mock, retireStmt, a.ID, a.LeaseExpiresAt)
				expectWrite(mock, retireStmt, b.ID, b.LeaseExpiresAt)
			}

			svc.queueAndRetire(map[string][]repository.PendingUpload{"src-1": {a, b}})
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("sql expectations: %v", err)
			}
		})
	}
}

// Purging runs on idle passes, but at most once per interval: every idle pass
// is up to once a second.
func TestPurgeIfDue_RunsOncePerInterval(t *testing.T) {
	svc, mock, _ := newResolveHarness(t, fakeObject{})
	mock.ExpectExec(`DELETE FROM meko_system.pending_uploads`).
		WithArgs(terminalRowRetention.Microseconds(), watcherPurgeBatch).
		WillReturnResult(sqlmock.NewResult(0, watcherPurgeBatch))
	mock.ExpectExec(`DELETE FROM meko_system.pending_uploads`).
		WithArgs(terminalRowRetention.Microseconds(), watcherPurgeBatch).
		WillReturnResult(sqlmock.NewResult(0, 3))

	svc.purgeIfDue() // a full batch, then a short one ends the purge
	first := svc.lastPurge
	svc.purgeIfDue() // within the interval: returns before any query
	if !svc.lastPurge.Equal(first) {
		t.Error("a second purge ran within the interval")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// Rows are only started with enough lease left to finish. Started with less,
// a row could outlive the lease and be worked by a second pod too. S3 answers
// 500 here, so a row the pass did start would be put back with that error
// rather than the empty one a release writes.
func TestPass_ReleasesRowsWhenTheLeaseRunsLow(t *testing.T) {
	svc, mock, _ := newResolveHarness(t, fakeObject{headStatus: http.StatusInternalServerError})
	lease := time.Now().Add(watcherLeaseMargin / 2)
	claimCols := []string{"id", "s3_key", "datapack_id", "owner_user_id", "uploader_id", "filename",
		"declared_size", "max_size_bytes", "prior_state", "attempts", "created_at", "expires_at",
		"lease_expires_at"}
	a := claimedRow(textBody, repository.PendingUploadAwaiting)
	b := claimedRow(textBody, repository.PendingUploadAwaitingDiscovery)
	b.ID = "pu-2"
	rows := sqlmock.NewRows(claimCols)
	for _, p := range []repository.PendingUpload{a, b} {
		rows.AddRow(p.ID, p.S3Key, p.DatapackID, p.OwnerUserID, p.UploaderID, p.Filename,
			p.DeclaredSize, p.MaxSizeBytes, p.State, p.Attempts, time.Now(), p.ExpiresAt, lease)
	}
	mock.ExpectQuery(`UPDATE meko_system.pending_uploads`).WillReturnRows(rows)
	expectWrite(mock, recheckLeaseFence, a.ID, "", lease)
	expectWrite(mock, discoveryLeaseFence, b.ID, lease)

	if n := svc.pass(context.Background()); n != 2 {
		t.Errorf("pass = %d, want 2 claimed", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}
