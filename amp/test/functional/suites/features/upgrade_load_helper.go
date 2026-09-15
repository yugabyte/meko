package features

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/suites/stress"
)

const (
	loadValueFactor     = 11
	loadRetryWait       = 2 * time.Second
	loadExecTimeout     = 30 * time.Second
	loadDialTimeout     = 8 * time.Minute
	loadMaxTransfer     = 10
	loadReportedIDs     = 10
	DefaultLoadWriters  = 2
	DefaultLoadAccounts = 20
	DefaultLoadInitial  = 1000
)

type upgradeLoadConfig struct {
	Writers  int
	Accounts int
	Initial  int64
}

func upgradeLoadConfigFrom(c *core.Config) upgradeLoadConfig {
	return upgradeLoadConfig{
		Writers:  c.GetInt("functional.upgrade.load.writers", DefaultLoadWriters),
		Accounts: c.GetInt("functional.upgrade.load.accounts", DefaultLoadAccounts),
		Initial:  int64(c.GetInt("functional.upgrade.load.initial", DefaultLoadInitial)),
	}
}

type upgradeLoad struct {
	prefix  string
	cfg     upgradeLoadConfig
	connect func(context.Context) (sqlConn, error)
	logf    func(string, ...any)
	seed    int64
	journal *stress.AckJournal

	mu   sync.RWMutex
	conn sqlConn
	gen  uint64

	nextID    atomic.Int64
	lastAck   atomic.Int64
	maxGap    atomic.Int64
	transfers atomic.Int64
	redials   atomic.Int64

	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopped bool
}

func newUpgradeLoad(prefix string, cfg upgradeLoadConfig, seed int64, connect func(context.Context) (sqlConn, error), logf func(string, ...any)) *upgradeLoad {
	return &upgradeLoad{prefix: prefix, cfg: cfg, seed: seed, connect: connect, logf: logf, journal: stress.NewAckJournal()}
}

func (l *upgradeLoad) journalTable() string { return l.prefix + "_journal" }
func (l *upgradeLoad) bankTable() string    { return l.prefix + "_bank" }

func (l *upgradeLoad) Setup(ctx context.Context) error {
	if l.cfg.Writers < 1 || l.cfg.Accounts < 2 || l.cfg.Initial < 1 {
		return fmt.Errorf("functional.upgrade.load is writers=%d accounts=%d initial=%d: a load that writes nothing would leave every post-upgrade check vacuous", l.cfg.Writers, l.cfg.Accounts, l.cfg.Initial)
	}
	conn, err := l.connect(ctx)
	if err != nil {
		return fmt.Errorf("connect the endpoint the write load runs on: %w", err)
	}
	l.mu.Lock()
	l.conn = conn
	l.mu.Unlock()
	if _, err := conn.Exec(ctx, "CREATE TABLE "+l.journalTable()+" (id bigint primary key, v bigint not null)"); err != nil {
		return fmt.Errorf("create %s: %w", l.journalTable(), err)
	}
	if _, err := conn.Exec(ctx, "CREATE TABLE "+l.bankTable()+" (id int primary key, bal bigint not null)"); err != nil {
		return fmt.Errorf("create %s: %w", l.bankTable(), err)
	}
	if _, err := conn.Exec(ctx, "INSERT INTO "+l.bankTable()+" SELECT g, $1::bigint FROM generate_series(1, $2) g", l.cfg.Initial, l.cfg.Accounts); err != nil {
		return fmt.Errorf("seed %s: %w", l.bankTable(), err)
	}
	return nil
}

func (l *upgradeLoad) Start(ctx context.Context) {
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l.cancel = cancel
	l.lastAck.Store(time.Now().UnixNano())
	for i := 0; i < l.cfg.Writers; i++ {
		l.wg.Add(1)
		go l.writeJournal(runCtx)
	}
	l.wg.Add(1)
	go l.moveMoney(runCtx)
}

func (l *upgradeLoad) Stop() {
	if l.stopped || l.cancel == nil {
		return
	}
	l.stopped = true
	l.cancel()
	l.wg.Wait()
	l.mu.Lock()
	if l.conn != nil {
		l.conn.Close()
		l.conn = nil
	}
	l.mu.Unlock()
}

