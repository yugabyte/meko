package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var latencyBoundsMicros = [...]int64{
	100, 250, 500,
	1_000, 2_000, 5_000,
	10_000, 20_000, 50_000,
	100_000, 200_000, 500_000,
	1_000_000, 2_000_000, 5_000_000,
}

const maxTxnIDKeyset int64 = 9_223_372_036_854_775_807

var lastPoolResetUnixNanos atomic.Int64

const (
	writeLockModeForUpdate    = "for-update"
	writeLockModeAdvisoryXact = "advisory-xact"
)

type benchmarkConfig struct {
	dsn                 string
	schema              string
	shouldInit          bool
	shouldRun           bool
	duration            time.Duration
	threads             int
	poolMinConns        int
	poolMaxConns        int
	nonTxReaders        int
	nonTxReadStatements int
	mixBasis            string
	writeRatio          float64
	ledgerLegs          int
	extraWriteEvents    int
	totalAccounts       int
	settlementAccounts  int
	trackPayments       int
	writeLockMode       string
	advisoryLowCard     int
	advisoryHighCard    int
	advisoryHotspotPct  float64
	advisoryHotspotCard int
}

func (c benchmarkConfig) operationalAccounts() int {
	return c.totalAccounts - c.settlementAccounts
}

func (c benchmarkConfig) estimatedWriteStatementsPerTxn() uint64 {
	// Single write workflow statement estimate.
	// Includes lock-bearing write statements on the write side (not read side).
	// fixed(~31 avg across standard/STP lanes) + ledger legs + extra audit writes.
	return uint64(c.ledgerLegs + c.extraWriteEvents + 31)
}

func (c benchmarkConfig) estimatedReadStatementsPerTxn(hasTrackedPayments bool) uint64 {
	if hasTrackedPayments {
		return 8
	}
	return 3
}

func (c benchmarkConfig) validate() error {
	if c.dsn == "" {
		return errors.New("dsn is required")
	}
	if !validSchemaName(c.schema) {
		return errors.New("schema must match [A-Za-z_][A-Za-z0-9_]*")
	}
	if _, ok := canonicalMixBasis(c.mixBasis); !ok {
		return errors.New("mix-basis must be one of: statement, transaction")
	}
	if _, ok := canonicalWriteLockMode(c.writeLockMode); !ok {
		return errors.New("write-lock-mode must be one of: for-update, advisory-xact")
	}
	if !c.shouldInit && !c.shouldRun {
		return errors.New("nothing to do: use -init and/or -run")
	}
	if c.duration <= 0 {
		return errors.New("duration must be positive")
	}
	if c.threads <= 0 {
		return errors.New("threads must be >= 1")
	}
	if c.poolMinConns < 0 {
		return errors.New("pool-min-conns must be >= 0")
	}
	if c.poolMaxConns < 0 {
		return errors.New("pool-max-conns must be >= 0")
	}
	if c.poolMaxConns > 0 && c.poolMinConns > c.poolMaxConns {
		return errors.New("pool-min-conns must be <= pool-max-conns")
	}
	if c.nonTxReaders < 0 {
		return errors.New("non-tx-readers must be >= 0")
	}
	if c.nonTxReadStatements < 1 {
		return errors.New("non-tx-read-statements must be >= 1")
	}
	if c.writeRatio < 0.0 || c.writeRatio > 1.0 {
		return errors.New("write-ratio must be between 0.0 and 1.0")
	}
	if c.ledgerLegs < 2 {
		return errors.New("ledger-legs must be >= 2")
	}
	if c.extraWriteEvents < 0 {
		return errors.New("extra-write-events must be >= 0")
	}
	if c.totalAccounts < 100 {
		return errors.New("accounts must be >= 100")
	}
	if c.settlementAccounts < 1 {
		return errors.New("settlement-accounts must be >= 1")
	}
	if c.settlementAccounts >= c.totalAccounts-1 {
		return errors.New("settlement-accounts must leave at least 2 operational accounts")
	}
	if c.trackPayments < 100 {
		return errors.New("track-payments must be >= 100")
	}
	if c.advisoryLowCard < 1 {
		return errors.New("advisory-low-cardinality must be >= 1")
	}
	if c.advisoryHighCard < 2 {
		return errors.New("advisory-high-cardinality must be >= 2")
	}
	if c.advisoryHotspotPct < 0 || c.advisoryHotspotPct > 100 {
		return errors.New("advisory-hotspot-percent must be between 0 and 100")
	}
	if c.advisoryHotspotCard < 1 {
		return errors.New("advisory-hotspot-cardinality must be >= 1")
	}
	if c.advisoryHotspotCard > c.advisoryHighCard {
		return errors.New("advisory-hotspot-cardinality must be <= advisory-high-cardinality")
	}
	return nil
}

type paymentStore struct {
	mu   sync.RWMutex
	ids  []string
	next int
}

func newPaymentStore(maxEntries int) *paymentStore {
	return &paymentStore{ids: make([]string, 0, maxEntries)}
}

func (s *paymentStore) Add(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.ids) < cap(s.ids) {
		s.ids = append(s.ids, id)
		return
	}
	if cap(s.ids) == 0 {
		return
	}
	s.ids[s.next] = id
	s.next = (s.next + 1) % cap(s.ids)
}

func (s *paymentStore) AddBatch(ids []string) {
	for _, id := range ids {
		s.Add(id)
	}
}

func (s *paymentStore) Random(rng *rand.Rand) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.ids) == 0 {
		return "", false
	}
	return s.ids[rng.Intn(len(s.ids))], true
}

func (s *paymentStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.ids)
}

type latencyHistogram struct {
	counts    [len(latencyBoundsMicros) + 1]uint64
	sumMicros uint64
	maxMicros int64
}

func (h *latencyHistogram) Record(d time.Duration) {
	micros := d.Microseconds()
	if micros < 0 {
		micros = 0
	}
	h.sumMicros += uint64(micros)
	if micros > h.maxMicros {
		h.maxMicros = micros
	}
	idx := sort.Search(len(latencyBoundsMicros), func(i int) bool {
		return micros <= latencyBoundsMicros[i]
	})
	h.counts[idx]++
}

func (h *latencyHistogram) Merge(other latencyHistogram) {
	for i := range h.counts {
		h.counts[i] += other.counts[i]
	}
	h.sumMicros += other.sumMicros
	if other.maxMicros > h.maxMicros {
		h.maxMicros = other.maxMicros
	}
}

func (h latencyHistogram) Total() uint64 {
	var total uint64
	for _, c := range h.counts {
		total += c
	}
	return total
}

func (h latencyHistogram) Average() time.Duration {
	total := h.Total()
	if total == 0 {
		return 0
	}
	avgMicros := float64(h.sumMicros) / float64(total)
	return time.Duration(avgMicros * float64(time.Microsecond))
}

func (h latencyHistogram) Percentile(p float64) time.Duration {
	total := h.Total()
	if total == 0 {
		return 0
	}
	if p <= 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	target := uint64(math.Ceil(float64(total) * p))
	if target == 0 {
		target = 1
	}

	var cumulative uint64
	for i, c := range h.counts {
		cumulative += c
		if cumulative >= target {
			if i < len(latencyBoundsMicros) {
				return time.Duration(latencyBoundsMicros[i]) * time.Microsecond
			}
			return time.Duration(h.maxMicros) * time.Microsecond
		}
	}
	return time.Duration(h.maxMicros) * time.Microsecond
}

type workerResult struct {
	reads            uint64
	writes           uint64
	nonTxReads       uint64
	readStatements   uint64
	writeStatements  uint64
	errors           uint64
	statements       uint64
	retries          uint64 // conflict retries only (40001/40P01)
	conflicts        uint64
	transientRetries uint64
	poolResets       uint64

	readLatency      latencyHistogram
	writeLatency     latencyHistogram
	nonTxReadLatency latencyHistogram
	errorCodes       map[string]uint64
}

func newWorkerResult() workerResult {
	return workerResult{
		errorCodes: make(map[string]uint64),
	}
}

func (r *workerResult) Merge(other workerResult) {
	r.reads += other.reads
	r.writes += other.writes
	r.nonTxReads += other.nonTxReads
	r.readStatements += other.readStatements
	r.writeStatements += other.writeStatements
	r.errors += other.errors
	r.statements += other.statements
	r.retries += other.retries
	r.conflicts += other.conflicts
	r.transientRetries += other.transientRetries
	r.poolResets += other.poolResets
	r.readLatency.Merge(other.readLatency)
	r.writeLatency.Merge(other.writeLatency)
	r.nonTxReadLatency.Merge(other.nonTxReadLatency)
	for code, count := range other.errorCodes {
		r.errorCodes[code] += count
	}
}

type mixTracker struct {
	readStatements  atomic.Uint64
	writeStatements atomic.Uint64
}

func (m *mixTracker) AddRead(statements uint64) {
	if statements == 0 {
		return
	}
	m.readStatements.Add(statements)
}

func (m *mixTracker) AddWrite(statements uint64) {
	if statements == 0 {
		return
	}
	m.writeStatements.Add(statements)
}

func (m *mixTracker) Snapshot() (writeStatements uint64, readStatements uint64) {
	return m.writeStatements.Load(), m.readStatements.Load()
}

type accountTxnKeysetCursor struct {
	initialized bool
	createdAt   time.Time
	txnID       int64
}

func newAccountTxnKeysetCursor() accountTxnKeysetCursor {
	return accountTxnKeysetCursor{}
}

func (c *accountTxnKeysetCursor) current() (time.Time, int64) {
	if !c.initialized {
		return time.Unix(0, 0).UTC(), 0
	}
	return c.createdAt, c.txnID
}

func (c *accountTxnKeysetCursor) advance(createdAt time.Time, txnID int64) {
	c.initialized = true
	c.createdAt = createdAt
	c.txnID = txnID
}

func (c *accountTxnKeysetCursor) reset() {
	c.initialized = false
	c.createdAt = time.Time{}
	c.txnID = 0
}

type rowExecutor interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type benchmarkSummary struct {
	result  workerResult
	elapsed time.Duration
}

func main() {
	log.SetFlags(0)

	cfg := parseFlags()
	if err := cfg.validate(); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	pool, err := openPool(cfg)
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	defer pool.Close()

	databaseName, err := databaseNameFromDSN(cfg.dsn)
	if err != nil {
		log.Fatalf("database name resolution failed: %v", err)
	}

	prepCtx, cancelPrep := context.WithTimeout(context.Background(), 45*time.Second)
	err = applyYugabytePrepSettings(prepCtx, pool, databaseName)
	cancelPrep()
	if err != nil {
		log.Fatalf("prep settings failed: %v", err)
	}

	if cfg.shouldInit {
		log.Printf("initializing schema + seed data (accounts=%d settlement_accounts=%d)", cfg.totalAccounts, cfg.settlementAccounts)
		initCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		err = initializeSchemaAndSeed(initCtx, pool, cfg)
		cancel()
		if err != nil {
			log.Fatalf("initialization failed: %v", err)
		}
		log.Println("initialization complete")
	}

	if !cfg.shouldRun {
		return
	}

	store := newPaymentStore(cfg.trackPayments)
	loadCtx, cancelLoad := context.WithTimeout(context.Background(), 30*time.Second)
	ids, err := loadRecentPayments(loadCtx, pool, minInt(cfg.trackPayments/2, 20_000))
	cancelLoad()
	if err != nil {
		log.Fatalf("loading recent workflow jobs failed: %v", err)
	}
	store.AddBatch(ids)

	log.Printf("starting benchmark (threads=%d pool_min=%d pool_max=%d non_tx_readers=%d duration=%s mix_basis=%s write_ratio=%.2f write_lock_mode=%s ledger_legs=%d extra_write_events=%d tracked_jobs=%d)",
		cfg.threads, cfg.poolMinConns, cfg.poolMaxConns, cfg.nonTxReaders, cfg.duration, cfg.mixBasis, cfg.writeRatio, cfg.writeLockMode, cfg.ledgerLegs, cfg.extraWriteEvents, store.Len())
	if cfg.writeLockMode == writeLockModeAdvisoryXact {
		log.Printf("advisory lock tuning (left_cardinality=%d right_cardinality=%d hotspot_percent=%.2f hotspot_right_cardinality=%d)",
			cfg.advisoryLowCard, cfg.advisoryHighCard, cfg.advisoryHotspotPct, cfg.advisoryHotspotCard)
	}
	summary, err := runBenchmark(pool, cfg, store)
	if err != nil {
		log.Fatalf("benchmark failed: %v", err)
	}
	printSummary(cfg, summary, store.Len())
}

