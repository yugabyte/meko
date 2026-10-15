package consumer

// These run the consumer against a real PostgreSQL-compatible server when EVENTS_TEST_DSN is set (a
// database where the test may create and drop a schema), and are skipped otherwise. Each test gets a
// throwaway schema with the tables of api_server migration 000029, minus the YugabyteDB-only key syntax,
// so the SQL runs for real: leases, the compare-and-swap, the row-comparison read and the partition
// catalog query.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"meko/event-consumers/datapack-metrics-consumer/internal/config"
	"meko/event-consumers/datapack-metrics-consumer/internal/stats"
	"meko/event-consumers/datapack-metrics-consumer/internal/store"
)

const eventType = "search.completed"

const schemaSQL = `
CREATE SCHEMA __S__;
CREATE TABLE __S__.events (
    shard smallint NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    id uuid NOT NULL, type text NOT NULL, event_schema_version smallint NOT NULL DEFAULT 1, payload jsonb NOT NULL,
    PRIMARY KEY (shard, created_at, id)) PARTITION BY RANGE (created_at);
CREATE TABLE __S__.events_default PARTITION OF __S__.events DEFAULT;
DO $$
DECLARE lo timestamp;
BEGIN
  FOR i IN -2..5 LOOP
    lo := date_trunc('month', now() AT TIME ZONE 'UTC') + make_interval(months => i);
    EXECUTE format('CREATE TABLE __S__.%I PARTITION OF __S__.events FOR VALUES FROM (%L) TO (%L)',
      'events_' || to_char(lo, 'YYYY_MM'), (lo AT TIME ZONE 'UTC')::text,
      ((lo + interval '1 month') AT TIME ZONE 'UTC')::text);
  END LOOP;
END $$;
CREATE TABLE __S__.event_consumers (
    consumer text NOT NULL, shard smallint NOT NULL, offset_ts timestamptz NOT NULL, offset_id uuid NOT NULL,
    lease_owner uuid, lease_expires_at timestamptz, updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, shard));
INSERT INTO __S__.event_consumers (consumer, shard, offset_ts, offset_id)
SELECT 'analytics', s, now() - interval '1 day', '00000000-0000-0000-0000-000000000000'
    FROM generate_series(0, 15) s;
CREATE TABLE __S__.event_dead_letters (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(), consumer text NOT NULL, shard smallint NOT NULL,
    event_id uuid NOT NULL, event_created_at timestamptz NOT NULL, type text NOT NULL,
    event_schema_version smallint NOT NULL, payload jsonb NOT NULL, error text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now());
CREATE INDEX ON __S__.event_dead_letters (created_at);
CREATE TABLE __S__.event_applied (
    consumer text NOT NULL, shard smallint NOT NULL, created_at timestamptz NOT NULL, event_id uuid NOT NULL,
    PRIMARY KEY (shard, consumer, created_at, event_id));
CREATE TABLE __S__.effects (event_id uuid NOT NULL);
`

type env struct {
	t      *testing.T
	dsn    string
	schema string
	admin  *sql.DB // setup and assertions; separate from any consumer's connection
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := os.Getenv("EVENTS_TEST_DSN")
	if dsn == "" {
		t.Skip("EVENTS_TEST_DSN not set")
	}
	admin, err := sql.Open("postgres", dsn)
	must(t, err)
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	e := &env{t: t, dsn: dsn, schema: "_ev_" + hex.EncodeToString(b), admin: admin}
	// Registered before the DDL, so a schema whose setup fails is still dropped.
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP SCHEMA IF EXISTS " + e.schema + " CASCADE")
		_ = admin.Close()
	})
	e.exec(strings.ReplaceAll(schemaSQL, "__S__", e.schema))
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) exec(q string, args ...any) {
	e.t.Helper()
	_, err := e.admin.Exec(strings.ReplaceAll(q, "__S__", e.schema), args...)
	must(e.t, err)
}

func (e *env) cfg(mod ...func(*config.Config)) config.Config {
	c := config.Config{
		DSN: e.dsn, Schema: e.schema, Consumer: "analytics", Shards: slices.Collect(func(yield func(int) bool) {
			for i := range 16 {
				if !yield(i) {
					return
				}
			}
		}),
		BatchSize: 500, SafetyLag: 3 * time.Second, Lease: 30 * time.Second, Renew: 10 * time.Second,
		PollMin: 10 * time.Millisecond, PollMax: 20 * time.Millisecond, Janitor: 5 * time.Second,
		DeadLetterDays: 14, MaxDeadLettersPerPass: 20,
		LookBackWindow: 5 * time.Minute, LookBackEvery: 30 * time.Second, BatchTimeout: 15 * time.Second,
	}
	for _, m := range mod {
		m(&c)
	}
	return c
}

