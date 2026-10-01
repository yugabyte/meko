package repository

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// The missing row is what "never reported" means, so a nil catalog must not be
// written as one.
func TestPutCatalogRefusesNil(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	if err := NewEnvironmentExtensionRepo(db).PutCatalog("env-1", nil); err == nil {
		t.Fatal("PutCatalog(nil) succeeded")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