func parseFlags() benchmarkConfig {
	cfg := benchmarkConfig{}
	flag.StringVar(&cfg.dsn, "dsn", "", "Yugabyte/Postgres connection string (or LEDGER_BENCH_DSN).")
	flag.StringVar(&cfg.schema, "schema", "ledger_bench", "Schema used for benchmark tables.")
	flag.BoolVar(&cfg.shouldInit, "init", false, "Create schema and seed accounts before running.")
	flag.BoolVar(&cfg.shouldRun, "run", true, "Run benchmark workload.")
	flag.DurationVar(&cfg.duration, "duration", 2*time.Minute, "Benchmark duration (e.g. 2m, 30s).")
	flag.IntVar(&cfg.threads, "threads", 4, "Number of concurrent worker goroutines.")
	flag.IntVar(&cfg.poolMinConns, "pool-min-conns", 0, "Minimum pool connections (0 = auto).")
	flag.IntVar(&cfg.poolMaxConns, "pool-max-conns", 0, "Maximum pool connections (0 = auto).")
	flag.IntVar(&cfg.nonTxReaders, "non-tx-readers", 2, "Concurrent non-transactional read worker goroutines.")
	flag.IntVar(&cfg.nonTxReadStatements, "non-tx-read-statements", 4, "Autocommit read statements per non-transactional read operation.")
	flag.StringVar(&cfg.mixBasis, "mix-basis", "statement", "Mix enforcement basis: statement or transaction.")
	flag.Float64Var(&cfg.writeRatio, "write-ratio", 0.62, "Target write ratio from 0.0 to 1.0 for selected -mix-basis.")
	flag.IntVar(&cfg.ledgerLegs, "ledger-legs", 6, "Ledger leg rows written per workflow.")
	flag.IntVar(&cfg.extraWriteEvents, "extra-write-events", 0, "Additional audit writes per write workflow.")
	flag.StringVar(&cfg.writeLockMode, "write-lock-mode", writeLockModeForUpdate, "Write lock strategy: for-update or advisory-xact.")
	flag.IntVar(&cfg.advisoryLowCard, "advisory-low-cardinality", 16, "Left advisory lock key cardinality when -write-lock-mode=advisory-xact.")
	flag.IntVar(&cfg.advisoryHighCard, "advisory-high-cardinality", 8192, "Right advisory lock key cardinality when -write-lock-mode=advisory-xact.")
	flag.Float64Var(&cfg.advisoryHotspotPct, "advisory-hotspot-percent", 3.0, "Percent of write txns routed to hotspot advisory right keys.")
	flag.IntVar(&cfg.advisoryHotspotCard, "advisory-hotspot-cardinality", 32, "Hotspot right-key cardinality when -write-lock-mode=advisory-xact.")
	flag.IntVar(&cfg.totalAccounts, "accounts", 20_000, "Total seeded accounts (operational + settlement).")
	flag.IntVar(&cfg.settlementAccounts, "settlement-accounts", 1000, "Number of settlement/house accounts from the seeded set.")
	flag.IntVar(&cfg.trackPayments, "track-payments", 200_000, "Max in-memory workflow job IDs available to read transactions.")
	flag.Parse()
	if cfg.dsn == "" {
		cfg.dsn = os.Getenv("LEDGER_BENCH_DSN")
	}

	if canonical, ok := canonicalMixBasis(cfg.mixBasis); ok {
		cfg.mixBasis = canonical
	}
	if canonical, ok := canonicalWriteLockMode(cfg.writeLockMode); ok {
		cfg.writeLockMode = canonical
	}
	return cfg
}

func openPool(cfg benchmarkConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	configureRoundRobinHostSelection(poolCfg)

	maxConns := maxInt(cfg.threads*4, 16)
	if cfg.poolMaxConns > 0 {
		maxConns = cfg.poolMaxConns
	}
	minConns := minInt(cfg.threads, 4)
	if cfg.poolMinConns > 0 {
		minConns = cfg.poolMinConns
	}
	if minConns > maxConns {
		minConns = maxConns
	}
	poolCfg.MaxConns = int32(maxConns)
	poolCfg.MinConns = int32(minConns)
	poolCfg.MaxConnIdleTime = 2 * time.Minute
	poolCfg.HealthCheckPeriod = 15 * time.Second
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnLifetimeJitter = 5 * time.Minute
	poolCfg.ConnConfig.RuntimeParams["application_name"] = "ledger_bench"
	poolCfg.ConnConfig.RuntimeParams["search_path"] = fmt.Sprintf("%s,public", cfg.schema)

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

func configureRoundRobinHostSelection(poolCfg *pgxpool.Config) {
	rawMode, hasMode := poolCfg.ConnConfig.RuntimeParams["load_balance"]
	if hasMode {
		delete(poolCfg.ConnConfig.RuntimeParams, "load_balance")
	}
	if !hasMode || !isTruthyLoadBalanceMode(rawMode) {
		return
	}

	candidateCount := 1 + len(poolCfg.ConnConfig.Fallbacks)
	if candidateCount <= 1 {
		return
	}

	var nextHost uint64
	poolCfg.BeforeConnect = func(_ context.Context, connCfg *pgx.ConnConfig) error {
		candidates := make([]*pgconn.FallbackConfig, 0, 1+len(connCfg.Fallbacks))
		candidates = append(candidates, &pgconn.FallbackConfig{
			Host:      connCfg.Host,
			Port:      connCfg.Port,
			TLSConfig: connCfg.TLSConfig,
		})
		for _, fallback := range connCfg.Fallbacks {
			if fallback == nil {
				continue
			}
			candidates = append(candidates, &pgconn.FallbackConfig{
				Host:      fallback.Host,
				Port:      fallback.Port,
				TLSConfig: fallback.TLSConfig,
			})
		}
		if len(candidates) <= 1 {
			return nil
		}

		startIndex := int(atomic.AddUint64(&nextHost, 1)-1) % len(candidates)
		primary := candidates[startIndex]
		connCfg.Host = primary.Host
		connCfg.Port = primary.Port
		connCfg.TLSConfig = primary.TLSConfig

		rotatedFallbacks := make([]*pgconn.FallbackConfig, 0, len(candidates)-1)
		for i := 1; i < len(candidates); i++ {
			candidate := candidates[(startIndex+i)%len(candidates)]
			rotatedFallbacks = append(rotatedFallbacks, &pgconn.FallbackConfig{
				Host:      candidate.Host,
				Port:      candidate.Port,
				TLSConfig: candidate.TLSConfig,
			})
		}
		connCfg.Fallbacks = rotatedFallbacks
		return nil
	}

	log.Printf("enabled client-side host load balancing across %d hosts", candidateCount)
}

func isTruthyLoadBalanceMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "1", "on", "true", "yes", "enabled", "random":
		return true
	default:
		return false
	}
}

func databaseNameFromDSN(dsn string) (string, error) {
	parsed, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn for database: %w", err)
	}
	if strings.TrimSpace(parsed.ConnConfig.Database) == "" {
		return "", errors.New("database name is empty in dsn")
	}
	return parsed.ConnConfig.Database, nil
}

func applyYugabytePrepSettings(ctx context.Context, pool *pgxpool.Pool, databaseName string) error {
	sessionStatements := []string{
		`SET yb_max_saop_merge_streams=64`,
		`SET yb_enable_derived_saops=true`,
		`SET yb_enable_derived_equalities=true`,
		`SET yb_enable_cbo=on`,
		`SET enable_nestloop=off`,
	}

	for _, stmt := range sessionStatements {
		if err := execPrepStatementWithRetry(ctx, pool, stmt); err != nil {
			if skip, reason := shouldSkipPrepStatement(err); skip {
				log.Printf("warning: skipping session prep statement (%s): %s", stmt, reason)
				continue
			}
			return fmt.Errorf("session prep statement failed (%s): %w", stmt, err)
		}
	}

	quotedDBName := pgx.Identifier{databaseName}.Sanitize()
	databaseStatements := []string{
		fmt.Sprintf(`ALTER DATABASE %s SET yb_max_saop_merge_streams=64`, quotedDBName),
		fmt.Sprintf(`ALTER DATABASE %s SET yb_enable_derived_saops=true`, quotedDBName),
		fmt.Sprintf(`ALTER DATABASE %s SET yb_enable_derived_equalities=true`, quotedDBName),
		fmt.Sprintf(`ALTER DATABASE %s SET yb_enable_cbo=on`, quotedDBName),
		fmt.Sprintf(`ALTER DATABASE %s SET enable_nestloop=off`, quotedDBName),
	}

	for _, stmt := range databaseStatements {
		if err := execPrepStatementWithRetry(ctx, pool, stmt); err != nil {
			if skip, reason := shouldSkipPrepStatement(err); skip {
				log.Printf("warning: skipping database prep statement (%s): %s", stmt, reason)
				continue
			}
			return fmt.Errorf("database prep statement failed (%s): %w", stmt, err)
		}
	}

	return nil
}

func execPrepStatementWithRetry(ctx context.Context, pool *pgxpool.Pool, stmt string) error {
	var conflictRetries uint64
	var transientRetries uint64
	for {
		_, err := pool.Exec(ctx, stmt)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if isRetryableConflict(err) {
			if conflictRetries >= maxConflictRetries {
				return err
			}
			conflictRetries++
			if !sleepWithContext(ctx, prepRetryBackoff(conflictRetries)) {
				return ctx.Err()
			}
			continue
		}

		if isRetryableTransient(err) {
			if transientRetries >= maxTransientRetries {
				return err
			}
			transientRetries++
			_ = maybeResetPool(pool)
			if !sleepWithContext(ctx, prepRetryBackoff(transientRetries)) {
				return ctx.Err()
			}
			continue
		}

		return err
	}
}

func shouldSkipPrepStatement(err error) (bool, string) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false, ""
	}

	switch pgErr.Code {
	case "42501":
		return true, fmt.Sprintf("insufficient privilege (%s)", pgErr.Message)
	case "42704":
		return true, fmt.Sprintf("parameter unsupported or undefined (%s)", pgErr.Message)
	case "0A000":
		return true, fmt.Sprintf("feature not supported (%s)", pgErr.Message)
	default:
		return false, ""
	}
}

