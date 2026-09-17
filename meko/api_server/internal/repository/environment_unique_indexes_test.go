package repository

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// controlPlaneMigrations is the set control-plane mode applies (see db.Migrate). The
// system set is a different, frozen lineage and does not describe this schema.
const controlPlaneMigrations = "../db/migrations/controlplane"

// envTableUniqueIndexes is every unique index on the two tables Insert writes, EXCEPT
// their primary keys. Insert's two error branches are exact only while each of these
// is the one conflict its statement can raise:
//
//   - environments: the INSERT never supplies uuid (gen_random_uuid() does), so the
//     primary key is unreachable and a 23505 is environments_account_id_uniq
//     -> ErrEnvironmentExists.
//   - environment_api_keys: env_uuid is the row created one statement earlier, so
//     environment_api_keys_env_status_uniq is unreachable and a 23505 is the
//     api_key_id primary key -> ErrAPIKeyBound.
var envTableUniqueIndexes = map[string][]string{
	"environments":         {"environments_account_id_uniq"},
	"environment_api_keys": {"environment_api_keys_env_status_uniq"},
}

// TestControlPlaneUniqueIndexesOnEnvironmentTables pins the assumption Insert rests on
// since CLOUDGA-36978: it identifies a 23505 by the statement that raised it, because
// YSQL does not send the index name. Add a unique index to either table and that
// inference silently becomes wrong -- its violation would be answered as
// ErrEnvironmentExists or ErrAPIKeyBound, a 409 naming a conflict that did not happen.
//
// amp holds the same inventory against a live schema (TestPgUniqueConstraintsOnTask).
// This reads the migrations instead so that it gates in CI: the live-DB harness in this
// package (openAMPTestDB) skips when YugabyteDB is unreachable, which in CI is always.
func TestControlPlaneUniqueIndexesOnEnvironmentTables(t *testing.T) {
	sql := migrationSQL(t)

	got := map[string][]string{}
	for _, m := range regexp.MustCompile(
		`(?i)CREATE\s+UNIQUE\s+INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s+ON\s+meko_system\.(\w+)`,
	).FindAllStringSubmatch(sql, -1) {
		index, table := m[1], m[2]
		if _, watched := envTableUniqueIndexes[table]; watched {
			got[table] = append(got[table], index)
		}
	}

	for table, want := range envTableUniqueIndexes {
		have := got[table]
		sort.Strings(have)
		sort.Strings(want)
		if strings.Join(have, ",") != strings.Join(want, ",") {
			t.Errorf("unique indexes on meko_system.%s = %v, want %v\n"+
				"EnvironmentRepo.Insert identifies a 23505 by which statement raised it, so a change here "+
				"silently remaps a conflict -- reconcile Insert's two error branches with the new schema.",
				table, have, want)
		}
	}

	// The regexp above only sees standalone CREATE UNIQUE INDEX. A uniqueness
	// constraint declared inline or bolted on with ALTER TABLE would be just as
	// load-bearing and invisible to it, so refuse those outright rather than let one
	// pass as "no new index".
	for _, stmt := range strings.Split(sql, ";") {
		if !strings.Contains(strings.ToUpper(stmt), "UNIQUE") {
			continue
		}
		if regexp.MustCompile(`(?i)^\s*CREATE\s+UNIQUE\s+INDEX`).MatchString(stmt) {
			continue
		}
		for table := range envTableUniqueIndexes {
			if regexp.MustCompile(`(?i)meko_system\.` + table + `\b`).MatchString(stmt) {
				t.Errorf("meko_system.%s gains uniqueness outside a CREATE UNIQUE INDEX, which this test "+
					"cannot inventory:\n%s", table, strings.TrimSpace(stmt))
			}
		}
	}
}

// migrationSQL concatenates every up migration with its comments stripped -- the
// migrations explain these very indexes in prose, so a scan over the raw text would
// match its own documentation.
func migrationSQL(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(controlPlaneMigrations, "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no control-plane migrations at %s: %v", controlPlaneMigrations, err)
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
