package handlers

import (
	"encoding/json"
	"testing"
	"time"

	"meko/event-consumers/datapack-metrics-consumer/internal/consumer"
	"meko/event-consumers/datapack-metrics-consumer/internal/store"
)

const (
	dpA   = "11111111-1111-4111-8111-111111111111"
	conv1 = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1"
	user1 = "cccccccc-cccc-4ccc-8ccc-ccccccccccc1"
)

var noon = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func event(t *testing.T, at time.Time, payload map[string]any) store.Event {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return store.Event{CreatedAt: at, ID: "00000000-0000-4000-8000-000000000001", Type: SearchCompleted, SchemaVersion: 1, Payload: b}
}

func search(over map[string]any) map[string]any {
	m := map[string]any{
		"search_type": "hybrid", "datapack_id": dpA, "conversation_id": conv1, "user_id": user1,
		"tokens_sent": 1000, "tokens_if_raw": 12000, "hits_high": 2, "hits_mid": 1, "hits_low": 0,
		"quota_metric": "context_search", "quota_audited": true,
	}
	for k, v := range over {
		m[k] = v
	}
	return m
}

func mustPlan(t *testing.T, events ...store.Event) *plan {
	t.Helper()
	p, err := planBatch(events)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRepeatedKeysInOneBatchAreCoalescedIntoOneRowEach(t *testing.T) {
	e := event(t, noon, search(nil))
	p := mustPlan(t, e, e, e)
	if len(p.daily) != 1 || p.daily[dailyKey{dpA, "2026-10-08", "hybrid"}] != 3 {
		t.Fatalf("daily = %v", p.daily)
	}
	d := p.summary[summaryKey{dpA, "hybrid"}]
	if len(p.summary) != 1 || d.requests != 3 || d.saved != 33000 || d.ifRaw != 36000 || d.high != 6 || d.mid != 3 || d.low != 0 {
		t.Fatalf("summary = %+v", d)
	}
	if len(p.log) != 3 || len(p.audit) != 3 {
		t.Fatalf("log=%d audit=%d, want one row per event", len(p.log), len(p.audit))
	}
}

func TestAZeroResultSearchCountsAsAQueryAndNothingElse(t *testing.T) {
	p := mustPlan(t, event(t, noon, search(map[string]any{"tokens_sent": 0, "tokens_if_raw": 0, "hits_high": 0, "hits_mid": 0})))
	if p.daily[dailyKey{dpA, "2026-10-08", "hybrid"}] != 1 {
		t.Fatalf("daily = %v", p.daily)
	}
	if len(p.log) != 0 || len(p.summary) != 0 {
		t.Fatalf("token rows written for a search that sent no tokens: %v %v", p.log, p.summary)
	}
	if len(p.audit) != 1 {
		t.Fatalf("audit = %v", p.audit)
	}
}

func TestACallWithoutADatapackIsNeitherCountedNorTracked(t *testing.T) {
	p := mustPlan(t, event(t, noon, search(map[string]any{"datapack_id": ""})))
	if len(p.daily) != 0 || len(p.log) != 0 || len(p.summary) != 0 {
		t.Fatalf("plan = %+v", p)
	}
	if len(p.audit) != 1 {
		t.Fatal("the audit row does not depend on the datapack")
	}
}

func TestAnInvalidConversationKeepsTheCountButDropsTheTokenRows(t *testing.T) {
	p := mustPlan(t, event(t, noon, search(map[string]any{"conversation_id": "not-a-uuid"})))
	if len(p.daily) != 1 || len(p.log) != 0 || len(p.summary) != 0 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestAnInvalidOrMissingUserIsStoredAsNullAndNeverAudited(t *testing.T) {
	for _, user := range []string{"", "bogus"} {
		p := mustPlan(t, event(t, noon, search(map[string]any{"user_id": user})))
		if len(p.log) != 1 || p.log[0].user != "" {
			t.Fatalf("user %q: log = %+v", user, p.log)
		}
		if len(p.audit) != 0 {
			t.Fatalf("user %q: audited without a user", user)
		}
	}
}

func TestNoAuditRowWhenTheInlineCodeWouldNotWriteOne(t *testing.T) {
	p := mustPlan(t, event(t, noon, search(map[string]any{"quota_audited": false})))
	if len(p.audit) != 0 {
		t.Fatalf("audit = %v", p.audit)
	}
}

func TestSendingMoreThanTheBaselineIsLoggedAsIsButNeverSubtractsFromTheSummary(t *testing.T) {
	p := mustPlan(t, event(t, noon, search(map[string]any{"tokens_sent": 500, "tokens_if_raw": 200})))
	if p.log[0].sent != 500 || p.log[0].ifRaw != 200 {
		t.Fatalf("log = %+v", p.log[0])
	}
	if got := p.summary[summaryKey{dpA, "hybrid"}].saved; got != 0 {
		t.Fatalf("summary saved = %d, want 0", got)
	}
}

func TestTheActivityDateIsTheEventsUTCDate(t *testing.T) {
	late := time.Date(2026, 10, 8, 23, 59, 59, 0, time.FixedZone("PDT", -7*3600)) // 06:59:59 UTC the next day
	p := mustPlan(t, event(t, late, search(nil)))
	if p.daily[dailyKey{dpA, "2026-10-09", "hybrid"}] != 1 {
		t.Fatalf("daily = %v", p.daily)
	}
}

func TestUnknownFieldsAndNewerVersionsAreRead(t *testing.T) {
	e := event(t, noon, search(map[string]any{"prompt": "hello", "something_new": []int{1}}))
	e.SchemaVersion = 2
	if p := mustPlan(t, e); len(p.daily) != 1 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestAnUnreadablePayloadIsTheEventsFault(t *testing.T) {
	for _, body := range []string{`[1,2]`, `"text"`, `{"tokens_sent":"many"}`, `not json`} {
		e := event(t, noon, nil)
		e.Payload = json.RawMessage(body)
		if _, err := planBatch([]store.Event{e}); err == nil || !consumer.IsPoison(err) {
			t.Errorf("payload %s: err = %v, poison = %v", body, err, err != nil && consumer.IsPoison(err))
		}
	}
}

func TestTheDayComesFromWhenTheSearchHappenedNotWhenTheRowWasInserted(t *testing.T) {
	// Searched at 23:59:58 UTC, queued by the producer and inserted 6 s later, after midnight.
	inserted := time.Date(2026, 10, 9, 0, 0, 4, 0, time.UTC)
	for name, occurred := range map[string]string{
		"offset form":  "2026-10-08T23:59:58.123456+00:00",
		"zulu form":    "2026-10-08T23:59:58Z",
		"other offset": "2026-10-09T01:59:58+02:00",
	} {
		p := mustPlan(t, event(t, inserted, search(map[string]any{"occurred_at": occurred})))
		if p.daily[dailyKey{dpA, "2026-10-08", "hybrid"}] != 1 || len(p.daily) != 1 {
			t.Fatalf("%s: daily = %v, want the 8th", name, p.daily)
		}
		if len(p.audit) != 1 || p.audit[0].at.Day() != 8 || p.log[0].at.Day() != 8 {
			t.Fatalf("%s: the audit and token-savings rows must carry the search time: %+v %+v", name, p.audit, p.log)
		}
	}
}

func TestWithoutAUsableOccurredAtTheInsertTimeIsUsed(t *testing.T) {
	inserted := time.Date(2026, 10, 9, 0, 0, 4, 0, time.UTC)
	for name, occurred := range map[string]any{
		"absent":               nil,
		"empty":                "",
		"not a time":           "yesterday",
		"in the future (skew)": "2026-10-09T00:00:30Z",
		"a number":             12345,
	} {
		over := map[string]any{}
		if occurred != nil {
			over["occurred_at"] = occurred
		}
		p := mustPlan(t, event(t, inserted, search(over)))
		if p.daily[dailyKey{dpA, "2026-10-09", "hybrid"}] != 1 {
			t.Fatalf("%s: daily = %v, want the 9th", name, p.daily)
		}
	}
}

func TestATokenCountThatDoesNotFitTheIntColumnsLosesOnlyTheTokenRows(t *testing.T) {
	cases := map[string]map[string]any{
		"if_raw above int4": {"tokens_if_raw": int64(3_000_000_000)},
		"sent above int4":   {"tokens_sent": int64(2_147_483_648)},
		"if_raw below int4": {"tokens_if_raw": int64(-2_147_483_649)},
	}
	for name, over := range cases {
		p := mustPlan(t, event(t, noon, search(over)), event(t, noon, search(nil)))
		if p.daily[dailyKey{dpA, "2026-10-08", "hybrid"}] != 2 {
			t.Fatalf("%s: daily = %v, want both searches counted", name, p.daily)
		}
		if len(p.audit) != 2 {
			t.Fatalf("%s: audit rows = %d, want 2", name, len(p.audit))
		}
		if len(p.log) != 1 || p.summary[summaryKey{dpA, "hybrid"}].requests != 1 {
			t.Fatalf("%s: log=%d summary=%+v, want only the search that fits", name, len(p.log), p.summary)
		}
		if p.tokenRowsSkipped != 1 {
			t.Fatalf("%s: skipped = %d, want 1", name, p.tokenRowsSkipped)
		}
	}
}

func TestTheLargestValuesThatFitAreKept(t *testing.T) {
	p := mustPlan(t, event(t, noon, search(map[string]any{"tokens_sent": int64(2_147_483_647), "tokens_if_raw": int64(2_147_483_647)})))
	if len(p.log) != 1 || p.tokenRowsSkipped != 0 {
		t.Fatalf("log=%d skipped=%d, want the row kept at the int4 limit", len(p.log), p.tokenRowsSkipped)
	}
}
