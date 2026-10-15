package consumer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"meko/event-consumers/datapack-metrics-consumer/internal/config"
	"meko/event-consumers/datapack-metrics-consumer/internal/stats"
	"meko/event-consumers/datapack-metrics-consumer/internal/store"
)

// lookBackKeyLimit caps how many keys one look-back scans. It takes the newest first, which is where a
// late commit shows up; a shard with more rows than this in its window is far above the expected rate.
const lookBackKeyLimit = 50000

// lookBack re-reads the window below a shard's offset and applies the events that were never handled.
//
// The main read leaves the newest SafetyLag unread, which covers a commit that is a little late. A
// commit later than that becomes visible below an offset that has already passed it, and the main read
// would never return it. The applied-event record says which events below the offset were handled, so
// the difference is exactly the late ones. It reports how many it applied. It never moves the offset.
func lookBack(
	ctx context.Context, db *sql.DB, cfg config.Config, t store.Tables, reg *Registry, shard int,
) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, batchTimeout(cfg))
	defer cancel()

	off, ok, err := store.ReadOffset(ctx, db, t, cfg.Consumer, shard)
	if err != nil || !ok {
		return 0, err
	}
	// Two bands below the offset. The inner one (one window) is recovered. The outer one (the window before
	// it) is only measured: an event there with no record committed later than the window and is lost, and
	// this is the only way anything can see that. event_applied keeps records for both bands.
	inner := off.TS.Add(-cfg.LookBackWindow)
	outer := off.TS.Add(-2 * cfg.LookBackWindow)
	keys, err := store.EventKeysBelow(ctx, db, t, shard, outer, off, lookBackKeyLimit)
	if err != nil || len(keys) == 0 {
		stats.Unrecovered.WithLabelValues(strconv.Itoa(shard)).Set(0)
		return 0, err
	}
	// Read the record after the events: an event handled in between only makes the record longer.
	applied, err := store.AppliedIDs(ctx, db, t, cfg.Consumer, shard, outer, off.TS)
	if err != nil {
		return 0, err
	}
	var missing []store.Position
	lost := 0
	for _, k := range keys {
		switch {
		case applied[k.ID]:
		case k.TS.Before(inner):
			lost++
		default:
			missing = append(missing, k)
		}
	}
	stats.Unrecovered.WithLabelValues(strconv.Itoa(shard)).Set(float64(lost))
	if lost > 0 {
		slog.Error("events committed later than the look-back window and were never applied",
			"shard", shard, "count", lost, "window", cfg.LookBackWindow)
	}
	if len(missing) == 0 {
		return 0, nil
	}
	if len(missing) > cfg.BatchSize {
		missing = missing[:cfg.BatchSize] // the rest are picked up by the next look-back
	}
	events, err := store.EventsByKey(ctx, db, t, shard, missing)
	if err != nil || len(events) == 0 {
		return 0, err
	}
	// An event of a type nothing here handles is left alone, not claimed: it is retried each pass while it is
	// in the window (a build with the handler may arrive), and it must not hold back the events beside it.
	handled := events[:0:0]
	for _, e := range events {
		if reg.handlers[e.Type] != nil {
			handled = append(handled, e)
		}
	}
	if skipped := len(events) - len(handled); skipped > 0 {
		stats.Errors.WithLabelValues("no_handler").Add(float64(skipped))
		slog.Warn("late events with no handler left unapplied", "shard", shard, "count", skipped)
	}
	if len(handled) == 0 {
		return 0, nil
	}
	n, err := applyLate(ctx, db, cfg, t, reg, shard, handled)
	if n > 0 {
		stats.LateEvents.Add(float64(n))
		// The oldest one shows how long the stall was; no payload goes in a log line.
		slog.Warn("applied events that committed after the offset had passed them",
			"shard", shard, "count", n, "oldest_age", time.Since(handled[0].CreatedAt).Round(time.Second))
	}
	return n, err
}

// applyLate handles events found below the offset. Each is claimed in event_applied first, in the same
// transaction as its effects, so an instance that finds the same event concurrently applies nothing.
func applyLate(
	ctx context.Context, db *sql.DB, cfg config.Config, t store.Tables, reg *Registry, shard int, events []store.Event,
) (int, error) {
	var counts map[string]int
	applied := 0
	err := inTx(ctx, db, func(tx *sql.Tx) error {
		mine, err := claim(ctx, tx, cfg, t, shard, events)
		if err != nil || len(mine) == 0 {
			return err
		}
		applied = len(mine)
		counts, err = dispatch(ctx, reg, tx, mine)
		return err
	})
	switch {
	case err == nil:
		record(counts)
		return applied, nil
	case !IsPoison(err):
		return 0, err
	}
	slog.Warn("late batch failed, retrying event by event",
		"shard", shard, "error", errClass(err), "sqlstate", sqlState(err))

	done, dead := 0, 0
	for _, e := range events {
		var counts map[string]int
		var claimed bool
		err := inTx(ctx, db, func(tx *sql.Tx) error {
			mine, err := claim(ctx, tx, cfg, t, shard, []store.Event{e})
			if err != nil || len(mine) == 0 {
				return err
			}
			claimed = true
			counts, err = dispatch(ctx, reg, tx, mine)
			return err
		})
		switch {
		case err == nil:
			if claimed {
				record(counts)
				done++
			}
		case !IsPoison(err):
			return done, err
		default:
			cause := err
			errText := fmt.Sprintf("%s: %s", errClass(cause), truncate(cause.Error(), 500))
			err = inTx(ctx, db, func(tx *sql.Tx) error {
				mine, err := claim(ctx, tx, cfg, t, shard, []store.Event{e})
				if err != nil || len(mine) == 0 {
					return err
				}
				return store.DeadLetter(ctx, tx, t, cfg.Consumer, shard, e, errText)
			})
			if err != nil {
				return done, err
			}
			stats.DeadLetters.WithLabelValues(stats.TypeLabel(e.Type)).Inc()
			slog.Error("late event dead-lettered", "event", e.ID, "type", e.Type, "shard", shard,
				"error", errClass(cause), "sqlstate", sqlState(cause))
			done++
			if dead++; dead >= cfg.MaxDeadLettersPerPass {
				stats.Errors.WithLabelValues("poison_paused").Inc()
				return done, errors.New("too many dead letters among late events")
			}
		}
	}
	return done, nil
}

// claim records the events as handled and returns the ones this transaction newly claimed.
func claim(
	ctx context.Context, tx *sql.Tx, cfg config.Config, t store.Tables, shard int, events []store.Event,
) ([]store.Event, error) {
	got, err := store.ClaimApplied(ctx, tx, t, cfg.Consumer, shard, events)
	if err != nil {
		return nil, err
	}
	var mine []store.Event
	for _, e := range events {
		if got[e.ID] {
			mine = append(mine, e)
		}
	}
	return mine, nil
}
