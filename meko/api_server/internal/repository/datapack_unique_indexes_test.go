package repository

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// systemMigrations is the frozen "system" lineage that defines meko_system.datapacks
// and its indexes (a different set from controlPlaneMigrations).
const systemMigrations = "../db/migrations/system"

// datapackUniqueIndexes is every unique index on meko_system.datapacks EXCEPT its
// primary key. Create/UpdateByID classify a 23505 on their statement as
// uq_datapacks_user_name, which is exact only while each of these is unreachable:
//
//   - datapacks_pkey (datapack_id): the INSERT never supplies datapack_id
//     (gen_random_uuid() does) and the UPDATE never changes it.
//   - idx_datapacks_user_id (user_id, datapack_id): neither statement changes
//     user_id or datapack_id, so this one-to-one index cannot collide.
//
// That leaves uq_datapacks_user_name (user_id, datapack_name) as the only reachable
// conflict on either statement — the assumption both branches rest on.
var datapackUniqueIndexes = []string{
	"idx_datapacks_user_id",
	"uq_datapacks_user_name",
}

// TestSystemUniqueIndexesOnDatapacks pins that assumption since CLOUDGA-36978:
// Create/UpdateByID identify a 23505 by the statement that raised it, because YSQL
// does not send the index name. Add any unique index to meko_system.datapacks and
// the inference silently becomes wrong — its violation would come back as a 409
// "a datapack with this name already exists" for a conflict that never happened
// (AGENTS.md). Reading the migrations gates this in CI, where no live DB exists.
func TestSystemUniqueIndexesOnDatapacks(t *testing.T) {
	sql := systemMigrationSQL(t)

	got := map[string]bool{}

	// Standalone: CREATE UNIQUE INDEX <name> ON meko_system.datapacks(...).
	for _, m := range regexp.MustCompile(
		`(?i)CREATE\s+UNIQUE\s+INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s+ON\s+meko_system\.datapacks\b`,
	).FindAllStringSubmatch(sql, -1) {
		got[m[1]] = true
	}

	// Inline: a CONSTRAINT <name> UNIQUE (...) in the CREATE TABLE body. The regexp
	// above cannot see these, so scan them separately — uq_datapacks_user_name is one.
	for _, m := range regexp.MustCompile(
		`(?i)CONSTRAINT\s+(\w+)\s+UNIQUE\b`,
	).FindAllStringSubmatch(datapacksCreateTable(sql), -1) {
		got[m[1]] = true
	}

	have := make([]string, 0, len(got))
	for name := range got {
		have = append(have, name)
	}
	sort.Strings(have)
	want := append([]string(nil), datapackUniqueIndexes...)
	sort.Strings(want)

	if strings.Join(have, ",") != strings.Join(want, ",") {
		t.Errorf("unique indexes on meko_system.datapacks = %v, want %v\n"+
			"Create/UpdateByID identify a 23505 by which statement raised it, so a change here "+
			"silently remaps a conflict — reconcile both \"already exists\" branches with the new schema.",
			have, want)
	}

	// A uniqueness constraint added by ALTER TABLE would be just as load-bearing and
	// invisible to both scans above, so refuse those outright.
	for _, stmt := range strings.Split(sql, ";") {
		up := strings.ToUpper(stmt)
		if !strings.Contains(up, "UNIQUE") || !strings.HasPrefix(strings.TrimSpace(up), "ALTER TABLE") {
			continue
		}
		if regexp.MustCompile(`(?i)meko_system\.datapacks\b`).MatchString(stmt) {
			t.Errorf("meko_system.datapacks gains uniqueness via ALTER TABLE, which this test "+
				"does not inventory:\n%s", strings.TrimSpace(stmt))
		}
	}
}

// datapacksCreateTable returns just the CREATE TABLE ... meko_system.datapacks (...)
// body, so inline CONSTRAINT scanning cannot stray into another table's definition.
func datapacksCreateTable(sql string) string {
	loc := regexp.MustCompile(`(?is)CREATE\s+TABLE[^;]*meko_system\.datapacks\b.*?\)\s*(?:SPLIT[^;]*)?;`).
		FindString(sql)
	return loc
}

// systemMigrationSQL concatenates every system up migration with comments stripped —
// the migrations document these indexes in prose, which a raw scan would match.
func systemMigrationSQL(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(systemMigrations, "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no system migrations at %s: %v", systemMigrations, err)
	}
	var b strings.Builder
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		b.Write(body)
		b.WriteString("\n")
	}
	sql := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(b.String(), "")
	return regexp.MustCompile(`--[^\n]*`).ReplaceAllString(sql, "")
}
