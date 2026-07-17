package features

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps/workloadgen"
)

// Querier is the DB surface the workload helpers use — the same structural
// interface workloadgen consumes, so a *pgxpool.Pool satisfies it and unit
// tests fake it. Aliased here so callers need not import workloadgen directly.
type Querier = workloadgen.Querier

// Connector opens a Querier scoped to a named database and returns a close func.
// The layer-1 helper uses it to reach each database it creates; unit tests
// inject a fake that never touches a real cluster.
type Connector func(ctx context.Context, dbName string) (q Querier, closeFn func(), err error)

// AdminReconnect opens a fresh Querier bound to the backing cluster's
// maintenance database (never one of the databases RunNative creates and
// later drops). The DROP DATABASE cleanups RunNative registers run at final
// teardown — long after the admin connection used to CREATE the databases has
// been closed by the workload phase — so they must never reuse that admin
// Querier and must instead reconnect through this.
type AdminReconnect func(ctx context.Context) (q Querier, closeFn func(), err error)

// RegisterCleanup registers a best-effort teardown callback (run LIFO by the
// lifecycle). The layer-1 helper registers a DROP DATABASE per database it
// creates, so the data survives the run for the cross-layer check and is only
// removed at final teardown.
type RegisterCleanup func(name string, fn func(context.Context) error)

// NativeSpec configures the YB-native (layer-1) workload: NumDBs databases,
// each with TablesPerDB verifiable tables (created with SPLIT INTO Tablets
// TABLETS + a secondary index) loaded with RowsPerTable rows. All names derive
// from Prefix (which carries the test seed) so parallel tests never collide.
type NativeSpec struct {
	Prefix       string
	NumDBs       int
	TablesPerDB  int
	RowsPerTable int
	Tablets      int
	Seed         int64
	Concurrency  int
	BatchSize    int
}

func (s NativeSpec) concurrency() int {
	if s.Concurrency > 0 {
		return s.Concurrency
	}
	return 1
}

// dbName is the i'th database name (0-indexed), seed-unique via Prefix.
func (s NativeSpec) dbName(i int) string { return fmt.Sprintf("%s_db_%d", s.Prefix, i) }

// wgSpec is the workloadgen spec for database i's verifiable tables.
func (s NativeSpec) wgSpec(i int) workloadgen.Spec {
	return workloadgen.Spec{
		TablePrefix:  fmt.Sprintf("%s_%d", s.Prefix, i),
		NumTables:    s.TablesPerDB,
		RowsPerTable: s.RowsPerTable,
		BatchSize:    s.BatchSize,
	}
}

// scratchTable is database i's DDL-exercise scratch table name.
func (s NativeSpec) scratchTable(i int) string { return fmt.Sprintf("%s_%d_scratch", s.Prefix, i) }

// DBResult is RunNative's per-database outcome.
type DBResult struct {
	DBName string
	Spec   workloadgen.Spec
	Report workloadgen.Report
}

// NativeReport is RunNative's outcome across every database (order stable).
type NativeReport struct {
	DBs []DBResult
	OK  bool
}

// RunNative runs the layer-1 YB-native workload: it creates NumDBs databases on
// the cluster (via admin — a Querier on the maintenance database), registers a
// DROP DATABASE cleanup for each, then across the databases concurrently
// (bounded by Concurrency) creates tablet-split tables + index, exercises a
// range of DDL/DML on a scratch table, loads the verifiable tables, and checks
// them with the workloadgen checksum oracle. The databases are left in place
// (removed only by the registered cleanups) so the cross-layer check can
// re-verify the data after amp is deployed.
//
// The registered DROP cleanups run at final teardown, well after admin's pool
// has been closed by the workload phase, so they never reuse admin — each
// opens its own short-lived connection via reconnectAdmin — and they are
// truly best-effort: a reconnect or drop failure is logged via logf (if
// non-nil) but never returned, so a cleanup hiccup never fails an otherwise
// green test.
func RunNative(ctx context.Context, admin Querier, connect Connector, reconnectAdmin AdminReconnect, register RegisterCleanup, spec NativeSpec, logf func(string, ...any)) (NativeReport, error) {
	if spec.NumDBs <= 0 {
		return NativeReport{}, fmt.Errorf("foundation native: NumDBs must be > 0")
	}
	if spec.TablesPerDB <= 0 {
		return NativeReport{}, fmt.Errorf("foundation native: TablesPerDB must be > 0")
	}

	// Create databases first (serially on the single admin connection), each
	// with its own DROP cleanup, before fanning out per-database work.
	for i := 0; i < spec.NumDBs; i++ {
		db := spec.dbName(i)
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoteIdent(db)); err != nil {
			return NativeReport{}, fmt.Errorf("create database %s: %w", db, err)
		}
		register("drop database "+db, func(ctx context.Context) error {
			dropDatabaseBestEffort(ctx, reconnectAdmin, db, logf)
			return nil
		})
	}

	results := make([]DBResult, spec.NumDBs)
	sem := make(chan struct{}, spec.concurrency())
	var wg sync.WaitGroup
	errs := make(chan error, spec.NumDBs)
	for i := 0; i < spec.NumDBs; i++ {
		i := i
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res, err := runNativeDB(ctx, connect, spec, i)
			if err != nil {
				errs <- err
				return
			}
			results[i] = res
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return NativeReport{}, err
		}
	}

	report := NativeReport{OK: true, DBs: results}
	for _, r := range results {
		if !r.Report.OK {
			report.OK = false
		}
	}
	return report, nil
}

