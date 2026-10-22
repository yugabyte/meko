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
	u, err := svc.CallUsage(ctx, "dp")
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
	// The UI is the user, not an agent: its searches count, it doesn't.
	cs, err := svc.CallStats(ctx, "dp")
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
