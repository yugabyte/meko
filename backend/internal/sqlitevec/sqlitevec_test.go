package sqlitevec_test

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/yugabyte/meko-local/backend/internal/sqlitevec"
)

// The vendored headers must match the SQLite that mattn compiles, and sqlite-vec must load.
func TestHeadersMatchCompiledSQLite(t *testing.T) {
	sqlitevec.Register()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var sqliteVersion, vecVersion string
	if err := db.QueryRow("SELECT sqlite_version(), vec_version()").Scan(&sqliteVersion, &vecVersion); err != nil {
		t.Fatal(err)
	}
	if h := sqlitevec.HeaderVersion(); h != sqliteVersion {
		t.Fatalf("third_party/sqlite-vec has SQLite %s headers, but mattn compiles SQLite %s: "+
			"copy mattn's sqlite3-binding.h and sqlite3ext.h there again", h, sqliteVersion)
	}
	if vecVersion != "v0.1.9" {
		t.Errorf("sqlite-vec %s; update third_party/sqlite-vec/VERSION if this was an upgrade", vecVersion)
	}
}
