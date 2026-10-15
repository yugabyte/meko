package consumer

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"time"

	"meko/event-consumers/datapack-metrics-consumer/internal/config"
	"meko/event-consumers/datapack-metrics-consumer/internal/contract"
	"meko/event-consumers/datapack-metrics-consumer/internal/health"
	"meko/event-consumers/datapack-metrics-consumer/internal/stats"
	"meko/event-consumers/datapack-metrics-consumer/internal/store"
)

// shardFailure is a shard that keeps failing for a reason of its own (a missing table, a revoked grant, an
// event type with no handler): it is skipped until retryAt so the other shards, the janitor and the lag
// gauge keep working.
type shardFailure struct {
	attempts int
	retryAt  time.Time
}

// shardBackoff is how long a shard that has failed n times in a row is left alone: 1 s, doubling to 30 s.
func shardBackoff(n int) time.Duration {
	return min(reconnectMax, time.Second<<min(max(n-1, 0), 5))
}

const (
	reconnectMax = 30 * time.Second
	shutdownWait = 5 * time.Second
	// pauseFirst is how long a shard that trips the poison breaker waits; each consecutive trip doubles it,
	// up to pauseMax, and a pass that does not trip resets it. A permanently broken handler therefore
	// dead-letters about one pass's worth of events an hour per shard instead of a pass a minute.
	pauseFirst = time.Minute
	pauseMax   = time.Hour
)

// pauseBackoff is how long a shard that has tripped the poison breaker n times in a row is paused.
func pauseBackoff(n int) time.Duration {
	return min(pauseMax, pauseFirst<<min(max(n-1, 0), 6))
}

// Consumer is one running instance: it holds shard leases and works through their batches.
//
// It is single-threaded: Tick and Run must not be called concurrently, and the database handle must
// allow a connection per open transaction (main sets one).
type Consumer struct {
	cfg  config.Config
	db   *sql.DB
	reg  *Registry
	t    store.Tables
	id   string
	beat *health.Heartbeat

	owned        []int
	pausedUntil  map[int]time.Time
	poisonTrips  map[int]int
	failing      map[int]shardFailure
	lastRenew    time.Time
	lastJanitor  time.Time
	lastLookBack map[int]time.Time

	stop     chan struct{}
	stopOnce sync.Once
}

// New builds a consumer. id identifies this instance in event_consumers.lease_owner and must be a UUID.
func New(cfg config.Config, db *sql.DB, reg *Registry, id string) *Consumer {
	return &Consumer{
		cfg: cfg, db: db, reg: reg, t: store.NewTables(cfg.Schema), id: id,
		// Longer than the longest backoff and than a lease, so a loop that is waiting out an outage or working
		// through a backlog is not reported as stalled.
		beat:         health.NewHeartbeat(max(2*reconnectMax, 2*cfg.Lease)),
		pausedUntil:  map[int]time.Time{},
		poisonTrips:  map[int]int{},
		failing:      map[int]shardFailure{},
		lastLookBack: map[int]time.Time{},
		stop:         make(chan struct{}),
	}
}

// Heartbeat is beaten every tick and backs /healthz.
func (c *Consumer) Heartbeat() *health.Heartbeat { return c.beat }

// ID is this instance's lease identity.
func (c *Consumer) ID() string { return c.id }

// Handlers lists the event types that have a handler.
func (c *Consumer) Handlers() []string { return c.reg.Types() }

// Stop asks Run to finish the batch in progress, release its leases and return.
func (c *Consumer) Stop() { c.stopOnce.Do(func() { close(c.stop) }) }

func (c *Consumer) stopping() bool {
	select {
	case <-c.stop:
		return true
	default:
		return false
	}
}

// wait sleeps for d, or until Stop.
func (c *Consumer) wait(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-c.stop:
	}
}

// renew takes or renews the leases this instance wants.
func (c *Consumer) renew(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, batchTimeout(c.cfg)) // a hung query must not hold the loop
	defer cancel()
	wanted := slices.Clone(c.cfg.Shards)
	// Instances starting together should not fight over the same shards.
	rand.Shuffle(len(wanted), func(i, j int) { wanted[i], wanted[j] = wanted[j], wanted[i] })
	before, err := store.LeaseSnapshot(ctx, c.db, c.t, c.cfg.Consumer, wanted)
	if err != nil {
		return err
	}
	if c.cfg.MaxShards > 0 {
		room := max(0, c.cfg.MaxShards-len(c.owned))
		var kept, fresh []int
		for _, s := range wanted {
			if slices.Contains(c.owned, s) {
				kept = append(kept, s)
			} else {
				fresh = append(fresh, s)
			}
		}
		wanted = append(kept, fresh[:min(room, len(fresh))]...)
	}
	acquired, err := store.Acquire(ctx, c.db, c.t, c.cfg.Consumer, wanted, c.id, c.cfg.Lease)
	if err != nil {
		return err
	}
	for _, s := range acquired {
		if l, ok := before[s]; ok && l.Owner.Valid && l.Owner.String != c.id && l.Free {
			stats.LeaseTakeovers.Inc()
			slog.Info("took a shard from an expired lease", "shard", s)
		}
	}
	c.owned, c.lastRenew = acquired, time.Now()
	stats.ShardsHeld.Set(float64(len(acquired)))
	return nil
}

