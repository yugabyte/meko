package store

import (
	"context"
	"testing"
	"time"
)

func TestUsageTotalsAndCallDays(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureDefaultDatapack(ctx); err != nil {
		t.Fatal(err)
	}
	tot, err := s.UsageTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tot.Datapacks != 1 || tot.Memories != 0 || tot.KBDocuments != 0 || tot.KBBytes != 0 || tot.DBBytes <= 0 {
		t.Errorf("totals: %+v", tot)
	}

	day := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	for _, c := range []Call{
		{At: day.Add(-time.Hour), Tool: "context_search", AgentID: "claude_code:a", OK: true},
		{At: day.Add(time.Hour), Tool: "context_search", AgentID: "claude_code:a", OK: true, Ms: 10, TokensSent: 100,
			TokensSaved: 900},
		{At: day.Add(2 * time.Hour), Tool: "context_search", AgentID: "claude_code:a", OK: false, Ms: 5},
		{At: day.Add(3 * time.Hour), Tool: "memory_add", AgentID: "meko_cli", OK: true, Ms: 7},
		{At: day.Add(25 * time.Hour), Tool: "memory_add", AgentID: "meko_cli", OK: true},
	} {
		if err = s.AddCall(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.CallDays(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	want := []CallDay{
		{Day: "2026-11-09", Tool: "context_search", AgentID: "claude_code:a", Calls: 2, Errors: 1, Ms: 15, TokensSent: 100,
			TokensSaved: 900},
		{Day: "2026-11-09", Tool: "memory_add", AgentID: "meko_cli", Calls: 1, Ms: 7},
		{Day: "2026-11-10", Tool: "memory_add", AgentID: "meko_cli", Calls: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}
