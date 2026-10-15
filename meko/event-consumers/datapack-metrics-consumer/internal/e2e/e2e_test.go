// Package e2e runs the real datapack-metrics-consumer binary against a real database and checks what the four
// bookkeeping tables end up holding, through the failures that matter: a process killed mid-run, a standby taking
// over, a malformed event, and a clean shutdown.
//
// The other tests call the consumer in-process; these start it as the Deployment does, so they cover the
// binary's startup, its configuration from the environment, signal handling and /metrics too. They need
// EVENTS_TEST_DSN (a database where a test may create and drop a schema; lib/pq reads its sslmode, so give it
// `sslmode=disable` for a plain local server) and the Go toolchain, which builds the binary once.
package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"meko/event-consumers/datapack-metrics-consumer/internal/testdb"
)

var binary string // built once by TestMain

func TestMain(m *testing.M) {
	code := 1
	defer func() { os.Exit(code) }()
	if os.Getenv("EVENTS_TEST_DSN") != "" {
		dir, err := os.MkdirTemp("", "datapack-metrics-consumer-e2e")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()
		binary = filepath.Join(dir, "datapack-metrics-consumer")
		build := exec.Command("go", "build", "-o", binary, ".")
		build.Dir = "../.."
		if out, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "go build failed: %v\n%s", err, out)
			return
		}
	}
	code = m.Run()
}

// ---- the environment: a schema with every table, and a way to produce events

type env struct {
	t  *testing.T
	db *testdb.DB
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return &env{t: t, db: testdb.New(t, testdb.EventLogSQL, testdb.BookkeepingSQL())}
}

