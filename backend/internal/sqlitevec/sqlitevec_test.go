package sqlitevec_test

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/yugabyte/meko/backend/internal/sqlitevec"
)

// The vendored header must match the SQLite that mattn compiles, and the sqlite-vec that loads must
// be the one third_party/sqlite-vec/VERSION names.
func TestHeadersMatchCompiledSQLite(t *testing.T) {
	sqlitevec.Register()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var sqliteVersion, vecVersion string
	if err = db.QueryRow("SELECT sqlite_version(), vec_version()").Scan(&sqliteVersion, &vecVersion); err != nil {
		t.Fatal(err)
	}
	if h := sqlitevec.HeaderVersion(); h != sqliteVersion {
		t.Fatalf("third_party/sqlite-vec has SQLite %s headers, but mattn compiles SQLite %s: "+
			"copy mattn's sqlite3-binding.h there again", h, sqliteVersion)
	}
	b, err := os.ReadFile("../../third_party/sqlite-vec/VERSION")
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(b), "\n")
	want := "v" + strings.TrimPrefix(strings.TrimSpace(first), "sqlite-vec ")
	if vecVersion != want {
		t.Errorf("sqlite-vec %s loaded, but third_party/sqlite-vec/VERSION says %s", vecVersion, want)
	}
}
