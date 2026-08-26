package api

import (
	"strings"
	"testing"

	"github.com/yugabyte/amp/controller/migrations"
)

// The 0009 migration rewrites the stored memory quantities this package rejects,
// using a copy of the pattern in SQL (YSQL's `!~` takes the same POSIX syntax).
// A drift between the two leaves the migration normalizing a different set than
// the API refuses — the case that produced meko#492 in the first place.
func TestMemoryQuantityPatternMatchesMigration(t *testing.T) {
	const name = "0009_endpoint_memory_units.up.sql"
	sql, err := migrations.FS.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if pattern := memoryQuantityRE.String(); !strings.Contains(string(sql), "'"+pattern+"'") {
		t.Errorf("%s does not contain the memory pattern %q — the migration and the API validation have drifted", name, pattern)
	}
}
