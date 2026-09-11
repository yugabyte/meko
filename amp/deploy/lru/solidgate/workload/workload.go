package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
)

const (
	writeToReadRatio  = 1.574
	insertToUpdateRat = 0.412
	scopeDefault      = "default"
	// Bounds r_profile_by_event: a profile accumulates events for as long as the
	// workload runs, and the unbounded self-join grew with the table.
	profileEventsLimit = 20
)

var syntheticInsertID uint64

func nextSyntheticInsertID() int64 {
	if atomic.LoadUint64(&syntheticInsertID) == 0 {
		_ = atomic.CompareAndSwapUint64(&syntheticInsertID, 0, uint64(time.Now().UnixNano()))
	}
	return int64(atomic.AddUint64(&syntheticInsertID, 1))
}

type weightedStmt struct {
	name   string
	weight float64
}

type weightedSet struct {
	total float64
	items []weightedStmt
}

func newWeightedSet(items []weightedStmt) weightedSet {
	t := 0.0
	for _, it := range items {
		t += it.weight
	}
	return weightedSet{total: t, items: items}
}

func (w weightedSet) pick(r *rand.Rand) string {
	if len(w.items) == 0 {
		return ""
	}
	t := r.Float64() * w.total
	acc := 0.0
	for _, it := range w.items {
		acc += it.weight
		if t <= acc {
			return it.name
		}
	}
	return w.items[len(w.items)-1].name
}

type counters struct {
	txns        uint64
	txnErrs     uint64
	txnLatNs    uint64
	readOps     uint64
	readErrs    uint64
	readLatNs   uint64
	insertOps   uint64
	insertErrs  uint64
	insertLatNs uint64
	updateOps   uint64
	updateErrs  uint64
	updateLatNs uint64
}

type cfg struct {
	seedOrders       int
	seedShops        int
	seedTokens       int
	stmtsPerTxn      int
	targetTPS        float64
	txnCTE           bool
	txnCTEFixedShape bool
	fixedOps         []txnOp
	workers          int
}

type nodeEndpoint struct {
	host string
	port int
}

type opKind int

const (
	opRead opKind = iota
	opInsert
	opUpdate
)

type txnOp struct {
	kind opKind
	name string
}

func planTxnOps(r *rand.Rand, cfg cfg, readSet, insertSet, updateSet weightedSet, readProb, insertProb float64) []txnOp {
	if cfg.txnCTEFixedShape && len(cfg.fixedOps) > 0 {
		return cfg.fixedOps
	}
	ops := make([]txnOp, 0, cfg.stmtsPerTxn)
	for i := 0; i < cfg.stmtsPerTxn; i++ {
		u := r.Float64()
		if u < readProb {
			ops = append(ops, txnOp{kind: opRead, name: readSet.pick(r)})
			continue
		}
		if u < readProb+insertProb {
			ops = append(ops, txnOp{kind: opInsert, name: insertSet.pick(r)})
			continue
		}
		ops = append(ops, txnOp{kind: opUpdate, name: updateSet.pick(r)})
	}
	return ops
}

func buildFixedTxnTemplate(stmtsPerTxn int, readSet, insertSet, updateSet weightedSet, readProb, insertProb float64) []txnOp {
	if stmtsPerTxn <= 0 {
		return nil
	}

	readN, insertN, updateN := fixedKindCounts(stmtsPerTxn, readProb, insertProb)
	kinds := make([]opKind, 0, stmtsPerTxn)
	for i := 0; i < readN; i++ {
		kinds = append(kinds, opRead)
	}
	for i := 0; i < insertN; i++ {
		kinds = append(kinds, opInsert)
	}
	for i := 0; i < updateN; i++ {
		kinds = append(kinds, opUpdate)
	}

	r := rand.New(rand.NewSource(20260509))
	r.Shuffle(len(kinds), func(i, j int) { kinds[i], kinds[j] = kinds[j], kinds[i] })

	ops := make([]txnOp, 0, len(kinds))
	for _, k := range kinds {
		switch k {
		case opRead:
			ops = append(ops, txnOp{kind: k, name: readSet.pick(r)})
		case opInsert:
			ops = append(ops, txnOp{kind: k, name: insertSet.pick(r)})
		case opUpdate:
			ops = append(ops, txnOp{kind: k, name: updateSet.pick(r)})
		}
	}
	return ops
}

func fixedKindCounts(stmtsPerTxn int, readProb, insertProb float64) (readN, insertN, updateN int) {
	if stmtsPerTxn <= 0 {
		return 0, 0, 0
	}
	if readProb < 0 {
		readProb = 0
	}
	if insertProb < 0 {
		insertProb = 0
	}
	if readProb+insertProb > 1 {
		insertProb = 1 - readProb
	}
	updateProb := 1 - readProb - insertProb

	expRead := float64(stmtsPerTxn) * readProb
	expInsert := float64(stmtsPerTxn) * insertProb
	expUpdate := float64(stmtsPerTxn) * updateProb

	readN = int(math.Floor(expRead))
	insertN = int(math.Floor(expInsert))
	updateN = int(math.Floor(expUpdate))

	type fracEntry struct {
		kind opKind
		frac float64
	}
	remainders := []fracEntry{
		{kind: opRead, frac: expRead - float64(readN)},
		{kind: opInsert, frac: expInsert - float64(insertN)},
		{kind: opUpdate, frac: expUpdate - float64(updateN)},
	}
	assigned := readN + insertN + updateN
	for i := 0; i < stmtsPerTxn-assigned; i++ {
		best := 0
		for j := 1; j < len(remainders); j++ {
			if remainders[j].frac > remainders[best].frac {
				best = j
			}
		}
		switch remainders[best].kind {
		case opRead:
			readN++
		case opInsert:
			insertN++
		case opUpdate:
			updateN++
		}
		remainders[best].frac = -1
	}

	return readN, insertN, updateN
}

