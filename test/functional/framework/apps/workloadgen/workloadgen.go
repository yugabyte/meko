// Package workloadgen is the framework's own verifiable-load primitive
// (architecture §6, implementation §3 M3): a small Go workload generator that
// runs THROUGH a pg connection from the coordinator. The data work happens in
// the database — the coordinator only issues SQL, which is coordination, not
// heavy in-process work (the framework's "nothing heavy runs on the
// orchestrator" rule).
//
// The pattern: Schema creates tables, Load inserts deterministic
// seed-and-row-derived rows with a checksum column, Verify recomputes the
// expected state and compares it to the database (the oracle — "workloads
// write verifiable patterns; checkers verify"), and RandomOps drives a mixed
// insert/update/delete/select loop while cheaply tracking an expected live-row
// count via applied-op accounting (no full-row shadow state).
package workloadgen

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the subset of *pgxpool.Pool this package uses. *pgxpool.Pool
// satisfies it by structural typing; unit tests fake it with a scripted stub.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// maxConcurrency bounds per-table worker fan-out regardless of NumTables.
const maxConcurrency = 8

// defaultBatchSize is used when Spec.BatchSize is unset.
const defaultBatchSize = 500

// payloadLen is the length of the generated pseudorandom payload string.
const payloadLen = 128

const payloadCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// Spec describes the schema/load shape: NumTables tables named
// <TablePrefix>_<i>, each loaded with RowsPerTable rows in batches of
// BatchSize, using up to Concurrency per-table workers.
type Spec struct {
	TablePrefix  string // default "wg"
	NumTables    int
	RowsPerTable int
	BatchSize    int // default 500
	Concurrency  int // default: min(NumTables, 8)
}

// TableName is the name of the i'th table (0-indexed).
func (s Spec) TableName(i int) string {
	prefix := s.TablePrefix
	if prefix == "" {
		prefix = "wg"
	}
	return fmt.Sprintf("%s_%d", prefix, i)
}

func (s Spec) batchSize() int {
	if s.BatchSize <= 0 {
		return defaultBatchSize
	}
	return s.BatchSize
}

func (s Spec) concurrency() int {
	c := s.Concurrency
	if c <= 0 {
		c = s.NumTables
	}
	if c <= 0 {
		c = 1
	}
	if c > maxConcurrency {
		c = maxConcurrency
	}
	return c
}

func (s Spec) validate() error {
	if s.NumTables <= 0 {
		return fmt.Errorf("workloadgen: spec.NumTables must be > 0")
	}
	if s.RowsPerTable < 0 {
		return fmt.Errorf("workloadgen: spec.RowsPerTable must be >= 0")
	}
	return nil
}

