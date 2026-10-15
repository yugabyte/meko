package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"meko/event-consumers/datapack-metrics-consumer/internal/config"
	"meko/event-consumers/datapack-metrics-consumer/internal/consumer"
	"meko/event-consumers/datapack-metrics-consumer/internal/store"
	"meko/event-consumers/datapack-metrics-consumer/internal/testdb"
)

// The row queries match testdata/gen_golden.py's, so the two sides compare like with like.
var tableQueries = map[string]string{
	"datapack_daily_activity": `SELECT datapack_id, CASE WHEN activity_date = $1::date THEN 'TODAY' ELSE activity_date::text END,
		search_type, query_count FROM __S__.datapack_daily_activity ORDER BY 1, 2, 3`,
	"token_savings_log": `SELECT datapack_id, user_id, conversation_id, source, tokens_sent, tokens_if_raw, tokens_saved
		FROM __S__.token_savings_log ORDER BY 1, 3, 2 NULLS FIRST, 4, 5, 6`,
	"token_savings_summary": `SELECT datapack_id, source, request_count, tokens_saved_total, tokens_if_raw_total,
		score_high_total, score_mid_total, score_low_total FROM __S__.token_savings_summary ORDER BY 1, 2`,
	"free_tier_audit_log": `SELECT user_id, metric_name FROM __S__.free_tier_audit_log ORDER BY 1, 2`,
}

// dump returns every table's rows as strings (NULL as "<nil>"), keyed by table name.
func dump(t *testing.T, d *testdb.DB, today string) map[string][][]string {
	t.Helper()
	out := map[string][][]string{}
	for table, q := range tableQueries {
		var args []any
		if strings.Contains(q, "$1") {
			args = append(args, today)
		}
		rows, err := d.Admin.Query(d.SQL(q), args...)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		out[table] = [][]string{}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			row := make([]string, len(cols))
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				row[i] = fmt.Sprint(v)
			}
			out[table] = append(out[table], row)
		}
		_ = rows.Close()
	}
	return out
}

