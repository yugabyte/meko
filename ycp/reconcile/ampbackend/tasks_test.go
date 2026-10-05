package ampbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"meko/ycp/internal/ampclient"
	"meko/ycp/internal/api"
)

// taskFeed is a fake amp task log that answers ListTasks the way the controller
// does: filtered by updated_since (inclusive), ordered updated_at ASC, paged.
// Modelling the contract rather than returning canned pages is what lets these
// tests assert the collector's watermark arithmetic instead of its call shape.
type taskFeed struct {
	t *testing.T

	rows []ampclient.Task
	// ordered reports the ordering the fake claims to have applied. Overridden by
	// the version-skew test to impersonate a controller that ignores updated_since.
	ordered *ampclient.TaskListOrderedBy
	// seen records every updated_since the collector sent, so a test can assert the
	// watermark advanced (or did not).
	seen  []time.Time
	calls int
}

func (f *taskFeed) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveCatalog(w, r) {
			return
		}
		if r.URL.Path != "/tasks" {
			f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.calls++
		q := r.URL.Query()
		since := f.parseSince(q)
		f.seen = append(f.seen, since)

		var matched []ampclient.Task
		for _, row := range f.rows {
			if !row.UpdatedAt.Before(since) { // inclusive, as the controller's >= is
				matched = append(matched, row)
			}
		}
		sortByUpdatedAtAsc(matched)

		limit, offset := f.page(q)
		total := len(matched)
		start := min(offset, total)
		end := min(start+limit, total)
		page := matched[start:end]

		ordered := ampclient.UpdatedAtAsc
		if f.ordered != nil {
			ordered = *f.ordered
		}
		body := ampclient.TaskList{
			Tasks: page, Total: total, Count: len(page), OrderedBy: &ordered,
		}
		if f.ordered != nil && *f.ordered == ampclient.CreatedAtDesc {
			// A controller predating updated_since would not echo the field at all.
			body.OrderedBy = nil
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			f.t.Errorf("encode task list: %v", err)
		}
	}))
}

func (f *taskFeed) parseSince(q url.Values) time.Time {
	raw := q.Get("updated_since")
	if raw == "" {
		f.t.Error("collector sent no updated_since; without it the response is not a change feed")
		return time.Time{}
	}
	since, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		f.t.Errorf("updated_since %q: %v", raw, err)
	}
	return since
}

func (f *taskFeed) page(q url.Values) (limit, offset int) {
	limit, offset = feedPageLimit, 0
	if v := q.Get("limit"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	if v := q.Get("offset"); v != "" {
		offset, _ = strconv.Atoi(v)
	}
	return limit, offset
}

func sortByUpdatedAtAsc(ts []ampclient.Task) {
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0; j-- {
			a, b := ts[j-1], ts[j]
			if a.UpdatedAt.After(b.UpdatedAt) ||
				(a.UpdatedAt.Equal(b.UpdatedAt) && a.Id.String() > b.Id.String()) {
				ts[j-1], ts[j] = b, a
				continue
			}
			break
		}
	}
}

// mkTask builds a controller task row. created/updated are offsets from base so a
// test reads as a timeline.
func mkTask(id string, kind string, status ampclient.TaskStatus, created, updated time.Time) ampclient.Task {
	return ampclient.Task{
		Id:        mustUUID(id),
		Kind:      kind,
		Status:    status,
		CreatedAt: created,
		UpdatedAt: updated,
	}
}

func mustUUID(s string) openapi_types.UUID {
	return uuid.MustParse(s)
}

// collectorOn returns a collector pointed at f, with a fixed clock so the backfill
// window is deterministic.
func collectorOn(t *testing.T, f *taskFeed, now time.Time) (*TaskCollector, func()) {
	t.Helper()
	srv := f.serve()
	b, err := New(srv.URL, "", nil, noCtl())
	if err != nil {
		srv.Close()
		t.Fatalf("New: %v", err)
	}
	c := NewTaskCollector(b, func(string, ...any) {})
	c.now = func() time.Time { return now }
	return c, srv.Close
}

// collectOK collects and commits, modelling a tick whose status POST succeeded.
// The commit is the caller's job in production (see TaskCollector.Collect), so a
// test that forgets it would silently be asserting the re-send path instead.
func collectOK(t *testing.T, c *TaskCollector, ctx context.Context) ([]api.TaskStatus, error) {
	t.Helper()
	tasks, commit, err := c.Collect(ctx)
	if err == nil {
		commit()
	}
	return tasks, err
}

func ids(ts []api.TaskStatus) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