func formatTxnOps(ops []txnOp) string {
	parts := make([]string, 0, len(ops))
	for i, op := range ops {
		parts = append(parts, fmt.Sprintf("s%d:%s/%s", i+1, opKindName(op.kind), op.name))
	}
	return strings.Join(parts, ",")
}

func opKindName(k opKind) string {
	switch k {
	case opRead:
		return "read"
	case opInsert:
		return "insert"
	case opUpdate:
		return "update"
	default:
		return "unknown"
	}
}

func main() {
	dsn := flag.String("dsn", os.Getenv("DSN"), "postgres DSN (required; defaults to DSN environment variable)")
	concurrency := flag.Int("concurrency", 64, "worker concurrency")
	duration := flag.Duration("duration", 2*time.Minute, "benchmark duration")
	reportEvery := flag.Duration("report-every", 10*time.Second, "report interval")
	maxPool := flag.Int("maxpool", 200, "max open connections")
	minPool := flag.Int("minpool", 200, "max idle connections")
	directEven := flag.Bool("direct-even", false, "discover primary nodes from yb_servers() and split workers/pool evenly across direct node connections")
	directEvenNodes := flag.Int("direct-even-nodes", 3, "number of primary nodes to use for direct-even mode (<=0 means all primary nodes)")
	stmtsPerTxn := flag.Int("stmts-per-txn", 8, "statements per transaction")
	txnCTE := flag.Bool("txn-cte", false, "execute each txn as a single CTE statement")
	txnCTEFixedShape := flag.Bool("txn-cte-fixed-shape", false, "use a fixed CTE op shape so PGSS aggregates into stable query texts")
	txnRetries := flag.Int("txn-retries", 4, "retries for retryable transaction failures")
	targetTPS := flag.Float64("target-tps", 0, "committed transactions per second to pace all workers to (0 = unpaced)")
	errorLogLimit := flag.Int("error-log-limit", 20, "max distinct errors to sample to stderr")
	seedOrders := flag.Int("seed-orders", 1200, "seeded orders cardinality")
	seedShops := flag.Int("seed-shops", 40, "seeded shops cardinality")
	seedTokens := flag.Int("seed-tokens", 800, "seeded tokens cardinality")
	flag.Parse()

	if strings.TrimSpace(*dsn) == "" {
		fmt.Fprintln(os.Stderr, "Set DSN or pass -dsn before running update_heavy_anon")
		os.Exit(2)
	}

	dbPools, err := buildDBPools(*dsn, *concurrency, *maxPool, *minPool, *directEven, *directEvenNodes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db pools: %v\n", err)
		os.Exit(1)
	}
	defer closeDBPools(dbPools)

	cfg := cfg{
		seedOrders:       *seedOrders,
		seedShops:        *seedShops,
		seedTokens:       *seedTokens,
		stmtsPerTxn:      *stmtsPerTxn,
		targetTPS:        *targetTPS,
		txnCTE:           *txnCTE,
		txnCTEFixedShape: *txnCTE && *txnCTEFixedShape,
		workers:          *concurrency,
	}

	readSet := newWeightedSet([]weightedStmt{
		{name: "r_gateway_event_by_order_ref", weight: 920255514.0},
		{name: "r_merchants_by_channel", weight: 824323495.0},
		{name: "r_order_profile_by_event", weight: 650921809.3},
		{name: "r_method_by_ref", weight: 540765022.8},
		{name: "r_settlement_by_gateway_event", weight: 431671234.9},
		{name: "r_profile_by_event", weight: 372924452.9},
		{name: "r_gateway_order_by_event", weight: 362959337.3},
		{name: "r_auth_by_order", weight: 344141383.4},
		{name: "r_order_event_by_gateway_event", weight: 337457972.5},
	})
	insertSet := newWeightedSet([]weightedStmt{
		{name: "i_gateway_event", weight: 796902548.3},
		{name: "i_order", weight: 614237748.5},
		{name: "i_auth", weight: 518259550.8},
		{name: "i_order_event", weight: 502234523.9},
	})
	updateSet := newWeightedSet([]weightedStmt{
		{name: "u_order_touch", weight: 2158253742.0},
		{name: "u_gateway_order_touch", weight: 1237322620.0},
		{name: "u_gateway_event_main", weight: 873835607.7},
		{name: "u_order_status", weight: 647389731.2},
		{name: "u_method_update", weight: 623856637.2},
		{name: "u_order_event_notify", weight: 451072983.7},
		{name: "u_gateway_order_status", weight: 427390988.3},
	})

	readProb := 1.0 / (1.0 + writeToReadRatio)
	writeProb := 1.0 - readProb
	insertProbWithinWrite := insertToUpdateRat / (1.0 + insertToUpdateRat)
	insertProb := writeProb * insertProbWithinWrite
	updateProb := writeProb - insertProb

	if cfg.txnCTEFixedShape {
		cfg.fixedOps = buildFixedTxnTemplate(cfg.stmtsPerTxn, readSet, insertSet, updateSet, readProb, insertProb)
		fmt.Fprintf(os.Stderr, "fixed_cte_template: %s\n", formatTxnOps(cfg.fixedOps))
	}

	fmt.Fprintf(
		os.Stderr,
		"config: readProb=%.6f insertProb=%.6f updateProb=%.6f txnCTE=%t txnCTEFixedShape=%t targetTPS=%.2f\n",
		readProb, insertProb, updateProb, cfg.txnCTE, cfg.txnCTEFixedShape, cfg.targetTPS,
	)
	fmt.Printf("timestamp_utc,event,concurrency,stmts_per_txn,total_qps,txns_per_s,read_qps,insert_qps,update_qps,read_avg_ms,insert_avg_ms,update_avg_ms,txn_avg_ms,read_errs,insert_errs,update_errs,txn_errs,open,in_use,idle\n")

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	var c counters
	errLog := newErrLogger(*errorLogLimit)
	var wg sync.WaitGroup
	wg.Add(*concurrency)
	for i := 0; i < *concurrency; i++ {
		db := dbPools[i%len(dbPools)]
		go worker(ctx, db, i, cfg, readSet, insertSet, updateSet, readProb, insertProb, *txnRetries, errLog, &c, &wg)
	}

	start := time.Now()
	ticker := time.NewTicker(*reportEvery)
	defer ticker.Stop()

	var prev counters
	printSnapshot := func(event string) {
		cur := snapshot(&c)
		delta := diffCounters(cur, prev)
		intervalSecs := reportEvery.Seconds()
		if event == "final" {
			intervalSecs = time.Since(start).Seconds()
			if intervalSecs <= 0 {
				intervalSecs = reportEvery.Seconds()
			}
		}

		totalOps := delta.readOps + delta.insertOps + delta.updateOps
		totalQPS := float64(totalOps) / intervalSecs
		txnsPerS := float64(delta.txns) / intervalSecs
		readQPS := float64(delta.readOps) / intervalSecs
		insertQPS := float64(delta.insertOps) / intervalSecs
		updateQPS := float64(delta.updateOps) / intervalSecs

		s := aggregateDBStats(dbPools)
		now := time.Now().UTC().Format(time.RFC3339)
		fmt.Printf("%s,%s,%d,%d,%.3f,%.3f,%.3f,%.3f,%.3f,%.3f,%.3f,%.3f,%.3f,%d,%d,%d,%d,%d,%d,%d\n",
			now, event, *concurrency, *stmtsPerTxn,
			totalQPS, txnsPerS, readQPS, insertQPS, updateQPS,
			avgMs(delta.readLatNs, delta.readOps),
			avgMs(delta.insertLatNs, delta.insertOps),
			avgMs(delta.updateLatNs, delta.updateOps),
			avgMs(delta.txnLatNs, delta.txns),
			delta.readErrs, delta.insertErrs, delta.updateErrs, delta.txnErrs,
			s.OpenConnections, s.InUse, s.Idle,
		)
		prev = cur
	}

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			printSnapshot("interval")
		}
	}

	wg.Wait()
	printSnapshot("final")
	errLog.printSummary()
}

