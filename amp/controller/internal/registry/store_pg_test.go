package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
)

// TestPgRecordBranchForkValidation is the fork ↔ GC-cutoff validation in
// RecordBranch against a real database — the touch on the ancestor's row,
// its error classification, and the roll-back of the whole registration.
// The Rust stress test (page_store.rs, fork_txn) covers the SQL protocol
// racing a live GC publish; this covers the Go path itself: sentinel
// errors, txn atomicity, and the newly-inserted gate that keeps
// re-records idempotent.
//
// Needs YugabyteDB (the amp_storage migration uses YB-specific HASH DDL),
// so it skips unless AMP_TEST_DATABASE_URL points at one.
func TestPgRecordBranchForkValidation(t *testing.T) {
	ctx := t.Context()
	pool := migratedPool(t)
	s := NewStore(pool)

	// Branches are rows in the control plane's resource model, so they need a
	// project to hang off — there is no separate flat registry to write into.
	projectID := seedProject(t, pool)

	newID := func() string { return strings.ReplaceAll(uuid.NewString(), "-", "") }
	parent := newID()

	// A root branch, with a GC cutoff as the pageserver would have
	// published it: 0/64 = 100.
	if err := s.RecordBranch(ctx, projectID, parent, BranchCreateRecord{
		BranchName: "fork-validation-parent", PgVersion: 18, StartLSN: "0/0",
	}); err != nil {
		t.Fatalf("record parent: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE amp_storage.branch_state SET applied_gc_cutoff_lsn = 100 WHERE branch_id = $1`,
		parent); err != nil {
		t.Fatalf("seed cutoff: %v", err)
	}

	// Each fork needs its own name: branch names are unique among a project's live
	// branches (branches_name_per_project), which the standalone registry did not
	// enforce.
	fork := func(id, name, lsn string) error {
		return s.RecordBranch(ctx, projectID, id, BranchCreateRecord{
			BranchName: name, PgVersion: 18,
			AncestorBranchID: &parent, StartLSN: lsn,
		})
	}
	stateRows := func(id string) int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM amp_storage.branch_state WHERE branch_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count branch_state: %v", err)
		}
		return n
	}
	branchRows := func(id string) int {
		parsed, err := ParseBranchID(id)
		if err != nil {
			t.Fatalf("parse branch id %s: %v", id, err)
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM amp_registry.branches WHERE project_id = $1::uuid AND branch_id = $2::uuid`,
			projectID.String(), parsed.String()).Scan(&n); err != nil {
			t.Fatalf("count branch: %v", err)
		}
		return n
	}

	// Below the cutoff (0/50 = 80 < 100): rejected as invalid (permanent
	// — the cutoff is monotone, a retry cannot succeed), and the whole
	// registration rolls back — neither row lands.
	below := newID()
	if err := fork(below, "fork-validation-below", "0/50"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("fork below cutoff: want ErrInvalid, got %v", err)
	}
	if n := stateRows(below); n != 0 {
		t.Fatalf("rejected fork left a branch_state row")
	}
	// The branch row is the one the product API serves, so a rejected fork must not
	// leave it behind either — that is the atomicity the single table buys.
	if n := branchRows(below); n != 0 {
		t.Fatalf("rejected fork left a branch row")
	}

	// At the cutoff (0/64 = 100): registered.
	at := newID()
	if err := fork(at, "fork-validation-at", "0/64"); err != nil {
		t.Fatalf("fork at cutoff: %v", err)
	}
	if n := stateRows(at); n != 1 {
		t.Fatalf("accepted fork missing its branch_state row")
	}
	if n := branchRows(at); n != 1 {
		t.Fatalf("accepted fork missing its branch row")
	}

	// Re-record after the cutoff advanced past the fork point: still a
	// no-op success — the validation is gated on the child row being
	// newly inserted.
	if _, err := pool.Exec(ctx,
		`UPDATE amp_storage.branch_state SET applied_gc_cutoff_lsn = 200 WHERE branch_id = $1`,
		parent); err != nil {
		t.Fatalf("advance cutoff: %v", err)
	}
	if err := fork(at, "fork-validation-at", "0/64"); err != nil {
		t.Fatalf("idempotent re-record must not re-validate: %v", err)
	}

	// A missing ancestor row: not found.
	missing := newID()
	orphan := newID()
	if err := s.RecordBranch(ctx, projectID, orphan, BranchCreateRecord{
		BranchName: "fork-validation-orphan", PgVersion: 18,
		AncestorBranchID: &missing, StartLSN: "1/0",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fork from missing ancestor: want ErrNotFound, got %v", err)
	}
}

// TestPgRecordBranchUnknownProject pins that this surface cannot conjure a branch
// outside the resource model: with one shared branch table, the project FK is what
// stops a launcher typo from creating an unreachable row.
func TestPgRecordBranchUnknownProject(t *testing.T) {
	ctx := t.Context()
	pool := migratedPool(t)

	err := NewStore(pool).RecordBranch(ctx, uuid.New(), strings.ReplaceAll(uuid.NewString(), "-", ""),
		BranchCreateRecord{BranchName: "orphan", PgVersion: 18, StartLSN: "0/0"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("record into an unknown project: want ErrNotFound, got %v", err)
	}
}

// seedProject inserts the user -> pool -> project ladder a branch needs and
// returns the project id.
//
// The names carry the pool's own id: every test in this package shares one
// scratch database, and storage_pools.name is unique cluster-wide, so a fixed name
// would make the second caller collide.
func seedProject(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	owner, poolID, projectID := uuid.New(), uuid.New(), uuid.New()
	name := "registry-store-test-" + poolID.String()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO amp_registry.users (id) VALUES ($1::uuid)`, []any{owner.String()}},
		{`INSERT INTO amp_registry.storage_pools (id, name, owner_user_id) VALUES ($1::uuid, $2, $3::uuid)`,
			[]any{poolID.String(), name, owner.String()}},
		{`INSERT INTO amp_registry.projects (id, storage_pool_id, name) VALUES ($1::uuid, $2::uuid, $3)`,
			[]any{projectID.String(), poolID.String(), name}},
	} {
		if _, err := pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed %q: %v", stmt.sql, err)
		}
	}
	return projectID
}