// runNativeDB does one database's layer-1 work: connect, tablet-split tables +
// index, a DDL/DML scratch exercise, then workloadgen load + verify.
func runNativeDB(ctx context.Context, connect Connector, spec NativeSpec, i int) (DBResult, error) {
	db := spec.dbName(i)
	q, closeFn, err := connect(ctx, db)
	if err != nil {
		return DBResult{}, fmt.Errorf("connect %s: %w", db, err)
	}
	defer closeFn()

	wg := spec.wgSpec(i)
	for tbl := 0; tbl < wg.NumTables; tbl++ {
		name := wg.TableName(tbl)
		if err := createVerifiableTable(ctx, q, name, spec.Tablets); err != nil {
			return DBResult{}, fmt.Errorf("db %s: %w", db, err)
		}
	}
	if err := ddlDMLExercise(ctx, q, spec.scratchTable(i), spec.Tablets, spec.Seed+int64(i)); err != nil {
		return DBResult{}, fmt.Errorf("db %s ddl/dml exercise: %w", db, err)
	}
	if err := workloadgen.Load(ctx, q, wg, spec.Seed, nil); err != nil {
		return DBResult{}, fmt.Errorf("db %s load: %w", db, err)
	}
	report, err := workloadgen.Verify(ctx, q, wg, spec.Seed)
	if err != nil {
		return DBResult{}, fmt.Errorf("db %s verify: %w", db, err)
	}
	return DBResult{DBName: db, Spec: wg, Report: report}, nil
}

// createVerifiableTable creates one workloadgen-shaped table with the YB tablet
// DDL (SPLIT INTO N TABLETS when tablets > 0) plus a secondary index — the
// tablet-creation + distributed-DDL path workloadgen alone doesn't exercise.
func createVerifiableTable(ctx context.Context, q Querier, name string, tablets int) error {
	ddl := fmt.Sprintf(
		"CREATE TABLE %s (id BIGINT PRIMARY KEY, seq BIGINT NOT NULL, payload TEXT NOT NULL, checksum TEXT NOT NULL)",
		name)
	if tablets > 0 {
		ddl += fmt.Sprintf(" SPLIT INTO %d TABLETS", tablets)
	}
	if _, err := q.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("create tablet-split table %s: %w", name, err)
	}
	if _, err := q.Exec(ctx, fmt.Sprintf("CREATE INDEX %s_seq_idx ON %s (seq)", name, name)); err != nil {
		return fmt.Errorf("create index on %s: %w", name, err)
	}
	return nil
}

// ddlDMLExercise runs a range of DDL variations (create-with-tablets, add
// column, create index, truncate, drop column) and DML (insert/update/delete)
// against a scratch table, asserting the row count the applied DML implies.
// Deterministic in the row volume for a given seed.
func ddlDMLExercise(ctx context.Context, q Querier, table string, tablets int, seed int64) error {
	if err := createVerifiableTable(ctx, q, table, tablets); err != nil {
		return err
	}
	if _, err := q.Exec(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN note TEXT", table)); err != nil {
		return fmt.Errorf("add column: %w", err)
	}

	r := rand.New(rand.NewPCG(uint64(seed), 0x9e3779b97f4a7c15))
	n := 50 + r.IntN(50) // 50..99 rows
	for id := 0; id < n; id++ {
		payload := workloadgen.DerivePayload(seed, table, int64(id))
		if _, err := q.Exec(ctx,
			fmt.Sprintf("INSERT INTO %s (id, seq, payload, checksum, note) VALUES ($1,$2,$3,$4,$5)", table),
			id, id, payload, workloadgen.Checksum(payload), "n"); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
	}
	// Update half, delete a few — exercised, not verified here (truncate follows).
	for id := 0; id < n/2; id++ {
		if _, err := q.Exec(ctx, fmt.Sprintf("UPDATE %s SET note=$1 WHERE id=$2", table), "u", id); err != nil {
			return fmt.Errorf("update: %w", err)
		}
	}
	for id := 0; id < n/10; id++ {
		if _, err := q.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE id=$1", table), id); err != nil {
			return fmt.Errorf("delete: %w", err)
		}
	}
	if _, err := q.Exec(ctx, "TRUNCATE TABLE "+table); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	count, err := scanCount(ctx, q, "SELECT count(*) FROM "+table)
	if err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("scratch %s count after truncate = %d, want 0", table, count)
	}
	if _, err := q.Exec(ctx, fmt.Sprintf("ALTER TABLE %s DROP COLUMN note", table)); err != nil {
		return fmt.Errorf("drop column: %w", err)
	}
	if _, err := q.Exec(ctx, "DROP TABLE "+table); err != nil {
		return fmt.Errorf("drop table: %w", err)
	}
	return nil
}