func buildDBPools(baseDSN string, workers, maxPool, minPool int, directEven bool, directEvenNodes int) ([]*sql.DB, error) {
	if !directEven {
		db, err := initDBPool(baseDSN, maxPool, minPool)
		if err != nil {
			return nil, err
		}
		return []*sql.DB{db}, nil
	}

	nodes, err := discoverPrimaryNodes(baseDSN, directEvenNodes)
	if err != nil {
		return nil, fmt.Errorf("discover primary nodes: %w", err)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no primary nodes returned by yb_servers()")
	}

	pools := make([]*sql.DB, 0, len(nodes))
	for i, n := range nodes {
		nodeDSN := withDSNHostPort(baseDSN, n.host, n.port)
		nodeMax := splitBudget(maxPool, len(nodes), i)
		nodeMin := splitBudget(minPool, len(nodes), i)
		nodeWorkers := splitBudget(workers, len(nodes), i)
		if nodeMax <= 0 {
			nodeMax = 1
		}
		if nodeMin > nodeMax {
			nodeMin = nodeMax
		}
		db, err := initDBPool(nodeDSN, nodeMax, nodeMin)
		if err != nil {
			closeDBPools(pools)
			return nil, fmt.Errorf("init direct node pool %s:%d: %w", n.host, n.port, err)
		}
		pools = append(pools, db)
		fmt.Fprintf(os.Stderr, "direct_pool[%d]: host=%s port=%d workers=%d maxpool=%d minpool=%d\n", i, n.host, n.port, nodeWorkers, nodeMax, nodeMin)
	}

	return pools, nil
}

func initDBPool(dsn string, maxPool, minPool int) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(maxPool)
	db.SetMaxIdleConns(minPool)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	ctxPing, cancelPing := context.WithTimeout(context.Background(), 30*time.Second)
	err = db.PingContext(ctxPing)
	cancelPing()
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}

	if err := prewarmPool(db, minPool); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("prewarm pool: %w", err)
	}

	return db, nil
}

func discoverPrimaryNodes(dsn string, limit int) ([]nodeEndpoint, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, `SELECT host, port FROM yb_servers() WHERE node_type='primary' ORDER BY host, port`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := make(map[string]struct{})
	nodes := make([]nodeEndpoint, 0, 6)
	for rows.Next() {
		var host string
		var port int
		if err := rows.Scan(&host, &port); err != nil {
			return nil, err
		}
		if host == "" || port <= 0 {
			continue
		}
		k := fmt.Sprintf("%s:%d", host, port)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		nodes = append(nodes, nodeEndpoint{host: host, port: port})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if limit > 0 && len(nodes) > limit {
		nodes = nodes[:limit]
	}
	return nodes, nil
}

func withDSNHostPort(dsn, host string, port int) string {
	parts := strings.Fields(dsn)
	out := make([]string, 0, len(parts)+2)
	hasHost := false
	hasPort := false

	for _, p := range parts {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			out = append(out, p)
			continue
		}
		switch kv[0] {
		case "host":
			out = append(out, "host="+host)
			hasHost = true
		case "port":
			out = append(out, "port="+strconv.Itoa(port))
			hasPort = true
		default:
			out = append(out, p)
		}
	}

	if !hasHost {
		out = append(out, "host="+host)
	}
	if !hasPort {
		out = append(out, "port="+strconv.Itoa(port))
	}
	return strings.Join(out, " ")
}

func splitBudget(total, slots, idx int) int {
	if total <= 0 || slots <= 0 || idx < 0 || idx >= slots {
		return 0
	}
	base := total / slots
	rem := total % slots
	if idx < rem {
		return base + 1
	}
	return base
}

func closeDBPools(pools []*sql.DB) {
	for _, db := range pools {
		if db != nil {
			_ = db.Close()
		}
	}
}