func (c *Consumer) renewDue() bool { return time.Since(c.lastRenew) >= c.cfg.Renew }

// Tick renews leases, processes one batch per held shard and runs janitor duty. It reports whether any
// event was consumed.
func (c *Consumer) Tick(ctx context.Context) (busy bool, err error) {
	if c.renewDue() {
		if err := c.renew(ctx); err != nil {
			return false, err
		}
	}
	for _, shard := range slices.Clone(c.owned) {
		if c.stopping() {
			break
		}
		if time.Now().Before(c.pausedUntil[shard]) {
			continue
		}
		if c.renewDue() { // a long pass must not let a lease run out under us
			if err := c.renew(ctx); err != nil {
				return busy, err
			}
		}
		// Checked for every shard, not only after a renew: a renew in an earlier iteration may have dropped
		// this one, and a lease we no longer hold is not ours to work on.
		if !slices.Contains(c.owned, shard) {
			continue
		}
		if f, ok := c.failing[shard]; ok && time.Now().Before(f.retryAt) {
			continue
		}
		res, err := processShard(ctx, c.db, c.cfg, c.t, c.reg, shard)
		busy = busy || res.Consumed > 0
		c.beat.Beat() // one shard's batch is bounded; a whole pass over 16 shards is not
		if errors.Is(err, ErrShardPaused) {
			c.poisonTrips[shard]++
			c.pausedUntil[shard] = time.Now().Add(pauseBackoff(c.poisonTrips[shard]))
			continue
		}
		if err != nil {
			c.shardFailed(shard, err)
			continue
		}
		delete(c.failing, shard)
		delete(c.poisonTrips, shard)
		// The look-back is its own batch-sized piece of work: renew first, and skip it if the lease is gone,
		// so it never runs on a shard another instance now holds.
		if c.lookBackDue(shard) {
			if c.renewDue() {
				if err := c.renew(ctx); err != nil {
					return busy, err
				}
			}
			if slices.Contains(c.owned, shard) {
				c.lookBack(ctx, shard)
			}
		}
	}

	if slices.Contains(c.owned, 0) && time.Since(c.lastJanitor) >= c.cfg.Janitor {
		c.lastJanitor = time.Now()
		if err := c.janitor(ctx); err != nil {
			stats.Errors.WithLabelValues("janitor").Inc()
			slog.Warn("janitor failed", "error", errClass(err), "sqlstate", sqlState(err))
		}
	}
	if !slices.Contains(c.owned, 0) {
		c.resetStaleGauges()
	}
	c.beat.Beat()
	return busy, nil
}

// resetStaleGauges zeroes what only the shard 0 holder measures (lag, partition runway, rows in the default
// partition) and the per-shard look-back measurement of shards this instance does not hold. An instance
// that lost shard 0, or whose tick failed, must not keep serving its last values: an alert on the maximum
// would stay fired after the cause is fixed, and a standby's frozen value would mask the live holder's.
func (c *Consumer) resetStaleGauges() {
	for s := range contract.ShardCount {
		stats.Lag.WithLabelValues(strconv.Itoa(s)).Set(0)
	}
	stats.RunwayMonths.Set(0)
	stats.DefaultRows.Set(0)
	for s := range contract.ShardCount {
		if !slices.Contains(c.owned, s) {
			stats.Unrecovered.WithLabelValues(strconv.Itoa(s)).Set(0)
		}
	}
}

// shardFailed records an error that belongs to one shard. It is counted, logged and backed off, and
// nothing else stops: returning the error from Tick would leave every shard after this one unread and
// would skip the janitor, which is what keeps the lag gauge honest.
func (c *Consumer) shardFailed(shard int, err error) {
	f := c.failing[shard]
	f.attempts++
	f.retryAt = time.Now().Add(shardBackoff(f.attempts))
	c.failing[shard] = f
	kind := "transient"
	switch {
	case errors.Is(err, ErrNoHandler):
		kind = "no_handler"
	case IsPoison(err):
		kind = "unexpected"
	}
	stats.Errors.WithLabelValues(kind).Inc()
	slog.Warn("shard failed, skipping it for a while", "shard", shard, "kind", kind, "error", errClass(err),
		"sqlstate", sqlState(err), "attempt", f.attempts, "retry_in", shardBackoff(f.attempts))
}

