package consumer

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"
	"unicode/utf8"

	"github.com/lib/pq"

	"meko/event-consumers/datapack-metrics-consumer/internal/config"
	"meko/event-consumers/datapack-metrics-consumer/internal/stats"
	"meko/event-consumers/datapack-metrics-consumer/internal/store"
)

var (
	// ErrOffsetConflict means another instance advanced the offset first. It is returned from inside the
	// transaction, so the batch rolls back.
	ErrOffsetConflict = errors.New("offset moved by another instance")
	// ErrNoHandler means an event has a type nothing handles. It is not the event's fault and it is not
	// skipped: the shard waits, so a consumer that is older than its producer (a rollback, or a pod that has
	// not been replaced yet) loses nothing, and the lag gauge shows the wait.
	ErrNoHandler = errors.New("no handler for the event type")
	// ErrShardPaused means too many dead letters in one pass: the handler is probably broken, so stop and wait.
	ErrShardPaused = errors.New("shard paused after repeated dead letters")
)

// defaultBatchTimeout applies when the config does not set one (tests build a Config by hand).
const defaultBatchTimeout = 15 * time.Second

// batchTimeout bounds one batch's database work, so a hung connection cannot hold a shard forever. It is
// shorter than the lease minus a renewal: leases are renewed between shards, and the batch's open
// transaction holds the only connection, so a longer batch could outlive its lease.
func batchTimeout(cfg config.Config) time.Duration {
	if cfg.BatchTimeout > 0 {
		return cfg.BatchTimeout
	}
	return defaultBatchTimeout
}

// IsPoison reports whether the event itself is at fault, as opposed to the database or the environment.
//
// Bad data (SQLSTATE class 22) and constraint violations (23) are the event's fault, and so is anything
// a handler returns that is not a database or connection error (a bad payload shape, a panic). Connection
// loss, serialization conflicts, timeouts, a missing table or a permission error say nothing about the
// event: dead-lettering on those would empty the queue into the dead-letter table during an outage or a
// bad deploy.
func IsPoison(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		class := pqErr.Code.Class()
		return class == "22" || class == "23"
	}
	if errors.Is(err, ErrNoHandler) {
		return false
	}
	var netErr net.Error
	switch {
	case errors.Is(err, driver.ErrBadConn), errors.Is(err, sql.ErrConnDone), errors.Is(err, sql.ErrTxDone),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.As(err, &netErr):
		return false
	}
	return true
}

// dispatch calls each type's handler once with that type's events in order. If any event has a type with
// no handler it runs nothing and returns ErrNoHandler, so the whole batch rolls back and the shard waits.
// It returns the counts for the caller to record once the transaction has committed: counting here would
// count a batch that is then rolled back.
func dispatch(ctx context.Context, reg *Registry, tx *sql.Tx, events []store.Event) (map[string]int, error) {
	var order []string
	grouped := map[string][]store.Event{}
	for _, e := range events {
		if reg.handlers[e.Type] == nil {
			return nil, fmt.Errorf("%w: %q", ErrNoHandler, e.Type)
		}
		if _, seen := grouped[e.Type]; !seen {
			order = append(order, e.Type)
		}
		grouped[e.Type] = append(grouped[e.Type], e)
	}
	counts := map[string]int{}
	for _, t := range order {
		if err := safely(ctx, reg.handlers[t], tx, grouped[t]); err != nil {
			return nil, err
		}
		counts[t] = len(grouped[t])
	}
	return counts, nil
}

// safely turns a handler panic into an error, which counts as the event's fault.
func safely(ctx context.Context, h Handler, tx *sql.Tx, events []store.Event) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return h(ctx, tx, events)
}

func record(counts map[string]int) {
	for t, n := range counts {
		stats.Processed.WithLabelValues(stats.TypeLabel(t)).Add(float64(n))
	}
}

// inTx runs fn in a transaction: commit if it returns nil, roll back otherwise.
func inTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Result is what one call to processShard consumed.
type Result struct {
	Consumed int
}

// processShard handles one batch for a shard. A returned error is one the loop must see. The Result can be
// non-zero alongside an error: with ErrShardPaused, and with an ordinary error once some events of a
// poison replay have committed.
func processShard(
	ctx context.Context, db *sql.DB, cfg config.Config, t store.Tables, reg *Registry, shard int,
) (Result, error) {
	started := time.Now()
	defer func() { stats.BatchSeconds.Observe(time.Since(started).Seconds()) }()
	ctx, cancel := context.WithTimeout(ctx, batchTimeout(cfg))
	defer cancel()

	res, err := wholeBatch(ctx, db, cfg, t, reg, shard)
	switch {
	case err == nil:
		return res, nil
	case errors.Is(err, ErrOffsetConflict):
		stats.OffsetConflicts.Inc()
		return Result{}, nil
	case !IsPoison(err):
		return Result{}, err
	}
	slog.Warn("batch failed, retrying event by event",
		"shard", shard, "error", errClass(err), "sqlstate", sqlState(err))
	return eventByEvent(ctx, db, cfg, t, reg, shard)
}

