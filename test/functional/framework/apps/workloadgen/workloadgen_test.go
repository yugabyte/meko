package workloadgen_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps/workloadgen"
)

// --- fake Querier ---

type call struct {
	sql  string
	args []any
}

// fakeQuerier is a scripted fake satisfying workloadgen.Querier: it records
// every Exec and answers Exec/Query/QueryRow via injectable functions
// (defaulting to "1 row affected" / empty result), so tests never touch a
// real database.
type fakeQuerier struct {
	mu         sync.Mutex
	execs      []call
	execFn     func(sql string, args []any) (pgconn.CommandTag, error)
	queryFn    func(sql string, args []any) (pgx.Rows, error)
	queryRowFn func(sql string, args []any) pgx.Row
}

func (f *fakeQuerier) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.mu.Lock()
	f.execs = append(f.execs, call{sql, args})
	f.mu.Unlock()
	if f.execFn != nil {
		return f.execFn(sql, args)
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (f *fakeQuerier) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	if f.queryFn != nil {
		return f.queryFn(sql, args)
	}
	return &fakeRows{}, nil
}

func (f *fakeQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if f.queryRowFn != nil {
		return f.queryRowFn(sql, args)
	}
	return fakeRow{vals: []any{int64(0)}}
}

func (f *fakeQuerier) recordedExecs() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]call, len(f.execs))
	copy(out, f.execs)
	return out
}

// fakeRows implements pgx.Rows over an in-memory slice of scan-target rows.
type fakeRows struct {
	rows [][]any
	idx  int
	err  error
}

func (r *fakeRows) Close()                                       {}
func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }

func (r *fakeRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	for i, d := range dest {
		switch v := d.(type) {
		case *int64:
			*v = row[i].(int64)
		case *string:
			*v = row[i].(string)
		default:
			return fmt.Errorf("unsupported scan dest %T", d)
		}
	}
	return nil
}

func (r *fakeRows) Values() ([]any, error) { return r.rows[r.idx-1], nil }

// fakeRow implements pgx.Row.
type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		switch v := d.(type) {
		case *int64:
			*v = r.vals[i].(int64)
		case *string:
			*v = r.vals[i].(string)
		default:
			return fmt.Errorf("unsupported scan dest %T", d)
		}
	}
	return nil
}

// --- DerivePayload / Checksum determinism ---

func TestDerivePayloadDeterministic(t *testing.T) {
	a := workloadgen.DerivePayload(42, "t_0", 7)
	b := workloadgen.DerivePayload(42, "t_0", 7)
	if a != b {
		t.Fatalf("same (seed, table, row) produced different payloads:\n%q\n%q", a, b)
	}
	if len(a) == 0 {
		t.Fatal("payload must not be empty")
	}
}

func TestDerivePayloadVariesByInput(t *testing.T) {
	base := workloadgen.DerivePayload(42, "t_0", 7)
	if got := workloadgen.DerivePayload(43, "t_0", 7); got == base {
		t.Error("different seed produced the same payload")
	}
	if got := workloadgen.DerivePayload(42, "t_1", 7); got == base {
		t.Error("different table produced the same payload")
	}
	if got := workloadgen.DerivePayload(42, "t_0", 8); got == base {
		t.Error("different row produced the same payload")
	}
}

func TestChecksumIsSHA256Hex(t *testing.T) {
	sum := workloadgen.Checksum("hello")
	// sha256("hello") is a well-known value.
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if sum != want {
		t.Errorf("checksum(%q) = %s, want %s", "hello", sum, want)
	}
	if len(sum) != 64 {
		t.Errorf("hex sha256 must be 64 chars, got %d", len(sum))
	}
}

// --- Schema ---

func TestSchemaCreatesEveryTable(t *testing.T) {
	q := &fakeQuerier{}
	spec := workloadgen.Spec{TablePrefix: "wg", NumTables: 3}
	if err := workloadgen.Schema(context.Background(), q, spec); err != nil {
		t.Fatalf("schema: %v", err)
	}
	execs := q.recordedExecs()
	if len(execs) != 3 {
		t.Fatalf("got %d CREATE TABLE execs, want 3", len(execs))
	}
	for i, c := range execs {
		want := spec.TableName(i)
		if !strings.Contains(c.sql, "CREATE TABLE IF NOT EXISTS "+want) {
			t.Errorf("exec %d = %q, want it to create %s", i, c.sql, want)
		}
	}
}

