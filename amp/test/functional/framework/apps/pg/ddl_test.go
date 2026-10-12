package pg_test

import (
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
)

// The gate is only worth having if it classifies by the statement's FIRST
// keyword. A substring match would call an INSERT whose payload contains the
// word "create" a DDL, and a gate that never opens is worse than none.
func TestIsDDLMatchesTheLeadingKeywordOnly(t *testing.T) {
	ddl := []string{
		"CREATE TABLE t (id int)",
		"  create index i on t (id)",
		"ALTER TABLE t ADD COLUMN v int",
		"DROP DATABASE jepsen",
		"TRUNCATE t",
		"REINDEX INDEX i",
		"GRANT ALL ON t TO jepsen",
		"-- a comment first\nCREATE TABLE t (id int)",
	}
	for _, q := range ddl {
		if !pg.IsDDL(q) {
			t.Errorf("IsDDL(%q) = false, want true", q)
		}
	}

	notDDL := []string{
		"",
		"SELECT 1",
		"INSERT INTO t (v) VALUES ('please create a table')",
		"UPDATE t SET v = 'drop database' WHERE id = 1",
		"BEGIN",
		"COMMIT",
		"SELECT * FROM pg_stat_activity",
	}
	for _, q := range notDDL {
		if pg.IsDDL(q) {
			t.Errorf("IsDDL(%q) = true, want false", q)
		}
	}
}