// lookBackDue reports whether a shard's look-back should run now.
func (c *Consumer) lookBackDue(shard int) bool {
	return time.Since(c.lastLookBack[shard]) >= c.cfg.LookBackEvery
}

// lookBack runs the late-commit safety net for a shard. A failure is counted and logged
// but never stops the main read: this is a net under the main path, not part of it.
func (c *Consumer) lookBack(ctx context.Context, shard int) {
	c.lastLookBack[shard] = time.Now()
	if _, err := lookBack(ctx, c.db, c.cfg, c.t, c.reg, shard); err != nil {
		stats.Errors.WithLabelValues("lookback").Inc()
		slog.Warn("look-back failed", "shard", shard, "error", errClass(err), "sqlstate", sqlState(err))
	}
}

// janitor takes the measurements nobody else makes, and trims old dead letters. It is read-only for
// events: it never runs DDL and never deletes events. Partitions are created and dropped by hand (the
// MEKO-873 runbook; MEKO-883 automates them), so the runway and default-partition gauges are what raise
// the alarm when someone forgets.
func (c *Consumer) janitor(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, batchTimeout(c.cfg))
	defer cancel()
	offsets, err := store.AllOffsets(ctx, c.db, c.t, c.cfg.Consumer)
	if err != nil {
		return err
	}
	for shard, off := range offsets {
		age, err := store.OldestUnreadAge(ctx, c.db, c.t, shard, off)
		if err != nil {
			return err
		}
		stats.Lag.WithLabelValues(strconv.Itoa(shard)).Set(age)
	}
	if months, ok, err := store.PartitionRunwayMonths(ctx, c.db, c.t); err != nil {
		return err
	} else if ok {
		stats.RunwayMonths.Set(months)
	}
	rows, err := store.DefaultPartitionRows(ctx, c.db, c.t)
	if err != nil {
		return err
	}
	stats.DefaultRows.Set(float64(rows))
	for shard, off := range offsets {
		// Trim behind each shard's own offset, not the clock: a shard that is behind still needs the
		// records near its offset, and without them the look-back would apply those events again.
		// Twice the window: the look-back recovers within one window and measures the second one (what it
		// finds there without a record committed too late to be recovered).
		before := off.TS.Add(-2 * c.cfg.LookBackWindow)
		if _, err := store.TrimApplied(ctx, c.db, c.t, c.cfg.Consumer, shard, before); err != nil {
			return err
		}
	}
	removed, err := store.TrimDeadLetters(ctx, c.db, c.t, c.cfg.DeadLetterDays, 1000)
	if err != nil {
		return err
	}
	if removed > 0 {
		slog.Info("removed old dead letters", "count", removed, "older_than_days", c.cfg.DeadLetterDays)
	}
	return nil
}

// Run works until Stop, then releases the leases. A database error is logged, counted and retried with
// backoff; the connection is re-established by database/sql.
func (c *Consumer) Run(ctx context.Context) {
	idle, failures := c.cfg.PollMin, 0
	for !c.stopping() {
		busy, err := c.Tick(ctx)
		if err != nil {
			failures++
			kind := "transient"
			if IsPoison(err) {
				kind = "unexpected"
			}
			stats.Errors.WithLabelValues(kind).Inc()
			slog.Warn("tick failed, backing off",
				"kind", kind, "error", errClass(err), "sqlstate", sqlState(err), "attempt", failures)
			// Keep c.owned: Acquire re-validates it, and clearing it would orphan this instance's leases (with
			// MAX_SHARDS, it would then pick other shards at random). Only force a renew on the next tick.
			c.lastRenew = time.Time{}
			c.resetStaleGauges()
			c.beat.Beat() // waiting out an outage is not a stalled loop
			c.wait(min(reconnectMax, time.Duration(1<<min(failures, 5))*time.Second))
			continue
		}
		failures = 0
		if busy {
			idle = c.cfg.PollMin
			continue
		}
		c.wait(idle)
		idle = min(c.cfg.PollMax, idle*2)
	}
	c.shutdown(ctx)
}

func (c *Consumer) shutdown(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownWait)
	defer cancel()
	if err := store.Release(ctx, c.db, c.t, c.cfg.Consumer, c.id); err != nil {
		slog.Warn("could not release leases; they expire on their own", "lease", c.cfg.Lease, "error", errClass(err))
	}
	slog.Info("events consumer stopped")
}