func initializeSchemaAndSeed(ctx context.Context, pool *pgxpool.Pool, cfg benchmarkConfig) error {
	if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", cfg.schema)); err != nil {
		return fmt.Errorf("create schema %q failed: %w", cfg.schema, err)
	}
	if err := resetBenchmarkTables(ctx, pool, cfg.schema); err != nil {
		return err
	}

	statements := []string{
		`CREATE TABLE IF NOT EXISTS core_account (
			account_id BIGINT PRIMARY KEY,
			account_name TEXT NOT NULL,
			account_type TEXT NOT NULL CHECK (account_type IN ('CLIENT', 'SETTLEMENT')),
			currency CHAR(3) NOT NULL CHECK (currency = 'GBP'),
			available_balance_minor BIGINT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		) SPLIT INTO 6 TABLETS`,
		`CREATE TABLE IF NOT EXISTS op_job (
			job_id UUID PRIMARY KEY,
			job_type TEXT NOT NULL,
			job_sub_type TEXT NULL,
			job_status TEXT NOT NULL,
			job_data TEXT NOT NULL DEFAULT '',
			next_event_time TIMESTAMPTZ NOT NULL,
			state_time TIMESTAMPTZ NOT NULL,
			time_created TIMESTAMPTZ NOT NULL,
			time_updated TIMESTAMPTZ NOT NULL,
			processing_tries_count INT NOT NULL DEFAULT 0,
			version INT NOT NULL DEFAULT 0,
			priority INT NOT NULL,
			txn_id BIGINT NOT NULL,
				debtor_account_id BIGINT NOT NULL,
				creditor_account_id BIGINT NOT NULL,
				settlement_account_id BIGINT NOT NULL,
				amount_minor BIGINT NOT NULL,
				currency CHAR(3) NOT NULL
			) SPLIT INTO 6 TABLETS`,
		`CREATE TABLE IF NOT EXISTS op_job_blob (
			job_id UUID PRIMARY KEY,
			data_format INT NOT NULL,
			data BYTEA NOT NULL,
			context_format INT NOT NULL,
			context_data BYTEA NOT NULL
		) SPLIT INTO 6 TABLETS`,
		`CREATE TABLE IF NOT EXISTS evt_audit (
			audit_id BIGINT PRIMARY KEY,
			change_type TEXT NOT NULL,
			object_ref BIGINT NOT NULL,
			change_note TEXT NOT NULL,
			actor_id TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			client_id TEXT NOT NULL
		) SPLIT INTO 6 TABLETS`,
		`CREATE TABLE IF NOT EXISTS fin_txn (
			txn_id BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			txn_type TEXT NOT NULL,
			txn_state TEXT NOT NULL,
			account_id BIGINT NOT NULL,
			creator_profile_id BIGINT NOT NULL,
			batch_id BIGINT NOT NULL,
			version INT NOT NULL DEFAULT 0,
			PRIMARY KEY (account_id, txn_id),
			UNIQUE (txn_id)
		) SPLIT INTO 6 TABLETS`,
		`CREATE TABLE IF NOT EXISTS fin_step (
			step_id BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			step_type TEXT NOT NULL,
			txn_id BIGINT NOT NULL,
			source_amount_currency CHAR(3) NOT NULL,
			source_amount_value NUMERIC(18,2) NOT NULL,
			tracing_reference_code TEXT NULL,
			reversed_step_id BIGINT NULL,
			source_balance_id BIGINT NULL,
			target_balance_id BIGINT NULL,
			channel_name TEXT NULL,
			channel_reference_id TEXT NULL,
			PRIMARY KEY (txn_id, step_id)
		) SPLIT INTO 6 TABLETS`,
		`CREATE TABLE IF NOT EXISTS fin_idem (
			idempotence_id UUID PRIMARY KEY,
			created_at TIMESTAMPTZ NOT NULL
		) SPLIT INTO 6 TABLETS`,
		`CREATE TABLE IF NOT EXISTS fin_idem_map (
			txn_id BIGINT NOT NULL,
			idempotence_id UUID NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (txn_id, idempotence_id)
		) SPLIT INTO 6 TABLETS`,
		`CREATE TABLE IF NOT EXISTS acct_bucket (
			bucket_uuid UUID PRIMARY KEY,
			bucket_id BIGINT NOT NULL UNIQUE,
			product_name TEXT NOT NULL,
			bucket_type TEXT NOT NULL,
			bucket_state TEXT NOT NULL,
			bucket_name TEXT NULL,
			currency CHAR(3) NOT NULL CHECK (currency = 'GBP'),
			icon JSONB NOT NULL DEFAULT 'null'::jsonb,
			creator_account_id BIGINT NOT NULL,
			creator_profile_id BIGINT NOT NULL,
			creator_user_id BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			version INT NOT NULL DEFAULT 0
		) SPLIT INTO 6 TABLETS`,
		`CREATE TABLE IF NOT EXISTS acct_link (
			link_uuid UUID NOT NULL,
			bucket_id BIGINT NOT NULL,
			account_id BIGINT NOT NULL,
			profile_id BIGINT NOT NULL,
			ownership TEXT NOT NULL,
			link_state TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			version INT NOT NULL DEFAULT 0,
			PRIMARY KEY (bucket_id, link_uuid)
		) SPLIT INTO 6 TABLETS`,
		`CREATE TABLE IF NOT EXISTS acct_balance (
			balance_id BIGINT NOT NULL,
			balance_type TEXT NOT NULL,
			icon JSONB NOT NULL DEFAULT 'null'::jsonb,
			account_id BIGINT NOT NULL,
			bucket_id BIGINT NOT NULL,
			currency CHAR(3) NOT NULL CHECK (currency = 'GBP'),
			is_visible BOOLEAN NOT NULL,
			is_primary BOOLEAN NOT NULL,
			investment_state TEXT NOT NULL,
			available_amount NUMERIC(18,2) NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			recipient_id BIGINT NULL,
			version INT NOT NULL DEFAULT 0,
			PRIMARY KEY (bucket_id, balance_id),
			UNIQUE (balance_id)
		) SPLIT INTO 6 TABLETS`,
		`CREATE TABLE IF NOT EXISTS acct_ledger (
			ledger_id BIGINT NOT NULL,
			balance_id BIGINT NOT NULL,
			ledger_type TEXT NOT NULL,
			amount_currency CHAR(3) NOT NULL,
			amount_value NUMERIC(18,2) NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (balance_id, ledger_type, ledger_id)
		) SPLIT INTO 6 TABLETS`,
		`CREATE TABLE IF NOT EXISTS acct_ledger_entry (
			entry_id BIGSERIAL PRIMARY KEY,
			ledger_id BIGINT NOT NULL,
			txn_id BIGINT NOT NULL,
			amount NUMERIC(18,2) NOT NULL,
			balance_after NUMERIC(18,2) NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			step_id BIGINT NOT NULL
		) SPLIT INTO 6 TABLETS`,
		`CREATE TABLE IF NOT EXISTS acct_balance_snapshot (
			snapshot_id UUID PRIMARY KEY,
			txn_id BIGINT NOT NULL,
			balance_id BIGINT NOT NULL,
			step_id BIGINT NOT NULL,
			ledger_amount_after NUMERIC(18,2) NOT NULL,
			available_amount_after NUMERIC(18,2) NOT NULL,
			total_worth_after NUMERIC(18,2) NOT NULL,
			holdings_accessible_amount_after NUMERIC(18,2) NOT NULL,
			holdings_total_worth_after NUMERIC(18,2) NOT NULL,
			currency CHAR(3) NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		) SPLIT INTO 6 TABLETS`,
		`CREATE TABLE IF NOT EXISTS out_msg_0 (
				message_id BIGSERIAL PRIMARY KEY,
				message BYTEA NOT NULL,
				created_at TIMESTAMPTZ NOT NULL DEFAULT now()
			) SPLIT INTO 6 TABLETS`,
		`ALTER SEQUENCE IF EXISTS acct_ledger_entry_entry_id_seq CACHE 1000`,
		`ALTER SEQUENCE IF EXISTS out_msg_0_message_id_seq CACHE 1000`,
		`CREATE INDEX NONCONCURRENTLY IF NOT EXISTS idx_op_job_status_txn ON op_job (job_status HASH, txn_id ASC) INCLUDE (job_id, priority, next_event_time) SPLIT INTO 6 TABLETS`,
		`CREATE INDEX NONCONCURRENTLY IF NOT EXISTS idx_fin_txn_account_created ON fin_txn (account_id, created_at, txn_id) SPLIT INTO 6 TABLETS`,
		`CREATE INDEX NONCONCURRENTLY IF NOT EXISTS idx_fin_txn_account_txn_cover ON fin_txn (account_id, txn_id) INCLUDE (created_at, txn_type) SPLIT INTO 6 TABLETS`,
		`CREATE INDEX NONCONCURRENTLY IF NOT EXISTS idx_evt_audit_object_created ON evt_audit (object_ref, created_at, audit_id) SPLIT INTO 6 TABLETS`,
		`CREATE INDEX NONCONCURRENTLY IF NOT EXISTS idx_fin_step_txn ON fin_step (txn_id, step_id) SPLIT INTO 6 TABLETS`,
		`CREATE INDEX NONCONCURRENTLY IF NOT EXISTS idx_fin_idem_map_idem ON fin_idem_map (idempotence_id) SPLIT INTO 6 TABLETS`,
		`CREATE INDEX NONCONCURRENTLY IF NOT EXISTS idx_acct_ledger_entry_txn ON acct_ledger_entry (txn_id, created_at, entry_id) SPLIT INTO 6 TABLETS`,
		`ALTER TABLE IF EXISTS op_job DROP COLUMN IF EXISTS virtualbucket`,
	}

	legacyDrops := []string{
		fmt.Sprintf("DROP INDEX IF EXISTS %s.idx_payments_created", cfg.schema),
		fmt.Sprintf("DROP INDEX IF EXISTS %s.idx_payments_debtor_created", cfg.schema),
		fmt.Sprintf("DROP INDEX IF EXISTS %s.idx_payments_creditor_created", cfg.schema),
		fmt.Sprintf("DROP INDEX IF EXISTS %s.idx_payments_status_created", cfg.schema),
		fmt.Sprintf("DROP INDEX IF EXISTS %s.idx_op_job_time", cfg.schema),
		fmt.Sprintf("DROP INDEX IF EXISTS %s.idx_op_job_time_keyset", cfg.schema),
		fmt.Sprintf("DROP INDEX IF EXISTS %s.idx_op_job_status_time", cfg.schema),
		fmt.Sprintf("DROP INDEX IF EXISTS %s.idx_op_job_status_job", cfg.schema),
		fmt.Sprintf("DROP INDEX IF EXISTS %s.idx_op_job_status_txn", cfg.schema),
		fmt.Sprintf("DROP INDEX IF EXISTS %s.idx_fin_txn_account_created", cfg.schema),
		fmt.Sprintf("DROP INDEX IF EXISTS %s.idx_fin_txn_account_txn_cover", cfg.schema),
		fmt.Sprintf("DROP INDEX IF EXISTS %s.idx_evt_audit_object_created", cfg.schema),
		fmt.Sprintf("DROP INDEX IF EXISTS %s.idx_fin_step_txn", cfg.schema),
		fmt.Sprintf("DROP INDEX IF EXISTS %s.idx_fin_idem_map_idem", cfg.schema),
		fmt.Sprintf("DROP INDEX IF EXISTS %s.idx_acct_ledger_entry_txn", cfg.schema),
		fmt.Sprintf("DROP INDEX IF EXISTS %s.idx_acct_balance_account", cfg.schema),
	}
	for _, dropStmt := range legacyDrops {
		if _, err := pool.Exec(ctx, dropStmt); err != nil {
			return fmt.Errorf("drop obsolete index failed (%s): %w", dropStmt, err)
		}
	}

	for i, stmt := range statements {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("schema statement %d failed: %w", i+1, err)
		}
	}

	if err := dropAllSchemaForeignKeys(ctx, pool); err != nil {
		return err
	}

	const initialClientBalanceMinor int64 = 10_000_000_000
	seedSQL := `
		INSERT INTO core_account (
			account_id, account_name, account_type, currency, available_balance_minor
		)
		SELECT
			gs,
			CASE
				WHEN gs > ($1 - $2) THEN 'SETTLEMENT-' || gs::text
				ELSE 'CLIENT-' || gs::text
			END,
			CASE
				WHEN gs > ($1 - $2) THEN 'SETTLEMENT'
				ELSE 'CLIENT'
			END,
			'GBP',
			CASE
				WHEN gs > ($1 - $2) THEN 0::BIGINT
				ELSE $3::BIGINT
			END
		FROM generate_series(1, $1) AS gs
		ON CONFLICT (account_id) DO NOTHING
	`
	if _, err := pool.Exec(ctx, seedSQL, cfg.totalAccounts, cfg.settlementAccounts, initialClientBalanceMinor); err != nil {
		return fmt.Errorf("seed accounts failed: %w", err)
	}

	seedBalanceSQL := `
		INSERT INTO acct_balance (
			balance_id, balance_type, icon, account_id, bucket_id, currency,
			is_visible, is_primary, investment_state, available_amount,
			created_at, updated_at, recipient_id, version
		)
		SELECT
			account_id,
			'STANDARD',
			'null'::jsonb,
			account_id,
			account_id,
			'GBP',
			true,
			true,
			'NOT_INVESTED',
			CASE
				WHEN account_type = 'SETTLEMENT' THEN 0::numeric(18,2)
				ELSE round(($1::numeric / 100.0), 2)
			END,
			now(),
			now(),
			account_id,
			0
		FROM core_account
		ON CONFLICT (bucket_id, balance_id) DO NOTHING
	`
	if _, err := pool.Exec(ctx, seedBalanceSQL, initialClientBalanceMinor); err != nil {
		return fmt.Errorf("seed balances failed: %w", err)
	}

	seedLedgerSQL := `
		INSERT INTO acct_ledger (
			ledger_id, balance_id, ledger_type, amount_currency, amount_value, created_at, updated_at
		)
		SELECT
			balance_id,
			balance_id,
			'AVAILABLE',
			currency,
			available_amount,
			now(),
			now()
		FROM acct_balance
		ON CONFLICT (balance_id, ledger_type, ledger_id) DO NOTHING
	`
	if _, err := pool.Exec(ctx, seedLedgerSQL); err != nil {
		return fmt.Errorf("seed ledgers failed: %w", err)
	}

	return nil
}

