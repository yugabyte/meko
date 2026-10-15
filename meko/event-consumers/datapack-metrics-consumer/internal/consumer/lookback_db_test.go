package consumer

// The late-commit case: created_at is stamped at INSERT but a row is visible only at COMMIT, so a row can
// become visible below an offset that has already passed it. These tests hold an insert transaction open
// while the consumer advances, then commit it, and check the event is still applied exactly once.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"meko/event-consumers/datapack-metrics-consumer/internal/config"
	"meko/event-consumers/datapack-metrics-consumer/internal/stats"
)

// everyTick makes the look-back run on every tick, so a test does not wait out the interval.
func everyTick(c *config.Config) { c.LookBackEvery = 0 }

// openInsert starts an insert of an event, created secondsAgo in the past, that is not committed until
// commit is called: the commit is what stalls in production.
func (e *env) openInsert(shard int, secondsAgo float64, payload string) (id string, commit func()) {
	e.t.Helper()
	tx, err := e.admin.Begin()
	must(e.t, err)
	id = uuid.NewString()
	_, err = tx.Exec(strings.ReplaceAll(
		`INSERT INTO __S__.events (shard, created_at, id, type, payload)
		 VALUES ($1, now() - make_interval(secs => $2::float8), $3::uuid, $4, $5::jsonb)`, "__S__", e.schema),
		shard, secondsAgo, id, eventType, payload)
	must(e.t, err)
	return id, func() { must(e.t, tx.Commit()) }
}

func lateEvents() float64 { return testutil.ToFloat64(stats.LateEvents) }

