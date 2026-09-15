package history

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Ids 0..Accounts-1 carry pair=-1 and move between each other by row-guarded
// transfer. The 2*SkewPairs ids after them are paired and drained into the sink, the last id.
type BankConfig struct {
	Table        string
	Accounts     int
	SkewPairs    int
	Initial      int64
	MaxAmount    int64
	Writers      int
	Readers      int
	Isolation    string
	Unguarded    bool
	ReadInterval time.Duration
}

const skewEveryNthOp = 5

type Bank struct {
	pool  *pgxpool.Pool
	rec   *Recorder
	cfg   BankConfig
	label string
	iso   pgx.TxIsoLevel

	skewBase int
	sinkID   int

	committed atomic.Int64
	failed    atomic.Int64
	unknown   atomic.Int64
	reads     atomic.Int64
	recErr    atomic.Pointer[error]
}

type Stats struct {
	Committed int64
	Failed    int64
	Unknown   int64
	Reads     int64
}

func NewBank(pool *pgxpool.Pool, rec *Recorder, label string, cfg BankConfig) (*Bank, error) {
	if cfg.Accounts < 2 {
		return nil, fmt.Errorf("a bank needs at least 2 accounts, got %d", cfg.Accounts)
	}
	if cfg.MaxAmount < 1 {
		return nil, fmt.Errorf("max amount must be positive, got %d", cfg.MaxAmount)
	}
	if !validIdentifier(cfg.Table) {
		return nil, fmt.Errorf("table name %q is not a plain identifier", cfg.Table)
	}
	iso, err := isoLevel(cfg.Isolation)
	if err != nil {
		return nil, err
	}
	if cfg.SkewPairs < 0 {
		cfg.SkewPairs = 0
	}
	b := &Bank{pool: pool, rec: rec, cfg: cfg, label: label, iso: iso}
	b.skewBase = cfg.Accounts
	b.sinkID = cfg.Accounts + 2*cfg.SkewPairs
	return b, nil
}

func isoLevel(name string) (pgx.TxIsoLevel, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "read committed", "read_committed":
		return pgx.ReadCommitted, nil
	case "repeatable read", "repeatable_read":
		return pgx.RepeatableRead, nil
	case "serializable":
		return pgx.Serializable, nil
	}
	return "", fmt.Errorf("unsupported isolation %q: want read committed, repeatable read or serializable", name)
}

func validIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r != '_' && !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func (b *Bank) NumAccounts() int { return b.sinkID + 1 }

func (b *Bank) Total() int64 { return int64(b.NumAccounts()) * b.cfg.Initial }

func (b *Bank) Stats() Stats {
	return Stats{
		Committed: b.committed.Load(),
		Failed:    b.failed.Load(),
		Unknown:   b.unknown.Load(),
		Reads:     b.reads.Load(),
	}
}

func (b *Bank) RecordError() error {
	if p := b.recErr.Load(); p != nil {
		return *p
	}
	return nil
}