// AmpSpec configures the layer-2 amp-compute workload: NumSchemas schemas
// exercised in parallel for DDL/DML, plus a set of verifiable workloadgen
// tables loaded and checked against amp's compute. Names derive from Prefix.
type AmpSpec struct {
	Prefix       string
	NumSchemas   int
	TablesPerSet int
	RowsPerTable int
	Seed         int64
	Concurrency  int
	BatchSize    int
}

func (s AmpSpec) concurrency() int {
	if s.Concurrency > 0 {
		return s.Concurrency
	}
	return 1
}

func (s AmpSpec) wgSpec() workloadgen.Spec {
	return workloadgen.Spec{
		TablePrefix:  s.Prefix + "_amp",
		NumTables:    s.TablesPerSet,
		RowsPerTable: s.RowsPerTable,
		BatchSize:    s.BatchSize,
	}
}

func (s AmpSpec) schemaName(i int) string { return fmt.Sprintf("%s_s%d", s.Prefix, i) }

// RunAmpCompute runs the layer-2 workload against amp's Postgres endpoint on a
// single pool: a parallel multi-schema DDL/DML exercise (create schema + table,
// insert rows, verify the count, drop) across NumSchemas schemas, plus the
// workloadgen schema/load/verify oracle over the verifiable tables. Amp compute
// is plain Postgres, so no tablet DDL here — that is the YB-native layer's job.
func RunAmpCompute(ctx context.Context, pool Querier, spec AmpSpec) (workloadgen.Report, error) {
	if spec.NumSchemas <= 0 {
		return workloadgen.Report{}, fmt.Errorf("foundation amp: NumSchemas must be > 0")
	}
	if err := parallelSchemaExercise(ctx, pool, spec); err != nil {
		return workloadgen.Report{}, err
	}

	wg := spec.wgSpec()
	if err := workloadgen.Schema(ctx, pool, wg); err != nil {
		return workloadgen.Report{}, fmt.Errorf("amp schema: %w", err)
	}
	if err := workloadgen.Load(ctx, pool, wg, spec.Seed, nil); err != nil {
		return workloadgen.Report{}, fmt.Errorf("amp load: %w", err)
	}
	return workloadgen.Verify(ctx, pool, wg, spec.Seed)
}

// parallelSchemaExercise creates NumSchemas schemas concurrently, each with a
// table it loads with a seed-determined row count and verifies, then drops.
func parallelSchemaExercise(ctx context.Context, pool Querier, spec AmpSpec) error {
	sem := make(chan struct{}, spec.concurrency())
	var wg sync.WaitGroup
	errs := make(chan error, spec.NumSchemas)
	for i := 0; i < spec.NumSchemas; i++ {
		i := i
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := oneSchemaExercise(ctx, pool, spec, i); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func oneSchemaExercise(ctx context.Context, pool Querier, spec AmpSpec, i int) error {
	schema := spec.schemaName(i)
	table := schema + ".t"
	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoteIdent(schema)); err != nil {
		return fmt.Errorf("create schema %s: %w", schema, err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE TABLE %s (id BIGINT PRIMARY KEY, v BIGINT NOT NULL)", table)); err != nil {
		return fmt.Errorf("create %s: %w", table, err)
	}
	r := rand.New(rand.NewPCG(uint64(spec.Seed), uint64(i)+1))
	n := 20 + r.IntN(30) // 20..49 rows
	for id := 0; id < n; id++ {
		if _, err := pool.Exec(ctx, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1,$2)", table), id, int64(id)*2); err != nil {
			return fmt.Errorf("insert %s: %w", table, err)
		}
	}
	count, err := scanCount(ctx, pool, "SELECT count(*) FROM "+table)
	if err != nil {
		return err
	}
	if int(count) != n {
		return fmt.Errorf("schema %s count = %d, want %d", schema, count, n)
	}
	if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+quoteIdent(schema)+" CASCADE"); err != nil {
		return fmt.Errorf("drop schema %s: %w", schema, err)
	}
	return nil
}

// scanCount runs a single-int query via the Querier's QueryRow.
func scanCount(ctx context.Context, q Querier, sql string, args ...any) (int64, error) {
	var n int64
	if err := q.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count: %w", err)
	}
	return n, nil
}

// quoteIdent double-quotes a SQL identifier, escaping embedded quotes.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// dropDatabaseBestEffort reconnects fresh (never the closed workload pool) and
// drops db, logging any failure via logf instead of returning it — reconnect
// and drop errors here must never fail the test.
func dropDatabaseBestEffort(ctx context.Context, reconnectAdmin AdminReconnect, db string, logf func(string, ...any)) {
	q, closeFn, err := reconnectAdmin(ctx)
	if err != nil {
		if logf != nil {
			logf("foundation teardown: drop database %s (best-effort, ignored): reconnect: %v", db, err)
		}
		return
	}
	defer closeFn()
	if _, err := q.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(db)); err != nil && logf != nil {
		logf("foundation teardown: drop database %s (best-effort, ignored): %v", db, err)
	}
}
