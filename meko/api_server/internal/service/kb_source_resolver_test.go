package service

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

const (
	getSourceIDsSQL = `SELECT rag_source_id\s+FROM meko_system\.knowledge_bases`
	lockDatapackSQL = `SELECT datapack_id FROM meko_system\.datapacks WHERE datapack_id = \$1::UUID FOR NO KEY UPDATE`
)

func newResolverWithMocks(t *testing.T) (*SourceResolver, sqlmock.Sqlmock, sqlmock.Sqlmock) {
	t.Helper()
	systemDB, systemMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	dataDB, dataMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { systemDB.Close(); dataDB.Close() })
	kbSvc := NewKnowledgeBaseService(repository.NewKnowledgeBaseRepo(systemDB, dataDB), "bucket")
	return NewSourceResolver(kbSvc, NewRagService(dataDB, "datapacks", "kb")), systemMock, dataMock
}

// A datapack whose cached source still exists resolves without taking the
// datapack lock: sqlmock would fail the unexpected BEGIN.
func TestResolve_ExistingSourceSkipsTheLock(t *testing.T) {
	r, systemMock, dataMock := newResolverWithMocks(t)
	systemMock.ExpectQuery(getSourceIDsSQL).
		WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}).AddRow("source-1"))
	dataMock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM dist_rag\.sources WHERE id = \$1::UUID\)`).
		WithArgs("source-1").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	sourceID, isNew, err := r.Resolve(context.Background(), &models.Datapack{ID: "dp-1"}, "s3://bucket/u/dp-1/")
	if err != nil || sourceID != "source-1" || isNew {
		t.Fatalf("Resolve = %q, %v, %v; want source-1, not new", sourceID, isNew, err)
	}
	for _, m := range []sqlmock.Sqlmock{systemMock, dataMock} {
		if err := m.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
	}
}

// A dist_rag call that hangs while the datapack lock is held is canceled at
// sourceResolveTimeout, and the lock's transaction is rolled back rather than
// held for as long as the call hangs.
func TestResolve_HungDistRagReleasesTheLock(t *testing.T) {
	prev := sourceResolveTimeout
	sourceResolveTimeout = 100 * time.Millisecond
	t.Cleanup(func() { sourceResolveTimeout = prev })

	r, systemMock, dataMock := newResolverWithMocks(t)
	systemMock.ExpectQuery(getSourceIDsSQL).WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}))
	systemMock.ExpectBegin()
	systemMock.ExpectQuery(lockDatapackSQL).WithArgs("dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"datapack_id"}).AddRow("dp-1"))
	systemMock.ExpectQuery(getSourceIDsSQL).WillReturnRows(sqlmock.NewRows([]string{"rag_source_id"}))
	dataMock.ExpectQuery(`SELECT id FROM dist_rag\.sources WHERE source_uri = \$1`).
		WillDelayFor(5 * time.Second).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	systemMock.ExpectRollback()

	start := time.Now()
	_, _, err := r.Resolve(context.Background(), &models.Datapack{ID: "dp-1"}, "s3://bucket/u/dp-1/")
	if err == nil {
		t.Fatal("Resolve succeeded past a hung dist_rag call")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("held the datapack lock for %v, want about sourceResolveTimeout", elapsed)
	}
	if err := systemMock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