func wholeBatch(
	ctx context.Context, db *sql.DB, cfg config.Config, t store.Tables, reg *Registry, shard int,
) (Result, error) {
	var counts map[string]int
	var n int
	err := inTx(ctx, db, func(tx *sql.Tx) error {
		off, ok, err := store.ReadOffset(ctx, tx, t, cfg.Consumer, shard)
		if err != nil || !ok {
			return err
		}
		events, err := store.ReadBatch(ctx, tx, t, shard, off, cfg.SafetyLag, cfg.BatchSize)
		if err != nil || len(events) == 0 {
			return err
		}
		if counts, err = dispatch(ctx, reg, tx, events); err != nil {
			return err
		}
		if err := store.MarkApplied(ctx, tx, t, cfg.Consumer, shard, events); err != nil {
			return err
		}
		advanced, err := store.AdvanceOffset(ctx, tx, t, cfg.Consumer, shard, off, events[len(events)-1].Pos())
		if err != nil {
			return err
		}
		if !advanced {
			return ErrOffsetConflict
		}
		n = len(events)
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	record(counts)
	return Result{Consumed: n}, nil
}

// eventByEvent applies a failed batch one event at a time, each in its own transaction, so one bad event
// is dead-lettered and the rest go through.
func eventByEvent(
	ctx context.Context, db *sql.DB, cfg config.Config, t store.Tables, reg *Registry, shard int,
) (Result, error) {
	off, ok, err := store.ReadOffset(ctx, db, t, cfg.Consumer, shard)
	if err != nil || !ok {
		return Result{}, err
	}
	events, err := store.ReadBatch(ctx, db, t, shard, off, cfg.SafetyLag, cfg.BatchSize)
	if err != nil {
		return Result{}, err
	}

	done, dead := 0, 0
	for _, e := range events {
		if ctx.Err() != nil { // out of time: what is done is committed, the rest is read again next pass
			return Result{Consumed: done}, nil
		}
		next := e.Pos()
		var counts map[string]int
		err := inTx(ctx, db, func(tx *sql.Tx) error {
			var err error
			if counts, err = dispatch(ctx, reg, tx, []store.Event{e}); err != nil {
				return err
			}
			if err := store.MarkApplied(ctx, tx, t, cfg.Consumer, shard, []store.Event{e}); err != nil {
				return err
			}
			return advance(ctx, tx, cfg, t, shard, off, next)
		})
		switch {
		case err == nil:
			record(counts)
		case errors.Is(err, ErrOffsetConflict):
			stats.OffsetConflicts.Inc()
			return Result{Consumed: done}, nil
		case !IsPoison(err):
			return Result{Consumed: done}, err
		default:
			// The failed transaction rolled back; a fresh one records the event and moves on.
			cause := err
			errText := fmt.Sprintf("%s: %s", errClass(cause), truncate(cause.Error(), 500))
			err = inTx(ctx, db, func(tx *sql.Tx) error {
				if err := store.DeadLetter(ctx, tx, t, cfg.Consumer, shard, e, errText); err != nil {
					return err
				}
				if err := store.MarkApplied(ctx, tx, t, cfg.Consumer, shard, []store.Event{e}); err != nil {
					return err
				}
				return advance(ctx, tx, cfg, t, shard, off, next)
			})
			if errors.Is(err, ErrOffsetConflict) {
				stats.OffsetConflicts.Inc()
				return Result{Consumed: done}, nil
			}
			if err != nil {
				return Result{Consumed: done}, err
			}
			stats.DeadLetters.WithLabelValues(stats.TypeLabel(e.Type)).Inc()
			slog.Error("event dead-lettered", "event", e.ID, "type", e.Type, "shard", shard,
				"error", errClass(cause), "sqlstate", sqlState(cause))
			dead++
			if dead >= cfg.MaxDeadLettersPerPass {
				done++
				stats.Errors.WithLabelValues("poison_paused").Inc()
				return Result{Consumed: done}, ErrShardPaused
			}
		}
		off = next
		done++
	}
	return Result{Consumed: done}, nil
}

func advance(
	ctx context.Context, tx *sql.Tx, cfg config.Config, t store.Tables, shard int, from, to store.Position,
) error {
	ok, err := store.AdvanceOffset(ctx, tx, t, cfg.Consumer, shard, from, to)
	if err != nil {
		return err
	}
	if !ok {
		return ErrOffsetConflict
	}
	return nil
}

// errClass names an error's type for logs; with sqlState it is all a log line says about a failure,
// because a database error can quote part of the payload (a prompt).
func errClass(err error) string { return fmt.Sprintf("%T", unwrapAll(err)) }

func unwrapAll(err error) error {
	for {
		u := errors.Unwrap(err)
		if u == nil {
			return err
		}
		err = u
	}
}

func sqlState(err error) string {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return string(pqErr.Code)
	}
	return ""
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