func TestAnEventThatCommitsAfterTheOffsetPassedItIsStillApplied(t *testing.T) {
	e := newEnv(t)
	c := e.newConsumer(e.registry(), everyTick)
	before := lateEvents()

	stalled, commit := e.openInsert(4, 20, `{}`) // created first, visible last
	later := e.addEvent(4, 10, nil)              // created after it, committed at once

	if _, err := c.Tick(ctx()); err != nil {
		t.Fatal(err)
	}
	if got := e.effects(); !slices.Equal(got, sorted(later)) {
		t.Fatalf("before the stalled commit: effects = %v, want only %s", got, later)
	}
	if got := e.offsetID(4); got != later {
		t.Fatalf("offset = %s, want it past the visible event %s", got, later)
	}

	commit() // now e1 is visible below the offset, where the main read will never look

	if _, err := c.Tick(ctx()); err != nil {
		t.Fatal(err)
	}
	if got := e.effects(); !slices.Equal(got, sorted(stalled, later)) {
		t.Fatalf("after the stalled commit: effects = %v, want both", got)
	}
	if got := e.offsetID(4); got != later {
		t.Fatalf("the look-back must not move the offset: %s", got)
	}
	if got := lateEvents() - before; got != 1 {
		t.Fatalf("late events counted = %v, want 1", got)
	}

	// Nothing is applied twice, however many times it runs.
	for range 3 {
		if _, err := c.Tick(ctx()); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.effects(); !slices.Equal(got, sorted(stalled, later)) {
		t.Fatalf("after more ticks: effects = %v", got)
	}
	if got := lateEvents() - before; got != 1 {
		t.Fatalf("late events counted = %v after more ticks, want 1", got)
	}
}

func TestEventsThatAreAlreadyHandledAreNeverReapplied(t *testing.T) {
	e := newEnv(t)
	c := e.newConsumer(e.registry(), everyTick)
	for range 3 {
		e.addEvent(2, 30, nil)
	}
	e.addEvent(2, 20, map[string]any{"bad": true}) // dead-lettered: also recorded as handled
	before := lateEvents()
	for range 5 {
		if _, err := c.Tick(ctx()); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.count(`SELECT count(*) FROM __S__.effects`); n != 3 {
		t.Fatalf("effects = %d, want 3", n)
	}
	if n := e.count(`SELECT count(*) FROM __S__.event_dead_letters`); n != 1 {
		t.Fatalf("dead letters = %d, want 1: a dead-lettered event must not come back through the look-back", n)
	}
	if got := lateEvents() - before; got != 0 {
		t.Fatalf("late events counted = %v, want 0", got)
	}
}

func TestALateEventThatIsMalformedIsDeadLetteredOnce(t *testing.T) {
	e := newEnv(t)
	c := e.newConsumer(e.registry(), everyTick)
	_, commit := e.openInsert(7, 20, `{"bad": true}`)
	good := e.addEvent(7, 10, nil)
	if _, err := c.Tick(ctx()); err != nil {
		t.Fatal(err)
	}
	commit()
	for range 3 {
		if _, err := c.Tick(ctx()); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.effects(); !slices.Equal(got, sorted(good)) {
		t.Fatalf("effects = %v", got)
	}
	if n := e.count(`SELECT count(*) FROM __S__.event_dead_letters`); n != 1 {
		t.Fatalf("dead letters = %d, want 1", n)
	}
}

func TestTwoInstancesDoNotBothApplyTheSameLateEvent(t *testing.T) {
	e := newEnv(t)
	a, b := e.newConsumer(e.registry(), everyTick), e.newConsumer(e.registry(), everyTick)
	for i := range 20 { // several rounds, so the two are likely to overlap at least once
		stalled, commit := e.openInsert(5, 20, `{}`)
		later := e.addEvent(5, 10, nil)
		if _, err := a.Tick(ctx()); err != nil {
			t.Fatal(err)
		}
		commit()
		var wg sync.WaitGroup
		for _, c := range []*Consumer{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// On YugabyteDB the loser of the concurrent claim can get a serialization failure (40001) instead
				// of waiting and conflicting; either way it applies nothing, which is what is asserted below.
				var pqErr *pq.Error
				if _, err := lookBack(context.Background(), c.db, c.cfg, c.t, c.reg, 5); err != nil &&
					!(errors.As(err, &pqErr) && pqErr.Code == "40001") {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if n := e.count(`SELECT count(*) FROM __S__.effects WHERE event_id = '` + stalled + `'`); n != 1 {
			t.Fatalf("round %d: the late event was applied %d times, want 1", i, n)
		}
		if n := e.count(`SELECT count(*) FROM __S__.effects WHERE event_id = '` + later + `'`); n != 1 {
			t.Fatalf("round %d: the visible event was applied %d times, want 1", i, n)
		}
		e.exec(`TRUNCATE __S__.events, __S__.event_applied, __S__.effects`)
		e.exec(`UPDATE __S__.event_consumers SET offset_ts = now() - interval '1 day',
		        offset_id = '00000000-0000-0000-0000-000000000000'`)
	}
}

func TestTheAppliedRecordIsKeptForTwoWindowsBehindTheShardsOwnOffset(t *testing.T) {
	e := newEnv(t)
	c := e.newConsumer(e.registry(), func(c *config.Config) { c.LookBackWindow = 5 * time.Minute })
	// A shard that is two hours behind: its offset is old, so a wall-clock cutoff would delete records
	// it still needs and the look-back would then apply those events a second time. Records are kept for
	// two windows (10 minutes) below the offset: the look-back recovers in the first and measures the second.
	e.exec(`UPDATE __S__.event_consumers SET offset_ts = now() - interval '2 hours' WHERE shard = 9`)
	e.exec(`INSERT INTO __S__.event_applied (shard, consumer, created_at, event_id) VALUES
	        (9, 'analytics', now() - interval '2 hours 20 minutes', gen_random_uuid()),
	        (9, 'analytics', now() - interval '2 hours 12 minutes', gen_random_uuid()),
	        (9, 'analytics', now() - interval '2 hours 8 minutes',  gen_random_uuid()),
	        (9, 'analytics', now() - interval '1 hour',             gen_random_uuid())`)
	must(t, c.janitor(ctx()))
	left := e.strings(`SELECT to_char(now() - created_at, 'HH24:MI') FROM __S__.event_applied WHERE shard = 9`)
	if len(left) != 2 || left[0] != "01:00" || left[1] != "02:08" {
		t.Fatalf("records left = %v, want the ones within two windows of the offset (02:08 and 01:00 ago)", left)
	}
}

func TestADatabaseWithoutTheAppliedTableIsAnEnvironmentErrorNotADeadLetter(t *testing.T) {
	e := newEnv(t)
	c := e.newConsumer(e.registry(), everyTick)
	id := e.addEvent(1, 10, nil)
	e.exec(`DROP TABLE __S__.event_applied`)
	before := testutil.ToFloat64(stats.Errors.WithLabelValues("transient"))
	// The main read records events as handled, so it fails too. That belongs to the shard, which backs off;
	// it is never the event's fault.
	if _, err := c.Tick(ctx()); err != nil {
		t.Fatal(err)
	}
	if _, failing := c.failing[1]; !failing {
		t.Fatal("shard 1 should be marked failing")
	}
	if got := testutil.ToFloat64(stats.Errors.WithLabelValues("transient")) - before; got < 1 {
		t.Fatalf("transient errors +%v, want at least 1", got)
	}
	if n := e.count(`SELECT count(*) FROM __S__.event_dead_letters`); n != 0 {
		t.Fatalf("a missing table must not dead-letter events: %d", n)
	}
	if got := e.effects(); len(got) != 0 {
		t.Fatalf("effects = %v for %s: the batch must have rolled back", got, id)
	}
}

// insertAt puts an event on a shard with an explicit created_at, as a row that was already below the offset
// when it became visible.
func (e *env) insertAt(shard int, createdAt, typ string) string {
	e.t.Helper()
	id := uuid.NewString()
	e.exec(`INSERT INTO __S__.events (shard, created_at, id, type, payload)
	        VALUES ($1, `+createdAt+`, $2::uuid, $3, '{}'::jsonb)`, shard, id, typ)
	return id
}

func TestAnEventBelowTheWindowWithNoRecordIsMeasuredNotApplied(t *testing.T) {
	e := newEnv(t)
	c := e.newConsumer(e.registry(), everyTick, func(c *config.Config) { c.LookBackWindow = 5 * time.Minute })
	// The offset is 30 minutes back. One event sits 3 minutes below it (inside the window: recovered), one 7
	// minutes below (outside the window: committed too late to recover, and counted so the loss is visible).
	e.exec(`UPDATE __S__.event_consumers SET offset_ts = now() - interval '30 minutes' WHERE shard = 6`)
	inner := e.insertAt(6, `now() - interval '33 minutes'`, eventType)
	e.insertAt(6, `now() - interval '37 minutes'`, eventType)
	if _, err := c.Tick(ctx()); err != nil {
		t.Fatal(err)
	}
	if got := e.effects(); !slices.Equal(got, sorted(inner)) {
		t.Fatalf("effects = %v, want only the event inside the window", got)
	}
	if got := testutil.ToFloat64(stats.Unrecovered.WithLabelValues("6")); got != 1 {
		t.Fatalf("unrecovered events on shard 6 = %v, want 1", got)
	}
	if got := testutil.ToFloat64(stats.Unrecovered.WithLabelValues("7")); got != 0 {
		t.Fatalf("unrecovered events on another shard = %v, want 0", got)
	}
}

func TestALateEventWithNoHandlerDoesNotHoldBackTheOthersBesideIt(t *testing.T) {
	e := newEnv(t)
	c := e.newConsumer(e.registry(), everyTick)
	e.exec(`UPDATE __S__.event_consumers SET offset_ts = now() - interval '10 minutes' WHERE shard = 4`)
	e.insertAt(4, `now() - interval '13 minutes'`, "a.new.type") // a type this build does not know
	good := e.insertAt(4, `now() - interval '12 minutes'`, eventType)
	before := testutil.ToFloat64(stats.Errors.WithLabelValues("no_handler"))
	for range 2 {
		if _, err := c.Tick(ctx()); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.effects(); !slices.Equal(got, sorted(good)) {
		t.Fatalf("effects = %v, want the event beside the unknown one to be applied", got)
	}
	if n := e.count(`SELECT count(*) FROM __S__.event_applied WHERE shard = 4`); n != 1 {
		t.Fatalf("applied records = %d, want 1: the unknown event must not be claimed", n)
	}
	if got := testutil.ToFloat64(stats.Errors.WithLabelValues("no_handler")) - before; got < 1 {
		t.Fatalf("no_handler errors +%v, want it counted", got)
	}
}