// TestCollectReportsTaskThatFinishedBetweenTicks is the regression this whole
// design turns on. A create_branch task that is planned and completes inside one
// reconcile interval is never observed pending or running, so a collector that
// watched only in-flight tasks would never report it at all — not even as
// succeeded, which is the state the console most needs to show.
func TestCollectReportsTaskThatFinishedBetweenTicks(t *testing.T) {
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	f := &taskFeed{t: t}
	c, done := collectorOn(t, f, base)
	defer done()
	ctx := context.Background()

	// Tick one: an empty log.
	if got, err := collectOK(t, c, ctx); err != nil || len(got) != 0 {
		t.Fatalf("first Collect = %v, %v; want no tasks", got, err)
	}

	// Between ticks the task is created AND succeeds. It is never in flight at a
	// moment the collector looks.
	f.rows = append(f.rows, mkTask("11111111-1111-1111-1111-111111111111", "create_branch",
		ampclient.TaskStatusSucceeded, base.Add(time.Second), base.Add(2*time.Second)))

	got, err := collectOK(t, c, ctx)
	if err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("second Collect returned %d tasks, want 1 — a task that finished between ticks was lost", len(got))
	}
	if got[0].Status != "succeeded" || got[0].Kind != "create_branch" {
		t.Errorf("reported %+v, want the succeeded create_branch", got[0])
	}
}

// TestCollectDoesNotResendUnchangedTasks covers the cost of the inclusive bound:
// the boundary row comes back on every sweep, and only the fingerprint stops it
// from being re-reported on a quiet tick.
func TestCollectDoesNotResendUnchangedTasks(t *testing.T) {
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	f := &taskFeed{t: t}
	f.rows = []ampclient.Task{
		mkTask("11111111-1111-1111-1111-111111111111", "create_endpoint",
			ampclient.TaskStatusRunning, base, base),
	}
	c, done := collectorOn(t, f, base.Add(time.Hour))
	defer done()
	ctx := context.Background()

	first, err := collectOK(t, c, ctx)
	if err != nil || len(first) != 1 {
		t.Fatalf("first Collect = %v, %v; want the running task", first, err)
	}

	// Nothing changed. The feed still returns the row (>= is inclusive), and the
	// collector must recognise it.
	second, err := collectOK(t, c, ctx)
	if err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second Collect re-sent %d unchanged tasks; a quiet tick must report none", len(second))
	}

	// The same task moving IS a change, even though its id was already reported.
	f.rows[0].Status = ampclient.TaskStatusFailed
	boom := "endpoint deploy timed out"
	f.rows[0].Error = &boom
	f.rows[0].UpdatedAt = base.Add(time.Minute)

	third, err := collectOK(t, c, ctx)
	if err != nil {
		t.Fatalf("third Collect: %v", err)
	}
	if len(third) != 1 || third[0].Status != "failed" {
		t.Fatalf("third Collect = %v, want the task re-reported as failed", third)
	}
	if third[0].Error != boom {
		t.Errorf("error = %q, want %q — the failure reason is the point of the feed", third[0].Error, boom)
	}
}

// TestCollectReportsSameInstantTwins pins the reason the bound is inclusive rather
// than exclusive: two tasks stamped at the same instant must both be reported, and
// an exclusive bound set to the last row's updated_at would drop the second.
func TestCollectReportsSameInstantTwins(t *testing.T) {
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	stamp := base.Add(time.Minute)
	f := &taskFeed{t: t}
	c, done := collectorOn(t, f, base)
	defer done()
	ctx := context.Background()

	// First sweep sees only the earlier twin, so the watermark lands exactly on the
	// instant the second twin also carries.
	f.rows = []ampclient.Task{
		mkTask("11111111-1111-1111-1111-111111111111", "create_endpoint", ampclient.TaskStatusSucceeded, base, stamp),
	}
	if got, err := collectOK(t, c, ctx); err != nil || len(got) != 1 {
		t.Fatalf("first Collect = %v, %v; want the first twin", got, err)
	}

	f.rows = append(f.rows,
		mkTask("22222222-2222-2222-2222-222222222222", "create_endpoint", ampclient.TaskStatusSucceeded, base, stamp))

	got, err := collectOK(t, c, ctx)
	if err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	if len(got) != 1 || got[0].ID != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("second Collect = %v, want just the same-instant twin", ids(got))
	}
}