func (b *Bank) Setup(ctx context.Context) error {
	_, err := b.pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s (id int PRIMARY KEY, pair int NOT NULL, bal bigint NOT NULL, ver bigint NOT NULL)",
		b.cfg.Table))
	if err != nil {
		return fmt.Errorf("create %s: %w", b.cfg.Table, err)
	}
	_, err = b.pool.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s (id, pair, bal, ver)
		 SELECT g, CASE WHEN g < $1 OR g = $2 THEN -1 ELSE (g - $1) / 2 END, $3::bigint, 0
		 FROM generate_series(0, $2) g ON CONFLICT (id) DO NOTHING`, b.cfg.Table),
		b.cfg.Accounts, b.sinkID, b.cfg.Initial)
	if err != nil {
		return fmt.Errorf("seed %s: %w", b.cfg.Table, err)
	}
	return nil
}

func (b *Bank) Run(ctx context.Context, seed int64, d time.Duration) {
	runCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < b.cfg.Writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			session := fmt.Sprintf("%s-w%d", b.label, i)
			rnd := rand.New(rand.NewPCG(uint64(seed), uint64(i)+1))
			for n := 0; runCtx.Err() == nil; n++ {
				if b.cfg.SkewPairs > 0 && n%skewEveryNthOp == skewEveryNthOp-1 {
					b.skew(runCtx, session, rnd)
					continue
				}
				b.transfer(runCtx, session, rnd)
			}
		}(i)
	}
	for i := 0; i < b.cfg.Readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			session := fmt.Sprintf("%s-r%d", b.label, i)
			for runCtx.Err() == nil {
				b.read(runCtx, session)
				select {
				case <-runCtx.Done():
				case <-time.After(b.cfg.ReadInterval):
				}
			}
		}(i)
	}
	wg.Wait()
}

func (b *Bank) debitSQL() string {
	guard := " AND bal >= $1"
	if b.cfg.Unguarded {
		guard = ""
	}
	return fmt.Sprintf("UPDATE %s SET bal = bal - $1, ver = ver + 1 WHERE id = $2%s", b.cfg.Table, guard)
}

func (b *Bank) creditSQL() string {
	return fmt.Sprintf("UPDATE %s SET bal = bal + $1, ver = ver + 1 WHERE id = $2", b.cfg.Table)
}

func (b *Bank) transfer(ctx context.Context, session string, rnd *rand.Rand) {
	from := rnd.IntN(b.cfg.Accounts)
	to := rnd.IntN(b.cfg.Accounts)
	if to == from {
		to = (to + 1) % b.cfg.Accounts
	}
	amount := 1 + rnd.Int64N(b.cfg.MaxAmount)
	op := Op{Session: session, Kind: KindTransfer, From: from, To: to, Amount: amount}
	b.finish(ctx, op, b.runTx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return b.move(ctx, tx, from, to, amount)
	}))
}

// Guarded on the pair's SUM, so under SERIALIZABLE one of two concurrent drains must abort.
func (b *Bank) skew(ctx context.Context, session string, rnd *rand.Rand) {
	pair := rnd.IntN(b.cfg.SkewPairs)
	from := b.skewBase + 2*pair + rnd.IntN(2)
	amount := 1 + rnd.Int64N(b.cfg.MaxAmount)
	op := Op{Session: session, Kind: KindSkew, From: from, To: b.sinkID, Amount: amount}
	b.finish(ctx, op, b.runTx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		var sum int64
		err := tx.QueryRow(ctx, fmt.Sprintf("SELECT coalesce(sum(bal), 0) FROM %s WHERE pair = $1", b.cfg.Table), pair).Scan(&sum)
		if err != nil {
			return false, err
		}
		if !b.cfg.Unguarded && sum < amount {
			return false, nil
		}
		return b.move(ctx, tx, from, b.sinkID, amount)
	}))
}

// Account-id order, so two transfers over the same rows queue, not deadlock.
func (b *Bank) move(ctx context.Context, tx pgx.Tx, from, to int, amount int64) (bool, error) {
	debit := func() (bool, error) {
		tag, err := tx.Exec(ctx, b.debitSQL(), amount, from)
		if err != nil {
			return false, err
		}
		return tag.RowsAffected() == 1, nil
	}
	credit := func() (bool, error) {
		tag, err := tx.Exec(ctx, b.creditSQL(), amount, to)
		if err != nil {
			return false, err
		}
		return tag.RowsAffected() == 1, nil
	}
	first, second := debit, credit
	if to < from {
		first, second = credit, debit
	}
	if ok, err := first(); err != nil || !ok {
		return false, err
	}
	return second()
}

func (b *Bank) read(ctx context.Context, session string) {
	op := Op{Session: session, Kind: KindRead, InvokeNS: time.Now().UnixNano()}
	accounts, err := b.selectAccounts(ctx, b.pool)
	op.DoneNS = time.Now().UnixNano()
	if err != nil {
		op.State = StateFail
		op.Err = err.Error()
	} else {
		op.State = StateOK
		op.Accounts = accounts
		b.reads.Add(1)
	}
	b.append(op)
}

// Only meaningful with no writer running: a commit between the LSN and the balances puts a fork at that LSN ahead of them.
func (b *Bank) Snapshot(ctx context.Context, session string) ([]Account, string, error) {
	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, "", fmt.Errorf("begin snapshot: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	var lsn string
	if err := tx.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&lsn); err != nil {
		return nil, "", fmt.Errorf("read the WAL position: %w", err)
	}
	accounts, err := b.selectAccounts(ctx, tx)
	if err != nil {
		return nil, "", err
	}
	b.append(Op{
		Session: session, Kind: KindRead, State: StateOK, LSN: lsn, Accounts: accounts,
		InvokeNS: time.Now().UnixNano(), DoneNS: time.Now().UnixNano(),
	})
	b.reads.Add(1)
	return accounts, lsn, nil
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func (b *Bank) selectAccounts(ctx context.Context, q querier) ([]Account, error) {
	rows, err := q.Query(ctx, fmt.Sprintf("SELECT id, pair, bal, ver FROM %s ORDER BY id", b.cfg.Table))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", b.cfg.Table, err)
	}
	defer rows.Close()
	out := make([]Account, 0, b.NumAccounts())
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Pair, &a.Bal, &a.Ver); err != nil {
			return nil, fmt.Errorf("scan %s: %w", b.cfg.Table, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", b.cfg.Table, err)
	}
	return out, nil
}

type txOutcome struct {
	state string
	err   error
	start int64
	end   int64
}

// Only a listed SQLSTATE on COMMIT proves an abort. Any other COMMIT error leaves the operation open.
func (b *Bank) runTx(ctx context.Context, body func(context.Context, pgx.Tx) (bool, error)) (out txOutcome) {
	out.start = time.Now().UnixNano()
	defer func() { out.end = time.Now().UnixNano() }()

	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: b.iso})
	if err != nil {
		out.state, out.err = StateFail, err
		return out
	}
	commit, err := body(ctx, tx)
	if err != nil || !commit {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		out.state, out.err = StateFail, err
		return out
	}
	if err := tx.Commit(ctx); err != nil {
		out.err = err
		if abortedOnCommit(err) {
			out.state = StateFail
		} else {
			out.state = StateUnknown
		}
		return out
	}
	out.state = StateOK
	return out
}

func abortedOnCommit(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "40001", "40P01", "25P02", "23514", "23505":
		return true
	}
	return false
}

func (b *Bank) finish(ctx context.Context, op Op, out txOutcome) {
	op.InvokeNS, op.DoneNS = out.start, out.end
	op.State = out.state
	if out.err != nil {
		op.Err = out.err.Error()
	}
	switch op.State {
	case StateOK:
		b.committed.Add(1)
	case StateUnknown:
		b.unknown.Add(1)
	default:
		b.failed.Add(1)
	}
	b.append(op)
}

func (b *Bank) append(op Op) {
	if err := b.rec.Append(op); err != nil && b.recErr.Load() == nil {
		b.recErr.CompareAndSwap(nil, &err)
	}
}
