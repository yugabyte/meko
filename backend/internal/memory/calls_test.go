package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/store"
)

func TestCallLogRecordsAndPrunes(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "meko.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	svc := New(st, nil, Options{})
	old := store.Call{At: time.Now().Add(-CallRetention - time.Hour), Tool: "memory_add", OK: true}
	recent := store.Call{At: time.Now(), Tool: "context_search", OK: true}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	svc.RecordCall(canceled, old) // recorded even when the request is already over
	svc.RecordCall(ctx, recent)

	run, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.RunCallPruner(run)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		calls, err := st.Calls(ctx, "", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) == 1 && calls[0].Tool == "context_search" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not pruned: %+v", calls)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	<-done
}

func TestCallUsage(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "meko.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err = st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	svc := New(st, nil, Options{})
	now := time.Now()
	for i, c := range []store.Call{
		{Tool: "context_search", AgentID: "claude_code:a", OK: true, Ms: 100,
			Steps: []store.Step{{Step: "memory_search", Ms: 40}, {Step: "knowledge_base_search", Ms: 90}}},
		{Tool: "context_search", AgentID: "claude_code:a", OK: true, Ms: 300,
			Steps: []store.Step{{Step: "memory_search", Ms: 60}}},
		{Tool: "memory_add", AgentID: "claude_code:b", OK: false, ErrorCode: "memory_add_failed", Ms: 5},
		{Tool: "context_search", AgentID: WebUIAgent, OK: true, Ms: 200},
		{Tool: "context_search", AgentID: "claude_code:a", DatapackID: "other", OK: true, Ms: 1},
	} {
		if c.DatapackID == "" {
			c.DatapackID = "dp"
		}
		c.At = now.Add(time.Duration(i) * time.Second)
		if err = st.AddCall(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	u, err := svc.CallUsage(ctx, "dp", 0)
	if err != nil {
		t.Fatal(err)
	}
	if u.Calls != 4 || u.Errors != 1 || len(u.Recent) != 4 || u.Recent[0].AgentID != WebUIAgent {
		t.Fatalf("usage: %+v", u)
	}
	if cs := u.Tools[0]; cs.Tool != "context_search" || cs.Calls != 3 || cs.P50Ms != 200 || cs.P95Ms != 300 {
		t.Errorf("context_search: %+v", cs)
	}
	if ma := u.Tools[1]; ma.Tool != "memory_add" || ma.Errors != 1 {
		t.Errorf("memory_add: %+v", ma)
	}
	if len(u.Steps) != 2 || u.Steps[1].Step != "memory_search" || u.Steps[1].Calls != 2 || u.Steps[1].P95Ms != 60 {
		t.Errorf("steps: %+v", u.Steps)
	}
	if len(u.Agents) != 3 || u.Agents[0].AgentID != WebUIAgent || u.Agents[2].AgentID != "claude_code:a" ||
		u.Agents[2].Searches != 2 {
		t.Errorf("agents: %+v", u.Agents)
	}
	if u.Recent[0].ID == 0 || len(u.Recent[3].Steps) != 2 {
		t.Errorf("recent calls carry their id and steps: %+v", u.Recent)
	}
	// The UI is the user, not an agent: its searches count, it doesn't.
	cs, err := svc.CallStats(ctx, "dp", 0)
	if err != nil {
		t.Fatal(err)
	}
	if cs.Agents != 2 || cs.Requests != 3 {
		t.Errorf("stats: %d agents, %d requests; want 2 and 3", cs.Agents, cs.Requests)
	}
}

func TestPercentile(t *testing.T) {
	for _, tc := range []struct {
		ms   []int64
		p    int
		want int64
	}{
		{nil, 50, 0},
		{[]int64{7}, 95, 7},
		{[]int64{4, 1, 3, 2}, 50, 2},
		{[]int64{4, 1, 3, 2}, 95, 4},
		{[]int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}, 95, 100},
		{[]int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}, 50, 50},
	} {
		if got := percentile(tc.ms, tc.p); got != tc.want {
			t.Errorf("percentile(%v, %d) = %d, want %d", tc.ms, tc.p, got, tc.want)
		}
	}
}

// days narrows the stats and usage to a window; 0 is the whole retention.
func TestCallWindow(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "meko.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err = st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	svc := New(st, nil, Options{})
	for _, ago := range []int{1, 10, 60} {
		c := store.Call{At: time.Now().AddDate(0, 0, -ago), Tool: "context_search", DatapackID: "dp", OK: true}
		if err = st.AddCall(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	for days, want := range map[int]int{0: 3, 7: 1, 30: 2, 90: 3, 365: 3} {
		cs, err := svc.CallStats(ctx, "dp", days)
		if err != nil {
			t.Fatal(err)
		}
		u, err := svc.CallUsage(ctx, "dp", days)
		if err != nil {
			t.Fatal(err)
		}
		if cs.Requests != want || u.Calls != want {
			t.Errorf("days %d: %d searches, %d calls; want %d", days, cs.Requests, u.Calls, want)
		}
	}
	// The default keeps 30 days of activity, Cloud's chart; a window keeps its own.
	if cs, _ := svc.CallStats(ctx, "dp", 0); len(cs.Daily) != 2 {
		t.Errorf("default activity: %+v", cs.Daily)
	}
	if cs, _ := svc.CallStats(ctx, "dp", 90); len(cs.Daily) != 3 {
		t.Errorf("90-day activity: %+v", cs.Daily)
	}
}

// Context efficiency and quality count only context_search calls that sent something, as Meko Cloud
// does. The docs' example: two searches against a 14,000-token baseline send 1,600 and 400 and save
// 26,000 of 28,000 (93%). A call recorded without a baseline counts 10,000, its saving against it.
func TestContextStats(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "meko.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err = st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	svc := New(st, nil, Options{})
	now := time.Now()
	for _, c := range []store.Call{
		{Tool: "context_search", OK: true, TokensSent: 1600, TokensSaved: 12400, TokensIfRaw: 14000, ScoreHigh: 1,
			ScoreMid: 1, ScoreLow: 2},
		{Tool: "context_search", OK: true, TokensSent: 400, TokensSaved: 13600, TokensIfRaw: 14000, ScoreLow: 2},
		{Tool: "context_search", OK: true},                                     // found nothing: not counted
		{Tool: "memory_search", OK: true, ScoreHigh: 3},                        // not a context search
		{Tool: "context_search", OK: false, TokensSent: 200, TokensIfRaw: 900}, // failed
	} {
		c.At, c.DatapackID = now, "d"
		if err = st.AddCall(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	got, err := svc.CallStats(ctx, "d", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := ContextStats{Requests: 2, Sent: 2000, Saved: 26000, IfRaw: 28000, ScoreHigh: 1, Mid: 1, Low: 4}
	if got.Context != want {
		t.Errorf("context %+v, want %+v", got.Context, want)
	}
	// An older call's saving was measured against another baseline (here a 50-chunk knowledge base):
	// it's counted against the default, so saved never exceeds the baseline.
	if err = st.AddCall(ctx, store.Call{At: now, DatapackID: "d", Tool: "context_search", OK: true, TokensSent: 400,
		TokensSaved: 49600}); err != nil {
		t.Fatal(err)
	}
	if got, _ = svc.CallStats(ctx, "d", 0); got.Context.IfRaw != 38000 || got.Context.Requests != 3 ||
		got.Context.Saved != 26000+9600 || got.Context.Sent != 2400 {
		t.Errorf("an older call: %+v", got.Context)
	}
}
