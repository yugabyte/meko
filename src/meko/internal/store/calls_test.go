package store

import (
	"context"
	"reflect"
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
			TokensSaved: 8400, TokensIfRaw: 9000,
			Steps: []Step{{Step: "memory_search", Ms: 9, N: 3, IDs: []string{"m1", "m2", "m3"}}}},
		{At: now, Tool: "memory_add", DatapackID: "d2", OK: false, ErrorCode: "memory_engine_unavailable"},
	}
	for _, c := range calls {
		if err := s.AddCall(ctx, c, nil); err != nil {
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
		c.TokensIfRaw != 9000 ||
		len(c.Steps) != 1 ||
		!reflect.DeepEqual(c.Steps[0], Step{Step: "memory_search", Ms: 9, N: 3, IDs: []string{"m1", "m2", "m3"}}) ||
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

func TestCallContents(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	add := func(c Call, cc *CallContents) {
		t.Helper()
		if err := s.AddCall(ctx, c, cc); err != nil {
			t.Fatal(err)
		}
	}
	add(Call{At: now.Add(-10 * 24 * time.Hour), Tool: "memory_add", AgentID: "a", OK: true},
		&CallContents{Args: `{"text":"old"}`, Reply: "added"})
	add(Call{At: now.Add(-time.Hour), Tool: "context_search", AgentID: "a", Client: "agentk", OK: true},
		&CallContents{Args: `{"query":"q"}`, Reply: `{"results":[]}`, Cut: true})
	add(Call{At: now, Tool: "datapack_list", AgentID: "b", Location: LocationCloud, OK: false, ErrorCode: "x"}, nil)

	got, more, err := s.ListCalls(ctx, CallFilter{}, 2)
	if err != nil || !more || len(got) != 2 || got[0].Tool != "datapack_list" || got[0].HasContents ||
		got[0].Location != LocationCloud || got[1].Client != "agentk" || !got[1].HasContents {
		t.Fatalf("first page: %+v %v %v", got, more, err)
	}
	older, more, err := s.ListCalls(ctx, CallFilter{Before: got[1].ID}, 2)
	if err != nil || more || len(older) != 1 || older[0].Tool != "memory_add" || !older[0].HasContents {
		t.Fatalf("older page: %+v %v %v", older, more, err)
	}
	for _, c := range []struct {
		f    CallFilter
		want int
	}{{CallFilter{AgentID: "a"}, 2}, {CallFilter{Client: "agentk"}, 1}, {CallFilter{Failed: true}, 1},
		{CallFilter{Tool: "memory_add"}, 1}, {CallFilter{AgentID: "a", Tool: "datapack_list"}, 0}} {
		if cs, _, _ := s.ListCalls(ctx, c.f, 50); len(cs) != c.want {
			t.Errorf("%+v: %d calls, want %d", c.f, len(cs), c.want)
		}
	}

	c, cc, ok, err := s.CallByID(ctx, got[1].ID)
	if err != nil || !ok || c.Tool != "context_search" || cc == nil || cc.Args != `{"query":"q"}` || !cc.Cut {
		t.Fatalf("by id: %+v %+v %v %v", c, cc, ok, err)
	}
	if _, cc, ok, _ = s.CallByID(ctx, got[0].ID); !ok || cc != nil {
		t.Errorf("a call without contents: %+v %v", cc, ok)
	}
	if _, _, ok, _ = s.CallByID(ctx, 999); ok {
		t.Error("an unknown id was found")
	}
	if n, bytes, err := s.CallContentsSize(ctx); err != nil || n != 2 || bytes == 0 {
		t.Errorf("size: %d %d %v", n, bytes, err)
	}

	if n, err := s.PruneCallContents(ctx, now.Add(-7*24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("pruned %d contents, %v", n, err)
	}
	// Pruning a call takes its contents with it.
	if _, err := s.PruneCalls(ctx, now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := s.CallContentsSize(ctx); n != 0 {
		t.Errorf("contents left after pruning their calls: %d", n)
	}
	add(Call{At: now, Tool: "memory_get_all", OK: true}, &CallContents{Args: "{}", Reply: "[]"})
	if n, err := s.ClearCallContents(ctx); err != nil || n != 1 {
		t.Errorf("cleared %d, %v", n, err)
	}
	if cs, _, _ := s.ListCalls(ctx, CallFilter{}, 50); len(cs) != 2 || cs[0].HasContents {
		t.Errorf("clearing kept the calls: %+v", cs)
	}
}