func (e *env) newConsumer(reg *Registry, mod ...func(*config.Config)) *Consumer {
	e.t.Helper()
	db, err := sql.Open("postgres", e.dsn)
	must(e.t, err)
	db.SetMaxOpenConns(1)
	e.t.Cleanup(func() { _ = db.Close() })
	return New(e.cfg(mod...), db, reg, uuid.NewString())
}

// addEvent inserts an event secondsAgo in the past on a shard and returns its id.
func (e *env) addEvent(shard int, secondsAgo float64, payload map[string]any) string {
	e.t.Helper()
	body, _ := json.Marshal(payload)
	id := uuid.NewString()
	e.exec(`INSERT INTO __S__.events (shard, created_at, id, type, payload)
	VALUES ($1, now() - make_interval(secs => $2::float8), $3::uuid, $4, $5::jsonb)`,
		shard, secondsAgo, id, eventType, string(body))
	return id
}

func (e *env) strings(q string, args ...any) []string {
	e.t.Helper()
	rows, err := e.admin.Query(strings.ReplaceAll(q, "__S__", e.schema), args...)
	must(e.t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		must(e.t, rows.Scan(&s))
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func (e *env) effects() []string { return e.strings(`SELECT event_id::text FROM __S__.effects`) }

func (e *env) offsetID(shard int) string {
	e.t.Helper()
	var id string
	must(e.t, e.admin.QueryRow(strings.ReplaceAll(
		`SELECT offset_id::text FROM __S__.event_consumers WHERE shard = $1`, "__S__", e.schema), shard).Scan(&id))
	return id
}

func (e *env) count(q string) int {
	e.t.Helper()
	var n int
	must(e.t, e.admin.QueryRow(strings.ReplaceAll(q, "__S__", e.schema)).Scan(&n))
	return n
}

func (e *env) expireLeases() {
	e.exec(`UPDATE __S__.event_consumers SET lease_expires_at = now() - interval '1 second'`)
}

// applyHandler records one effect per event. A payload with "bad" fails, with "sql" runs that statement
// first, and with "panic" panics.
func (e *env) applyHandler() Handler {
	return func(ctx context.Context, tx *sql.Tx, events []store.Event) error {
		for _, ev := range events {
			var p map[string]any
			_ = json.Unmarshal(ev.Payload, &p)
			if p["bad"] == true {
				return errors.New("cannot apply this event")
			}
			if p["panic"] == true {
				panic("index out of range")
			}
			if q, ok := p["sql"].(string); ok {
				if _, err := tx.ExecContext(ctx, q); err != nil {
					return err
				}
			}
			insert := strings.ReplaceAll(`INSERT INTO __S__.effects VALUES ($1::uuid)`, "__S__", e.schema)
			if _, err := tx.ExecContext(ctx, insert, ev.ID); err != nil {
				return err
			}
		}
		return nil
	}
}

func (e *env) registry() *Registry {
	r := NewRegistry()
	must(e.t, r.Register(eventType, e.applyHandler()))
	return r
}

func ctx() context.Context { return context.Background() }

func sorted(ids ...string) []string { s := slices.Clone(ids); slices.Sort(s); return s }

// ---------------------------------------------------------------- exactly once

func TestABatchIsAppliedOnceAndTheOffsetMovesToItsLastEvent(t *testing.T) {
	e := newEnv(t)
	var ids []string
	for i := range 5 {
		ids = append(ids, e.addEvent(3, float64(100-i), nil))
	}
	c := e.newConsumer(e.registry())
	busy, err := c.Tick(ctx())
	must(t, err)
	if !busy {
		t.Fatal("the first tick should have consumed events")
	}
	if got := e.effects(); !slices.Equal(got, sorted(ids...)) {
		t.Fatalf("effects = %v", got)
	}
	if got := e.offsetID(3); got != ids[4] {
		t.Fatalf("offset = %s, want %s", got, ids[4])
	}
	if busy, err = c.Tick(ctx()); err != nil || busy {
		t.Fatalf("second tick: busy=%v err=%v", busy, err)
	}
	if len(e.effects()) != 5 {
		t.Fatalf("effects = %v", e.effects())
	}
}

func TestACrashBetweenTheHandlerAndTheOffsetAppliesNothingThenEverythingOnce(t *testing.T) {
	e := newEnv(t)
	var ids []string
	for i := range 4 {
		ids = append(ids, e.addEvent(3, float64(100-i), nil))
	}
	before := e.offsetID(3)
	// A trigger that fails the offset update stands in for a crash after the handler ran.
	e.exec(`CREATE FUNCTION __S__.fail_offset() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN IF NEW.offset_ts IS DISTINCT FROM OLD.offset_ts THEN
	RAISE EXCEPTION 'simulated crash'; END IF; RETURN NEW; END $$`)
	e.exec(`CREATE TRIGGER fail_offset BEFORE UPDATE ON __S__.event_consumers
	        FOR EACH ROW EXECUTE FUNCTION __S__.fail_offset()`)

	crashed := e.newConsumer(e.registry())
	if _, err := crashed.Tick(ctx()); err != nil {
		t.Fatal(err)
	}
	if _, failing := crashed.failing[3]; !failing {
		t.Fatal("the crash should mark the shard as failing")
	}
	if len(e.effects()) != 0 || e.offsetID(3) != before {
		t.Fatalf("a failed batch left effects %v or moved the offset", e.effects())
	}
	if n := e.count(`SELECT count(*) FROM __S__.event_dead_letters`); n != 0 {
		t.Fatalf("an environment error was dead-lettered (%d)", n)
	}

	// The crashed instance's leases run out; a replacement takes over and applies each event once.
	e.exec(`DROP TRIGGER fail_offset ON __S__.event_consumers`)
	e.expireLeases()
	_, err := e.newConsumer(e.registry()).Tick(ctx())
	must(t, err)
	if got := e.effects(); !slices.Equal(got, sorted(ids...)) {
		t.Fatalf("effects = %v", got)
	}
}

func TestACompetingAdvanceRollsTheWholeBatchBack(t *testing.T) {
	e := newEnv(t)
	e.addEvent(3, 50, nil)
	reg := NewRegistry()
	inner := e.applyHandler()
	must(t, reg.Register(eventType, func(ctx context.Context, tx *sql.Tx, evs []store.Event) error {
		if err := inner(ctx, tx, evs); err != nil {
			return err
		}
		// another instance commits an advance while this one is mid-batch
		_, err := e.admin.ExecContext(ctx, strings.ReplaceAll(
			`UPDATE __S__.event_consumers SET offset_ts = offset_ts + interval '1 microsecond'
			 WHERE shard = 3`, "__S__", e.schema))
		return err
	}))
	before := testutil.ToFloat64(stats.OffsetConflicts)
	busy, err := e.newConsumer(reg).Tick(ctx())
	must(t, err)
	if busy {
		t.Fatal("a rolled-back batch is not consumption")
	}
	if len(e.effects()) != 0 {
		t.Fatalf("effects survived the rollback: %v", e.effects())
	}
	if got := testutil.ToFloat64(stats.OffsetConflicts) - before; got != 1 {
		t.Fatalf("offset conflicts +%v, want +1", got)
	}
}

func TestEventsInsideTheSafetyLagAreNotReadUntilItPasses(t *testing.T) {
	e := newEnv(t)
	e.addEvent(3, 0, nil)
	c := e.newConsumer(e.registry(), func(c *config.Config) { c.SafetyLag = time.Second })
	before := e.offsetID(3)
	if busy, err := c.Tick(ctx()); err != nil || busy {
		t.Fatalf("busy=%v err=%v", busy, err)
	}
	if e.offsetID(3) != before {
		t.Fatal("the offset passed an event inside the safety lag")
	}
	time.Sleep(1200 * time.Millisecond)
	if busy, err := c.Tick(ctx()); err != nil || !busy {
		t.Fatalf("busy=%v err=%v", busy, err)
	}
	if len(e.effects()) != 1 {
		t.Fatalf("effects = %v", e.effects())
	}
}

func TestEventsAreReadInOrderAcrossBatches(t *testing.T) {
	e := newEnv(t)
	var ids []string
	for i := range 7 {
		ids = append(ids, e.addEvent(3, float64(500-i), nil))
	}
	var order []string
	reg := NewRegistry()
	must(t, reg.Register(eventType, func(_ context.Context, _ *sql.Tx, evs []store.Event) error {
		for _, ev := range evs {
			order = append(order, ev.ID)
		}
		return nil
	}))
	c := e.newConsumer(reg, func(c *config.Config) { c.BatchSize = 3 })
	for {
		busy, err := c.Tick(ctx())
		must(t, err)
		if !busy {
			break
		}
	}
	if !slices.Equal(order, ids) {
		t.Fatalf("order = %v, want %v", order, ids)
	}
}

// ---------------------------------------------------------------- poison and environment errors

func TestAPoisonEventIsDeadLetteredAndTheShardContinues(t *testing.T) {
	e := newEnv(t)
	ok1 := e.addEvent(3, 30, nil)
	bad := e.addEvent(3, 20, map[string]any{"bad": true})
	ok2 := e.addEvent(3, 10, nil)
	before := testutil.ToFloat64(stats.DeadLetters.WithLabelValues(eventType))

	_, err := e.newConsumer(e.registry()).Tick(ctx())
	must(t, err)

	if got := e.effects(); !slices.Equal(got, sorted(ok1, ok2)) {
		t.Fatalf("effects = %v", got)
	}
	dl := e.strings(`SELECT event_id::text || ' ' || type || ' ' || error FROM __S__.event_dead_letters`)
	if len(dl) != 1 || !strings.HasPrefix(dl[0], bad+" "+eventType+" ") ||
		!strings.Contains(dl[0], "cannot apply this event") {
		t.Fatalf("dead letters = %v", dl)
	}
	// A replay needs the event as it was: its own created_at (the day it counts toward) and its payload version.
	if n := e.count(`SELECT count(*) FROM __S__.event_dead_letters d JOIN __S__.events ev ON ev.id = d.event_id
	                 WHERE d.event_created_at = ev.created_at AND d.event_schema_version = ev.event_schema_version
	                   AND d.created_at > ev.created_at`); n != 1 {
		t.Fatalf("the dead letter does not keep the event's created_at and event_schema_version (%d matching)", n)
	}
	if e.offsetID(3) != ok2 {
		t.Fatalf("offset = %s, want %s", e.offsetID(3), ok2)
	}
	if got := testutil.ToFloat64(stats.DeadLetters.WithLabelValues(eventType)) - before; got != 1 {
		t.Fatalf("dead letters metric +%v", got)
	}
}

func TestBadDataInAHandlerStatementIsPoison(t *testing.T) {
	e := newEnv(t)
	e.addEvent(3, 20, map[string]any{"sql": "SELECT 'x'::int"})
	ok := e.addEvent(3, 10, nil)
	_, err := e.newConsumer(e.registry()).Tick(ctx())
	must(t, err)
	if got := e.effects(); !slices.Equal(got, []string{ok}) {
		t.Fatalf("effects = %v", got)
	}
	if n := e.count(`SELECT count(*) FROM __S__.event_dead_letters`); n != 1 {
		t.Fatalf("dead letters = %d", n)
	}
}

func TestAHandlerPanicIsDeadLetteredNotACrash(t *testing.T) {
	e := newEnv(t)
	e.addEvent(3, 20, map[string]any{"panic": true})
	ok := e.addEvent(3, 10, nil)
	_, err := e.newConsumer(e.registry()).Tick(ctx())
	must(t, err)
	if got := e.effects(); !slices.Equal(got, []string{ok}) {
		t.Fatalf("effects = %v", got)
	}
	if n := e.count(`SELECT count(*) FROM __S__.event_dead_letters WHERE error LIKE '%handler panic%'`); n != 1 {
		t.Fatalf("dead letters = %d", n)
	}
}

func TestAMissingTableIsTheEnvironmentNotTheEvent(t *testing.T) {
	e := newEnv(t)
	e.addEvent(3, 20, map[string]any{"sql": "SELECT 1 FROM no_such_table_anywhere"})
	before := e.offsetID(3)
	c := e.newConsumer(e.registry())
	if _, err := c.Tick(ctx()); err != nil {
		t.Fatal(err)
	}
	if _, failing := c.failing[3]; !failing {
		t.Fatal("the shard should be marked failing")
	}
	if e.count(`SELECT count(*) FROM __S__.event_dead_letters`) != 0 || e.offsetID(3) != before {
		t.Fatal("the event was dead-lettered or the offset moved")
	}
}

func TestARunOfPoisonPausesTheShardInsteadOfEmptyingTheQueue(t *testing.T) {
	e := newEnv(t)
	for i := range 6 {
		e.addEvent(3, float64(60-i), map[string]any{"bad": true})
	}
	c := e.newConsumer(e.registry(), func(c *config.Config) { c.MaxDeadLettersPerPass = 2 })
	_, err := c.Tick(ctx())
	must(t, err)
	if n := e.count(`SELECT count(*) FROM __S__.event_dead_letters`); n != 2 {
		t.Fatalf("dead letters = %d, want 2", n)
	}
	if _, paused := c.pausedUntil[3]; !paused {
		t.Fatal("the shard was not paused")
	}
	_, err = c.Tick(ctx())
	must(t, err)
	if n := e.count(`SELECT count(*) FROM __S__.event_dead_letters`); n != 2 {
		t.Fatalf("a paused shard kept dead-lettering: %d", n)
	}
}

func TestAnEventWithNoHandlerHoldsTheShardAndLosesNothing(t *testing.T) {
	e := newEnv(t)
	start := e.offsetID(3)
	id := e.addEvent(3, 10, nil)
	before := testutil.ToFloat64(stats.Errors.WithLabelValues("no_handler"))
	c := e.newConsumer(NewRegistry()) // an older consumer than its producer
	_, err := c.Tick(ctx())
	must(t, err)
	if e.offsetID(3) != start {
		t.Fatal("the offset moved past an event nothing could handle")
	}
	if e.count(`SELECT count(*) FROM __S__.event_dead_letters`) != 0 {
		t.Fatal("an unknown type was dead-lettered")
	}
	if got := testutil.ToFloat64(stats.Errors.WithLabelValues("no_handler")) - before; got != 1 {
		t.Fatalf("no_handler errors +%v, want 1", got)
	}
	// The consumer that knows the type picks it up: nothing was lost.
	e.expireLeases()
	_, err = e.newConsumer(e.registry()).Tick(ctx())
	must(t, err)
	if got := e.effects(); !slices.Equal(got, sorted(id)) {
		t.Fatalf("effects = %v", got)
	}
}

func TestOneFailingShardDoesNotStopTheOthersOrTheJanitor(t *testing.T) {
	e := newEnv(t)
	e.exec(`INSERT INTO __S__.events (shard, created_at, id, type, payload)
	        VALUES (2, now() - interval '30 seconds', gen_random_uuid(), 'a.new.type', '{}')`)
	ids := []string{e.addEvent(5, 10, nil), e.addEvent(9, 10, nil), e.addEvent(14, 10, nil)}
	c := e.newConsumer(e.registry(), func(c *config.Config) { c.Janitor = time.Millisecond })
	before := testutil.ToFloat64(stats.Errors.WithLabelValues("no_handler"))

	busy, err := c.Tick(ctx())
	must(t, err)
	if !busy {
		t.Fatal("the other shards should have been consumed")
	}
	if got := e.effects(); !slices.Equal(got, sorted(ids...)) {
		t.Fatalf("effects = %v: shards after the failing one must still run", got)
	}
	if testutil.ToFloat64(stats.Lag.WithLabelValues("2")) < 20 {
		t.Fatal("the janitor did not run, so the lag gauge for the stuck shard was not updated")
	}
	// Shard 2 is left alone until its backoff ends, so it is not retried (and not counted) every tick.
	for range 3 {
		_, err = c.Tick(ctx())
		must(t, err)
	}
	if got := testutil.ToFloat64(stats.Errors.WithLabelValues("no_handler")) - before; got != 1 {
		t.Fatalf("no_handler errors +%v, want 1: the failing shard was retried inside its backoff", got)
	}
}

func TestAShardRecoversAndForgetsItsFailure(t *testing.T) {
	e := newEnv(t)
	e.exec(`INSERT INTO __S__.events (shard, created_at, id, type, payload)
	        VALUES (2, now() - interval '30 seconds', gen_random_uuid(), 'a.new.type', '{}')`)
	c := e.newConsumer(NewRegistry())
	_, err := c.Tick(ctx())
	must(t, err)
	if _, failing := c.failing[2]; !failing {
		t.Fatal("shard 2 should be failing")
	}
	e.exec(`DELETE FROM __S__.events WHERE type = 'a.new.type'`)
	c.failing[2] = shardFailure{attempts: 1} // its backoff has ended
	_, err = c.Tick(ctx())
	must(t, err)
	if _, failing := c.failing[2]; failing {
		t.Fatal("a shard that went through cleanly is still marked failing")
	}
}

func TestTheShardBackoffDoublesUpToTheCap(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if got := shardBackoff(i + 1); got != w {
			t.Errorf("shardBackoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

// ---------------------------------------------------------------- leases

func (e *env) holders() map[string]int {
	rows, err := e.admin.Query(strings.ReplaceAll(
		`SELECT coalesce(lease_owner::text, ''), count(*) FROM __S__.event_consumers GROUP BY 1`,
		"__S__", e.schema))
	must(e.t, err)
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var h string
		var n int
		must(e.t, rows.Scan(&h, &n))
		out[h] = n
	}
	return out
}

func TestASecondInstanceCannotTakeALiveLeaseButTakesAnExpiredOne(t *testing.T) {
	e := newEnv(t)
	a, b := e.newConsumer(e.registry()), e.newConsumer(e.registry())
	_, err := a.Tick(ctx())
	must(t, err)
	_, err = b.Tick(ctx())
	must(t, err)
	if len(a.owned) != 16 || len(b.owned) != 0 {
		t.Fatalf("owned a=%d b=%d", len(a.owned), len(b.owned))
	}
	if h := e.holders(); h[a.id] != 16 {
		t.Fatalf("holders = %v", h)
	}

	e.expireLeases()
	before := testutil.ToFloat64(stats.LeaseTakeovers)
	b.lastRenew = time.Time{}
	_, err = b.Tick(ctx())
	must(t, err)
	if len(b.owned) != 16 || e.holders()[b.id] != 16 {
		t.Fatalf("b owns %d, holders %v", len(b.owned), e.holders())
	}
	if got := testutil.ToFloat64(stats.LeaseTakeovers) - before; got != 16 {
		t.Fatalf("takeovers +%v, want +16", got)
	}
}

func TestAnInstanceHoldsNoMoreThanMaxShards(t *testing.T) {
	e := newEnv(t)
	c := e.newConsumer(e.registry(), func(c *config.Config) { c.MaxShards = 4 })
	_, err := c.Tick(ctx())
	must(t, err)
	if len(c.owned) != 4 {
		t.Fatalf("owned %d", len(c.owned))
	}
	c.lastRenew = time.Time{}
	_, err = c.Tick(ctx())
	must(t, err)
	if len(c.owned) != 4 {
		t.Fatalf("owned %d after renewing", len(c.owned))
	}
}

func TestShutdownFinishesTheWorkAndReleasesTheLeases(t *testing.T) {
	e := newEnv(t)
	var ids []string
	for i := range 3 {
		ids = append(ids, e.addEvent(3, float64(60-i), nil))
	}
	c := e.newConsumer(e.registry())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); c.Run(ctx()) }()

	deadline := time.Now().Add(10 * time.Second)
	for len(e.effects()) < 3 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	c.Stop()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
	if got := e.effects(); !slices.Equal(got, sorted(ids...)) {
		t.Fatalf("effects = %v", got)
	}
	if h := e.holders(); h[""] != 16 || len(h) != 1 {
		t.Fatalf("leases not released: %v", h)
	}
}

// ---------------------------------------------------------------- janitor

func TestTheJanitorReportsLagRunwayAndDefaultRowsAndTrimsOldDeadLetters(t *testing.T) {
	e := newEnv(t)
	e.addEvent(5, 600, nil)
	e.addEvent(5, 300, nil)
	c := e.newConsumer(NewRegistry())
	must(t, c.janitor(ctx()))

	if lag := testutil.ToFloat64(stats.Lag.WithLabelValues("5")); lag < 590 || lag > 700 {
		t.Fatalf("lag(5) = %v", lag)
	}
	if lag := testutil.ToFloat64(stats.Lag.WithLabelValues("6")); lag != 0 {
		t.Fatalf("lag(6) = %v", lag)
	}
	if r := testutil.ToFloat64(stats.RunwayMonths); r < 4.9 || r > 6.2 {
		t.Fatalf("runway = %v months", r)
	}
	if n := testutil.ToFloat64(stats.DefaultRows); n != 0 {
		t.Fatalf("default rows = %v", n)
	}

	e.exec(`INSERT INTO __S__.events (shard, created_at, id, type, payload)
	        VALUES (1, '2099-01-01', gen_random_uuid(), 't', '{}')`)
	e.exec(`INSERT INTO __S__.event_dead_letters
	        (consumer, shard, event_id, event_created_at, type, event_schema_version, payload, error, created_at) VALUES
	('analytics', 1, gen_random_uuid(), now() - interval '20 days', 't', 1, '{}', 'old',
	 now() - interval '20 days'),
	('analytics', 1, gen_random_uuid(), now() - interval '2 days', 't', 1, '{}', 'new',
	 now() - interval '2 days')`)
	must(t, c.janitor(ctx()))
	if n := testutil.ToFloat64(stats.DefaultRows); n != 1 {
		t.Fatalf("default rows = %v, want 1", n)
	}
	if got := e.strings(`SELECT error FROM __S__.event_dead_letters`); !slices.Equal(got, []string{"new"}) {
		t.Fatalf("dead letters = %v", got)
	}
}

func TestTheJanitorNeverDeletesEvents(t *testing.T) {
	e := newEnv(t)
	e.addEvent(3, 60*60*24*40, nil)
	must(t, e.newConsumer(NewRegistry()).janitor(ctx()))
	if n := e.count(`SELECT count(*) FROM __S__.events`); n != 1 {
		t.Fatalf("events = %d", n)
	}
}

func TestARepeatedPoisonTripPausesTheShardLongerEachTime(t *testing.T) {
	e := newEnv(t)
	for i := range 8 {
		e.addEvent(3, float64(60-i), map[string]any{"bad": true})
	}
	c := e.newConsumer(e.registry(), func(c *config.Config) { c.MaxDeadLettersPerPass = 2 })
	for trip, atLeast := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		c.pausedUntil[3] = time.Time{} // the previous pause is over
		if _, err := c.Tick(ctx()); err != nil {
			t.Fatal(err)
		}
		if got := c.poisonTrips[3]; got != trip+1 {
			t.Fatalf("trip %d: poison trips = %d", trip+1, got)
		}
		if left := time.Until(c.pausedUntil[3]); left < atLeast-5*time.Second {
			t.Fatalf("trip %d: paused for %v, want about %v", trip+1, left.Round(time.Second), atLeast)
		}
	}
}

func TestThePoisonPauseDoublesUpToAnHour(t *testing.T) {
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute,
		32 * time.Minute, time.Hour, time.Hour}
	for i, w := range want {
		if got := pauseBackoff(i + 1); got != w {
			t.Errorf("pauseBackoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

func TestAnInstanceThatDoesNotHoldShardZeroDoesNotServeTheJanitorsGauges(t *testing.T) {
	e := newEnv(t)
	// This instance only wants shards 1 and 2, so it never holds shard 0: whatever the janitor gauges said
	// (it held shard 0 a moment ago, or this is a standby) must be zero, not frozen.
	c := e.newConsumer(e.registry(), func(c *config.Config) { c.Shards = []int{1, 2} })
	stats.Lag.WithLabelValues("3").Set(99)
	stats.RunwayMonths.Set(5)
	stats.DefaultRows.Set(7)
	stats.Unrecovered.WithLabelValues("3").Set(4)
	if _, err := c.Tick(ctx()); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(stats.Lag.WithLabelValues("3")); got != 0 {
		t.Fatalf("lag gauge = %v, want 0", got)
	}
	if testutil.ToFloat64(stats.RunwayMonths) != 0 || testutil.ToFloat64(stats.DefaultRows) != 0 {
		t.Fatal("runway and default-rows gauges must be zero on an instance without shard 0")
	}
	if got := testutil.ToFloat64(stats.Unrecovered.WithLabelValues("3")); got != 0 {
		t.Fatalf("unrecovered gauge for a shard this instance does not hold = %v, want 0", got)
	}
}

func TestTheServerTimeZoneCanBeRead(t *testing.T) {
	e := newEnv(t)
	tz, err := store.ServerTimeZone(ctx(), e.admin)
	if err != nil || tz == "" {
		t.Fatalf("time zone = %q, err = %v", tz, err)
	}
}