func resetBenchmarkTables(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	// Drop benchmark-owned tables so every -init is a clean rebuild, not incremental.
	for _, table := range benchmarkTableNames() {
		stmt := fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", pgx.Identifier{schema, table}.Sanitize())
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("drop table %s failed: %w", table, err)
		}
	}
	return nil
}

func benchmarkTableNames() []string {
	return []string{
		"out_msg_0",
		"acct_balance_snapshot",
		"acct_ledger_entry",
		"acct_ledger",
		"acct_balance",
		"acct_link",
		"acct_bucket",
		"fin_idem_map",
		"fin_idem",
		"fin_step",
		"fin_txn",
		"evt_audit",
		"op_job_blob",
		"op_job",
		"core_account",
	}
}

func loadRecentPayments(ctx context.Context, pool *pgxpool.Pool, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}

	paymentIDs := make([]string, 0, limit)
	rows, err := pool.Query(ctx, `
		SELECT job_id
		FROM op_job
		WHERE job_status = 'SETTLED'
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var paymentID string
		if err = rows.Scan(&paymentID); err != nil {
			return nil, err
		}
		paymentIDs = append(paymentIDs, paymentID)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return paymentIDs, nil
}

func dropAllSchemaForeignKeys(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, `
		SELECT
			quote_ident(n.nspname) || '.' || quote_ident(t.relname) AS table_name,
			quote_ident(c.conname) AS constraint_name
		FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE c.contype = 'f'
		  AND n.nspname = current_schema()
	`)
	if err != nil {
		return fmt.Errorf("list schema foreign keys failed: %w", err)
	}
	defer rows.Close()

	type fkConstraint struct {
		tableName      string
		constraintName string
	}
	constraints := make([]fkConstraint, 0, 16)
	for rows.Next() {
		var item fkConstraint
		if err = rows.Scan(&item.tableName, &item.constraintName); err != nil {
			return fmt.Errorf("scan schema foreign keys failed: %w", err)
		}
		constraints = append(constraints, item)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("iterate schema foreign keys failed: %w", err)
	}

	for _, item := range constraints {
		dropStmt := fmt.Sprintf(`ALTER TABLE %s DROP CONSTRAINT %s`, item.tableName, item.constraintName)
		if _, err = pool.Exec(ctx, dropStmt); err != nil {
			return fmt.Errorf("drop foreign key %s on %s failed: %w", item.constraintName, item.tableName, err)
		}
	}
	return nil
}

func runBenchmark(pool *pgxpool.Pool, cfg benchmarkConfig, store *paymentStore) (benchmarkSummary, error) {
	benchCtx, cancel := context.WithTimeout(context.Background(), cfg.duration)
	defer cancel()

	totalWorkers := cfg.threads + cfg.nonTxReaders
	results := make(chan workerResult, totalWorkers)
	var wg sync.WaitGroup
	mix := &mixTracker{}

	started := time.Now()
	go runOneShotAnalyze(benchCtx, pool, cfg.schema, time.Minute)

	for i := 0; i < cfg.threads; i++ {
		wg.Add(1)
		workerID := i + 1
		go func() {
			defer wg.Done()
			results <- runWorker(benchCtx, pool, cfg, store, workerID, mix)
		}()
	}
	for i := 0; i < cfg.nonTxReaders; i++ {
		wg.Add(1)
		workerID := i + 1
		go func() {
			defer wg.Done()
			results <- runNonTxReader(benchCtx, pool, cfg, store, workerID, mix)
		}()
	}

	wg.Wait()
	close(results)

	summary := benchmarkSummary{
		result:  newWorkerResult(),
		elapsed: time.Since(started),
	}
	for worker := range results {
		summary.result.Merge(worker)
	}
	return summary, nil
}

func runOneShotAnalyze(ctx context.Context, pool *pgxpool.Pool, schema string, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}

	analyzeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	for _, table := range benchmarkTableNames() {
		stmt := fmt.Sprintf("ANALYZE %s", pgx.Identifier{schema, table}.Sanitize())
		if _, err := pool.Exec(analyzeCtx, stmt); err != nil {
			log.Printf("one-shot analyze failed (%s): %v", stmt, err)
			return
		}
	}

	log.Printf("one-shot analyze completed at +%s", delay)
}

func runWorker(
	ctx context.Context,
	pool *pgxpool.Pool,
	cfg benchmarkConfig,
	store *paymentStore,
	workerID int,
	mix *mixTracker,
) workerResult {
	result := newWorkerResult()
	rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)*1_000_000))
	accountCursor := newAccountTxnKeysetCursor()
	var sequence uint64

	for {
		if ctx.Err() != nil {
			return result
		}

		start := time.Now()
		doWrite := shouldExecuteWrite(cfg, rng, mix, store.Len() > 0)

		var stmtCount int
		var err error
		var retries uint64
		var conflicts uint64
		var transientRetries uint64
		var poolResets uint64
		if doWrite {
			sequence++
			stmtCount, err, retries, conflicts, transientRetries, poolResets = executeWithRetry(ctx, pool, rng, func(opCtx context.Context) (int, error) {
				return executeWriteTransaction(opCtx, pool, cfg, store, rng, workerID, sequence)
			})
			if err == nil {
				result.writes++
				result.writeStatements += uint64(stmtCount)
				result.statements += uint64(stmtCount)
				result.writeLatency.Record(time.Since(start))
				mix.AddWrite(uint64(stmtCount))
			}
		} else {
			stmtCount, err, retries, conflicts, transientRetries, poolResets = executeWithRetry(ctx, pool, rng, func(opCtx context.Context) (int, error) {
				return executeReadTransaction(opCtx, pool, cfg, store, rng, &accountCursor)
			})
			if err == nil {
				result.reads++
				result.readStatements += uint64(stmtCount)
				result.statements += uint64(stmtCount)
				result.readLatency.Record(time.Since(start))
				mix.AddRead(uint64(stmtCount))
			}
		}
		result.retries += retries
		result.conflicts += conflicts
		result.transientRetries += transientRetries
		result.poolResets += poolResets

		if err != nil {
			if ctx.Err() != nil {
				return result
			}
			result.errors++
			result.errorCodes[errorCode(err)]++
			sleepForOperationError(err, rng)
		}
	}
}

const (
	maxConflictRetries  = 6
	maxTransientRetries = 10
	poolResetCooldown   = 2 * time.Second
)

func executeWithRetry(
	ctx context.Context,
	pool *pgxpool.Pool,
	rng *rand.Rand,
	op func(context.Context) (int, error),
) (int, error, uint64, uint64, uint64, uint64) {
	var conflictRetries uint64
	var conflicts uint64
	var transientRetries uint64
	var poolResets uint64

	for {
		stmtCount, err := op(ctx)
		if err == nil {
			return stmtCount, nil, conflictRetries, conflicts, transientRetries, poolResets
		}
		if ctx.Err() != nil {
			return 0, ctx.Err(), conflictRetries, conflicts, transientRetries, poolResets
		}

		if isRetryableConflict(err) {
			conflicts++
			if conflictRetries >= maxConflictRetries {
				return 0, err, conflictRetries, conflicts, transientRetries, poolResets
			}
			conflictRetries++
			backoff := conflictRetryBackoff(conflictRetries, rng)
			if !sleepWithContext(ctx, backoff) {
				return 0, ctx.Err(), conflictRetries, conflicts, transientRetries, poolResets
			}
			continue
		}

		if isRetryableTransient(err) {
			if transientRetries >= maxTransientRetries {
				return 0, err, conflictRetries, conflicts, transientRetries, poolResets
			}
			transientRetries++
			if maybeResetPool(pool) {
				poolResets++
			}
			backoff := transientRetryBackoff(transientRetries, rng)
			if !sleepWithContext(ctx, backoff) {
				return 0, ctx.Err(), conflictRetries, conflicts, transientRetries, poolResets
			}
			continue
		}

		return 0, err, conflictRetries, conflicts, transientRetries, poolResets
	}
}

func isRetryableConflict(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "40001" || pgErr.Code == "40P01"
}

func isRetryableTransient(err error) bool {
	if err == nil {
		return false
	}
	if pgconn.SafeToRetry(err) {
		return true
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if strings.HasPrefix(pgErr.Code, "08") {
			return true
		}
		switch pgErr.Code {
		case "57P01", "57P02", "57P03", "53300":
			return true
		}
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if errors.Is(err, io.EOF) {
		return true
	}

	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "connection refused"),
		strings.Contains(message, "connection reset"),
		strings.Contains(message, "connection closed"),
		strings.Contains(message, "broken pipe"),
		strings.Contains(message, "i/o timeout"),
		strings.Contains(message, "failed to connect"),
		strings.Contains(message, "server closed the connection"),
		strings.Contains(message, "terminating connection"),
		strings.Contains(message, "cannot connect now"):
		return true
	default:
		return false
	}
}

func conflictRetryBackoff(retries uint64, rng *rand.Rand) time.Duration {
	capped := minInt(int(retries), 6)
	base := time.Duration(1<<capped) * 5 * time.Millisecond
	jitter := time.Duration(rng.Intn(6)+1) * time.Millisecond
	delay := base + jitter
	maxDelay := 500 * time.Millisecond
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

func prepRetryBackoff(retries uint64) time.Duration {
	capped := minInt(int(retries), 7)
	delay := time.Duration(1<<capped) * 25 * time.Millisecond
	maxDelay := 2 * time.Second
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

func transientRetryBackoff(retries uint64, rng *rand.Rand) time.Duration {
	capped := minInt(int(retries), 8)
	base := time.Duration(1<<capped) * 25 * time.Millisecond
	jitter := time.Duration(rng.Intn(25)+5) * time.Millisecond
	delay := base + jitter
	maxDelay := 2 * time.Second
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func maybeResetPool(pool *pgxpool.Pool) bool {
	if pool == nil {
		return false
	}
	now := time.Now().UnixNano()
	last := lastPoolResetUnixNanos.Load()
	if last != 0 && now-last < poolResetCooldown.Nanoseconds() {
		return false
	}
	if !lastPoolResetUnixNanos.CompareAndSwap(last, now) {
		return false
	}
	pool.Reset()
	return true
}

func sleepForOperationError(err error, rng *rand.Rand) {
	if isRetryableTransient(err) {
		time.Sleep(transientRetryBackoff(1, rng))
		return
	}
	time.Sleep(time.Duration(rng.Intn(5)+1) * time.Millisecond)
}

func shouldExecuteWrite(cfg benchmarkConfig, rng *rand.Rand, mix *mixTracker, hasTrackedPayments bool) bool {
	if cfg.mixBasis == "transaction" {
		return rng.Float64() < cfg.writeRatio
	}
	currentWriteStatements, currentReadStatements := mix.Snapshot()
	return chooseWriteForStatementTarget(
		cfg.writeRatio,
		currentWriteStatements,
		currentReadStatements,
		cfg.estimatedWriteStatementsPerTxn(),
		cfg.estimatedReadStatementsPerTxn(hasTrackedPayments),
		rng,
	)
}

func chooseWriteForStatementTarget(
	targetWriteRatio float64,
	currentWriteStatements uint64,
	currentReadStatements uint64,
	estimatedWriteStatements uint64,
	estimatedReadStatements uint64,
	rng *rand.Rand,
) bool {
	if targetWriteRatio <= 0 {
		return false
	}
	if targetWriteRatio >= 1 {
		return true
	}

	if currentWriteStatements+currentReadStatements == 0 {
		return rng.Float64() < targetWriteRatio
	}

	projectedWriteRatioIfWrite := float64(currentWriteStatements+estimatedWriteStatements) /
		float64(currentWriteStatements+currentReadStatements+estimatedWriteStatements)
	projectedWriteRatioIfRead := float64(currentWriteStatements) /
		float64(currentWriteStatements+currentReadStatements+estimatedReadStatements)

	writeDistance := math.Abs(projectedWriteRatioIfWrite - targetWriteRatio)
	readDistance := math.Abs(projectedWriteRatioIfRead - targetWriteRatio)

	if math.Abs(writeDistance-readDistance) < 1e-9 {
		return rng.Intn(2) == 0
	}
	return writeDistance < readDistance
}

func runNonTxReader(
	ctx context.Context,
	pool *pgxpool.Pool,
	cfg benchmarkConfig,
	store *paymentStore,
	workerID int,
	mix *mixTracker,
) workerResult {
	result := newWorkerResult()
	rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)*2_000_000))
	accountCursor := newAccountTxnKeysetCursor()

	for {
		if ctx.Err() != nil {
			return result
		}

		if !shouldExecuteNonTxRead(cfg, mix, rng) {
			time.Sleep(500 * time.Microsecond)
			continue
		}

		start := time.Now()
		stmtCount, err, retries, conflicts, transientRetries, poolResets := executeWithRetry(ctx, pool, rng, func(opCtx context.Context) (int, error) {
			return executeNonTxReadOperation(opCtx, pool, cfg, store, rng, &accountCursor)
		})
		result.retries += retries
		result.conflicts += conflicts
		result.transientRetries += transientRetries
		result.poolResets += poolResets
		if err == nil {
			result.nonTxReads++
			result.readStatements += uint64(stmtCount)
			result.statements += uint64(stmtCount)
			result.nonTxReadLatency.Record(time.Since(start))
		} else {
			if ctx.Err() != nil {
				return result
			}
			result.errors++
			result.errorCodes[errorCode(err)]++
			sleepForOperationError(err, rng)
		}
	}
}

func shouldExecuteNonTxRead(cfg benchmarkConfig, mix *mixTracker, rng *rand.Rand) bool {
	return true
}

const latestTxnByAccountSQL = `
	SELECT txn_id, txn_state
	FROM fin_txn
	WHERE account_id = $1
	  AND txn_id > $2
	ORDER BY txn_id ASC
	LIMIT 1
`

const txnIDByJobIDSQL = `
	SELECT txn_id
	FROM op_job
	WHERE job_id = $1::uuid
`

const settledStatusByTxnSQL = `
	SELECT job_id, job_status, priority::bigint, next_event_time
	FROM op_job
	WHERE job_status = 'SETTLED'
	  AND txn_id = $1
	LIMIT 1
`

const latestTxnCoverByAccountSQL = `
	SELECT txn_id, created_at
	FROM fin_txn
	WHERE account_id = $1
	  AND txn_id <= $2
	ORDER BY txn_id ASC
	LIMIT 1
`

const latestAuditByTxnSQL = `
	SELECT object_ref, created_at, audit_id
	FROM evt_audit
	WHERE object_ref = $1
	ORDER BY created_at ASC, audit_id ASC
	LIMIT 1
`

const accountProfileByIDSQL = `
	SELECT account_type, currency
	FROM core_account
	WHERE account_id = $1
`

const availableLedgerByBalancePKSQL = `
	SELECT amount_value
	FROM acct_ledger
	WHERE balance_id = $1
	  AND ledger_type = 'AVAILABLE'
	  AND ledger_id = $1
`

const latestLedgerEntryByTxnSQL = `
	SELECT txn_id, created_at, entry_id
	FROM acct_ledger_entry
	WHERE txn_id = $1
	ORDER BY created_at ASC, entry_id ASC
	LIMIT 1
`

const latestStepByTxnSQL = `
	SELECT txn_id, step_id
	FROM fin_step
	WHERE txn_id = $1
	ORDER BY step_id ASC
	LIMIT 1
`

func queryTxnIDByJobID(ctx context.Context, queryer rowExecutor, jobID string) (int64, bool, error) {
	var txnID int64
	err := queryer.QueryRow(ctx, txnIDByJobIDSQL, jobID).Scan(&txnID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return txnID, true, nil
}

func querySettledByTxnID(ctx context.Context, queryer rowExecutor, txnID int64) error {
	var matchedJobID string
	var status string
	var priority int64
	var nextEventTime time.Time

	err := queryer.QueryRow(ctx, settledStatusByTxnSQL, txnID).Scan(&matchedJobID, &status, &priority, &nextEventTime)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return nil
}

func queryLatestTxnByAccount(
	ctx context.Context,
	queryer rowExecutor,
	accountID int64,
	cursor *accountTxnKeysetCursor,
) error {
	_, keysetTxnID := cursor.current()

	var txnID int64
	var status string
	err := queryer.QueryRow(
		ctx,
		latestTxnByAccountSQL,
		accountID,
		keysetTxnID,
	).Scan(&txnID, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			cursor.reset()
			return nil
		}
		return err
	}

	cursor.advance(time.Now().UTC(), txnID)
	return nil
}

func executeNonTxReadOperation(
	ctx context.Context,
	pool *pgxpool.Pool,
	cfg benchmarkConfig,
	store *paymentStore,
	rng *rand.Rand,
	accountCursor *accountTxnKeysetCursor,
) (int, error) {
	statementCount := 0
	pkLookupAccountID := int64(rng.Intn(cfg.operationalAccounts()) + 1)

	// Always-on autocommit PK lookups to increase read pressure without changing write paths.
	var accountType string
	var accountCurrency string
	err := pool.QueryRow(ctx, accountProfileByIDSQL, pkLookupAccountID).Scan(&accountType, &accountCurrency)
	statementCount++
	if err != nil {
		return statementCount, err
	}

	var ledgerAmount float64
	err = pool.QueryRow(ctx, availableLedgerByBalancePKSQL, pkLookupAccountID).Scan(&ledgerAmount)
	statementCount++
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return statementCount, err
	}

	// Deterministic production-like read chain across a known workflow:
	// op_job -> op_job_blob -> fin_txn -> fin_step -> evt_audit -> acct_balance.
	jobID, hasTrackedJob := store.Random(rng)
	if hasTrackedJob {
		var status string
		var amountMinor int64
		var txnID int64
		var debtorAccountID int64
		err := pool.QueryRow(ctx, `
				SELECT job_status, amount_minor, txn_id, debtor_account_id
				FROM op_job
				WHERE job_id = $1::uuid
			`, jobID).Scan(&status, &amountMinor, &txnID, &debtorAccountID)
		statementCount++
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return statementCount, err
		}

		if err == nil {
			var dataFormat int
			var contextFormat int
			err = pool.QueryRow(ctx, `
					SELECT data_format, context_format
					FROM op_job_blob
					WHERE job_id = $1::uuid
				`, jobID).Scan(&dataFormat, &contextFormat)
			statementCount++
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return statementCount, err
			}

			var txnType string
			var txnCreatedAt time.Time
			err = pool.QueryRow(ctx, `
					SELECT txn_type, created_at
					FROM fin_txn
					WHERE account_id = $1
					  AND txn_id = $2
				`, debtorAccountID, txnID).Scan(&txnType, &txnCreatedAt)
			statementCount++
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return statementCount, err
			}

			// Keep optional enrichment reads at 50% to reduce added select pressure.
			if rng.Intn(2) == 0 {
				var stepTxnID int64
				var stepID int64
				err = pool.QueryRow(ctx, latestStepByTxnSQL, txnID).Scan(&stepTxnID, &stepID)
				statementCount++
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return statementCount, err
				}

				var objectRef int64
				var auditCreatedAt time.Time
				var auditID int64
				err = pool.QueryRow(ctx, latestAuditByTxnSQL, txnID).Scan(&objectRef, &auditCreatedAt, &auditID)
				statementCount++
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return statementCount, err
				}

				var availableAmount float64
				err = pool.QueryRow(ctx, `
						SELECT available_amount
						FROM acct_balance
						WHERE bucket_id = $1
						  AND balance_id = $1
					`, debtorAccountID).Scan(&availableAmount)
				statementCount++
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return statementCount, err
				}
			}
		}
	} else {
		// Fallback path if no tracked jobs are available yet.
		prefetchAccountID := int64(rng.Intn(cfg.operationalAccounts()) + 1)
		var prefetchTxnID int64
		var prefetchTxnCreatedAt time.Time
		err := pool.QueryRow(ctx, latestTxnCoverByAccountSQL, prefetchAccountID, maxTxnIDKeyset).Scan(&prefetchTxnID, &prefetchTxnCreatedAt)
		statementCount++
		hasPrefetchTxn := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return statementCount, err
		}

		var accountType string
		var accountCurrency string
		err = pool.QueryRow(ctx, accountProfileByIDSQL, prefetchAccountID).Scan(&accountType, &accountCurrency)
		statementCount++
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return statementCount, err
		}

		if hasPrefetchTxn {
			var stepTxnID int64
			var stepID int64
			err = pool.QueryRow(ctx, latestStepByTxnSQL, prefetchTxnID).Scan(&stepTxnID, &stepID)
			statementCount++
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return statementCount, err
			}
		}
	}

	for i := 0; i < cfg.nonTxReadStatements; i++ {
		pattern := rng.Intn(7)
		switch pattern {
		case 0:
			accountID := int64(rng.Intn(cfg.operationalAccounts()) + 1)
			var balance float64
			err := pool.QueryRow(ctx, `
				SELECT available_amount
				FROM acct_balance
				WHERE bucket_id = $1
				  AND balance_id = $1
			`, accountID).Scan(&balance)
			if err != nil {
				return statementCount, err
			}
		case 1:
			jobID, ok := store.Random(rng)
			if !ok {
				accountID := int64(rng.Intn(cfg.operationalAccounts()) + 1)
				var balance float64
				err := pool.QueryRow(ctx, `
					SELECT available_amount
					FROM acct_balance
					WHERE bucket_id = $1
					  AND balance_id = $1
				`, accountID).Scan(&balance)
				if err != nil {
					return statementCount, err
				}
			} else {
				var status string
				var amountMinor int64
				var txnID int64
				var debtorAccountID int64
				err := pool.QueryRow(ctx, `
						SELECT job_status, amount_minor, txn_id, debtor_account_id
						FROM op_job
						WHERE job_id = $1::uuid
					`, jobID).Scan(&status, &amountMinor, &txnID, &debtorAccountID)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return statementCount, err
				}
				if err == nil {
					var dataFormat int
					var contextFormat int
					err = pool.QueryRow(ctx, `
							SELECT data_format, context_format
							FROM op_job_blob
							WHERE job_id = $1::uuid
						`, jobID).Scan(&dataFormat, &contextFormat)
					if err != nil && !errors.Is(err, pgx.ErrNoRows) {
						return statementCount, err
					}
					statementCount++

					// Realistic primary-key read on the transaction row after job fetch.
					var txnType string
					var createdAt time.Time
					err = pool.QueryRow(ctx, `
							SELECT txn_type, created_at
							FROM fin_txn
							WHERE account_id = $1
							  AND txn_id = $2
						`, debtorAccountID, txnID).Scan(&txnType, &createdAt)
					if err != nil && !errors.Is(err, pgx.ErrNoRows) {
						return statementCount, err
					}
					statementCount++
				}
			}
		case 2:
			accountID := int64(rng.Intn(cfg.operationalAccounts()) + 1)
			err := queryLatestTxnByAccount(ctx, pool, accountID, accountCursor)
			if err != nil {
				return statementCount, err
			}
		case 3:
			jobID, ok := store.Random(rng)
			if ok {
				txnID, found, err := queryTxnIDByJobID(ctx, pool, jobID)
				if err != nil {
					return statementCount, err
				}
				if found {
					statementCount++
					err = querySettledByTxnID(ctx, pool, txnID)
					if err != nil {
						return statementCount, err
					}
					statementCount++
				}
			}
		case 4:
			accountID := int64(rng.Intn(cfg.operationalAccounts()) + 1)
			var txnID int64
			var createdAt time.Time
			err := pool.QueryRow(ctx, latestTxnCoverByAccountSQL, accountID, maxTxnIDKeyset).Scan(&txnID, &createdAt)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return statementCount, err
			}
			if err == nil {
				var ledgerTxnID int64
				var ledgerCreatedAt time.Time
				var entryID int64
				err = pool.QueryRow(ctx, latestLedgerEntryByTxnSQL, txnID).Scan(&ledgerTxnID, &ledgerCreatedAt, &entryID)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return statementCount, err
				}
				statementCount++
			}
		case 5:
			accountID := int64(rng.Intn(cfg.operationalAccounts()) + 1)
			var txnID int64
			var createdAt time.Time
			err := pool.QueryRow(ctx, latestTxnCoverByAccountSQL, accountID, maxTxnIDKeyset).Scan(&txnID, &createdAt)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return statementCount, err
			}
		default:
			jobID, ok := store.Random(rng)
			if !ok {
				accountID := int64(rng.Intn(cfg.operationalAccounts()) + 1)
				var accountType string
				var accountCurrency string
				err := pool.QueryRow(ctx, accountProfileByIDSQL, accountID).Scan(&accountType, &accountCurrency)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return statementCount, err
				}
			} else {
				var txnID int64
				err := pool.QueryRow(ctx, `
						SELECT txn_id
						FROM op_job
						WHERE job_id = $1::uuid
					`, jobID).Scan(&txnID)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return statementCount, err
				}
				if err == nil {
					var objectRef int64
					var createdAt time.Time
					var auditID int64
					err = pool.QueryRow(ctx, latestAuditByTxnSQL, txnID).Scan(&objectRef, &createdAt, &auditID)
					if err != nil && !errors.Is(err, pgx.ErrNoRows) {
						return statementCount, err
					}
					statementCount++
				}
			}
		}
		statementCount++
	}

	return statementCount, nil
}

func executeWriteTransaction(
	ctx context.Context,
	pool *pgxpool.Pool,
	cfg benchmarkConfig,
	store *paymentStore,
	rng *rand.Rand,
	workerID int,
	sequence uint64,
) (int, error) {
	workflow := buildWriteWorkflowContext(cfg, rng, workerID, sequence)
	var statementCount int

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return statementCount, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if cfg.writeLockMode == writeLockModeAdvisoryXact {
		leftKey, rightKey := advisoryLockKeys(cfg, workflow, rng)
		_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::integer, $2::integer)`, leftKey, rightKey)
		if err != nil {
			return statementCount, err
		}
		statementCount++
	}

	stageStatements, err := executeIngestAndValidateStage(ctx, tx, workflow)
	statementCount += stageStatements
	if err != nil {
		return statementCount, err
	}

	stageStatements, err = executeRouteAndSettleStage(ctx, tx, cfg, workflow)
	statementCount += stageStatements
	if err != nil {
		return statementCount, err
	}

	if err = tx.Commit(ctx); err != nil {
		return statementCount, err
	}

	store.Add(workflow.paymentID)
	return statementCount, nil
}