func (l *upgradeLoad) ResetGap() {
	l.lastAck.Store(time.Now().UnixNano())
	l.maxGap.Store(0)
}

func (l *upgradeLoad) MaxGap() time.Duration {
	gap := l.maxGap.Load()
	if open := time.Now().UnixNano() - l.lastAck.Load(); open > gap {
		gap = open
	}
	return time.Duration(gap)
}

func (l *upgradeLoad) Counts() stress.AckCounts { return l.journal.Counts() }

func (l *upgradeLoad) Transfers() int64 { return l.transfers.Load() }

func (l *upgradeLoad) LastWriterError() string {
	if e := l.journal.LastErr(); e != "" {
		return e
	}
	return "none recorded"
}

func (l *upgradeLoad) Redials() int64 { return l.redials.Load() }

func (l *upgradeLoad) writeJournal(ctx context.Context) {
	defer l.wg.Done()
	for ctx.Err() == nil {
		id := l.nextID.Add(1)
		v := id * loadValueFactor
		err := l.exec(ctx, "INSERT INTO "+l.journalTable()+" (id, v) VALUES ($1, $2)", id, v)
		switch {
		case err == nil:
			l.journal.RecordAcked(id, v)
			l.ack()
		case ctx.Err() != nil:
			l.journal.RecordCancelled(id, v)
		default:
			l.journal.RecordUnknown(id, v)
			l.journal.RecordErr(err)
			sleepFor(ctx, loadRetryWait)
		}
	}
}

// One statement, so the debit and the credit commit together whatever the upgrade does.
func (l *upgradeLoad) moveMoney(ctx context.Context) {
	defer l.wg.Done()
	rnd := rand.New(rand.NewPCG(uint64(l.seed), 1))
	sql := fmt.Sprintf(
		"WITH d AS (UPDATE %s SET bal = bal - $3 WHERE id = $1 AND bal >= $3 RETURNING id) "+
			"UPDATE %s SET bal = bal + $3 WHERE id = $2 AND EXISTS (SELECT 1 FROM d)",
		l.bankTable(), l.bankTable())
	for ctx.Err() == nil {
		from := 1 + rnd.IntN(l.cfg.Accounts)
		to := 1 + rnd.IntN(l.cfg.Accounts)
		if to == from {
			to = 1 + to%l.cfg.Accounts
		}
		amount := 1 + rnd.Int64N(loadMaxTransfer)
		if err := l.exec(ctx, sql, from, to, amount); err != nil {
			sleepFor(ctx, loadRetryWait)
			continue
		}
		l.transfers.Add(1)
	}
}

func (l *upgradeLoad) ack() {
	now := time.Now().UnixNano()
	prev := l.lastAck.Swap(now)
	gap := now - prev
	for {
		max := l.maxGap.Load()
		if gap <= max || l.maxGap.CompareAndSwap(max, gap) {
			return
		}
	}
}

func (l *upgradeLoad) exec(ctx context.Context, sql string, args ...any) error {
	l.mu.RLock()
	conn, gen := l.conn, l.gen
	var err error
	if conn != nil {
		ectx, cancel := context.WithTimeout(ctx, loadExecTimeout)
		_, err = conn.Exec(ectx, sql, args...)
		cancel()
	} else {
		err = fmt.Errorf("the endpoint is not connected")
	}
	l.mu.RUnlock()
	if err != nil && ctx.Err() == nil {
		l.redial(ctx, gen)
	}
	return err
}

func (l *upgradeLoad) redial(ctx context.Context, gen uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.gen != gen {
		return
	}
	if l.conn != nil {
		l.conn.Close()
		l.conn = nil
	}
	dctx, cancel := context.WithTimeout(ctx, loadDialTimeout)
	conn, err := l.connect(dctx)
	cancel()
	l.gen++
	if err != nil {
		l.logf("write load: redial failed: %v", err)
		return
	}
	l.conn = conn
	l.redials.Add(1)
}