func aggregateDBStats(pools []*sql.DB) sql.DBStats {
	var out sql.DBStats
	for _, db := range pools {
		if db == nil {
			continue
		}
		s := db.Stats()
		out.MaxOpenConnections += s.MaxOpenConnections
		out.OpenConnections += s.OpenConnections
		out.InUse += s.InUse
		out.Idle += s.Idle
		out.WaitCount += s.WaitCount
		out.WaitDuration += s.WaitDuration
		out.MaxIdleClosed += s.MaxIdleClosed
		out.MaxIdleTimeClosed += s.MaxIdleTimeClosed
		out.MaxLifetimeClosed += s.MaxLifetimeClosed
	}
	return out
}

func worker(ctx context.Context, db *sql.DB, workerID int, cfg cfg, readSet, insertSet, updateSet weightedSet, readProb, insertProb float64, txnRetries int, errLog *errLogger, c *counters, wg *sync.WaitGroup) {
	defer wg.Done()
	r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID*1013)))

	var paceEvery time.Duration
	if cfg.targetTPS > 0 && cfg.workers > 0 {
		paceEvery = time.Duration(float64(time.Second) * float64(cfg.workers) / cfg.targetTPS)
	}
	nextTxnAt := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if paceEvery > 0 {
			if wait := time.Until(nextTxnAt); wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			} else if wait < -paceEvery {
				// Fell behind (slow endpoint, retries): do not burst to catch up.
				nextTxnAt = time.Now()
			}
			nextTxnAt = nextTxnAt.Add(paceEvery)
		}

		userTxnStart := time.Now()
		committed := false
		for attempt := 0; ; attempt++ {
			ops := planTxnOps(r, cfg, readSet, insertSet, updateSet, readProb, insertProb)
			readCnt, insertCnt, updateCnt := countTxnOps(ops)
			tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
			if err != nil {
				if isShutdownErr(ctx, err) {
					return
				}
				atomic.AddUint64(&c.txnErrs, 1)
				errLog.log("begin", err)
				if isConnectionErr(err) {
					time.Sleep(connRetryBackoff(r))
					continue
				}
				break
			}

			failed := false
			retryable := false
			if cfg.txnCTE {
				opStart := time.Now()
				err = execTxnCTE(ctx, tx, ops, r, cfg, workerID)
				totalNs := uint64(time.Since(opStart).Nanoseconds())
				addOpLatencyShares(c, totalNs, readCnt, insertCnt, updateCnt)
				if err != nil {
					if isShutdownErr(ctx, err) {
						_ = tx.Rollback()
						return
					}
					retryable = isRetryableTxErr(err)
					errLog.log("txn_cte", err)
					atomic.AddUint64(&c.readErrs, uint64(readCnt))
					atomic.AddUint64(&c.insertErrs, uint64(insertCnt))
					atomic.AddUint64(&c.updateErrs, uint64(updateCnt))
					failed = true
				} else {
					atomic.AddUint64(&c.readOps, uint64(readCnt))
					atomic.AddUint64(&c.insertOps, uint64(insertCnt))
					atomic.AddUint64(&c.updateOps, uint64(updateCnt))
				}
			} else {
				for _, op := range ops {
					opStart := time.Now()
					switch op.kind {
					case opRead:
						err = execRead(ctx, tx, op.name, r, cfg)
						atomic.AddUint64(&c.readLatNs, uint64(time.Since(opStart).Nanoseconds()))
						if err != nil {
							if isShutdownErr(ctx, err) {
								_ = tx.Rollback()
								return
							}
							atomic.AddUint64(&c.readErrs, 1)
							retryable = isRetryableTxErr(err)
							errLog.log("read", err)
							failed = true
						} else {
							atomic.AddUint64(&c.readOps, 1)
						}
					case opInsert:
						err = execInsert(ctx, tx, op.name, r, cfg)
						atomic.AddUint64(&c.insertLatNs, uint64(time.Since(opStart).Nanoseconds()))
						if err != nil {
							if isShutdownErr(ctx, err) {
								_ = tx.Rollback()
								return
							}
							atomic.AddUint64(&c.insertErrs, 1)
							retryable = isRetryableTxErr(err)
							errLog.log("insert", err)
							failed = true
						} else {
							atomic.AddUint64(&c.insertOps, 1)
						}
					case opUpdate:
						err = execUpdate(ctx, tx, op.name, r, cfg, workerID)
						atomic.AddUint64(&c.updateLatNs, uint64(time.Since(opStart).Nanoseconds()))
						if err != nil {
							if isShutdownErr(ctx, err) {
								_ = tx.Rollback()
								return
							}
							atomic.AddUint64(&c.updateErrs, 1)
							retryable = isRetryableTxErr(err)
							errLog.log("update", err)
							failed = true
						} else {
							atomic.AddUint64(&c.updateOps, 1)
						}
					}
					if failed {
						break
					}
				}
			}

			if failed {
				_ = tx.Rollback()
				if isConnectionErr(err) {
					time.Sleep(connRetryBackoff(r))
					continue
				}
				if retryable && attempt < txnRetries {
					time.Sleep(txnRetryBackoff(r))
					continue
				}
				atomic.AddUint64(&c.txnErrs, 1)
				break
			}

			if err := tx.Commit(); err != nil {
				if isShutdownErr(ctx, err) {
					return
				}
				retryable = isRetryableTxErr(err)
				errLog.log("commit", err)
				if isConnectionErr(err) {
					atomic.AddUint64(&c.txnErrs, 1)
					time.Sleep(connRetryBackoff(r))
					continue
				}
				if retryable && attempt < txnRetries {
					time.Sleep(txnRetryBackoff(r))
					continue
				}
				atomic.AddUint64(&c.txnErrs, 1)
				break
			}
			committed = true
			atomic.AddUint64(&c.txns, 1)
			atomic.AddUint64(&c.txnLatNs, uint64(time.Since(userTxnStart).Nanoseconds()))
			break
		}
		if !committed {
			continue
		}
	}
}

