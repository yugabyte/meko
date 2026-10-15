package service

import (
	"database/sql/driver"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/repository"
)

func newTestPendingUploadService(t *testing.T) (*PendingUploadService, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	svc := NewPendingUploadService(
		repository.NewPendingUploadRepo(db), nil, nil, nil, nil)
	return svc, mock, func() { db.Close() }
}

// The deadline is the only thing that ever turns "not there yet" into
// "abandoned", so it must outlast the URL's own validity — a transfer that
// started just before expiry may still be arriving.
func TestPendingUploadService_Promise_DefaultsExpiryBeyondURLValidity(t *testing.T) {
	svc, mock, closeDB := newTestPendingUploadService(t)
	defer closeDB()

	floor := time.Now().Add(UploadURLTTL + UploadGracePeriod)
	mock.ExpectExec(`INSERT INTO meko_system\.pending_uploads`).
		WithArgs("u/dp/h/a.pdf", "dp-1", "u-owner", "u-uploader", "a.pdf",
			int64(0), int64(5<<20),
			timeAtLeast{floor}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := svc.Promise(repository.PendingUpload{
		S3Key:        "u/dp/h/a.pdf",
		DatapackID:   "dp-1",
		OwnerUserID:  "u-owner",
		UploaderID:   "u-uploader",
		Filename:     "a.pdf",
		MaxSizeBytes: 5 << 20,
	}); err != nil {
		t.Fatalf("Promise: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// timeAtLeast matches a timestamp argument at or after a floor, which is how
// the expiry assertion above avoids depending on the exact instant Promise ran.
type timeAtLeast struct{ floor time.Time }

func (m timeAtLeast) Match(v driver.Value) bool {
	got, ok := v.(time.Time)
	return ok && !got.Before(m.floor)
}

// A promise wakes the watcher so the first look happens about a second later
// rather than at the next idle tick.
func TestPendingUploadService_Promise_Nudges(t *testing.T) {
	svc, mock, closeDB := newTestPendingUploadService(t)
	defer closeDB()

	mock.ExpectExec(`INSERT INTO meko_system\.pending_uploads`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := svc.Promise(repository.PendingUpload{S3Key: "u/dp/h/a.pdf"}); err != nil {
		t.Fatalf("Promise: %v", err)
	}
	select {
	case <-svc.nudge:
	default:
		t.Fatal("expected a nudge")
	}
}

// The nudge must never block the presign it is attached to, so a second promise
// while the watcher is mid-pass is dropped rather than queued.
func TestPendingUploadService_Promise_NudgeNeverBlocks(t *testing.T) {
	svc, mock, closeDB := newTestPendingUploadService(t)
	defer closeDB()

	mock.ExpectExec(`INSERT INTO meko_system\.pending_uploads`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO meko_system\.pending_uploads`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	for i := 0; i < 2; i++ {
		if err := svc.Promise(repository.PendingUpload{S3Key: "u/dp/h/a.pdf"}); err != nil {
			t.Fatalf("Promise %d: %v", i, err)
		}
	}
	if len(svc.nudge) != 1 {
		t.Fatalf("expected the nudge to coalesce, got %d", len(svc.nudge))
	}
}

// On the AlreadyUploaded (dedup) path, PromiseUpload must record MaxSizeBytes
// 0, not today's PerFileCap: the object already exists under a cap nobody
// signed for this call, and a lower cap here would let the watcher's
// oversized backstop delete a file its original uploader legitimately
// cleared.
func TestPendingUploadService_PromiseUpload_AlreadyUploaded_DisablesCap(t *testing.T) {
	svc, mock, closeDB := newTestPendingUploadService(t)
	defer closeDB()

	mock.ExpectExec(`INSERT INTO meko_system\.pending_uploads`).
		WithArgs("u/dp/h/a.pdf", "dp-1", "u-owner", "u-uploader", "a.pdf",
			int64(1024), int64(0), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := svc.PromiseUpload(PromiseUploadRequest{
		S3Key:           "u/dp/h/a.pdf",
		DatapackID:      "dp-1",
		OwnerUserID:     "u-owner",
		UploaderID:      "u-uploader",
		Filename:        "a.pdf",
		DeclaredSize:    1024,
		PerFileCap:      5 << 20,
		AlreadyUploaded: true,
	}); err != nil {
		t.Fatalf("PromiseUpload: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// On a fresh presign (no dedup), PromiseUpload must record the caller's
// PerFileCap as MaxSizeBytes, so the watcher's oversized backstop actually
// enforces the policy that was signed.
func TestPendingUploadService_PromiseUpload_NewUpload_UsesPerFileCap(t *testing.T) {
	svc, mock, closeDB := newTestPendingUploadService(t)
	defer closeDB()

	mock.ExpectExec(`INSERT INTO meko_system\.pending_uploads`).
		WithArgs("u/dp/h/a.pdf", "dp-1", "u-owner", "u-uploader", "a.pdf",
			int64(1024), int64(5<<20), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := svc.PromiseUpload(PromiseUploadRequest{
		S3Key:           "u/dp/h/a.pdf",
		DatapackID:      "dp-1",
		OwnerUserID:     "u-owner",
		UploaderID:      "u-uploader",
		Filename:        "a.pdf",
		DeclaredSize:    1024,
		PerFileCap:      5 << 20,
		AlreadyUploaded: false,
	}); err != nil {
		t.Fatalf("PromiseUpload: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// Cancel abandons the live row for the key with the caller's reason — this is
// what lets document-status report a rejection immediately instead of waiting
// out the row's own expiry. It goes through Abandon, which a watcher's lease
// doesn't fence, so a pass already holding the row can't overwrite it.
// Cancel abandons only a row whose URL can no longer be used: the grace it
// passes is how far the row's deadline runs past the URL's expiry.
func TestPendingUploadService_Cancel_AbandonsLiveRow(t *testing.T) {
	svc, mock, closeDB := newTestPendingUploadService(t)
	defer closeDB()

	mock.ExpectExec(`UPDATE meko_system\.pending_uploads`).
		WithArgs("u/dp/h/a.pdf", "dp-1", "content does not match extension", UploadGracePeriod.Microseconds()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	svc.Cancel("dp-1", "u/dp/h/a.pdf", "content does not match extension")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