func TestSchemaRejectsZeroTables(t *testing.T) {
	q := &fakeQuerier{}
	if err := workloadgen.Schema(context.Background(), q, workloadgen.Spec{}); err == nil {
		t.Error("want an error for NumTables=0")
	}
}

// --- Load / batch splitting ---

func TestLoadBatchSplitting(t *testing.T) {
	q := &fakeQuerier{}
	spec := workloadgen.Spec{TablePrefix: "wg", NumTables: 1, RowsPerTable: 1250, BatchSize: 500}
	if err := workloadgen.Load(context.Background(), q, spec, 1, nil); err != nil {
		t.Fatalf("load: %v", err)
	}
	execs := q.recordedExecs()
	if len(execs) != 3 {
		t.Fatalf("got %d insert batches, want 3 (500+500+250)", len(execs))
	}
	// Each batch's arg count is 4 per row.
	wantRows := []int{500, 500, 250}
	for i, c := range execs {
		if got := len(c.args) / 4; got != wantRows[i] {
			t.Errorf("batch %d has %d rows, want %d", i, got, wantRows[i])
		}
	}
}

func TestLoadProgressCallback(t *testing.T) {
	q := &fakeQuerier{}
	spec := workloadgen.Spec{TablePrefix: "wg", NumTables: 1, RowsPerTable: 100, BatchSize: 40}
	var mu sync.Mutex
	var seen []workloadgen.Progress
	err := workloadgen.Load(context.Background(), q, spec, 1, func(p workloadgen.Progress) {
		mu.Lock()
		seen = append(seen, p)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(seen) != 3 {
		t.Fatalf("got %d progress calls, want 3", len(seen))
	}
	if seen[len(seen)-1].RowsDone != 100 {
		t.Errorf("final progress RowsDone = %d, want 100", seen[len(seen)-1].RowsDone)
	}
}

func TestLoadInsertsDeterministicPayloads(t *testing.T) {
	q := &fakeQuerier{}
	spec := workloadgen.Spec{TablePrefix: "t", NumTables: 1, RowsPerTable: 3, BatchSize: 10}
	seed := int64(99)
	if err := workloadgen.Load(context.Background(), q, spec, seed, nil); err != nil {
		t.Fatalf("load: %v", err)
	}
	execs := q.recordedExecs()
	if len(execs) != 1 {
		t.Fatalf("want 1 batch, got %d", len(execs))
	}
	args := execs[0].args
	for row := 0; row < 3; row++ {
		gotPayload := args[row*4+2].(string)
		gotChecksum := args[row*4+3].(string)
		wantPayload := workloadgen.DerivePayload(seed, spec.TableName(0), int64(row))
		if gotPayload != wantPayload {
			t.Errorf("row %d payload = %q, want %q", row, gotPayload, wantPayload)
		}
		if gotChecksum != workloadgen.Checksum(wantPayload) {
			t.Errorf("row %d checksum mismatch", row)
		}
	}
}

// --- Verify ---

func expectedChecksums(spec workloadgen.Spec, seed int64, tableIdx, n int) []string {
	table := spec.TableName(tableIdx)
	out := make([]string, n)
	for row := 0; row < n; row++ {
		out[row] = workloadgen.Checksum(workloadgen.DerivePayload(seed, table, int64(row)))
	}
	return out
}

func TestVerifyMatch(t *testing.T) {
	spec := workloadgen.Spec{TablePrefix: "wg", NumTables: 1, RowsPerTable: 5}
	seed := int64(7)
	checksums := expectedChecksums(spec, seed, 0, 5)

	q := &fakeQuerier{
		queryRowFn: func(sql string, args []any) pgx.Row {
			return fakeRow{vals: []any{int64(5)}}
		},
		queryFn: func(sql string, args []any) (pgx.Rows, error) {
			rows := make([][]any, len(checksums))
			for i, c := range checksums {
				rows[i] = []any{c}
			}
			return &fakeRows{rows: rows}, nil
		},
	}
	report, err := workloadgen.Verify(context.Background(), q, spec, seed)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !report.OK {
		t.Fatalf("report.OK = false, want true: %+v", report)
	}
	if len(report.Tables) != 1 || !report.Tables[0].OK {
		t.Fatalf("table report not OK: %+v", report.Tables)
	}
}

func TestVerifyCountMismatch(t *testing.T) {
	spec := workloadgen.Spec{TablePrefix: "wg", NumTables: 1, RowsPerTable: 5}
	seed := int64(7)
	checksums := expectedChecksums(spec, seed, 0, 5)

	q := &fakeQuerier{
		queryRowFn: func(sql string, args []any) pgx.Row {
			return fakeRow{vals: []any{int64(4)}} // one row missing
		},
		queryFn: func(sql string, args []any) (pgx.Rows, error) {
			rows := make([][]any, len(checksums))
			for i, c := range checksums {
				rows[i] = []any{c}
			}
			return &fakeRows{rows: rows}, nil
		},
	}
	report, err := workloadgen.Verify(context.Background(), q, spec, seed)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.OK {
		t.Fatal("report.OK = true, want false on count mismatch")
	}
	tr := report.Tables[0]
	if tr.ExpectedCount != 5 || tr.ActualCount != 4 {
		t.Errorf("counts = %d/%d, want 5/4", tr.ActualCount, tr.ExpectedCount)
	}
}

func TestVerifyChecksumMismatch(t *testing.T) {
	spec := workloadgen.Spec{TablePrefix: "wg", NumTables: 1, RowsPerTable: 3}
	seed := int64(7)

	q := &fakeQuerier{
		queryRowFn: func(sql string, args []any) pgx.Row {
			return fakeRow{vals: []any{int64(3)}}
		},
		queryFn: func(sql string, args []any) (pgx.Rows, error) {
			// Wrong checksums (count matches, content doesn't).
			return &fakeRows{rows: [][]any{{"bad1"}, {"bad2"}, {"bad3"}}}, nil
		},
	}
	report, err := workloadgen.Verify(context.Background(), q, spec, seed)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.OK {
		t.Fatal("report.OK = true, want false on checksum mismatch")
	}
}

// --- RandomOps accounting ---

func TestRandomOpsAccountsInsertsAndDeletes(t *testing.T) {
	spec := workloadgen.Spec{TablePrefix: "wg", NumTables: 1, RowsPerTable: 10}
	seed := int64(3)

	var mu sync.Mutex
	live := int64(spec.RowsPerTable)
	q := &fakeQuerier{
		execFn: func(sql string, args []any) (pgconn.CommandTag, error) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case strings.HasPrefix(sql, "INSERT"):
				live++
				return pgconn.NewCommandTag("INSERT 0 1"), nil
			case strings.HasPrefix(sql, "DELETE"):
				live--
				return pgconn.NewCommandTag("DELETE 1"), nil
			case strings.HasPrefix(sql, "UPDATE"):
				return pgconn.NewCommandTag("UPDATE 1"), nil
			default:
				return pgconn.CommandTag{}, fmt.Errorf("unexpected exec %q", sql)
			}
		},
		queryRowFn: func(sql string, args []any) pgx.Row {
			mu.Lock()
			defer mu.Unlock()
			if strings.Contains(sql, "WHERE") {
				return fakeRow{vals: []any{int64(1)}} // per-op select probe
			}
			// Final verify: report the fake DB's own live count.
			return fakeRow{vals: []any{live}}
		},
	}

	report, err := workloadgen.RandomOps(context.Background(), q, spec, seed, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("random ops: %v", err)
	}
	if !report.OK {
		t.Fatalf("report.OK = false, want true: %+v", report.Tables)
	}
	if len(report.Tables) != 1 {
		t.Fatalf("want 1 table report, got %d", len(report.Tables))
	}
}

func TestRandomOpsRespectsContextCancellation(t *testing.T) {
	spec := workloadgen.Spec{TablePrefix: "wg", NumTables: 1, RowsPerTable: 10}
	q := &fakeQuerier{
		queryRowFn: func(sql string, args []any) pgx.Row {
			return fakeRow{vals: []any{int64(10)}}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := workloadgen.RandomOps(ctx, q, spec, 1, time.Second)
	if err != nil {
		t.Fatalf("random ops: %v", err)
	}
	if !report.OK {
		t.Errorf("cancelled-before-start run should report OK (no ops applied, count matches initial load)")
	}
}

// --- Spec defaults ---

func TestTableNameDefaultPrefix(t *testing.T) {
	s := workloadgen.Spec{NumTables: 1}
	if got := s.TableName(0); got != "wg_0" {
		t.Errorf("default table name = %q, want wg_0", got)
	}
}