func (l *upgradeLoad) Verify(ctx context.Context, conn sqlConn) error {
	pool := poolOf(conn)
	if pool == nil {
		return fmt.Errorf("the connection to the endpoint carries no pgx pool, so %s cannot be read back id by id", l.journalTable())
	}
	present := map[int64]int64{}
	rows, err := pool.Query(ctx, "SELECT id, v FROM "+l.journalTable())
	if err != nil {
		return fmt.Errorf("read %s back after the upgrade: %w", l.journalTable(), err)
	}
	for rows.Next() {
		var id, v int64
		if err := rows.Scan(&id, &v); err != nil {
			rows.Close()
			return fmt.Errorf("scan a row of %s: %w", l.journalTable(), err)
		}
		present[id] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read %s back after the upgrade: %w", l.journalTable(), err)
	}

	v := l.journal.Verify(present)
	if !v.OK() {
		return fmt.Errorf("the endpoint does not serve what it acknowledged before and during the upgrade (%s): lost %s; never issued %s; wrong value %s. Last writer error: %s",
			v.Summary(), loadIDs(v.Lost), loadIDs(v.Unexpected), loadMismatches(v.Mismatched), l.journal.LastErr())
	}
	if v.Counts.Acked == 0 {
		return fmt.Errorf("the writers acknowledged no write at all across the upgrade (%s), so nothing was verified; the load never reached the endpoint", v.Summary())
	}

	accounts, err := conn.QueryInt(ctx, "SELECT count(*) FROM "+l.bankTable())
	if err != nil {
		return fmt.Errorf("count %s: %w", l.bankTable(), err)
	}
	total, err := conn.QueryInt(ctx, "SELECT coalesce(sum(bal), 0) FROM "+l.bankTable())
	if err != nil {
		return fmt.Errorf("sum %s: %w", l.bankTable(), err)
	}
	want := int64(l.cfg.Accounts) * l.cfg.Initial
	if accounts != int64(l.cfg.Accounts) || total != want {
		return fmt.Errorf("%s holds %d accounts totalling %d after %d transfers, want %d accounts totalling %d: money was created or destroyed across the upgrade",
			l.bankTable(), accounts, total, l.transfers.Load(), l.cfg.Accounts, want)
	}
	neg, err := conn.QueryInt(ctx, "SELECT count(*) FROM "+l.bankTable()+" WHERE bal < 0")
	if err != nil {
		return fmt.Errorf("check %s for negative balances: %w", l.bankTable(), err)
	}
	if neg != 0 {
		return fmt.Errorf("%d of %d accounts in %s hold a negative balance after the upgrade, so a debit committed without its guard", neg, l.cfg.Accounts, l.bankTable())
	}
	return nil
}

func (l *upgradeLoad) record(set func(string, any), gap time.Duration) {
	c := l.Counts()
	set("load_acked", c.Acked)
	set("load_unknown", c.Unknown)
	set("load_transfers", l.transfers.Load())
	set("load_redials", l.redials.Load())
	set("write_gap_seconds", gap.Seconds())
}

func poolOf(c sqlConn) *pgxpool.Pool {
	if p, ok := c.(interface{ Pool() *pgxpool.Pool }); ok {
		return p.Pool()
	}
	return nil
}

func sleepFor(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func loadIDs(ids []int64) string {
	if len(ids) == 0 {
		return "none"
	}
	shown := ids
	if len(shown) > loadReportedIDs {
		shown = shown[:loadReportedIDs]
	}
	parts := make([]string, 0, len(shown))
	for _, id := range shown {
		parts = append(parts, fmt.Sprintf("%d", id))
	}
	out := strings.Join(parts, ", ")
	if len(ids) > len(shown) {
		out += fmt.Sprintf(", and %d more", len(ids)-len(shown))
	}
	return out
}

func loadMismatches(ms []stress.AckValueMismatch) string {
	if len(ms) == 0 {
		return "none"
	}
	shown := ms
	if len(shown) > loadReportedIDs {
		shown = shown[:loadReportedIDs]
	}
	parts := make([]string, 0, len(shown))
	for _, m := range shown {
		parts = append(parts, fmt.Sprintf("id %d is %d, want %d (%s)", m.ID, m.Got, m.Want, m.State))
	}
	out := strings.Join(parts, "; ")
	if len(ms) > len(shown) {
		out += fmt.Sprintf("; and %d more", len(ms)-len(shown))
	}
	return out
}
