package store

import (
	"context"
	"testing"
	"time"
)

func TestCallLog(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	calls := []Call{
		{At: now.Add(-100 * 24 * time.Hour), Tool: "context_search", DatapackID: "d1", OK: true},
		{At: now.Add(-time.Hour), Tool: "context_search", DatapackID: "d1", AgentID: "a", Credential: "client",
			OK: true, Ms: 12, MemoryN: 3, ConversationN: 1, ScoreHigh: 2, ScoreLow: 2, TokensSent: 1600,
			TokensSaved: 8400, Steps: []Step{{Step: "memory_search", Ms: 9, N: 3}}},
		{At: now, Tool: "memory_add", DatapackID: "d2", OK: false, ErrorCode: "memory_engine_unavailable"},
	}
	for _, c := range calls {
		if err := s.AddCall(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Calls(ctx, "d1", now.Add(-24*time.Hour))
	if err != nil || len(got) != 1 {
		t.Fatalf("d1's last day: %+v %v", got, err)
	}
	c := got[0]
	if c.Tool != "context_search" || c.AgentID != "a" || c.Credential != "client" || !c.OK || c.Ms != 12 ||
		c.MemoryN != 3 || c.ConversationN != 1 || c.ScoreHigh != 2 || c.ScoreLow != 2 || c.TokensSaved != 8400 ||
		len(c.Steps) != 1 || c.Steps[0] != (Step{Step: "memory_search", Ms: 9, N: 3}) ||
		!c.At.Equal(calls[1].At.Truncate(time.Nanosecond)) {
		t.Errorf("round trip: %+v", c)
	}
	all, _ := s.Calls(ctx, "", time.Time{})
	if len(all) != 3 || all[2].OK || all[2].ErrorCode != "memory_engine_unavailable" || all[2].Steps == nil {
		t.Errorf("all: %+v", all)
	}

	n, err := s.PruneCalls(ctx, now.Add(-90*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if all, _ := s.Calls(ctx, "", time.Time{}); len(all) != 2 {
		t.Errorf("after pruning: %+v", all)
	}
}