// Schema creates spec.NumTables tables (id BIGINT PK, seq BIGINT, payload
// TEXT, checksum TEXT), idempotently.
func Schema(ctx context.Context, q Querier, spec Spec) error {
	if err := spec.validate(); err != nil {
		return err
	}
	for i := 0; i < spec.NumTables; i++ {
		name := spec.TableName(i)
		ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			id BIGINT PRIMARY KEY,
			seq BIGINT NOT NULL,
			payload TEXT NOT NULL,
			checksum TEXT NOT NULL
		)`, name)
		if _, err := q.Exec(ctx, ddl); err != nil {
			return fmt.Errorf("schema: create table %s: %w", name, err)
		}
	}
	return nil
}

// rowSeed mixes the run seed, table name, and row index into a stable 64-bit
// value so DerivePayload is a pure function of (seed, table, row).
func rowSeed(seed int64, table string, row int64) uint64 {
	h := fnv.New64a()
	var b [16]byte
	binary.LittleEndian.PutUint64(b[0:8], uint64(seed))
	binary.LittleEndian.PutUint64(b[8:16], uint64(row))
	_, _ = h.Write(b[:])
	_, _ = h.Write([]byte(table))
	return h.Sum64()
}

// DerivePayload deterministically derives a pseudorandom payload string from
// (seed, table, row). Same inputs always produce the same payload, so Verify
// can recompute expected values without reading them back from anywhere.
func DerivePayload(seed int64, table string, row int64) string {
	hi := rowSeed(seed, table, row)
	lo := rowSeed(seed, table, row+1) ^ 0x9e3779b97f4a7c15
	r := rand.New(rand.NewPCG(hi, lo))
	b := make([]byte, payloadLen)
	for i := range b {
		b[i] = payloadCharset[r.IntN(len(payloadCharset))]
	}
	return string(b)
}

// Checksum is the hex-encoded sha256 of payload.
func Checksum(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// Progress reports incremental Load progress for one table.
type Progress struct {
	Table     string
	RowsDone  int
	RowsTotal int
}

// ProgressFunc receives Load progress updates. May be called concurrently
// across tables; implementations must be safe for concurrent use.
type ProgressFunc func(Progress)

// batchRanges splits [0,total) into batches of at most size rows each.
func batchRanges(total, size int) [][2]int {
	if size <= 0 {
		size = 1
	}
	var out [][2]int
	for start := 0; start < total; start += size {
		end := start + size
		if end > total {
			end = total
		}
		out = append(out, [2]int{start, end})
	}
	return out
}

// Load inserts spec.RowsPerTable rows per table in batches, running up to
// spec.concurrency() tables at once. Each row's payload/checksum are derived
// from seed via DerivePayload/Checksum. ctx-cancellable; onProgress (may be
// nil) is called after each batch.
func Load(ctx context.Context, q Querier, spec Spec, seed int64, onProgress ProgressFunc) error {
	if err := spec.validate(); err != nil {
		return err
	}
	sem := make(chan struct{}, spec.concurrency())
	var wg sync.WaitGroup
	errs := make(chan error, spec.NumTables)
	for i := 0; i < spec.NumTables; i++ {
		i := i
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := loadTable(ctx, q, spec, seed, i, onProgress); err != nil {
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

func loadTable(ctx context.Context, q Querier, spec Spec, seed int64, tableIdx int, onProgress ProgressFunc) error {
	table := spec.TableName(tableIdx)
	for _, br := range batchRanges(spec.RowsPerTable, spec.batchSize()) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := insertBatch(ctx, q, table, seed, br[0], br[1]); err != nil {
			return fmt.Errorf("load %s rows [%d,%d): %w", table, br[0], br[1], err)
		}
		if onProgress != nil {
			onProgress(Progress{Table: table, RowsDone: br[1], RowsTotal: spec.RowsPerTable})
		}
	}
	return nil
}

func insertBatch(ctx context.Context, q Querier, table string, seed int64, start, end int) error {
	var sb []byte
	sb = append(sb, "INSERT INTO "+table+" (id, seq, payload, checksum) VALUES "...)
	args := make([]any, 0, (end-start)*4)
	for row := start; row < end; row++ {
		if row > start {
			sb = append(sb, ", "...)
		}
		payload := DerivePayload(seed, table, int64(row))
		checksum := Checksum(payload)
		base := len(args)
		sb = append(sb, fmt.Sprintf("($%d,$%d,$%d,$%d)", base+1, base+2, base+3, base+4)...)
		args = append(args, int64(row), int64(row), payload, checksum)
	}
	_, err := q.Exec(ctx, string(sb), args...)
	return err
}

// TableReport is Verify's per-table outcome.
type TableReport struct {
	Table            string
	OK               bool
	ExpectedCount    int64
	ActualCount      int64
	ExpectedChecksum string
	ActualChecksum   string
}

// Report is Verify's outcome across every table in a Spec.
type Report struct {
	Tables []TableReport
	OK     bool
}

// Verify recomputes the expected row count and an aggregate checksum (sha256
// of every row's checksum column, ordered by id) for every table in spec, and
// compares against the database. This is the oracle: workloadgen writes
// verifiable patterns, Verify checks them.
func Verify(ctx context.Context, q Querier, spec Spec, seed int64) (Report, error) {
	if err := spec.validate(); err != nil {
		return Report{}, err
	}
	report := Report{OK: true}
	for i := 0; i < spec.NumTables; i++ {
		table := spec.TableName(i)
		tr, err := verifyTable(ctx, q, table, spec.RowsPerTable, seed)
		if err != nil {
			return Report{}, fmt.Errorf("verify %s: %w", table, err)
		}
		if !tr.OK {
			report.OK = false
		}
		report.Tables = append(report.Tables, tr)
	}
	return report, nil
}

func verifyTable(ctx context.Context, q Querier, table string, expectedRows int, seed int64) (TableReport, error) {
	var actualCount int64
	if err := q.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&actualCount); err != nil {
		return TableReport{}, fmt.Errorf("count: %w", err)
	}

	rows, err := q.Query(ctx, "SELECT checksum FROM "+table+" ORDER BY id")
	if err != nil {
		return TableReport{}, fmt.Errorf("query rows: %w", err)
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var checksum string
		if err := rows.Scan(&checksum); err != nil {
			return TableReport{}, fmt.Errorf("scan row: %w", err)
		}
		h.Write([]byte(checksum))
	}
	if err := rows.Err(); err != nil {
		return TableReport{}, fmt.Errorf("iterate rows: %w", err)
	}
	actualChecksum := hex.EncodeToString(h.Sum(nil))

	expH := sha256.New()
	for row := 0; row < expectedRows; row++ {
		payload := DerivePayload(seed, table, int64(row))
		expH.Write([]byte(Checksum(payload)))
	}
	expectedChecksum := hex.EncodeToString(expH.Sum(nil))

	tr := TableReport{
		Table:            table,
		ExpectedCount:    int64(expectedRows),
		ActualCount:      actualCount,
		ExpectedChecksum: expectedChecksum,
		ActualChecksum:   actualChecksum,
	}
	tr.OK = tr.ActualCount == tr.ExpectedCount && actualChecksum == expectedChecksum
	return tr, nil
}

// Op is one kind of random operation RandomOps applies.
type Op int

const (
	OpInsert Op = iota
	OpUpdate
	OpDelete
	OpSelect
)

// opWeight pairs an Op with its share of the seeded proportions.
type opWeight struct {
	op     Op
	weight int
}

// defaultOpWeights are the seeded proportions RandomOps draws from: insert
// 40%, update 30%, delete 10%, select 20% — insert-heavy so a long run doesn't
// empty the tables. The *sequence* of ops is what the seed makes reproducible;
// these proportions are fixed constants.
var defaultOpWeights = []opWeight{
	{OpInsert, 40},
	{OpUpdate, 30},
	{OpDelete, 10},
	{OpSelect, 20},
}

func pickOp(r *rand.Rand) Op {
	total := 0
	for _, w := range defaultOpWeights {
		total += w.weight
	}
	n := r.IntN(total)
	for _, w := range defaultOpWeights {
		if n < w.weight {
			return w.op
		}
		n -= w.weight
	}
	return OpSelect
}

// applyOp runs one operation against table and adjusts live by the DB's
// actually-reported RowsAffected — never an assumed delta — so accounting
// stays correct even when an op targets an id that no longer exists. This is
// the "applied-op accounting" that lets RandomOps track only a live-row
// count, not a full shadow of which rows exist.
func applyOp(ctx context.Context, q Querier, table string, seed int64, op Op, id int64, salt int64, live, next *atomic.Int64) error {
	switch op {
	case OpInsert:
		newID := next.Add(1) - 1
		payload := DerivePayload(seed, table, newID)
		checksum := Checksum(payload)
		tag, err := q.Exec(ctx, "INSERT INTO "+table+" (id, seq, payload, checksum) VALUES ($1,$2,$3,$4)",
			newID, newID, payload, checksum)
		if err != nil {
			return fmt.Errorf("insert: %w", err)
		}
		live.Add(tag.RowsAffected())
		return nil
	case OpUpdate:
		payload := DerivePayload(seed, table+"/update", salt)
		checksum := Checksum(payload)
		if _, err := q.Exec(ctx, "UPDATE "+table+" SET payload=$1, checksum=$2 WHERE id=$3", payload, checksum, id); err != nil {
			return fmt.Errorf("update: %w", err)
		}
		return nil
	case OpDelete:
		tag, err := q.Exec(ctx, "DELETE FROM "+table+" WHERE id=$1", id)
		if err != nil {
			return fmt.Errorf("delete: %w", err)
		}
		live.Add(-tag.RowsAffected())
		return nil
	case OpSelect:
		var count int64
		if err := q.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE id=$1", id).Scan(&count); err != nil {
			return fmt.Errorf("select: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unknown op %d", op)
	}
}

// randomExistingID picks a pseudorandom id in [0, next), or 0 when next <= 0
// (nothing to target yet).
func randomExistingID(r *rand.Rand, next int64) int64 {
	if next <= 0 {
		return 0
	}
	return r.Int64N(next)
}

// OpsTableReport is RandomOps' per-table live-row accounting outcome.
type OpsTableReport struct {
	Table            string
	OK               bool
	ExpectedLiveRows int64
	ActualLiveRows   int64
}

// OpsReport is RandomOps' outcome across every table in a Spec.
type OpsReport struct {
	Tables []OpsTableReport
	OK     bool
}

// RandomOps runs a seed-driven mixed insert/update/delete/select loop against
// every table in spec for duration (or until ctx is cancelled), then verifies
// each table's actual row count against the count implied by applied-op
// accounting. KISS: exact-count verification only — no full-row shadow state.
func RandomOps(ctx context.Context, q Querier, spec Spec, seed int64, duration time.Duration) (OpsReport, error) {
	if err := spec.validate(); err != nil {
		return OpsReport{}, err
	}
	deadline := time.Now().Add(duration)
	live := make([]atomic.Int64, spec.NumTables)
	next := make([]atomic.Int64, spec.NumTables)
	for i := range live {
		live[i].Store(int64(spec.RowsPerTable))
		next[i].Store(int64(spec.RowsPerTable))
	}

	sem := make(chan struct{}, spec.concurrency())
	var wg sync.WaitGroup
	errs := make(chan error, spec.NumTables)
	for i := 0; i < spec.NumTables; i++ {
		i := i
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := opsLoop(ctx, q, spec, seed, i, deadline, &live[i], &next[i]); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return OpsReport{}, err
		}
	}

	report := OpsReport{OK: true}
	for i := 0; i < spec.NumTables; i++ {
		table := spec.TableName(i)
		var actual int64
		if err := q.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&actual); err != nil {
			return OpsReport{}, fmt.Errorf("verify live rows %s: %w", table, err)
		}
		expected := live[i].Load()
		tr := OpsTableReport{Table: table, ExpectedLiveRows: expected, ActualLiveRows: actual, OK: expected == actual}
		if !tr.OK {
			report.OK = false
		}
		report.Tables = append(report.Tables, tr)
	}
	return report, nil
}

func opsLoop(ctx context.Context, q Querier, spec Spec, seed int64, tableIdx int, deadline time.Time, live, next *atomic.Int64) error {
	table := spec.TableName(tableIdx)
	r := rand.New(rand.NewPCG(rowSeed(seed, table, 0), rowSeed(seed, table, 1)))
	for iter := int64(0); time.Now().Before(deadline); iter++ {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		op := pickOp(r)
		id := randomExistingID(r, next.Load())
		if err := applyOp(ctx, q, table, seed, op, id, iter, live, next); err != nil {
			return fmt.Errorf("random ops %s: %w", table, err)
		}
	}
	return nil
}