type fixture struct {
	Searches []map[string]any `json:"searches"`
	// Events are the payloads the MCP server's own emit code built for those searches.
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
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // keep 1000000 from becoming 1e+06
	if err := dec.Decode(&f); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestTheHandlerLeavesTheTablesAsTheInlineCodeDid replays, as one batch, the events the MCP server's emit code
// built for the searches testdata/gen_golden.py ran through its inline bookkeeping, and compares every table
// with what that inline code wrote.
func TestTheHandlerLeavesTheTablesAsTheInlineCodeDid(t *testing.T) {
	d := testdb.New(t, testdb.BookkeepingSQL())
	f := loadFixture(t)
	now := time.Now().UTC()
	var events []store.Event
	for _, s := range f.Events {
		// The fixture's occurred_at is a placeholder. The inline code wrote CURRENT_DATE, so the searches
		// must happen "now" for the golden rows to match.
		s["occurred_at"] = now.Format(time.RFC3339Nano)
		b, _ := json.Marshal(s)
		events = append(events, store.Event{CreatedAt: now, ID: uuid.NewString(), Type: SearchCompleted, SchemaVersion: 1, Payload: b})
	}

	db, err := sql.Open("postgres", d.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := NewSearchCompleted(d.Schema)(context.Background(), tx, events); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	got := dump(t, d, now.Format("2006-01-02"))
	for table, want := range f.Golden {
		wantRows := make([][]string, len(want))
		for i, r := range want {
			wantRows[i] = make([]string, len(r))
			for j, v := range r {
				wantRows[i][j] = fmt.Sprint(v)
			}
		}
		if !slices.EqualFunc(got[table], wantRows, slices.Equal[[]string]) {
			t.Errorf("%s differs from the inline code\n got:  %v\n want: %v", table, got[table], wantRows)
		}
	}
}

// ---- through the consumer

func startConsumer(t *testing.T, d *testdb.DB, reg *consumer.Registry) *consumer.Consumer {
	t.Helper()
	db, err := sql.Open("postgres", d.DSN)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	shards := make([]int, 16)
	for i := range shards {
		shards[i] = i
	}
	cfg := config.Config{
		DSN: d.DSN, Schema: d.Schema, Consumer: "analytics", Shards: shards, BatchSize: 500,
		SafetyLag: 3 * time.Second, Lease: 30 * time.Second, Renew: 10 * time.Second,
		PollMin: 10 * time.Millisecond, PollMax: 20 * time.Millisecond, Janitor: time.Hour,
		DeadLetterDays: 14, MaxDeadLettersPerPass: 20,
	}
	return consumer.New(cfg, db, reg, uuid.NewString())
}

func registryFor(t *testing.T, d *testdb.DB) *consumer.Registry {
	t.Helper()
	reg := consumer.NewRegistry()
	if err := Register(reg, d.Schema); err != nil {
		t.Fatal(err)
	}
	return reg
}

func insertEvent(t *testing.T, d *testdb.DB, shard int, secondsAgo float64, body string) {
	t.Helper()
	d.Exec(`INSERT INTO __S__.events (shard, created_at, id, type, payload)
	        VALUES ($1, now() - make_interval(secs => $2::float8), gen_random_uuid(), 'search.completed', $3::jsonb)`,
		shard, secondsAgo, body)
}

func oneSearch(t *testing.T) string {
	t.Helper()
	b, _ := json.Marshal(search(nil))
	return string(b)
}

func count(t *testing.T, d *testdb.DB, q string) int {
	t.Helper()
	var n int
	if err := d.Admin.QueryRow(d.SQL(q)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func allTables() []string {
	return []string{"datapack_daily_activity", "token_savings_log", "token_savings_summary", "free_tier_audit_log"}
}

func TestReplayingABatchAfterARollbackLeavesTheCountersUnchanged(t *testing.T) {
	d := testdb.New(t, testdb.EventLogSQL, testdb.BookkeepingSQL())
	for i := range 5 {
		insertEvent(t, d, 3, float64(100-i), oneSearch(t))
	}
	real := NewSearchCompleted(d.Schema)

	// The handler writes everything, then the database fails before the offset moves (a serialization
	// conflict, which is not the events' fault): the whole batch must roll back.
	failing := consumer.NewRegistry()
	_ = failing.Register(SearchCompleted, func(ctx context.Context, tx *sql.Tx, es []store.Event) error {
		if err := real(ctx, tx, es); err != nil {
			return err
		}
		return &pq.Error{Code: "40001", Message: "could not serialize access"}
	})
	// The failure belongs to the shard, which backs off; Tick itself carries on and returns no error.
	if _, err := startConsumer(t, d, failing).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, table := range allTables() {
		if n := count(t, d, "SELECT count(*) FROM __S__."+table); n != 0 {
			t.Fatalf("%s kept %d rows from a rolled-back batch", table, n)
		}
	}

	// Another instance takes over and applies the same batch once.
	d.Exec(`UPDATE __S__.event_consumers SET lease_expires_at = now() - interval '1 second'`)
	if _, err := startConsumer(t, d, registryFor(t, d)).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, `SELECT query_count FROM __S__.datapack_daily_activity`); n != 5 {
		t.Fatalf("query_count = %d, want 5", n)
	}
	if n := count(t, d, `SELECT request_count FROM __S__.token_savings_summary`); n != 5 {
		t.Fatalf("request_count = %d, want 5", n)
	}
	if n := count(t, d, `SELECT count(*) FROM __S__.token_savings_log`); n != 5 {
		t.Fatalf("log rows = %d, want 5", n)
	}
	if n := count(t, d, `SELECT count(*) FROM __S__.free_tier_audit_log`); n != 5 {
		t.Fatalf("audit rows = %d, want 5", n)
	}
}

func TestAManyEventBatchWithRepeatedKeysIsAppliedWithoutAConflict(t *testing.T) {
	d := testdb.New(t, testdb.EventLogSQL, testdb.BookkeepingSQL())
	for i := range 200 {
		insertEvent(t, d, 3, float64(1000-i), oneSearch(t))
	}
	if _, err := startConsumer(t, d, registryFor(t, d)).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, `SELECT query_count FROM __S__.datapack_daily_activity`); n != 200 {
		t.Fatalf("query_count = %d, want 200", n)
	}
	if n := count(t, d, `SELECT tokens_saved_total FROM __S__.token_savings_summary`); n != 200*11000 {
		t.Fatalf("tokens_saved_total = %d", n)
	}
	if n := count(t, d, `SELECT count(*) FROM __S__.event_dead_letters`); n != 0 {
		t.Fatalf("dead letters = %d", n)
	}
}

func TestAnUnreadableEventIsDeadLetteredAndItsNeighboursStillCount(t *testing.T) {
	d := testdb.New(t, testdb.EventLogSQL, testdb.BookkeepingSQL())
	insertEvent(t, d, 3, 30, oneSearch(t))
	insertEvent(t, d, 3, 20, `[1, 2]`)
	insertEvent(t, d, 3, 10, oneSearch(t))
	if _, err := startConsumer(t, d, registryFor(t, d)).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, `SELECT query_count FROM __S__.datapack_daily_activity`); n != 2 {
		t.Fatalf("query_count = %d, want 2", n)
	}
	if n := count(t, d, `SELECT count(*) FROM __S__.event_dead_letters`); n != 1 {
		t.Fatalf("dead letters = %d, want 1", n)
	}
}

func TestTheHandlerIsRegisteredOnce(t *testing.T) {
	reg := consumer.NewRegistry()
	if err := Register(reg, "meko_system"); err != nil {
		t.Fatal(err)
	}
	if got := reg.Types(); !slices.Equal(got, []string{SearchCompleted}) {
		t.Fatalf("types = %v", got)
	}
	if err := Register(reg, "meko_system"); err == nil {
		t.Fatal("a second registration was accepted")
	}
}

// A datapack with more than about 2.1 million chunks has tokens_if_raw above the INT column's range. The
// inline code lost only the token-savings rows for such a search; losing the whole event would also lose its
// daily count and audit row.
func TestAnOversizedTokenCountDoesNotCostTheDailyCountOrTheAuditRow(t *testing.T) {
	d := testdb.New(t, testdb.BookkeepingSQL())
	now := time.Now().UTC()
	mk := func(over map[string]any) store.Event {
		b, _ := json.Marshal(search(over))
		return store.Event{CreatedAt: now, ID: uuid.NewString(), Type: SearchCompleted, SchemaVersion: 1, Payload: b}
	}
	events := []store.Event{mk(map[string]any{"tokens_if_raw": int64(3_000_000_000)}), mk(nil)}

	db, err := sql.Open("postgres", d.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := NewSearchCompleted(d.Schema)(context.Background(), tx, events); err != nil {
		_ = tx.Rollback()
		t.Fatalf("the batch failed, which would dead-letter both events: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got := dump(t, d, now.Format("2006-01-02"))
	if n := len(got["datapack_daily_activity"]); n != 1 || got["datapack_daily_activity"][0][3] != "2" {
		t.Fatalf("daily = %v, want one row counting both searches", got["datapack_daily_activity"])
	}
	if n := len(got["free_tier_audit_log"]); n != 2 {
		t.Fatalf("audit rows = %d, want 2", n)
	}
	if n := len(got["token_savings_log"]); n != 1 {
		t.Fatalf("token_savings_log rows = %d, want 1 (the search that fits)", n)
	}
}
