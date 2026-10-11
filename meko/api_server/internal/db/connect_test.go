package db

import (
	"embed"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// golang-migrate builds its version table while constructing the driver, so by the
// time getMigrator returns, meko_system must already exist and the table must have
// been looked for there rather than in whatever the search path resolves to. The
// mock is ordered, so it also pins CREATE SCHEMA ahead of the driver.
func TestControlPlaneMigratorTracksInMekoSystem(t *testing.T) {
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = conn.Close() }()

	mock.ExpectExec("CREATE SCHEMA IF NOT EXISTS meko_system").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT CURRENT_DATABASE\(\)`).
		WillReturnRows(sqlmock.NewRows([]string{"current_database"}).AddRow("meko"))
	mock.ExpectExec("SELECT pg_advisory_lock").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("FROM information_schema.tables").
		WithArgs("meko_system", "schema_migrations_controlplane").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec("SELECT pg_advisory_unlock").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectClose()

	m, err := getMigrator(conn, DBPurposeControlPlane, controlPlaneMigrationsFS, "migrations/controlplane")
	if err != nil {
		t.Fatalf("getMigrator: %v", err)
	}
	if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
		t.Errorf("close: source=%v database=%v", srcErr, dbErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A migration that writes rows has to be the only statement in its file, and must
// not carry DDL alongside -- see the batching rule above systemMigrationsFS. Nothing
// in the boot path notices a violation: it applies cleanly on a quiet database and
// on plain Postgres, and only fails against YugabyteDB with a concurrent writer,
// which is the upgrade of a live fleet.
//
// Down migrations are out of scope: they are never run at boot.
func TestBackfillMigrationsAreSingleStatement(t *testing.T) {
	// Predate the rule and are already applied everywhere. system/000001 seeds tables
	// it creates in the same file, so no other session can be writing them; the other
	// three write tables that already exist and got away with it -- data/000001's
	// INSERT goes to dist_rag.vector_indexes, which no migration creates.
	grandfathered := map[string]bool{
		"migrations/system/000001_meko_platform_schema.up.sql":             true,
		"migrations/system/000016_token_savings_enrichment.up.sql":         true,
		"migrations/data/000001_meko_datapacks_schema.up.sql":              true,
		"migrations/data/000005_conversation_search_chunked_outbox.up.sql": true,
	}

	for _, set := range []struct {
		fs   embed.FS
		root string
	}{
		{systemMigrationsFS, "migrations/system"},
		{dataMigrationsFS, "migrations/data"},
		{controlPlaneMigrationsFS, "migrations/controlplane"},
	} {
		entries, err := set.fs.ReadDir(set.root)
		if err != nil {
			t.Fatalf("ReadDir %s: %v", set.root, err)
		}
		for _, e := range entries {
			name := path.Join(set.root, e.Name())
			if !strings.HasSuffix(name, ".up.sql") || grandfathered[name] {
				continue
			}
			body, err := set.fs.ReadFile(name)
			if err != nil {
				t.Fatalf("ReadFile %s: %v", name, err)
			}
			sql := stripSQLComments(string(body))
			if !dmlPattern.MatchString(sql) {
				continue
			}
			if stmts := splitStatements(sql); len(stmts) != 1 {
				t.Errorf("%s writes rows in %d statements; split it one statement per file", name, len(stmts))
			}
			// Also inside a DO block, where the two would share its transaction just
			// as they would share the file's.
			if loc := ddlPattern.FindString(sql); loc != "" {
				t.Errorf("%s writes rows beside DDL (%q); move the DDL to its own migration", name, loc)
			}
		}
	}
}

// What counts as writing rows and as DDL. UPDATE needs its SET so that an
// `ON UPDATE CASCADE` in a foreign key is not read as a write.
var (
	dmlPattern = regexp.MustCompile(`(?is)\binsert\s+into\b|\bdelete\s+from\b|\bupdate\s+[\w."]+(\s+as\s+\w+)?\s+set\b`)
	ddlPattern = regexp.MustCompile(`(?is)\b(alter|create|drop|truncate)\s+((unique|materialized|or\s+replace)\s+)*(table|index|schema|view|sequence|type|extension|function)\b`)
)

// splitStatements returns the non-empty statements of a comment-stripped migration.
// Dollar-quoted bodies are passed through whole, so a DO block counts as the one
// statement it is -- and the patterns above are matched against the file including
// those bodies, since a backfill hidden in a DO block is still a backfill.
func splitStatements(sql string) []string {
	var stmts []string
	var cur strings.Builder
	add := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			stmts = append(stmts, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(sql); {
		if tag := dollarTag(sql, i); tag != "" {
			end := strings.Index(sql[i+len(tag):], tag)
			if end < 0 { // unterminated -- take the rest verbatim
				cur.WriteString(sql[i:])
				break
			}
			stop := i + len(tag) + end + len(tag)
			cur.WriteString(sql[i:stop])
			i = stop
			continue
		}
		if sql[i] == ';' {
			add()
			i++
			continue
		}
		cur.WriteByte(sql[i])
		i++
	}
	add()
	return stmts
}

// dollarTag returns the dollar-quote tag opening at i ("$$", "$body$"), or "".
func dollarTag(s string, i int) string {
	if s[i] != '$' {
		return ""
	}
	for j := i + 1; j < len(s); j++ {
		switch {
		case s[j] == '$':
			return s[i : j+1]
		case s[j] == '_' || ('a' <= s[j] && s[j] <= 'z') || ('A' <= s[j] && s[j] <= 'Z') ||
			('0' <= s[j] && s[j] <= '9'):
		default:
			return ""
		}
	}
	return ""
}

func stripSQLComments(sql string) string {
	for {
		start := strings.Index(sql, "/*")
		if start < 0 {
			break
		}
		end := strings.Index(sql[start:], "*/")
		if end < 0 {
			break
		}
		sql = sql[:start] + sql[start+end+2:]
	}
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// The check above is only worth having if it fires, and the shapes it has to see are
// not the ones it reads literally: DML hidden in a DO block (this set's usual way of
// writing something idempotent, and how the old 000007 added its constraint), and an
// ON UPDATE clause that is not a write at all.
func TestBackfillRuleDetectors(t *testing.T) {
	for _, c := range []struct {
		name       string
		sql        string
		dml, ddl   bool
		statements int
	}{
		{"lone backfill", "UPDATE meko_system.environments\n SET state = 'DEGRADED'\n WHERE btrim(state) = '';",
			true, false, 1},
		{"backfill then DDL", "UPDATE t SET state='D';\nDO $$ BEGIN ALTER TABLE t ADD CONSTRAINT c CHECK (x); END $$;",
			true, true, 2},
		{"backfill inside a DO block", "DO $$ BEGIN UPDATE t SET state='D'; ALTER TABLE t ADD CONSTRAINT c CHECK (x); END $$;",
			true, true, 1},
		{"CTE before the write", "WITH d AS (SELECT 1) UPDATE t SET state='D' FROM d;",
			true, false, 1},
		{"ON UPDATE is not a write", "CREATE TABLE a (b UUID REFERENCES c (u) ON DELETE CASCADE ON UPDATE CASCADE);",
			false, true, 1},
		{"nor is an updated_at column", "CREATE TABLE a (updated_at TIMESTAMPTZ NOT NULL DEFAULT now());",
			false, true, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			sql := stripSQLComments(c.sql)
			if got := dmlPattern.MatchString(sql); got != c.dml {
				t.Errorf("writes rows = %v, want %v", got, c.dml)
			}
			if got := ddlPattern.MatchString(sql); got != c.ddl {
				t.Errorf("carries DDL = %v, want %v", got, c.ddl)
			}
			if got := splitStatements(sql); len(got) != c.statements {
				t.Errorf("%d statements, want %d: %q", len(got), c.statements, got)
			}
		})
	}
}

// newDataDriver takes a *sql.Conn, so the SET and the migrations share a session
// by type. This pins the order: the SET runs before anything the driver does.
func TestDataMigratorSetsHashPartitionsFirst(t *testing.T) {
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = conn.Close() }()

	mock.ExpectExec("SET meko.hash_partitions = 8").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT CURRENT_DATABASE\(\)`).
		WillReturnRows(sqlmock.NewRows([]string{"current_database"}).AddRow("meko"))
	mock.ExpectQuery(`SELECT CURRENT_SCHEMA\(\)`).
		WillReturnRows(sqlmock.NewRows([]string{"current_schema"}).AddRow("public"))
	mock.ExpectExec("SELECT pg_advisory_lock").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("FROM information_schema.tables").
		WithArgs("public", "schema_migrations").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec("SELECT pg_advisory_unlock").
		WillReturnResult(sqlmock.NewResult(0, 0))

	m, err := getDataMigrator(conn, 8)
	if err != nil {
		t.Fatalf("getDataMigrator: %v", err)
	}
	if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
		t.Errorf("close: source=%v database=%v", srcErr, dbErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestRunMigrationsRejectsHashPartitionsOutOfRange(t *testing.T) {
	for _, n := range []int{0, -1, maxHashPartitions + 1} {
		if _, err := RunMigrations(nil, nil, MigrationScope{Data: true}, n); err == nil {
			t.Errorf("hash partitions %d: want an error", n)
		}
	}
}

// A partition loop with a literal MODULUS ignores --hash-partitions and builds
// the default count on every install.
func TestDataPartitionLoopsReadHashPartitions(t *testing.T) {
	entries, err := dataMigrationsFS.ReadDir("migrations/data")
	if err != nil {
		t.Fatal(err)
	}
	literal := regexp.MustCompile(`(?i)MODULUS\s+\d`)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		body, err := dataMigrationsFS.ReadFile(path.Join("migrations/data", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sql := string(body)
		if !strings.Contains(sql, "PARTITION OF") {
			continue
		}
		if literal.MatchString(sql) {
			t.Errorf("%s: partition loop has a literal MODULUS", e.Name())
		}
		if !strings.Contains(sql, "current_setting('"+hashPartitionsSetting+"', true)") {
			t.Errorf("%s: partition loop does not read %s", e.Name(), hashPartitionsSetting)
		}
	}
}