// The events are the payloads the MCP server's producer really stored for a fixed sequence of searches, and
// the golden rows are what the inline bookkeeping wrote for the same searches (testdata/gen_golden.py records
// both). A "round" replays the whole sequence once, so what the tables must hold after n rounds is the golden
// rows n times over: every table, every column, per datapack and day.
type fixture struct {
	Events []map[string]any   `json:"events"`
	Golden map[string][][]any `json:"golden"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	b, err := os.ReadFile("../../testdata/search_completed.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber() // keep the numbers as written
	if err := dec.Decode(&f); err != nil {
		t.Fatal(err)
	}
	if len(f.Events) == 0 || len(f.Golden) == 0 {
		t.Fatal("the fixture holds no events or no golden rows")
	}
	return f
}

// want is what a number of rounds must leave in the tables, by the UTC day the searches happened on.
type want map[string]int

func (w want) plus(o want) want {
	out := want{}
	for _, m := range []want{w, o} {
		for day, n := range m {
			out[day] += n
		}
	}
	return out
}

func (w want) rounds() int {
	n := 0
	for _, r := range w {
		n += r
	}
	return n
}

const userID = "cccccccc-cccc-4ccc-8ccc-ccccccccccc1"

// emit inserts rounds replays of the fixture's events, as the producer would: the shard is the producer's
// hash of the datapack, occurred_at is the search's own time, and created_at is the database's clock a few
// seconds back (past the consumer's safety lag, and increasing, so a later round is never "late").
func (e *env) emit(rounds int) want {
	e.t.Helper()
	w, err := e.emitErr(rounds, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

// emitErr is emit for a goroutine other than the test's: it returns an error instead of calling T.Fatal (which
// is only allowed on the test goroutine), and stops between events once stop is closed.
func (e *env) emitErr(rounds int, stop <-chan struct{}) (want, error) {
	f := loadFixture(e.t)
	w := want{}
	for range rounds {
		at := time.Now().UTC().Add(-6 * time.Second)
		for i, payload := range f.Events {
			select {
			case <-stop:
				return w, nil
			default:
			}
			p := map[string]any{}
			for k, v := range payload {
				p[k] = v
			}
			p["occurred_at"] = at.Format(time.RFC3339Nano)
			body, _ := json.Marshal(p)
			if _, err := e.insertErr(producerShard(payload["datapack_id"], i), string(body)); err != nil {
				return w, err
			}
		}
		w[at.Format("2006-01-02")]++
	}
	return w, nil
}

// producerShard is the MCP server's shard_for: a CRC of the datapack's UUID bytes, or any spread when the
// datapack is not a UUID (it falls back to the event's own id).
func producerShard(datapack any, i int) int {
	if u, err := uuid.Parse(fmt.Sprint(datapack)); err == nil {
		return int(crc32.ChecksumIEEE(u[:]) % 16)
	}
	return i % 16
}

func (e *env) insert(shard int, payload string) string {
	e.t.Helper()
	id, err := e.insertErr(shard, payload)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// insertErr inserts one event and returns the error; it is safe to call from any goroutine.
func (e *env) insertErr(shard int, payload string) (string, error) {
	id := uuid.NewString()
	_, err := e.db.Admin.Exec(e.db.SQL(`INSERT INTO __S__.events (shard, created_at, id, type, payload)
	           VALUES ($1, now() - interval '5 seconds', $2::uuid, 'search.completed', $3::jsonb)`), shard, id, payload)
	return id, err
}

func (e *env) scalar(q string) int64 {
	e.t.Helper()
	var n sql.NullInt64
	if err := e.db.Admin.QueryRow(e.db.SQL(q)).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n.Int64
}

// queries is how many searches have been counted so far.
func (e *env) queries() int64 {
	return e.scalar(`SELECT coalesce(sum(query_count), 0) FROM __S__.datapack_daily_activity`)
}

func (e *env) unread() int64 {
	return e.scalar(`SELECT count(*) FROM __S__.events e JOIN __S__.event_consumers c
	                 ON c.consumer = 'analytics' AND c.shard = e.shard
	                 WHERE (e.created_at, e.id) > (c.offset_ts, c.offset_id)`)
}

// table is a table's rows as a multiset: each row's columns joined, with how many times it appears.
type table map[string]int

func (t table) add(n int, cols ...any) {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = fmt.Sprint(c)
	}
	t[strings.Join(parts, " | ")] += n
}

// expected is what the golden rows say the four tables hold after w, one multiset per table.
func (f fixture) expected(w want) map[string]table {
	out := map[string]table{}
	num := func(v any) int {
		n, err := strconv.Atoi(fmt.Sprint(v))
		if err != nil {
			panic(fmt.Sprintf("golden value %v is not an integer", v))
		}
		return n
	}
	rounds := w.rounds()
	daily := table{}
	for day, r := range w {
		for _, row := range f.Golden["datapack_daily_activity"] {
			daily.add(1, row[0], day, row[2], num(row[3])*r)
		}
	}
	out["datapack_daily_activity"] = daily
	log, summary, audit := table{}, table{}, table{}
	for _, row := range f.Golden["token_savings_log"] {
		log.add(rounds, orNil(row[0]), orNil(row[1]), orNil(row[2]), orNil(row[3]), row[4], row[5], row[6])
	}
	for _, row := range f.Golden["token_savings_summary"] {
		summary.add(1, row[0], row[1], num(row[2])*rounds, num(row[3])*rounds, num(row[4])*rounds,
			num(row[5])*rounds, num(row[6])*rounds, num(row[7])*rounds)
	}
	for _, row := range f.Golden["free_tier_audit_log"] {
		audit.add(rounds, orNil(row[0]), row[1])
	}
	out["token_savings_log"], out["token_savings_summary"], out["free_tier_audit_log"] = log, summary, audit
	return out
}

func orNil(v any) any {
	if v == nil {
		return "<nil>"
	}
	return v
}

// actual reads the four tables, one multiset each, in the same shape as expected.
func (e *env) actual() map[string]table {
	e.t.Helper()
	read := func(q string, cols int) table {
		rows, err := e.db.Admin.Query(e.db.SQL(q))
		if err != nil {
			e.t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		out := table{}
		for rows.Next() {
			vals := make([]any, cols)
			ptrs := make([]any, cols)
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				e.t.Fatal(err)
			}
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					vals[i] = string(b)
				}
			}
			out.add(1, vals...)
		}
		return out
	}
	return map[string]table{
		"datapack_daily_activity": read(`SELECT datapack_id::text, activity_date::text, search_type, query_count
			FROM __S__.datapack_daily_activity`, 4),
		"token_savings_log": read(`SELECT datapack_id::text, coalesce(user_id::text, '<nil>'), conversation_id::text, source,
			tokens_sent, tokens_if_raw, tokens_saved FROM __S__.token_savings_log`, 7),
		"token_savings_summary": read(`SELECT datapack_id::text, source, request_count, tokens_saved_total, tokens_if_raw_total,
			score_high_total, score_mid_total, score_low_total FROM __S__.token_savings_summary`, 8),
		"free_tier_audit_log": read(`SELECT user_id::text, metric_name FROM __S__.free_tier_audit_log`, 2),
	}
}

// check fails unless every table holds exactly what w says, naming the rows that differ.
func (e *env) check(w want) {
	e.t.Helper()
	want, got := loadFixture(e.t).expected(w), e.actual()
	var diffs []string
	for _, name := range []string{"datapack_daily_activity", "token_savings_log", "token_savings_summary", "free_tier_audit_log"} {
		for row, n := range want[name] {
			if got[name][row] != n {
				diffs = append(diffs, fmt.Sprintf("%s: want %d x [%s], got %d", name, n, row, got[name][row]))
			}
		}
		for row, n := range got[name] {
			if _, ok := want[name][row]; !ok {
				diffs = append(diffs, fmt.Sprintf("%s: unexpected %d x [%s]", name, n, row))
			}
		}
	}
	if len(diffs) > 0 {
		slices.Sort(diffs)
		if len(diffs) > 12 {
			diffs = append(diffs[:12], fmt.Sprintf("... and %d more", len(diffs)-12))
		}
		e.t.Fatalf("the tables differ from what the recorded inline rows say after %d rounds:\n  %s", w.rounds(), strings.Join(diffs, "\n  "))
	}
}

// settle waits until every event is read, then fails unless the tables hold exactly w.
func (e *env) settle(w want, procs ...*proc) {
	e.t.Helper()
	waitFor(e.t, 45*time.Second, "every event to be consumed", func() bool { return e.unread() == 0 }, procs...)
	e.check(w)
}

// ---- a running consumer process

type proc struct {
	t    *testing.T
	name string
	cmd  *exec.Cmd
	port int
	logs *syncBuffer
	done chan struct{}
	err  error
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// start runs the binary with a short lease, so a takeover shows up in seconds, and waits for /healthz.
func (e *env) start(name string, extra ...string) *proc {
	e.t.Helper()
	p := &proc{t: e.t, name: name, port: freePort(e.t), logs: &syncBuffer{}, done: make(chan struct{})}
	p.cmd = exec.Command(binary)
	p.cmd.Env = append(os.Environ(),
		"YUGABYTEDB_SYSTEM_URL="+e.db.DSN,
		"MEKO_EVENTS_CONSUMER_SCHEMA="+e.db.Schema,
		"MEKO_EVENTS_CONSUMER_HTTP_PORT="+strconv.Itoa(p.port),
		"MEKO_EVENTS_CONSUMER_LEASE_SECONDS=5",
		"MEKO_EVENTS_CONSUMER_RENEW_SECONDS=1",
		"MEKO_EVENTS_CONSUMER_SAFETY_LAG_SECONDS=0.5",
		"MEKO_EVENTS_CONSUMER_POLL_MIN_MS=20",
		"MEKO_EVENTS_CONSUMER_POLL_MAX_MS=50",
		"MEKO_EVENTS_CONSUMER_BATCH_SIZE=50",
	)
	p.cmd.Env = append(p.cmd.Env, extra...)
	p.cmd.Stdout, p.cmd.Stderr = p.logs, p.logs
	if err := p.cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	e.t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Kill()
			<-p.done
		}
		if e.t.Failed() {
			e.t.Logf("---- %s log ----\n%s", name, p.logs.String())
		}
	})
	waitFor(e.t, 15*time.Second, name+" to answer /healthz", func() bool {
		resp, err := http.Get(p.url("/healthz"))
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == 200
	}, p)
	return p
}

func (p *proc) url(path string) string { return fmt.Sprintf("http://127.0.0.1:%d%s", p.port, path) }

func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// kill is SIGKILL: no handler runs, no lease is released, a batch in flight is cut off.
func (p *proc) kill() {
	p.t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		p.t.Fatal(err)
	}
	<-p.done
}

// term is SIGTERM, which the consumer answers by finishing its batch, releasing its leases and exiting 0.
func (p *proc) term() {
	p.t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		p.t.Fatal(err)
	}
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		p.t.Fatalf("%s did not exit within 15s of SIGTERM", p.name)
	}
}

// metric reads one unlabelled series, or the one whose full name and labels are given, from /metrics.
func (p *proc) metric(series string) float64 {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.url("/metrics"), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(body), "\n") {
		if name, val, ok := strings.Cut(line, " "); ok && name == series {
			f, _ := strconv.ParseFloat(val, 64)
			return f
		}
	}
	p.t.Fatalf("%s has no series %s", p.name, series)
	return 0
}

// waitFor polls cond until it is true. If a process listed in procs dies first, or the time runs out, the test
// fails saying what it was waiting for.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool, procs ...*proc) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		for _, p := range procs {
			if p.exited() {
				t.Fatalf("%s exited (%v) while waiting for %s", p.name, p.err, what)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

// ---- scenarios

func TestTheTablesHoldExactlyWhatTheSearchesProduce(t *testing.T) {
	e := newEnv(t)
	rounds := 20
	w := e.emit(rounds)
	c := e.start("consumer")
	e.settle(w, c)
	if got, want := c.metric(`meko_events_consumer_events_processed_total{type="search.completed"}`),
		float64(rounds*len(loadFixture(t).Events)); got != want {
		t.Fatalf("the consumer reports %v events processed, want %v", got, want)
	}
	if n := e.scalar(`SELECT count(*) FROM __S__.event_dead_letters`); n != 0 {
		t.Fatalf("dead letters = %d", n)
	}
}

func TestKillingTheConsumerMidRunLosesAndDoublesNothing(t *testing.T) {
	e := newEnv(t)
	a := e.start("consumer-a")

	// A steady producer, so the kill lands while there is work in flight and more still arriving. It runs on
	// its own goroutine, so it must never call T.Fatal: it reports through errc. It is stopped and waited for
	// in t.Cleanup (registered before it starts, so it runs before the schema is dropped), whatever ends the
	// test, so it cannot insert into a schema that no longer exists.
	total := want{}
	var mu sync.Mutex
	stop := make(chan struct{})
	finished := make(chan struct{})
	errc := make(chan error, 1)
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() { close(stop) })
		<-finished
	})
	go func() {
		defer close(finished)
		for range 30 {
			w, err := e.emitErr(8, stop)
			mu.Lock()
			total = total.plus(w)
			mu.Unlock()
			if err != nil {
				errc <- err
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()

	// Kill once some events are applied while more are still unread, so the kill is neither before any work
	// nor after all of it. If it never happens in time that is a failure, not a skip.
	waitFor(t, 30*time.Second, "events applied while others are still unread",
		func() bool { return e.queries() >= 100 && e.unread() > 0 }, a)
	a.kill() // the lease is not released and a batch may be cut off mid-transaction
	b := e.start("consumer-b")
	<-finished
	select {
	case err := <-errc:
		t.Fatalf("the producer failed: %v", err)
	default:
	}

	mu.Lock()
	w := total
	mu.Unlock()
	e.settle(w, b)
	if b.metric(`meko_events_consumer_lease_takeovers_total`) == 0 {
		t.Fatal("the second consumer never took over a lease from the killed one")
	}
}

func TestAStandbyTakesOverWhenTheHolderDies(t *testing.T) {
	e := newEnv(t)
	a := e.start("consumer-a")
	waitFor(t, 10*time.Second, "consumer-a to hold every shard", func() bool { return a.metric("meko_events_consumer_shards_held") == 16 }, a)
	b := e.start("consumer-b")

	first := e.emit(8)
	e.settle(first, a, b)
	if held := b.metric("meko_events_consumer_shards_held"); held != 0 {
		t.Fatalf("the standby holds %v shards while the first consumer is alive, want 0", held)
	}

	a.kill()
	second := e.emit(8)
	e.settle(first.plus(second), b)
	if held := b.metric("meko_events_consumer_shards_held"); held != 16 {
		t.Fatalf("after the first consumer died the standby holds %v shards, want 16", held)
	}
	if n := b.metric("meko_events_consumer_lease_takeovers_total"); n != 16 {
		t.Fatalf("lease takeovers = %v, want 16", n)
	}
}

func TestAMalformedEventIsDeadLetteredAndLaterEventsStillProcess(t *testing.T) {
	e := newEnv(t)
	w := e.emit(2)
	bad := e.insert(3, `[1, 2]`) // not a search.completed payload
	after := e.emit(2)

	c := e.start("consumer")
	e.settle(w.plus(after), c)
	var id string
	if err := e.db.Admin.QueryRow(e.db.SQL(`SELECT event_id::text FROM __S__.event_dead_letters`)).Scan(&id); err != nil || id != bad {
		t.Fatalf("dead letter = %q (%v), want the malformed event %s", id, err, bad)
	}
	if n := e.scalar(`SELECT count(*) FROM __S__.event_dead_letters`); n != 1 {
		t.Fatalf("dead letters = %d, want 1", n)
	}
}

func TestSIGTERMFinishesCleanlyAndReleasesTheLeases(t *testing.T) {
	e := newEnv(t)
	w := e.emit(5)
	c := e.start("consumer")
	e.settle(w, c)
	if n := e.scalar(`SELECT count(*) FROM __S__.event_consumers WHERE lease_owner IS NOT NULL`); n != 16 {
		t.Fatalf("leases held = %d, want 16", n)
	}
	c.term()
	if c.err != nil {
		t.Fatalf("exit after SIGTERM: %v", c.err)
	}
	if n := e.scalar(`SELECT count(*) FROM __S__.event_consumers WHERE lease_owner IS NOT NULL`); n != 0 {
		t.Fatalf("leases still held after SIGTERM = %d, want 0", n)
	}
}

// created_at is stamped at INSERT but a row is visible only at COMMIT. This holds one insert open while a
// newer event on the same shard is committed and consumed, so the offset passes the held one, and then commits
// it. The event must still be applied, once, by the look-back.
func TestAnEventThatCommitsAfterTheOffsetPassedItIsStillApplied(t *testing.T) {
	e := newEnv(t)
	search := loadFixture(t).Events[0] // an ordinary search: it writes all four tables
	payload := func() string {
		p := map[string]any{}
		for k, v := range search {
			p[k] = v
		}
		p["occurred_at"] = time.Now().UTC().Add(-6 * time.Second).Format(time.RFC3339Nano)
		b, _ := json.Marshal(p)
		return string(b)
	}
	c := e.start("consumer", "MEKO_EVENTS_CONSUMER_LOOKBACK_INTERVAL_SECONDS=5")

	held, err := e.db.Admin.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := held.Exec(e.db.SQL(`INSERT INTO __S__.events (shard, created_at, id, type, payload)
	        VALUES (3, now() - interval '20 seconds', $1::uuid, 'search.completed', $2::jsonb)`), uuid.NewString(), payload()); err != nil {
		t.Fatal(err)
	}
	e.insert(3, payload()) // newer, committed at once

	waitFor(t, 20*time.Second, "the visible event to be applied and the offset to pass the held one",
		func() bool { return e.queries() == 1 && e.unread() == 0 }, c)

	if err := held.Commit(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 40*time.Second, "the look-back to apply the event that committed late",
		func() bool { return e.queries() == 2 }, c)

	// One more look-back interval: nothing may be applied a second time.
	time.Sleep(7 * time.Second)
	for name, q := range map[string]string{
		"datapack_daily_activity (searches counted)": `SELECT coalesce(sum(query_count), 0) FROM __S__.datapack_daily_activity`,
		"token_savings_log (rows)":                   `SELECT count(*) FROM __S__.token_savings_log`,
		"free_tier_audit_log (rows)":                 `SELECT count(*) FROM __S__.free_tier_audit_log`,
	} {
		if got := e.scalar(q); got != 2 {
			t.Fatalf("%s = %d, want exactly 2", name, got)
		}
	}
	if got := e.scalar(`SELECT coalesce(sum(request_count), 0) FROM __S__.token_savings_summary`); got != 2 {
		t.Fatalf("token_savings_summary counts %d requests, want 2", got)
	}
	if got := c.metric(`meko_events_consumer_late_events_total`); got != 1 {
		t.Fatalf("late events counted = %v, want 1", got)
	}
	if n := e.scalar(`SELECT count(*) FROM __S__.event_dead_letters`); n != 0 {
		t.Fatalf("dead letters = %d", n)
	}
}