type writeWorkflowContext struct {
	paymentID       string
	idempotenceID   string
	bucketID        int64
	bucketUUID      string
	linkUUID        string
	snapshotUUID    string
	clientRef       string
	debtor          int64
	creditor        int64
	settlement      int64
	amountMinor     int64
	feeMinor        int64
	txnID           int64
	batchID         int64
	priority        int
	auditBaseID     int64
	createdAt       time.Time
	straightThrough bool
	jobPayload      []byte
	jobContext      []byte
	outgoingBinary  []byte
}

func buildWriteWorkflowContext(cfg benchmarkConfig, rng *rand.Rand, workerID int, sequence uint64) writeWorkflowContext {
	operationalMax := cfg.operationalAccounts()
	opShardStart, opShardEnd := workerShardBounds(operationalMax, cfg.threads, workerID)
	settlementShardStart, settlementShardEnd := workerShardBounds(cfg.settlementAccounts, cfg.threads, workerID)

	debtor := int64(randomInBounds(rng, opShardStart, opShardEnd))
	creditor := debtor
	if opShardEnd-opShardStart >= 1 {
		for creditor == debtor {
			creditor = int64(randomInBounds(rng, opShardStart, opShardEnd))
		}
	} else {
		for creditor == debtor {
			creditor = int64(rng.Intn(operationalMax) + 1)
		}
	}

	settlementOffset := randomInBounds(rng, settlementShardStart, settlementShardEnd)
	settlement := int64(operationalMax + settlementOffset)

	amountMinor := int64(rng.Intn(900_000) + 100_00) // 100.00 to 9,999.99 GBP.
	feeMinor := maxInt64(25, amountMinor/200)        // ~0.5% fee with a floor.
	straightThrough := rng.Intn(100) < 50            // ~50% STP lane to reduce mutable update pressure.

	now := time.Now().UTC()
	txnID := now.UnixNano() + int64(workerID)*1_000_000 + int64(sequence)
	batchID := debtor*100_000 + creditor
	paymentID := randomUUID(rng)
	clientRef := fmt.Sprintf("wf-%02d-%013d-%06d", workerID, now.UnixMilli(), sequence)

	return writeWorkflowContext{
		paymentID:       paymentID,
		idempotenceID:   randomUUID(rng),
		bucketID:        batchID,
		bucketUUID:      randomUUID(rng),
		linkUUID:        randomUUID(rng),
		snapshotUUID:    randomUUID(rng),
		clientRef:       clientRef,
		debtor:          debtor,
		creditor:        creditor,
		settlement:      settlement,
		amountMinor:     amountMinor,
		feeMinor:        feeMinor,
		txnID:           txnID,
		batchID:         batchID,
		priority:        rng.Intn(5) + 1,
		auditBaseID:     txnID,
		createdAt:       now,
		straightThrough: straightThrough,
		jobPayload:      []byte{0x1f, 0x8b, 0x08, 0x00, 0x2a, 0x2b, 0x2c, 0x2d},
		jobContext:      []byte("null"),
		outgoingBinary:  []byte{0x00, 0x00, 0x01, 0x1f, 0x8b, 0x08, 0x00, 0x7f, 0x7e, 0x7d},
	}
}

