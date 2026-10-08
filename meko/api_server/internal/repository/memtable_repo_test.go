package repository

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/memtable"
)

// The api-server reads the memory table meko-mcp-server writes, so a switched
// MEM0_COLLECTION_NAME has to reach its SQL, not just the setting.
func TestMemoriesRepoReadsTheConfiguredCollection(t *testing.T) {
	if err := memtable.Set("meko_mem0_memories"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = memtable.Set("") }) //nolint:errcheck // "" always succeeds

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	want := "SELECT COUNT(*) FROM datapacks.meko_mem0_memories WHERE meko_datapack_id = $1::uuid"
	mock.ExpectQuery(regexp.QuoteMeta(want)).
		WithArgs("dp").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))

	repo := NewMemoriesRepo(db, nil, "", "", "", true)
	got, err := repo.CountByDatapack(context.Background(), "dp")
	if err != nil || got != 3 {
		t.Fatalf("CountByDatapack = %d, %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