func countTxnOps(ops []txnOp) (reads, inserts, updates int) {
	for _, op := range ops {
		switch op.kind {
		case opRead:
			reads++
		case opInsert:
			inserts++
		case opUpdate:
			updates++
		}
	}
	return reads, inserts, updates
}

func addOpLatencyShares(c *counters, totalNs uint64, reads, inserts, updates int) {
	totalOps := reads + inserts + updates
	if totalOps <= 0 {
		return
	}
	share := totalNs / uint64(totalOps)
	if reads > 0 {
		atomic.AddUint64(&c.readLatNs, share*uint64(reads))
	}
	if inserts > 0 {
		atomic.AddUint64(&c.insertLatNs, share*uint64(inserts))
	}
	if updates > 0 {
		atomic.AddUint64(&c.updateLatNs, share*uint64(updates))
	}
}

type sqlSpec struct {
	stmt string
	args []any
}

var placeholderRe = regexp.MustCompile(`\$(\d+)`)

func rebindPlaceholders(stmt string, offset int) string {
	if offset == 0 {
		return stmt
	}
	return placeholderRe.ReplaceAllStringFunc(stmt, func(s string) string {
		n, err := strconv.Atoi(strings.TrimPrefix(s, "$"))
		if err != nil {
			return s
		}
		return fmt.Sprintf("$%d", n+offset)
	})
}

func execTxnCTE(ctx context.Context, tx *sql.Tx, ops []txnOp, r *rand.Rand, cfg cfg, workerID int) error {
	if len(ops) == 0 {
		return nil
	}

	ctes := make([]string, 0, len(ops))
	sumRefs := make([]string, 0, len(ops))
	args := make([]any, 0, len(ops)*8)

	for i, op := range ops {
		spec, err := buildOpSpec(op, r, cfg, workerID)
		if err != nil {
			return err
		}
		boundStmt := rebindPlaceholders(spec.stmt, len(args))
		name := fmt.Sprintf("s%d", i+1)
		ctes = append(ctes, fmt.Sprintf("%s AS (%s)", name, boundStmt))
		sumRefs = append(sumRefs, fmt.Sprintf("(SELECT count(*) FROM %s)", name))
		args = append(args, spec.args...)
	}

	stmt := fmt.Sprintf("WITH %s SELECT %s",
		strings.Join(ctes, ","),
		strings.Join(sumRefs, " + "),
	)
	var sink int64
	return tx.QueryRowContext(ctx, stmt, args...).Scan(&sink)
}

func buildOpSpec(op txnOp, r *rand.Rand, cfg cfg, workerID int) (sqlSpec, error) {
	switch op.kind {
	case opRead:
		spec, err := buildReadSpec(op.name, r, cfg)
		if err != nil {
			return sqlSpec{}, err
		}
		return sqlSpec{
			stmt: fmt.Sprintf("SELECT 1 FROM (%s) q LIMIT 1", spec.stmt),
			args: spec.args,
		}, nil
	case opInsert:
		spec, err := buildInsertSpec(op.name, r, cfg)
		if err != nil {
			return sqlSpec{}, err
		}
		return sqlSpec{
			stmt: spec.stmt + " RETURNING 1",
			args: spec.args,
		}, nil
	case opUpdate:
		spec, err := buildUpdateSpec(op.name, r, cfg, workerID)
		if err != nil {
			return sqlSpec{}, err
		}
		return sqlSpec{
			stmt: spec.stmt + " RETURNING 1",
			args: spec.args,
		}, nil
	default:
		return sqlSpec{}, fmt.Errorf("unknown op kind %d", op.kind)
	}
}

func execRead(ctx context.Context, tx *sql.Tx, name string, r *rand.Rand, cfg cfg) error {
	spec, err := buildReadSpec(name, r, cfg)
	if err != nil {
		return err
	}
	return execSelectAnyRow(ctx, tx, spec.stmt, spec.args...)
}