// TestCollectBackfillsAWindowNotTheWholeLog covers the first sweep. The feed is
// ascending, so a zero watermark would start at the oldest task in the log; the
// collector starts one window back instead, which is both what a console opening on
// recent history needs and what covers tasks that moved while the agent was down.
func TestCollectBackfillsAWindowNotTheWholeLog(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	ancient := now.Add(-30 * 24 * time.Hour)
	duringRestart := now.Add(-10 * time.Minute)

	f := &taskFeed{t: t}
	f.rows = []ampclient.Task{
		mkTask("11111111-1111-1111-1111-111111111111", "create_cluster", ampclient.TaskStatusSucceeded, ancient, ancient),
		mkTask("22222222-2222-2222-2222-222222222222", "resize_endpoint", ampclient.TaskStatusFailed, duringRestart, duringRestart),
	}
	c, done := collectorOn(t, f, now)
	defer done()

	got, err := collectOK(t, c, context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got) != 1 || got[0].ID != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("backfill = %v, want only the task inside the window", ids(got))
	}
	if len(f.seen) == 0 {
		t.Fatal("no request reached the feed")
	}
	if want := now.Add(-backfillWindow); !f.seen[0].Equal(want) {
		t.Errorf("first updated_since = %s, want %s (now minus the backfill window)", f.seen[0], want)
	}
}

// TestCollectPagesForward covers the multi-page sweep: every row is reported once,
// and offset advances rather than the same page being read repeatedly.
func TestCollectPagesForward(t *testing.T) {
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	f := &taskFeed{t: t}
	// One more than a page, so the loop must fetch twice.
	for i := 0; i < feedPageLimit+1; i++ {
		f.rows = append(f.rows, mkTask(
			fmt.Sprintf("00000000-0000-0000-0000-%012d", i),
			"create_endpoint", ampclient.TaskStatusSucceeded,
			base.Add(time.Duration(i)*time.Millisecond),
			base.Add(time.Duration(i)*time.Millisecond),
		))
	}
	c, done := collectorOn(t, f, base.Add(time.Hour))
	defer done()

	got, err := collectOK(t, c, context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got) != feedPageLimit+1 {
		t.Fatalf("Collect returned %d tasks, want %d — paging dropped rows", len(got), feedPageLimit+1)
	}
	if f.calls != 2 {
		t.Errorf("feed was called %d times, want 2 (one full page then the remainder)", f.calls)
	}
	seen := map[string]bool{}
	for _, ts := range got {
		if seen[ts.ID] {
			t.Fatalf("task %s reported twice in one sweep", ts.ID)
		}
		seen[ts.ID] = true
	}
}

// TestCollectRefusesAFeedTheControllerIgnored is the version-skew guardrail. A
// controller predating updated_since answers with a created_at-ordered page and no
// ordered_by. Advancing a watermark over that would silently skip every task the
// collector had not already seen, so it disables itself loudly instead.
func TestCollectRefusesAFeedTheControllerIgnored(t *testing.T) {
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	old := ampclient.CreatedAtDesc
	f := &taskFeed{t: t, ordered: &old}
	f.rows = []ampclient.Task{
		mkTask("11111111-1111-1111-1111-111111111111", "create_endpoint", ampclient.TaskStatusRunning, base, base),
	}
	c, done := collectorOn(t, f, base.Add(time.Hour))
	defer done()

	var logged int
	c.logf = func(string, ...any) { logged++ }
	ctx := context.Background()

	got, err := collectOK(t, c, ctx)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Collect returned %v; an unhonoured watermark must report nothing rather than a wrongly-ordered page", ids(got))
	}
	if logged == 0 {
		t.Error("disabling the feed was not logged; a silent disable is indistinguishable from an idle environment")
	}

	// And it stays off without hammering the controller every tick.
	before := f.calls
	if got, err := collectOK(t, c, ctx); err != nil || len(got) != 0 {
		t.Fatalf("second Collect = %v, %v; want the feed to stay disabled", got, err)
	}
	if f.calls != before {
		t.Errorf("feed was called again after being disabled (%d -> %d)", before, f.calls)
	}
}