// scratchURL is the throwaway database this package's Postgres-backed tests share,
// provisioned once by TestMain. Empty when AMP_TEST_DATABASE_URL is unset, which
// is what makes those tests skip.
var scratchURL string

// migrateSchema is the --db-schema default: the schema the migrator and the pool
// point at. The branch and endpoint tables this store reads live in the fixed
// db.ResourceSchema and are addressed schema-qualified.
const migrateSchema = "controller"

// TestMain provisions one throwaway DATABASE for the whole package and drops it
// afterwards.
//
// A database rather than a schema because the migrations create two fixed schemas
// — the resource model under --db-schema plus the always-`controller` task log —
// so relocating one isolates nothing. Once per package rather than once per test
// because YugabyteDB does not free a dropped keyspace's name synchronously, so a
// drop-and-recreate between two tests in the same binary races itself.
func TestMain(m *testing.M) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	u, drop, err := db.ScratchMigrated(ctx, url, "amp_test_registry_store", migrateSchema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "provision scratch database: %v\n", err)
		os.Exit(1)
	}
	scratchURL = u
	code := m.Run()
	drop()
	os.Exit(code)
}

// migratedPool returns a pool over the package's migrated scratch database, or
// skips the test when there is no database to talk to.
func migratedPool(t *testing.T) *db.Pool {
	t.Helper()
	if scratchURL == "" {
		t.Skip("AMP_TEST_DATABASE_URL not set; these tests need a YugabyteDB")
	}
	pool, err := db.Connect(t.Context(), scratchURL, migrateSchema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestPgForkInheritsPgSettings: a fork starts from its ancestor's settings, and
// the caller's own merge over them — a fork can override one without restating
// the rest.
func TestPgForkInheritsPgSettings(t *testing.T) {
	ctx := t.Context()
	pool := migratedPool(t)
	s := NewStore(pool)
	projectID := seedProject(t, pool)

	parent := FormatBranchID(uuid.New())
	if err := s.RecordBranch(ctx, projectID, parent, BranchCreateRecord{
		BranchName: "settings-parent", PgVersion: 18, StartLSN: "0/0",
		PgSettings: map[string]string{"work_mem": "16MB", "jit": "off"},
	}); err != nil {
		t.Fatalf("record parent: %v", err)
	}

	// A fork that says nothing inherits the whole set.
	plain := FormatBranchID(uuid.New())
	if err := s.RecordBranch(ctx, projectID, plain, BranchCreateRecord{
		BranchName: "settings-plain", PgVersion: 18, StartLSN: "0/16B8",
		AncestorBranchID: &parent,
	}); err != nil {
		t.Fatalf("record plain fork: %v", err)
	}
	if got := readPgSettings(t, pool, plain); got["work_mem"] != "16MB" || got["jit"] != "off" || len(got) != 2 {
		t.Errorf("plain fork pg_settings = %v, want the ancestor's two", got)
	}

	// A fork that overrides one keeps the rest.
	over := FormatBranchID(uuid.New())
	if err := s.RecordBranch(ctx, projectID, over, BranchCreateRecord{
		BranchName: "settings-override", PgVersion: 18, StartLSN: "0/16B8",
		AncestorBranchID: &parent,
		PgSettings:       map[string]string{"work_mem": "64MB", "hash_mem_multiplier": "2"},
	}); err != nil {
		t.Fatalf("record overriding fork: %v", err)
	}
	got := readPgSettings(t, pool, over)
	want := map[string]string{"work_mem": "64MB", "jit": "off", "hash_mem_multiplier": "2"}
	if len(got) != len(want) {
		t.Fatalf("overriding fork pg_settings = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("pg_settings[%q] = %q, want %q", k, got[k], v)
		}
	}

	// A root branch starts empty — there is no ancestor to inherit from.
	root := FormatBranchID(uuid.New())
	if err := s.RecordBranch(ctx, projectID, root, BranchCreateRecord{
		BranchName: "settings-root", PgVersion: 18, StartLSN: "0/0",
	}); err != nil {
		t.Fatalf("record root: %v", err)
	}
	if got := readPgSettings(t, pool, root); len(got) != 0 {
		t.Errorf("root pg_settings = %v, want empty", got)
	}
}

func readPgSettings(t *testing.T, pool *db.Pool, branchID string) map[string]string {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(t.Context(),
		`SELECT pg_settings FROM amp_registry.branches
		  WHERE replace(branch_id::text, '-', '') = $1`, branchID).Scan(&raw); err != nil {
		t.Fatalf("read pg_settings for %s: %v", branchID, err)
	}
	out := map[string]string{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode pg_settings for %s: %v", branchID, err)
	}
	return out
}
