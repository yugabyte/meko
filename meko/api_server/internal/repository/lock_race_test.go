package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// A mock connection is single-threaded and never evaluates SQL: it only
// matches statements and returns scripted results. So these tests pin the
// statement sequence -- lock, critical section, commit or rollback -- and
// nothing else. Whether the locks really serialize concurrent callers, and
// whether the upsert really avoids a duplicate, are only provable against a
// real database.

func TestKnowledgeBaseRepo_WithDatapackRowLock_RunsCriticalSectionThenCommits(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT datapack_id FROM meko_system\.datapacks WHERE datapack_id = \$1::UUID FOR NO KEY UPDATE`).
		WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"datapack_id"}).AddRow("dp-1"))
	mock.ExpectCommit()

	repo := NewKnowledgeBaseRepo(db, db)
	ran := false
	err = repo.WithDatapackRowLock(context.Background(), "dp-1", func() error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("WithDatapackRowLock: %v", err)
	}
	if !ran {
		t.Error("criticalSection was never run")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_WithDatapackRowLock_RollsBackWhenCriticalSectionFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT datapack_id FROM meko_system\.datapacks WHERE datapack_id = \$1::UUID FOR NO KEY UPDATE`).
		WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"datapack_id"}).AddRow("dp-1"))
	mock.ExpectRollback()

	repo := NewKnowledgeBaseRepo(db, db)
	wantErr := errors.New("caller-supplied failure")
	err = repo.WithDatapackRowLock(context.Background(), "dp-1", func() error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("got error %v, want %v", err, wantErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_WithDatapackRowLock_RollsBackWhenLockFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT datapack_id FROM meko_system\.datapacks WHERE datapack_id = \$1::UUID FOR NO KEY UPDATE`).
		WithArgs("missing-dp").
		WillReturnError(errors.New("no rows in result set"))
	mock.ExpectRollback()

	repo := NewKnowledgeBaseRepo(db, db)
	ran := false
	err = repo.WithDatapackRowLock(context.Background(), "missing-dp", func() error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatal("expected an error when the row lock fails")
	}
	if ran {
		t.Error("criticalSection ran despite the lock never being acquired")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRagRepo_InsertDocument_SecondCallerSeesFirstCallersInsert pins that each
// of two sequential InsertDocument calls for the same (source_id,
// document_uri) takes the source lock, runs the single UPDATE-then-INSERT
// statement on that transaction, and commits. The scripted results stand in
// for "first inserts, second updates"; sqlmock doesn't execute the statement,
// so it doesn't prove the second call avoids a duplicate.
func TestRagRepo_InsertDocument_SecondCallerSeesFirstCallersInsert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	repo := NewRagRepo(db, "datapacks", "knowledge_bases")

	const sourceID = "source-1"
	const documentURI = "s3://bucket/tenant/source-1/file.txt"

	// Caller A: document does not exist yet -> the UPDATE half of the CTE
	// touches nothing, the INSERT half fires.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM dist_rag\.sources WHERE id = \$1::UUID FOR NO KEY UPDATE`).
		WithArgs(sourceID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(sourceID))
	mock.ExpectExec(`WITH updated AS \(\s*UPDATE dist_rag\.documents`).
		WithArgs(sourceID, documentURI, "text/plain", "file.txt", "checksum-a", []byte(`{}`)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	ctx := context.Background()
	if err := repo.InsertDocument(ctx, sourceID, "file.txt", documentURI, "checksum-a", "text/plain", nil); err != nil {
		t.Fatalf("caller A InsertDocument: %v", err)
	}

	// Caller B: same statement sequence; the scripted result stands in for
	// the UPDATE half finding A's row.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM dist_rag\.sources WHERE id = \$1::UUID FOR NO KEY UPDATE`).
		WithArgs(sourceID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(sourceID))
	mock.ExpectExec(`WITH updated AS \(\s*UPDATE dist_rag\.documents`).
		WithArgs(sourceID, documentURI, "text/plain", "file.txt", "checksum-b", []byte(`{}`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := repo.InsertDocument(ctx, sourceID, "file.txt", documentURI, "checksum-b", "text/plain", nil); err != nil {
		t.Fatalf("caller B InsertDocument: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The wait for a lock another transaction holds gives up when ctx ends, and
// the critical section never runs.
// A deleted datapack has no row to lock. Callers tell that apart from a
// failure worth retrying: the pending-upload watcher abandons such a row.
func TestKnowledgeBaseRepo_WithDatapackRowLock_ReportsADeletedDatapack(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT datapack_id FROM meko_system\.datapacks WHERE datapack_id = \$1::UUID FOR NO KEY UPDATE`).
		WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"datapack_id"}))
	mock.ExpectRollback()

	err = NewKnowledgeBaseRepo(db, db).WithDatapackRowLock(context.Background(), "dp-1", func() error {
		t.Fatal("critical section ran without the lock")
		return nil
	})
	if !errors.Is(err, ErrDatapackNotFound) {
		t.Fatalf("err = %v, want ErrDatapackNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_WithDatapackRowLock_GivesUpWaitingWhenCtxEnds(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT datapack_id FROM meko_system\.datapacks WHERE datapack_id = \$1::UUID FOR NO KEY UPDATE`).
		WithArgs("dp-1").
		WillDelayFor(time.Second).
		WillReturnRows(sqlmock.NewRows([]string{"datapack_id"}).AddRow("dp-1"))
	mock.ExpectRollback()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	repo := NewKnowledgeBaseRepo(db, db)
	ran := false
	start := time.Now()
	err = repo.WithDatapackRowLock(ctx, "dp-1", func() error {
		ran = true
		return nil
	})
	if err == nil || ran {
		t.Fatalf("err = %v, ran = %v; want an error and no critical section", err, ran)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("waited %v for a lock whose ctx ended after 50ms", elapsed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Once taken, the datapack lock is held until the critical section returns,
// even if ctx ends first. database/sql rolls back a transaction whose context
// ends, and here that would release the lock while the critical section is
// still working on other connections. sqlmock fails that rollback as
// unexpected, and the commit after it with ErrTxDone.
func TestKnowledgeBaseRepo_WithDatapackRowLock_HoldsTheLockAfterCtxEnds(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT datapack_id FROM meko_system\.datapacks WHERE datapack_id = \$1::UUID FOR NO KEY UPDATE`).
		WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"datapack_id"}).AddRow("dp-1"))
	mock.ExpectCommit()

	ctx, cancel := context.WithCancel(context.Background())
	repo := NewKnowledgeBaseRepo(db, db)
	err = repo.WithDatapackRowLock(ctx, "dp-1", func() error {
		cancel()
		time.Sleep(50 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("WithDatapackRowLock: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestRagRepo_WithSourceRowLock_GivesUpWaitingWhenCtxEnds(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM dist_rag\.sources WHERE id = \$1::UUID FOR NO KEY UPDATE`).
		WithArgs("source-1").
		WillDelayFor(time.Second).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("source-1"))
	mock.ExpectRollback()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	repo := NewRagRepo(db, "datapacks", "knowledge_bases")
	err = repo.InsertDocument(ctx, "source-1", "file.txt", "s3://b/k", "checksum", "text/plain", nil)
	if err == nil {
		t.Fatal("InsertDocument succeeded without the source lock")
	}
	// This transaction follows ctx, so database/sql rolls it back from its own
	// goroutine when ctx ends, which can land just after InsertDocument returns.
	deadline := time.Now().Add(time.Second)
	for mock.ExpectationsWereMet() != nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The user lock behaves as the datapack lock does: the wait gives up when ctx
// ends, and once taken the lock is held until the critical section returns.
func TestKnowledgeBaseRepo_WithUserRowLock_GivesUpWaitingWhenCtxEnds(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT user_id FROM meko_system\.users WHERE user_id = \$1::UUID FOR NO KEY UPDATE`).
		WithArgs("u-1").
		WillDelayFor(time.Second).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("u-1"))
	mock.ExpectRollback()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = NewKnowledgeBaseRepo(db, db).WithUserRowLock(ctx, "u-1", func() error {
		t.Fatal("critical section ran without the lock")
		return nil
	})
	if err == nil {
		t.Fatal("WithUserRowLock succeeded; want the wait to give up")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("waited %v for a lock whose ctx ended after 50ms", elapsed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestKnowledgeBaseRepo_WithUserRowLock_HoldsTheLockAfterCtxEnds(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT user_id FROM meko_system\.users WHERE user_id = \$1::UUID FOR NO KEY UPDATE`).
		WithArgs("u-1").
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("u-1"))
	mock.ExpectCommit()

	ctx, cancel := context.WithCancel(context.Background())
	err = NewKnowledgeBaseRepo(db, db).WithUserRowLock(ctx, "u-1", func() error {
		cancel()
		time.Sleep(50 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("WithUserRowLock: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