// TestCollectHoldsTheWatermarkOnError: a sweep that fails must not advance past the
// rows it did not read, or those changes are lost for good.
func TestCollectHoldsTheWatermarkOnError(t *testing.T) {
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	var fail bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveCatalog(w, r) {
			return
		}
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"store unreachable"}`))
			return
		}
		ordered := ampclient.UpdatedAtAsc
		body := ampclient.TaskList{
			Tasks: []ampclient.Task{mkTask("11111111-1111-1111-1111-111111111111",
				"create_endpoint", ampclient.TaskStatusSucceeded, base, base.Add(time.Minute))},
			Total: 1, Count: 1, OrderedBy: &ordered,
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	b, err := New(srv.URL, "", nil, noCtl())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := NewTaskCollector(b, func(string, ...any) {})
	c.now = func() time.Time { return base.Add(time.Hour) }
	ctx := context.Background()

	// Prime the probe with a good sweep, then fail and assert the watermark held.
	if _, err := collectOK(t, c, ctx); err != nil {
		t.Fatalf("priming Collect: %v", err)
	}
	held := c.watermark
	fail = true
	if _, _, err := c.Collect(ctx); err == nil {
		t.Fatal("Collect returned nil error on a 503; the caller cannot log what it is not told")
	}
	if !c.watermark.Equal(held) {
		t.Errorf("watermark moved to %s on a failed sweep (was %s); rows behind the error would be skipped",
			c.watermark, held)
	}
}

// TestCollectResendsWhenTheReportIsNotCommitted covers the two-phase contract. A
// delta is delivered once, so if the status POST carrying it fails the collector
// must offer it again — otherwise a transient control-plane outage permanently loses
// every task transition inside it.
func TestCollectResendsWhenTheReportIsNotCommitted(t *testing.T) {
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	f := &taskFeed{t: t}
	f.rows = []ampclient.Task{
		mkTask("11111111-1111-1111-1111-111111111111", "resize_endpoint",
			ampclient.TaskStatusFailed, base, base),
	}
	c, done := collectorOn(t, f, base.Add(time.Hour))
	defer done()
	ctx := context.Background()

	// The tick whose POST fails: collect, then DO NOT commit.
	first, _, err := c.Collect(ctx)
	if err != nil {
		t.Fatalf("first Collect: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first Collect = %v, want the failed resize", ids(first))
	}

	// The next tick must offer it again.
	second, commit, err := c.Collect(ctx)
	if err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	if len(second) != 1 || second[0].ID != first[0].ID {
		t.Fatalf("second Collect = %v, want the uncommitted task re-sent", ids(second))
	}
	commit()

	// And once committed, it stops.
	third, err := collectOK(t, c, ctx)
	if err != nil {
		t.Fatalf("third Collect: %v", err)
	}
	if len(third) != 0 {
		t.Fatalf("third Collect re-sent %v after a committed report", ids(third))
	}
}

// TestCollectCommitsOnlyWhatItRead covers a sweep that fails partway: the first page
// is delivered, the second errors. The commit the caller gets must cover exactly the
// rows returned, so the unread ones are picked up next tick rather than skipped.
func TestCollectCommitsOnlyWhatItRead(t *testing.T) {
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	var page int
	var rows []ampclient.Task
	for i := 0; i < feedPageLimit+1; i++ {
		rows = append(rows, mkTask(
			fmt.Sprintf("00000000-0000-0000-0000-%012d", i),
			"create_endpoint", ampclient.TaskStatusSucceeded,
			base.Add(time.Duration(i)*time.Millisecond),
			base.Add(time.Duration(i)*time.Millisecond),
		))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveCatalog(w, r) {
			return
		}
		page++
		if page == 2 { // the second page of the first sweep
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"store unreachable"}`))
			return
		}
		since, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("updated_since"))
		var matched []ampclient.Task
		for _, row := range rows {
			if !row.UpdatedAt.Before(since) {
				matched = append(matched, row)
			}
		}
		sortByUpdatedAtAsc(matched)
		end := min(feedPageLimit, len(matched))
		ordered := ampclient.UpdatedAtAsc
		_ = json.NewEncoder(w).Encode(ampclient.TaskList{
			Tasks: matched[:end], Total: len(matched), Count: end, OrderedBy: &ordered,
		})
	}))
	defer srv.Close()

	b, err := New(srv.URL, "", nil, noCtl())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := NewTaskCollector(b, func(string, ...any) {})
	c.now = func() time.Time { return base.Add(time.Hour) }
	ctx := context.Background()

	partial, commit, err := c.Collect(ctx)
	if err == nil {
		t.Fatal("Collect returned nil error though the second page 503'd")
	}
	if len(partial) != feedPageLimit {
		t.Fatalf("partial sweep returned %d tasks, want the one page it did read (%d)", len(partial), feedPageLimit)
	}
	// The caller posted this page successfully even though the sweep errored, so it
	// commits — and must not thereby claim the page it never saw.
	commit()

	rest, err := collectOK(t, c, ctx)
	if err != nil {
		t.Fatalf("resumed Collect: %v", err)
	}
	if len(rest) != 1 {
		t.Fatalf("resumed sweep returned %d tasks, want the 1 row behind the error", len(rest))
	}
	want := rows[len(rows)-1].Id.String()
	if rest[0].ID != want {
		t.Errorf("resumed sweep returned %s, want %s", rest[0].ID, want)
	}
}