func buildReadSpec(name string, r *rand.Rand, cfg cfg) (sqlSpec, error) {
	orderID := randOrderID(r, cfg.seedOrders)
	tokenID := randTokenID(r, cfg.seedTokens)
	channelID := (orderID % cfg.seedShops) + 1

	switch name {
	case "r_gateway_event_by_order_ref":
		return sqlSpec{stmt: `select * from gateway_events ge where (ge.gateway_order_id, ge.gateway_order_scope)=($1,$2)`,
			args: []any{orderID, scopeDefault}}, nil
	case "r_merchants_by_channel":
		return sqlSpec{stmt: `select * from merchants where channel_id = $1 and deleted_at is null limit $2`,
			args: []any{channelID, 1}}, nil
	case "r_order_profile_by_event":
		return sqlSpec{stmt: `select *
from orders o
left join order_events oe on (oe.order_id, oe.order_scope) = (o.id, o.scope)
left join devices d on o.id = d.order_ref_id and o.scope = d.order_ref_scope
left join order_meta om on o.id = om.order_ref_id and o.scope = om.order_ref_scope
where (oe.id, oe.scope) = ($1,$2)`,
			args: []any{orderID, scopeDefault}}, nil
	case "r_method_by_ref":
		return sqlSpec{stmt: `select * from payment_methods pm where pm.method_ref = $1 limit $2`,
			args: []any{fmt.Sprintf("tok-%d", tokenID), 1}}, nil
	case "r_settlement_by_gateway_event":
		evenOrderID := ((orderID / 2) * 2)
		if evenOrderID < 2 {
			evenOrderID = 2
		}
		return sqlSpec{stmt: `select *
from settlement_links sl
left join gateway_events ge on ge.id = sl.gateway_event_id and ge.scope = sl.gateway_event_scope
left join orders o on o.id = sl.order_ref_id and o.scope = sl.order_ref_scope
left join devices d on o.id = d.order_ref_id and o.scope = d.order_ref_scope
left join order_data od on o.id = od.order_ref_id and o.scope = od.order_ref_scope
left join lateral (
  select *
  from risk_events re
  where re.order_ref_id = o.id and re.order_ref_scope = o.scope
  order by re.phase
) re on true
where (sl.gateway_event_id, sl.gateway_event_scope) in (($1,$2))`,
			args: []any{evenOrderID, scopeDefault}}, nil
	case "r_profile_by_event":
		return sqlSpec{stmt: `select *
from payment_profiles pp
left join order_events oe1 on (oe1.pay_profile_id, oe1.pay_profile_scope) = (pp.id, pp.scope)
left join lateral (
  select *
  from order_events oe
  where oe.pay_profile_id = pp.id and oe.pay_profile_scope = pp.scope
  order by oe.created_at desc
  limit $3
) oe2 on true
where (oe1.id, oe1.scope) = ($1,$2)`,
			args: []any{orderID, scopeDefault, profileEventsLimit}}, nil
	case "r_gateway_order_by_event":
		return sqlSpec{stmt: `select *
from gateway_orders go
left join gateway_events ge on (ge.gateway_order_id, ge.gateway_order_scope) = (go.id, go.scope)
where (ge.id, ge.scope) = ($1,$2)`,
			args: []any{orderID, scopeDefault}}, nil
	case "r_auth_by_order":
		ord := ((orderID / 3) * 3)
		if ord < 3 {
			ord = 3
		}
		return sqlSpec{stmt: `select * from auth_events
where auth_events.order_id = $1
  and (auth_events.gateway_order_id = $2 or auth_events.gateway_order_id is null)`,
			args: []any{ord, ord}}, nil
	case "r_order_event_by_gateway_event":
		return sqlSpec{stmt: `select *
from order_events oe
left join gateway_events ge on (ge.event_ref_id, ge.event_ref_scope) = (oe.id, oe.scope)
left join payment_methods pm on (pm.event_ref_id, pm.event_ref_scope) = (oe.id, oe.scope)
where (ge.id, ge.scope) = ($1,$2)`,
			args: []any{orderID, scopeDefault}}, nil
	default:
		return sqlSpec{}, fmt.Errorf("unknown read query %q", name)
	}
}

func execInsert(ctx context.Context, tx *sql.Tx, name string, r *rand.Rand, cfg cfg) error {
	spec, err := buildInsertSpec(name, r, cfg)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, spec.stmt, spec.args...)
	return err
}

