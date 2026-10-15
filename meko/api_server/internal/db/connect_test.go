package db

import (
	"context"
	"embed"
	"fmt"
	"path"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/golang-migrate/migrate/v4/database/postgres"
)

func TestPoolConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  PoolConfig
		wantErr bool
	}{
		{
			name: "positive limits",
			config: PoolConfig{
				MaxLifetime: 15 * time.Minute,
				MaxIdleTime: 2 * time.Minute,
			},
		},
		{
			name:   "zero disables limits",
			config: PoolConfig{},
		},
		{
			name: "negative lifetime",
			config: PoolConfig{
				MaxLifetime: -time.Second,
			},
			wantErr: true,
		},
		{
			name: "negative idle time",
			config: PoolConfig{
				MaxIdleTime: -time.Second,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

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

// A container killed during data migration 1 leaves version 1 dirty, and golang-migrate
// then refuses to run anything. The recovery has to clear exactly that row and say
// whether it did, so the retry is visible in the log.
func TestRecoverInterruptedFirstDataMigration(t *testing.T) {
	const stmt = `DELETE FROM "schema_migrations" WHERE version = 1 AND dirty`
	for name, tc := range map[string]struct {
		affected int64
		want     bool
	}{
		"dirty at version 1 is cleared":           {affected: 1, want: true},
		"clean, or past version 1, is left alone": {affected: 0, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			conn, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer func() { _ = conn.Close() }()
			mock.ExpectExec(stmt).WillReturnResult(sqlmock.NewResult(0, tc.affected))

			got, err := recoverInterruptedFirstDataMigration(context.Background(), conn)
			if err != nil {
				t.Fatalf("recoverInterruptedFirstDataMigration: %v", err)
			}
			if got != tc.want {
				t.Errorf("recovered = %v, want %v", got, tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestRecoverInterruptedFirstDataMigrationSurfacesErrors(t *testing.T) {
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = conn.Close() }()
	mock.ExpectExec(`DELETE FROM "schema_migrations"`).WillReturnError(fmt.Errorf("connection reset"))

	if _, err := recoverInterruptedFirstDataMigration(context.Background(), conn); err == nil {
		t.Error("want the database error, got nil")
	}
}

// Migration 1 is rerun from the top after an interrupted first install, so every
// statement that cannot be repeated is a statement that wedges that install again.
// retryUnsafe returns the statements in sql that would fail or duplicate on a second
// run: DDL without IF NOT EXISTS, an ALTER or DROP without IF [NOT] EXISTS, an INSERT
// without ON CONFLICT, a CREATE TYPE with no existence check, and any MAGE call (a
// graph cannot be created twice, and the extension is no longer required).
func retryUnsafe(sql string) []string {
	sql = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(sql, "")
	sql = regexp.MustCompile(`(?m)--.*$`).ReplaceAllString(sql, "")

	// The text of the statement starting at m, up to its terminating semicolon.
	stmt := func(m []int) string {
		end := strings.Index(sql[m[0]:], ";")
		if end < 0 {
			return sql[m[0]:]
		}
		return sql[m[0] : m[0]+end]
	}

	var bad []string
	createDDL := regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?(?:TABLE|INDEX|SCHEMA|EXTENSION|VIEW)\b`)
	for _, m := range createDDL.FindAllStringIndex(sql, -1) {
		if !regexp.MustCompile(`(?i)^CREATE\s+(?:UNIQUE\s+)?\w+\s+IF\s+NOT\s+EXISTS\b`).MatchString(sql[m[0]:]) {
			bad = append(bad, "no IF NOT EXISTS: "+strings.TrimSpace(stmt(m)))
		}
	}
	alterDrop := regexp.MustCompile(`(?i)\b(?:ALTER\s+TABLE|DROP\s+(?:TABLE|INDEX|SCHEMA|TYPE|COLUMN))\b`)
	for _, m := range alterDrop.FindAllStringIndex(sql, -1) {
		if !regexp.MustCompile(`(?i)\bIF\s+(?:NOT\s+)?EXISTS\b`).MatchString(stmt(m)) {
			bad = append(bad, "no IF [NOT] EXISTS: "+strings.TrimSpace(stmt(m)))
		}
	}
	for _, m := range regexp.MustCompile(`(?i)\bINSERT\s+INTO\b`).FindAllStringIndex(sql, -1) {
		if !regexp.MustCompile(`(?i)\bON\s+CONFLICT\b`).MatchString(stmt(m)) {
			bad = append(bad, "INSERT without ON CONFLICT: "+strings.TrimSpace(stmt(m)))
		}
	}
	for _, m := range regexp.MustCompile(`(?i)\bCREATE\s+TYPE\s+(\S+)`).FindAllStringSubmatch(sql, -1) {
		// A schema-qualified type is guarded by a pg_type lookup on its bare name.
		_, name, qualified := strings.Cut(m[1], ".")
		if !qualified {
			bad = append(bad, "CREATE TYPE "+m[1]+" is not schema-qualified, so its guard cannot be checked")
			continue
		}
		if !strings.Contains(sql, "typname = '"+name+"'") {
			bad = append(bad, "CREATE TYPE "+m[1]+" has no existence check")
		}
	}
	if strings.Contains(sql, "mag_catalog") {
		bad = append(bad, "calls mag_catalog")
	}
	return bad
}

func TestFirstDataMigrationIsRetrySafe(t *testing.T) {
	body, err := dataMigrationsFS.ReadFile("migrations/data/000001_meko_datapacks_schema.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range retryUnsafe(string(body)) {
		t.Errorf("000001 is not safe to rerun after an interrupted first install: %s", s)
	}
}

// The rag workers embed documents, and the MCP server embeds search queries, from the
// embedder these rows name. Hosted dev/prod use Bedrock Titan v1 (a fixed 1536 dimensions,
// the width of every vector column), so an install seeded with another embedder disagrees
// with what the rest of the platform assumes. Pinned so a later edit cannot quietly put an
// OpenAI row back.
func TestVectorIndexRowsSeedBedrockTitan(t *testing.T) {
	want := []string{
		"'AWS_BEDROCK'::dist_rag.ai_provider_enum",
		`"model": "amazon.titan-embed-text-v1"`,
		`"dimensions": 1536`,
	}

	// knowledge_bases is a plain row.
	file := "migrations/data/000001_meko_datapacks_schema.up.sql"
	body, err := dataMigrationsFS.ReadFile(file)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", file, err)
	}
	var row string
	for _, stmt := range splitStatements(stripSQLComments(string(body))) {
		if strings.Contains(stmt, "INSERT INTO dist_rag.vector_indexes") {
			row = stmt
		}
	}
	if row == "" || !strings.Contains(row, "'knowledge_bases'") {
		t.Fatalf("%s no longer inserts the knowledge_bases dist_rag.vector_indexes row", file)
	}
	for _, w := range want {
		if !strings.Contains(row, w) {
			t.Errorf("%s: the knowledge_bases row does not contain %s", file, w)
		}
	}
}

// conversation_history_search must be REGISTERED, not just have a row: the conversations
// worker only embeds a table with a column_embedding_registrations row, and
// init_column_embedding inserts the vector_indexes row itself under a unique index_name, so
// a bare INSERT of that row makes the registration fail. Pinned to the registration calls
// and to the same embedder as knowledge_bases.
func TestConversationIndexIsRegisteredNotInserted(t *testing.T) {
	file := "migrations/data/000007_conversation_history_search_embedding_registration.up.sql"
	body, err := dataMigrationsFS.ReadFile(file)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", file, err)
	}
	sql := stripSQLComments(string(body))
	if strings.Contains(sql, "INSERT INTO dist_rag.vector_indexes") {
		t.Errorf("%s inserts the vector_indexes row itself; init_column_embedding creates it, and a "+
			"row inserted first makes that call fail on index_name", file)
	}
	for _, want := range []string{
		"dist_rag.create_column_embedding_mapping(",
		"dist_rag.init_column_embedding(",
		"r_registration_name            := 'conversation_history_search'",
		"'AWS_BEDROCK'::dist_rag.ai_provider_enum",
		`"model": "amazon.titan-embed-text-v1"`,
		`"dimensions": 1536`,
		// An install that already has the registration or the row must be left alone.
		"dist_rag.column_embedding_registrations",
		"dist_rag.vector_indexes",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("%s does not contain %s", file, want)
		}
	}
}

// The detector is only worth trusting if it catches each shape of statement it claims
// to, so pin one bad and one good example of each.
func TestRetryUnsafeDetector(t *testing.T) {
	for name, tc := range map[string]struct {
		sql string
		bad bool
	}{
		"create table":         {"CREATE TABLE a.b (x int);", true},
		"create table guarded": {"CREATE TABLE IF NOT EXISTS a.b (x int);", false},
		"unique index":         {"CREATE UNIQUE INDEX i ON a.b (x);", true},
		"unique index guarded": {"CREATE UNIQUE INDEX IF NOT EXISTS i ON a.b (x);", false},
		"alter table":          {"ALTER TABLE a.b ADD COLUMN c int;", true},
		"alter table guarded":  {"ALTER TABLE a.b ADD COLUMN IF NOT EXISTS c int;", false},
		"drop":                 {"DROP TABLE a.b;", true},
		"drop guarded":         {"DROP TABLE IF EXISTS a.b;", false},
		"insert":               {"INSERT INTO a.b VALUES (1);", true},
		"insert guarded":       {"INSERT INTO a.b VALUES (1) ON CONFLICT DO NOTHING;", false},
		"type unguarded":       {"CREATE TYPE a.t AS ENUM ('x');", true},
		"type guarded": {
			"IF NOT EXISTS (SELECT 1 WHERE t.typname = 't') THEN CREATE TYPE a.t AS ENUM ('x');", false,
		},
		"unqualified type":           {"CREATE TYPE t AS ENUM ('x');", true},
		"mage":                       {"SELECT mag_catalog.create_graph('g');", true},
		"commented out is ignored":   {"-- CREATE TABLE a.b (x int);\n/* INSERT INTO a.b VALUES (1); */", false},
		"format string in a DO body": {"EXECUTE format('CREATE TABLE IF NOT EXISTS a.b_%s (x int)', 1);", false},
	} {
		if got := len(retryUnsafe(tc.sql)) > 0; got != tc.bad {
			t.Errorf("%s: flagged = %v, want %v (%v)", name, got, tc.bad, retryUnsafe(tc.sql))
		}
	}
}

// The recovery DELETE must hit the table the data migrator actually writes. Both come
// from getPostgresConfig, so a table or schema added to the data config reaches the
// DELETE too; this pins the shape for each way a config can name it.
func TestMigrationsTableRef(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  *postgres.Config
		want string
	}{
		"default table, search path": {&postgres.Config{}, `"schema_migrations"`},
		"named table": {
			&postgres.Config{MigrationsTable: "schema_migrations_system"}, `"schema_migrations_system"`,
		},
		"named table and schema": {
			&postgres.Config{MigrationsTable: "schema_migrations_controlplane", SchemaName: "meko_system"},
			`"meko_system"."schema_migrations_controlplane"`,
		},
		"identifier is quoted": {&postgres.Config{MigrationsTable: `a"b`}, `"a""b"`},
	} {
		if got := migrationsTableRef(tc.cfg); got != tc.want {
			t.Errorf("%s: migrationsTableRef = %s, want %s", name, got, tc.want)
		}
	}
	if got := migrationsTableRef(getPostgresConfig(DBPurposeData)); got != `"schema_migrations"` {
		t.Errorf("data set tracks in %s, but the recovery statement is written for the default table", got)
	}
}