func advisoryLockKeys(cfg benchmarkConfig, workflow writeWorkflowContext, rng *rand.Rand) (int32, int32) {
	// {left,right} where left is intentionally low-cardinality and right is mostly high-cardinality.
	leftKey := int32((workflow.debtor-1)%int64(cfg.advisoryLowCard) + 1)

	mix := uint64(workflow.txnID)
	mix ^= uint64(workflow.batchID)
	mix ^= uint64(workflow.debtor) << 13
	mix ^= uint64(workflow.creditor) << 7
	rightKey := int32(mix%uint64(cfg.advisoryHighCard) + 1)

	// Route a small share into a compact right-key hotspot to create low single-digit blocking.
	if rng.Float64()*100.0 < cfg.advisoryHotspotPct {
		rightKey = int32((workflow.debtor-1)%int64(cfg.advisoryHotspotCard) + 1)
	}

	return leftKey, rightKey
}

func workerShardBounds(total int, workers int, workerID int) (int, int) {
	if total <= 1 {
		return 1, 1
	}
	if workers < 1 {
		workers = 1
	}
	shards := minInt(total, workers)
	idx := workerID % shards
	base := total / shards
	extra := total % shards

	startOffset := idx * base
	startOffset += minInt(idx, extra)
	size := base
	if idx < extra {
		size++
	}
	if size < 1 {
		size = 1
	}
	start := startOffset + 1
	end := start + size - 1
	return start, end
}

func randomInBounds(rng *rand.Rand, start int, end int) int {
	if end <= start {
		return start
	}
	return start + rng.Intn(end-start+1)
}

func randomUUID(rng *rand.Rand) string {
	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		rng.Uint32(),
		uint16(rng.Uint32()),
		uint16(rng.Uint32()),
		uint16(rng.Uint32()),
		rng.Uint64()&0x0000ffffffffffff,
	)
}

func executeIngestAndValidateStage(
	ctx context.Context,
	tx pgx.Tx,
	workflow writeWorkflowContext,
) (int, error) {
	var statementCount int
	var err error
	initialJobStatus := "SUBMITTED"
	if workflow.straightThrough {
		initialJobStatus = "SETTLED"
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO op_job (
			job_id, job_type, job_sub_type, job_status, job_data,
			next_event_time, state_time, time_created, time_updated,
			processing_tries_count, version, priority,
			txn_id, debtor_account_id, creditor_account_id, settlement_account_id, amount_minor, currency
		) VALUES (
			$1::uuid, 'CREATE_BANK_TRANSACTION', NULL, $9, '',
			$2, $2, $2, $2,
			0, 0, $3,
			$4, $5, $6, $7, $8, 'GBP'
		)
	`, workflow.paymentID, workflow.createdAt, workflow.priority, workflow.txnID, workflow.debtor, workflow.creditor, workflow.settlement, workflow.amountMinor, initialJobStatus)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	_, err = tx.Exec(ctx, `
		INSERT INTO op_job_blob (job_id, data_format, data, context_format, context_data)
		VALUES ($1::uuid, 1, $2::bytea, 0, $3::bytea)
		ON CONFLICT (job_id) DO UPDATE
		SET data = EXCLUDED.data,
		    context_data = EXCLUDED.context_data
	`, workflow.paymentID, workflow.jobPayload, workflow.jobContext)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	_, err = tx.Exec(ctx, `
		INSERT INTO evt_audit (audit_id, change_type, object_ref, change_note, actor_id, created_at, client_id)
		VALUES ($1, 'TRANSACTION', $2, 'step=submitted', 'workflow-service', $3, '')
	`, workflow.auditBaseID, workflow.txnID, workflow.createdAt)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	_, err = tx.Exec(ctx, `
		INSERT INTO fin_idem (idempotence_id, created_at)
		VALUES ($1::uuid, $2)
	`, workflow.idempotenceID, workflow.createdAt)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	if !workflow.straightThrough {
		commandTag, err := tx.Exec(ctx, `
			UPDATE op_job
			SET job_status = 'VALIDATED',
			    state_time = $2,
			    time_updated = $2,
			    version = version + 1
			WHERE job_id = $1::uuid
			  AND job_status = 'SUBMITTED'
		`, workflow.paymentID, workflow.createdAt)
		if err != nil {
			return statementCount, err
		}
		if commandTag.RowsAffected() != 1 {
			return statementCount, errors.New("job status transition to VALIDATED failed")
		}
		statementCount++
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO evt_audit (audit_id, change_type, object_ref, change_note, actor_id, created_at, client_id)
		VALUES ($1, 'TRANSACTION', $2, 'step=validated', 'workflow-service', $3, '')
	`, workflow.auditBaseID+1, workflow.txnID, workflow.createdAt)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	return statementCount, nil
}