func buildInsertSpec(name string, r *rand.Rand, cfg cfg) (sqlSpec, error) {
	now := time.Now().UTC()
	status := pickStatus(r)
	ordRef := randOrderID(r, cfg.seedOrders)
	shopID := ((ordRef - 1) % cfg.seedShops) + 1

	switch name {
	case "i_gateway_event":
		id := nextSyntheticInsertID()
		ordRef = int(id)
		return sqlSpec{
			stmt: `insert into gateway_events
			(action,amount,created_at,currency_id,decline_type,deleted_at,fee_amount,fee_currency_id,forced_3d,is_secured,payment_type,gateway_order_id,gateway_order_scope,gateway_event_id,public_gateway_event_id,revenue_amount,revenue_currency_id,event_ref_id,event_ref_scope,status,event_link_id,type,updated_at,id,scope)
			values
			($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)`,
			args: []any{
				pickType(r),
				100 + r.Intn(500000),
				now,
				840,
				pickDeclineType(r),
				nil,
				float64(r.Intn(500)) / 100.0,
				840,
				r.Intn(2) == 0,
				r.Intn(2) == 0,
				pickPaymentType(r),
				ordRef,
				scopeDefault,
				fmt.Sprintf("ge-ins-%d", id),
				fmt.Sprintf("pge-ins-%d", id),
				float64(r.Intn(900)) / 100.0,
				840,
				ordRef,
				scopeDefault,
				status,
				fmt.Sprintf("link-ins-%d", id),
				pickType(r),
				now,
				id,
				scopeDefault,
			},
		}, nil
	case "i_order":
		id := nextSyntheticInsertID()
		return sqlSpec{
			stmt: `insert into orders
			(amount,created_at,deleted_at,gateway_order_id,gateway_order_scope,order_id,shop_id,status,updated_at,id,scope)
			values
			($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			args: []any{
				100 + r.Intn(500000),
				now,
				nil,
				id,
				scopeDefault,
				fmt.Sprintf("ord-ins-%d", id),
				shopID,
				status,
				now,
				id,
				scopeDefault,
			},
		}, nil
	case "i_auth":
		id := nextSyntheticInsertID()
		ordRef = int(id)
		return sqlSpec{
			stmt: `insert into auth_events
			(code,created_at,deleted_at,fingerprint_collection_started_at,is_active,order_id,gateway_order_id,return_url,returned,type_of_secure,updated_at,verify_url,visited,id)
			values
			($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			args: []any{
				fmt.Sprintf("auth-ins-%d", id),
				now,
				nil,
				nil,
				true,
				ordRef,
				ordRef,
				fmt.Sprintf("https://example.invalid/return/ins/%d", id),
				false,
				"3ds2",
				now,
				fmt.Sprintf("https://example.invalid/verify/ins/%d", id),
				false,
				id,
			},
		}, nil
	case "i_order_event":
		id := nextSyntheticInsertID()
		ordRef = int(id)
		return sqlSpec{
			stmt: `insert into order_events
			(action,amount,code,created_at,order_id,order_scope,pay_profile_id,pay_profile_scope,shop_notified_at,status,type,updated_at,id,scope)
			values
			($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			args: []any{
				pickType(r),
				100 + r.Intn(500000),
				fmt.Sprintf("ev-ins-%d", id),
				now,
				ordRef,
				scopeDefault,
				ordRef,
				scopeDefault,
				nil,
				status,
				pickType(r),
				now,
				id,
				scopeDefault,
			},
		}, nil
	default:
		return sqlSpec{}, fmt.Errorf("unknown insert query %q", name)
	}
}

func execSelectAnyRow(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return err
	}

	if rows.Next() {
		values := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return err
		}
	}
	return rows.Err()
}

func execUpdate(ctx context.Context, tx *sql.Tx, name string, r *rand.Rand, cfg cfg, workerID int) error {
	spec, err := buildUpdateSpec(name, r, cfg, workerID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, spec.stmt, spec.args...)
	return err
}

func buildUpdateSpec(name string, r *rand.Rand, cfg cfg, workerID int) (sqlSpec, error) {
	now := time.Now().UTC()
	status := pickStatus(r)
	orderID := randIDForWorker(r, cfg.seedOrders, workerID, cfg.workers)
	tokenID := randIDForWorker(r, cfg.seedTokens, workerID, cfg.workers)

	switch name {
	case "u_order_touch":
		return sqlSpec{
			stmt: `update orders set updated_at=$1 where id=$2 and scope=$3`,
			args: []any{now, orderID, scopeDefault},
		}, nil
	case "u_gateway_order_touch":
		return sqlSpec{
			stmt: `update gateway_orders set updated_at=$1 where id=$2 and scope=$3`,
			args: []any{now, orderID, scopeDefault},
		}, nil
	case "u_gateway_event_main":
		eventID := fmt.Sprintf("ge-upd-%d-%d", orderID, r.Intn(1_000_000))
		publicEventID := fmt.Sprintf("pge-upd-%d-%d", orderID, r.Intn(1_000_000))
		return sqlSpec{
			stmt: `update gateway_events set action=$1,amount=$2,currency_id=$3,decline_type=$4,deleted_at=$5,fee_amount=$6,fee_currency_id=$7,forced_3d=$8,is_secured=$9,payment_type=$10,gateway_order_id=$11,gateway_order_scope=$12,gateway_event_id=$13,public_gateway_event_id=$14,revenue_amount=$15,revenue_currency_id=$16,event_ref_id=$17,event_ref_scope=$18,status=$19,event_link_id=$20,type=$21,updated_at=$22 where id=$23 and scope=$24`,
			args: []any{
				pickType(r),
				100 + r.Intn(500000),
				840,
				pickDeclineType(r),
				nil,
				float64(r.Intn(1000)) / 100.0,
				840,
				r.Intn(2) == 0,
				r.Intn(2) == 0,
				pickPaymentType(r),
				orderID,
				scopeDefault,
				eventID,
				publicEventID,
				float64(r.Intn(2000)) / 100.0,
				840,
				orderID,
				scopeDefault,
				status,
				fmt.Sprintf("link-upd-%d", orderID),
				pickType(r),
				now,
				orderID,
				scopeDefault,
			},
		}, nil
	case "u_order_status":
		return sqlSpec{
			stmt: `update orders set status=$1,updated_at=$2 where id=$3 and scope=$4`,
			args: []any{status, now, orderID, scopeDefault},
		}, nil
	case "u_method_update":
		methodRef := fmt.Sprintf("tok-%d", tokenID)
		return sqlSpec{
			stmt: `update payment_methods set bank=$1,bin=$2,card_brand=$3,card_country=$4,card_holder=$5,expiration_date=$6,is_enabled=$7,masked_card_number=$8,metadata=$9,gateway_method=$10,gateway_event_id=$11,gateway_event_scope=$12,recurring_count=$13,shop_id=$14,method_ref=$15,event_ref_id=$16,event_ref_scope=$17,updated_at=$18 where id=$19 and scope=$20`,
			args: []any{
				fmt.Sprintf("Bank-%d", r.Intn(50)),
				400000 + r.Intn(999),
				pickCardBrand(r),
				pickCardCountry(r),
				fmt.Sprintf("Holder %d", tokenID),
				time.Now().AddDate(2+r.Intn(3), 0, 0),
				r.Intn(10) != 0,
				fmt.Sprintf("411111XXXXXX%04d", tokenID%10000),
				fmt.Sprintf(`{"u":%d}`, tokenID),
				fmt.Sprintf("gwm-%d", tokenID),
				tokenID,
				scopeDefault,
				r.Intn(30),
				((tokenID - 1) % cfg.seedShops) + 1,
				methodRef,
				tokenID,
				scopeDefault,
				now,
				tokenID,
				scopeDefault,
			},
		}, nil
	case "u_order_event_notify":
		return sqlSpec{
			stmt: `UPDATE order_events SET shop_notified_at = now() where id = $1 AND scope = cast($2 as scope_t)`,
			args: []any{orderID, scopeDefault},
		}, nil
	case "u_gateway_order_status":
		return sqlSpec{
			stmt: `update gateway_orders set status=$1,updated_at=$2 where id=$3 and scope=$4`,
			args: []any{status, now, orderID, scopeDefault},
		}, nil
	default:
		return sqlSpec{}, fmt.Errorf("unknown update query %q", name)
	}
}

func randOrderID(r *rand.Rand, max int) int {
	return r.Intn(max) + 1
}

func randTokenID(r *rand.Rand, max int) int {
	return r.Intn(max) + 1
}

func randIDForWorker(r *rand.Rand, max, workerID, workers int) int {
	if max <= 1 || workers <= 1 {
		if max <= 1 {
			return 1
		}
		return r.Intn(max) + 1
	}
	if workerID < 0 || workerID >= workers {
		return r.Intn(max) + 1
	}

	base := max / workers
	remainder := max % workers
	if base == 0 {
		return r.Intn(max) + 1
	}

	start := workerID*base + 1
	if workerID < remainder {
		start += workerID
	} else {
		start += remainder
	}

	size := base
	if workerID < remainder {
		size++
	}
	if size <= 0 {
		return r.Intn(max) + 1
	}
	return start + r.Intn(size)
}

func pickStatus(r *rand.Rand) string {
	switch r.Intn(4) {
	case 0:
		return "created"
	case 1:
		return "processing"
	case 2:
		return "success"
	default:
		return "failed"
	}
}

func pickType(r *rand.Rand) string {
	if r.Intn(10) == 0 {
		return "refund"
	}
	return "charge"
}

func pickDeclineType(r *rand.Rand) string {
	switch r.Intn(3) {
	case 0:
		return "hard"
	case 1:
		return "soft"
	default:
		return "none"
	}
}

func pickPaymentType(r *rand.Rand) string {
	switch r.Intn(3) {
	case 0:
		return "card"
	case 1:
		return "wallet"
	default:
		return "bank_transfer"
	}
}

func pickCardBrand(r *rand.Rand) string {
	if r.Intn(2) == 0 {
		return "VISA"
	}
	return "MASTERCARD"
}

func pickCardCountry(r *rand.Rand) string {
	if r.Intn(2) == 0 {
		return "USA"
	}
	return "GBR"
}

func prewarmPool(db *sql.DB, target int) error {
	if target <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conns := make([]*sql.Conn, 0, target)
	for i := 0; i < target; i++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			for _, c := range conns {
				_ = c.Close()
			}
			return err
		}
		if err := conn.PingContext(ctx); err != nil {
			_ = conn.Close()
			for _, c := range conns {
				_ = c.Close()
			}
			return err
		}
		conns = append(conns, conn)
	}
	for _, c := range conns {
		_ = c.Close()
	}
	return nil
}

func snapshot(c *counters) counters {
	return counters{
		txns:        atomic.LoadUint64(&c.txns),
		txnErrs:     atomic.LoadUint64(&c.txnErrs),
		txnLatNs:    atomic.LoadUint64(&c.txnLatNs),
		readOps:     atomic.LoadUint64(&c.readOps),
		readErrs:    atomic.LoadUint64(&c.readErrs),
		readLatNs:   atomic.LoadUint64(&c.readLatNs),
		insertOps:   atomic.LoadUint64(&c.insertOps),
		insertErrs:  atomic.LoadUint64(&c.insertErrs),
		insertLatNs: atomic.LoadUint64(&c.insertLatNs),
		updateOps:   atomic.LoadUint64(&c.updateOps),
		updateErrs:  atomic.LoadUint64(&c.updateErrs),
		updateLatNs: atomic.LoadUint64(&c.updateLatNs),
	}
}

func diffCounters(cur, prev counters) counters {
	return counters{
		txns:        cur.txns - prev.txns,
		txnErrs:     cur.txnErrs - prev.txnErrs,
		txnLatNs:    cur.txnLatNs - prev.txnLatNs,
		readOps:     cur.readOps - prev.readOps,
		readErrs:    cur.readErrs - prev.readErrs,
		readLatNs:   cur.readLatNs - prev.readLatNs,
		insertOps:   cur.insertOps - prev.insertOps,
		insertErrs:  cur.insertErrs - prev.insertErrs,
		insertLatNs: cur.insertLatNs - prev.insertLatNs,
		updateOps:   cur.updateOps - prev.updateOps,
		updateErrs:  cur.updateErrs - prev.updateErrs,
		updateLatNs: cur.updateLatNs - prev.updateLatNs,
	}
}

func avgMs(latNs uint64, ops uint64) float64 {
	if ops == 0 {
		return 0
	}
	return (float64(latNs) / float64(ops)) / 1e6
}

func txnRetryBackoff(r *rand.Rand) time.Duration {
	return time.Duration(r.Intn(3)+1) * time.Millisecond
}

func connRetryBackoff(r *rand.Rand) time.Duration {
	return time.Duration(r.Intn(200)+100) * time.Millisecond
}

func isConnectionErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sql.ErrConnDone) {
		return true
	}

	var pqe *pq.Error
	if errors.As(err, &pqe) {
		if strings.HasPrefix(string(pqe.Code), "08") {
			return true
		}
	}

	s := strings.ToLower(err.Error())
	return strings.Contains(s, "driver: bad connection") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "server closed the connection") ||
		strings.Contains(s, "terminating connection") ||
		strings.Contains(s, "no such host") ||
		strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "eof")
}

func isRetryableTxErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var pqe *pq.Error
	if errors.As(err, &pqe) {
		code := string(pqe.Code)
		if strings.HasPrefix(code, "40") || code == "55P03" || strings.HasPrefix(code, "08") {
			return true
		}
	}

	s := strings.ToLower(err.Error())
	return strings.Contains(s, "restart read required") ||
		strings.Contains(s, "serialization failure") ||
		strings.Contains(s, "deadlock detected") ||
		strings.Contains(s, "conflict") ||
		strings.Contains(s, "try again")
}

func isShutdownErr(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

type errLogger struct {
	mu    sync.Mutex
	seen  map[string]uint64
	limit int
}

func newErrLogger(limit int) *errLogger {
	if limit < 0 {
		limit = 0
	}
	return &errLogger{
		seen:  make(map[string]uint64),
		limit: limit,
	}
}

func (l *errLogger) log(phase string, err error) {
	if l == nil || err == nil {
		return
	}

	key := phase + ": " + err.Error()
	l.mu.Lock()
	l.seen[key]++
	count := l.seen[key]
	printNow := l.limit > 0 && count == 1 && len(l.seen) <= l.limit
	l.mu.Unlock()

	if printNow {
		fmt.Fprintf(os.Stderr, "sample_error[%s] %v\n", phase, err)
	}
}

func (l *errLogger) printSummary() {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "error_summary_begin total_distinct=%d\n", len(l.seen))
	for k, v := range l.seen {
		fmt.Fprintf(os.Stderr, "error_summary count=%d err=%s\n", v, k)
	}
	fmt.Fprintf(os.Stderr, "error_summary_end\n")
}