func executeRouteAndSettleStage(
	ctx context.Context,
	tx pgx.Tx,
	cfg benchmarkConfig,
	workflow writeWorkflowContext,
) (int, error) {
	var statementCount int
	var err error

	stageTime := workflow.createdAt.Add(5 * time.Millisecond)
	if !workflow.straightThrough {
		if cfg.writeLockMode == writeLockModeForUpdate {
			var status string
			err = tx.QueryRow(ctx, `
				SELECT job_status
				FROM op_job
				WHERE job_id = $1::uuid
				FOR UPDATE
			`, workflow.paymentID).Scan(&status)
			if err != nil {
				return statementCount, err
			}
			statementCount++
			if status != "VALIDATED" {
				return statementCount, fmt.Errorf("job %s in unexpected status %s before routing", workflow.paymentID, status)
			}
		}

		commandTag, err := tx.Exec(ctx, `
			UPDATE op_job
			SET job_status = 'ROUTING',
			    processing_tries_count = processing_tries_count + 1,
			    state_time = $2,
			    time_updated = $2,
			    version = version + 1
			WHERE job_id = $1::uuid
			  AND job_status = 'VALIDATED'
		`, workflow.paymentID, stageTime)
		if err != nil {
			return statementCount, err
		}
		if commandTag.RowsAffected() != 1 {
			return statementCount, errors.New("job status transition to ROUTING failed")
		}
		statementCount++
	}

	initialTxnState := "PROCESSING"
	if workflow.straightThrough {
		initialTxnState = "COMPLETED"
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO fin_txn (txn_id, created_at, txn_type, txn_state, account_id, creator_profile_id, batch_id, version)
		VALUES ($1, $2, 'TRANSFER', $5, $3, $3, $4, 0)
		ON CONFLICT (account_id, txn_id) DO UPDATE
		SET txn_state = EXCLUDED.txn_state
	`, workflow.txnID, stageTime, workflow.debtor, workflow.batchID, initialTxnState)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	_, err = tx.Exec(ctx, `
		INSERT INTO fin_idem_map (txn_id, idempotence_id, created_at)
		VALUES ($1, $2::uuid, $3)
	`, workflow.txnID, workflow.idempotenceID, stageTime)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	var debtorType string
	var debtorCurrency string
	err = tx.QueryRow(ctx, accountProfileByIDSQL, workflow.debtor).Scan(&debtorType, &debtorCurrency)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	var creditorType string
	var creditorCurrency string
	err = tx.QueryRow(ctx, accountProfileByIDSQL, workflow.creditor).Scan(&creditorType, &creditorCurrency)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	var settlementType string
	var settlementCurrency string
	err = tx.QueryRow(ctx, accountProfileByIDSQL, workflow.settlement).Scan(&settlementType, &settlementCurrency)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	var idemCreatedAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT created_at
		FROM fin_idem
		WHERE idempotence_id = $1::uuid
	`, workflow.idempotenceID).Scan(&idemCreatedAt)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	var mappedTxnID int64
	err = tx.QueryRow(ctx, `
		SELECT txn_id
		FROM fin_idem_map
		WHERE idempotence_id = $1::uuid
		ORDER BY txn_id ASC
		LIMIT 1
	`, workflow.idempotenceID).Scan(&mappedTxnID)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	_, err = tx.Exec(ctx, `
		INSERT INTO evt_audit (audit_id, change_type, object_ref, change_note, actor_id, created_at, client_id)
		VALUES ($1, 'TRANSACTION', $2, 'step=screening_started', 'workflow-service', $3, '')
	`, workflow.auditBaseID+2, workflow.txnID, stageTime)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	_, err = tx.Exec(ctx, `
		INSERT INTO acct_bucket (
			bucket_uuid, bucket_id, product_name, bucket_type, bucket_state, bucket_name, currency, icon,
			creator_account_id, creator_profile_id, creator_user_id, created_at, updated_at, version
		) VALUES (
			$1::uuid, $2, 'MAIN', 'NONE', 'ACTIVE', NULL, 'GBP', 'null'::jsonb,
			$3, $3, $3, $4, $4, 0
		)
		ON CONFLICT (bucket_id) DO UPDATE
		SET bucket_state = EXCLUDED.bucket_state,
		    bucket_name = EXCLUDED.bucket_name,
		    currency = EXCLUDED.currency,
		    icon = EXCLUDED.icon,
		    updated_at = EXCLUDED.updated_at,
		    creator_user_id = EXCLUDED.creator_user_id,
		    version = acct_bucket.version + 1
		WHERE acct_bucket.version = 0 OR acct_bucket.bucket_type <> 'SHARED'
	`, workflow.bucketUUID, workflow.bucketID, workflow.debtor, stageTime)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	_, err = tx.Exec(ctx, `
		INSERT INTO acct_link (
			link_uuid, bucket_id, account_id, profile_id, ownership, link_state, created_at, updated_at, version
		) VALUES (
			$1::uuid, $2, $3, $3, 'OWNER', 'ACTIVE', $4, $4, 0
		)
		ON CONFLICT (bucket_id, link_uuid) DO UPDATE
		SET link_state = EXCLUDED.link_state,
		    updated_at = EXCLUDED.updated_at,
		    version = acct_link.version + 1
	`, workflow.linkUUID, workflow.bucketID, workflow.debtor, stageTime)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	for i := 0; i < cfg.extraWriteEvents; i++ {
		_, err = tx.Exec(ctx, `
			INSERT INTO evt_audit (audit_id, change_type, object_ref, change_note, actor_id, created_at, client_id)
			VALUES ($1, 'TRANSACTION', $2, $3, 'workflow-service', $4, '')
		`, workflow.auditBaseID+3+int64(i), workflow.txnID, fmt.Sprintf("step=screening_%02d", i+1), stageTime)
		if err != nil {
			return statementCount, err
		}
		statementCount++
	}

	type leg struct {
		seq       int
		accountID int64
		legType   string
		amount    int64
	}

	legs := make([]leg, 0, cfg.ledgerLegs)
	accountDeltas := make(map[int64]int64, 3)
	for legSeq := 1; legSeq <= cfg.ledgerLegs; legSeq++ {
		legAccount, legType, legAmount := deriveLedgerLeg(
			legSeq,
			workflow.debtor,
			workflow.creditor,
			workflow.settlement,
			workflow.amountMinor,
			workflow.feeMinor,
		)
		legs = append(legs, leg{seq: legSeq, accountID: legAccount, legType: legType, amount: legAmount})
		_, err = tx.Exec(ctx, `
			INSERT INTO fin_step (
				step_id, created_at, step_type, txn_id,
				source_amount_currency, source_amount_value,
				tracing_reference_code, reversed_step_id,
				source_balance_id, target_balance_id,
				channel_name, channel_reference_id
			) VALUES (
				$1, $2, $3, $4,
				'GBP', $5,
				$6, NULL,
				$7, $8,
				'TRANSFER', $9
			)
		`, int64(legSeq), stageTime, legType, workflow.txnID, float64(legAmount)/100.0, workflow.clientRef, legAccount, legAccount, workflow.clientRef)
		if err != nil {
			return statementCount, err
		}
		statementCount++

		accountDeltas[legAccount] += legAmount
	}

	accountIDs := make([]int64, 0, len(accountDeltas))
	for accountID, delta := range accountDeltas {
		if delta != 0 {
			accountIDs = append(accountIDs, accountID)
		}
	}
	sort.Slice(accountIDs, func(i, j int) bool {
		return accountIDs[i] < accountIDs[j]
	})

	type accountAfter struct {
		balanceID      int64
		availableAfter float64
		ledgerAfter    float64
	}
	accountAfterState := make(map[int64]accountAfter, len(accountIDs))

	for idx, accountID := range accountIDs {
		deltaMajor := float64(accountDeltas[accountID]) / 100.0

		var balanceID int64
		var availableAfter float64
		err = tx.QueryRow(ctx, `
			UPDATE acct_balance b
			SET available_amount = available_amount + $1,
			    updated_at = $2,
			    version = version + 1
			WHERE b.bucket_id = $3
			  AND b.balance_id = $3
			  AND (
			    EXISTS (
			      SELECT 1
			      FROM core_account a
			      WHERE a.account_id = $3
			        AND a.account_type = 'SETTLEMENT'
			    )
			    OR available_amount + $1 >= 0
			  )
			RETURNING balance_id, available_amount
		`, deltaMajor, stageTime, accountID).Scan(&balanceID, &availableAfter)
		if err != nil {
			return statementCount, err
		}
		statementCount++

		ledgerAfter := availableAfter
		_, err = tx.Exec(ctx, `
			INSERT INTO acct_ledger (ledger_id, balance_id, ledger_type, amount_currency, amount_value, created_at, updated_at)
			VALUES ($1, $2, 'AVAILABLE', 'GBP', $3, $4, $4)
		`, workflow.txnID, balanceID, ledgerAfter, stageTime)
		if err != nil {
			return statementCount, err
		}
		statementCount++

		_, err = tx.Exec(ctx, `
			INSERT INTO acct_ledger_entry (ledger_id, txn_id, amount, balance_after, created_at, step_id)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, balanceID, workflow.txnID, deltaMajor, ledgerAfter, stageTime, int64(100+idx))
		if err != nil {
			return statementCount, err
		}
		statementCount++

		accountAfterState[accountID] = accountAfter{
			balanceID:      balanceID,
			availableAfter: availableAfter,
			ledgerAfter:    ledgerAfter,
		}
	}

	creditorAfter, ok := accountAfterState[workflow.creditor]
	if !ok {
		creditorAfter = accountAfter{
			balanceID:      workflow.creditor,
			availableAfter: 0,
			ledgerAfter:    0,
		}
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO acct_balance_snapshot (
			snapshot_id, txn_id, balance_id, step_id,
			ledger_amount_after, available_amount_after, total_worth_after,
			holdings_accessible_amount_after, holdings_total_worth_after,
			currency, created_at
		) VALUES (
			$1::uuid, $2, $3, $4,
			$5, $6, $7,
			0.00, 0.00,
			'GBP', $8
		)
	`, workflow.snapshotUUID, workflow.txnID, creditorAfter.balanceID, int64(len(legs)), creditorAfter.ledgerAfter, creditorAfter.availableAfter, creditorAfter.availableAfter, stageTime)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	_, err = tx.Exec(ctx, `
		INSERT INTO out_msg_0 (message)
		VALUES ($1::bytea)
	`, workflow.outgoingBinary)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	_, err = tx.Exec(ctx, `
		INSERT INTO evt_audit (audit_id, change_type, object_ref, change_note, actor_id, created_at, client_id)
		VALUES ($1, 'TRANSACTION', $2, 'step=posted', 'workflow-service', $3, '')
	`, workflow.auditBaseID+100, workflow.txnID, stageTime)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	if !workflow.straightThrough {
		commandTag, err := tx.Exec(ctx, `
			UPDATE fin_txn
			SET txn_state = 'COMPLETED',
			    version = version + 1
			WHERE account_id = $1
			  AND txn_id = $2
		`, workflow.debtor, workflow.txnID)
		if err != nil {
			return statementCount, err
		}
		if commandTag.RowsAffected() != 1 {
			return statementCount, errors.New("transaction state transition to COMPLETED failed")
		}
		statementCount++

		commandTag, err = tx.Exec(ctx, `
			UPDATE op_job
			SET job_status = 'SETTLED',
			    state_time = $2,
			    time_updated = $2,
			    version = version + 1
			WHERE job_id = $1::uuid
			  AND job_status = 'ROUTING'
		`, workflow.paymentID, stageTime)
		if err != nil {
			return statementCount, err
		}
		if commandTag.RowsAffected() != 1 {
			return statementCount, errors.New("job status transition to SETTLED failed")
		}
		statementCount++
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO evt_audit (audit_id, change_type, object_ref, change_note, actor_id, created_at, client_id)
		VALUES ($1, 'TRANSACTION', $2, 'step=settled', 'workflow-service', $3, '')
	`, workflow.auditBaseID+101, workflow.txnID, stageTime)
	if err != nil {
		return statementCount, err
	}
	statementCount++

	return statementCount, nil
}

func executeReadTransaction(
	ctx context.Context,
	pool *pgxpool.Pool,
	cfg benchmarkConfig,
	store *paymentStore,
	rng *rand.Rand,
	accountCursor *accountTxnKeysetCursor,
) (int, error) {
	var statementCount int

	jobID, ok := store.Random(rng)
	if ok {
		var txnID int64
		var debtorID int64
		var creditorID int64
		var status string
		var amountMinor int64

		err := pool.QueryRow(ctx, `
			SELECT txn_id, job_status, debtor_account_id, creditor_account_id, amount_minor
			FROM op_job
			WHERE job_id = $1::uuid
		`, jobID).Scan(&txnID, &status, &debtorID, &creditorID, &amountMinor)
		if err == nil {
			statementCount++

			var payloadBytes int64
			var contextBytes int64
			err = pool.QueryRow(ctx, `
				SELECT octet_length(data), octet_length(context_data)
				FROM op_job_blob
				WHERE job_id = $1::uuid
			`, jobID).Scan(&payloadBytes, &contextBytes)
			if err != nil {
				return statementCount, err
			}
			statementCount++

			var stepCount int64
			var stepSum float64
			err = pool.QueryRow(ctx, `
				SELECT count(*), COALESCE(sum(source_amount_value), 0)
				FROM fin_step
				WHERE txn_id = $1
			`, txnID).Scan(&stepCount, &stepSum)
			if err != nil {
				return statementCount, err
			}
			statementCount++

			var ledgerRows int64
			var ledgerAmount float64
			err = pool.QueryRow(ctx, `
				SELECT count(*), COALESCE(sum(amount), 0)
				FROM acct_ledger_entry
				WHERE txn_id = $1
			`, txnID).Scan(&ledgerRows, &ledgerAmount)
			if err != nil {
				return statementCount, err
			}
			statementCount++

			var auditRows int64
			var latestAudit int64
			err = pool.QueryRow(ctx, `
				SELECT count(*), COALESCE(max(audit_id), 0)
				FROM evt_audit
				WHERE object_ref = $1
			`, txnID).Scan(&auditRows, &latestAudit)
			if err != nil {
				return statementCount, err
			}
			statementCount++

			var debtorBalance float64
			err = pool.QueryRow(ctx, `
				SELECT available_amount
				FROM acct_balance
				WHERE bucket_id = $1
				  AND balance_id = $1
			`, debtorID).Scan(&debtorBalance)
			if err != nil {
				return statementCount, err
			}
			statementCount++

			var creditorBalance float64
			err = pool.QueryRow(ctx, `
							SELECT available_amount
							FROM acct_balance
							WHERE bucket_id = $1
							  AND balance_id = $1
						`, creditorID).Scan(&creditorBalance)
			if err != nil {
				return statementCount, err
			}
			statementCount++

			var debtorAccountType string
			var debtorCurrency string
			err = pool.QueryRow(ctx, accountProfileByIDSQL, debtorID).Scan(&debtorAccountType, &debtorCurrency)
			if err != nil {
				return statementCount, err
			}
			statementCount++

			var creditorAccountType string
			var creditorCurrency string
			err = pool.QueryRow(ctx, accountProfileByIDSQL, creditorID).Scan(&creditorAccountType, &creditorCurrency)
			if err != nil {
				return statementCount, err
			}
			statementCount++

		} else if !errors.Is(err, pgx.ErrNoRows) {
			return statementCount, err
		}
	}

	if statementCount == 0 {
		accountID := int64(rng.Intn(cfg.operationalAccounts()) + 1)
		var balance float64
		err := pool.QueryRow(ctx, `
			SELECT available_amount
			FROM acct_balance
			WHERE bucket_id = $1
			  AND balance_id = $1
		`, accountID).Scan(&balance)
		if err != nil {
			return statementCount, err
		}
		statementCount++

		err = queryLatestTxnByAccount(ctx, pool, accountID, accountCursor)
		if err != nil {
			return statementCount, err
		}
		statementCount++

	}

	return statementCount, nil
}

func deriveLedgerLeg(
	legSeq int,
	debtor int64,
	creditor int64,
	settlement int64,
	amountMinor int64,
	feeMinor int64,
) (int64, string, int64) {
	switch legSeq {
	case 1:
		return debtor, "DEBIT_PRINCIPAL", -amountMinor
	case 2:
		return creditor, "CREDIT_PRINCIPAL", amountMinor
	case 3:
		return debtor, "DEBIT_FEE", -feeMinor
	case 4:
		return settlement, "CREDIT_FEE", feeMinor
	default:
		// Extra legs are audit/trace writes and intentionally zero-value to preserve reconciliation.
		if legSeq%2 == 0 {
			return settlement, "SETTLEMENT_TRACE", 0
		}
		return debtor, "CLEARING_TRACE", 0
	}
}

func errorCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "DEADLINE_EXCEEDED"
	}
	if errors.Is(err, context.Canceled) {
		return "CANCELED"
	}
	return "UNKNOWN"
}

func printSummary(cfg benchmarkConfig, summary benchmarkSummary, trackedPayments int) {
	result := summary.result
	total := result.reads + result.writes
	totalStatements := result.readStatements + result.writeStatements
	seconds := summary.elapsed.Seconds()
	if seconds <= 0 {
		seconds = 1
	}

	ops := float64(total) / seconds
	dbTxPerSecond := float64(result.reads+result.writes) / seconds
	qps := float64(result.statements) / seconds

	readPct := 0.0
	writePct := 0.0
	if total > 0 {
		readPct = 100.0 * float64(result.reads) / float64(total)
		writePct = 100.0 * float64(result.writes) / float64(total)
	}

	readStmtPct := 0.0
	writeStmtPct := 0.0
	if totalStatements > 0 {
		readStmtPct = 100.0 * float64(result.readStatements) / float64(totalStatements)
		writeStmtPct = 100.0 * float64(result.writeStatements) / float64(totalStatements)
	}

	fmt.Println()
	fmt.Println("=== ledger_bench ===")
	fmt.Printf("elapsed: %s\n", summary.elapsed.Truncate(time.Millisecond))
	fmt.Printf("threads: %d\n", cfg.threads)
	fmt.Printf("non-tx readers: %d (statements/op=%d)\n", cfg.nonTxReaders, cfg.nonTxReadStatements)
	fmt.Printf("mix basis: %s\n", cfg.mixBasis)
	fmt.Printf("write lock mode: %s\n", cfg.writeLockMode)
	if cfg.writeLockMode == writeLockModeAdvisoryXact {
		fmt.Printf("advisory lock key shape: {left_card=%d,right_card=%d} hotspot=%.2f%% right_hot_card=%d\n",
			cfg.advisoryLowCard, cfg.advisoryHighCard, cfg.advisoryHotspotPct, cfg.advisoryHotspotCard)
	}
	fmt.Printf("target %s mix: %.1f%% read / %.1f%% write\n", cfg.mixBasis, (1.0-cfg.writeRatio)*100, cfg.writeRatio*100)
	fmt.Printf("actual operation mix: %.1f%% read / %.1f%% write\n", readPct, writePct)
	fmt.Printf("actual statement mix: %.1f%% read / %.1f%% write\n", readStmtPct, writeStmtPct)
	fmt.Printf("ledger legs per write workflow: %d\n", cfg.ledgerLegs)
	fmt.Printf("extra write events per write workflow: %d\n", cfg.extraWriteEvents)
	fmt.Printf("write workflow commits: %d\n", 1)
	fmt.Printf("tracked workflow jobs in memory: %d\n", trackedPayments)
	fmt.Println()
	fmt.Printf("successful operations: %d (read-ops=%d write-workflows=%d)\n", total, result.reads, result.writes)
	fmt.Printf("non-tx read ops: %d\n", result.nonTxReads)
	fmt.Printf("statement totals: %d (read=%d write=%d)\n", totalStatements, result.readStatements, result.writeStatements)
	fmt.Printf("retries applied (40001/40P01): %d\n", result.retries)
	fmt.Printf("conflicts observed (40001/40P01): %d\n", result.conflicts)
	fmt.Printf("transient retries applied (reconnect/failover): %d\n", result.transientRetries)
	fmt.Printf("pool resets triggered: %d\n", result.poolResets)
	if attempts := total + result.errors; attempts > 0 {
		fmt.Printf("terminal conflict/error rate: %.2f%%\n", 100.0*float64(result.errors)/float64(attempts))
	}
	fmt.Printf("errors: %d\n", result.errors)
	fmt.Printf("workflow op/s: %.2f\n", ops)
	fmt.Printf("database tx/s (approx): %.2f\n", dbTxPerSecond)
	fmt.Printf("statement qps: %.2f\n", qps)
	fmt.Println()
	printLatency("read-op", result.readLatency)
	printLatency("non-tx-read", result.nonTxReadLatency)
	printLatency("write-workflow", result.writeLatency)
	printErrorCodes(result.errorCodes)
}

func printLatency(name string, hist latencyHistogram) {
	total := hist.Total()
	if total == 0 {
		fmt.Printf("%s latency: no samples\n", strings.ToUpper(name))
		return
	}

	fmt.Printf(
		"%s latency: avg=%s p50~%s p95~%s p99~%s max~%s\n",
		strings.ToUpper(name),
		formatDuration(hist.Average()),
		formatDuration(hist.Percentile(0.50)),
		formatDuration(hist.Percentile(0.95)),
		formatDuration(hist.Percentile(0.99)),
		formatDuration(time.Duration(hist.maxMicros)*time.Microsecond),
	)
}

func printErrorCodes(codes map[string]uint64) {
	if len(codes) == 0 {
		return
	}

	type pair struct {
		code  string
		count uint64
	}
	list := make([]pair, 0, len(codes))
	for code, count := range codes {
		list = append(list, pair{code: code, count: count})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].count == list[j].count {
			return list[i].code < list[j].code
		}
		return list[i].count > list[j].count
	})

	fmt.Println()
	fmt.Println("error code counts:")
	for _, item := range list {
		fmt.Printf("- %s: %d\n", item.code, item.count)
	}
}

func formatDuration(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%.2fs", d.Seconds())
	case d >= time.Millisecond:
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	case d >= time.Microsecond:
		return fmt.Sprintf("%.2fµs", float64(d)/float64(time.Microsecond))
	default:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

var schemaNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validSchemaName(name string) bool {
	return schemaNameRE.MatchString(name)
}

func canonicalMixBasis(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "statement", "statements", "stmt":
		return "statement", true
	case "transaction", "transactions", "txn":
		return "transaction", true
	default:
		return "", false
	}
}

func canonicalWriteLockMode(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "for-update", "for_update", "forupdate", "row-lock", "row_lock":
		return writeLockModeForUpdate, true
	case "advisory-xact", "advisory_xact", "advisory", "advisory-lock", "advisory_lock":
		return writeLockModeAdvisoryXact, true
	default:
		return "", false
	}
}
