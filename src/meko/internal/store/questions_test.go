package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestQuestions(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDatapack(ctx, "project", "")
	if err != nil {
		t.Fatal(err)
	}
	// Noon UTC, so the asks an hour or two before it fall on its UTC day, which the usage stats count by.
	now := time.Now().UTC().Truncate(24 * time.Hour).Add(12 * time.Hour)
	score := 0.4
	asks := []Ask{
		{DatapackID: d.ID, Key: "where is x", At: now.Add(-2 * time.Hour), Query: "Where is X", Quality: "empty",
			Tool: "context_search", AgentID: "a", ConversationID: "c1"},
		{DatapackID: d.ID, Key: "where is x", At: now.Add(-time.Hour), Query: "where  is X?", Quality: "weak",
			Tool: "memory_search", AgentID: "b", ConversationID: "c2", Results: 2, TopScore: &score},
		{DatapackID: d.ID, Key: "old", At: now.Add(-100 * 24 * time.Hour), Query: "old", Quality: "empty",
			Tool: "context_search", ConversationID: "c1"},
	}
	for _, a := range asks {
		if err = s.AddAsk(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Asks(ctx, d.ID)
	if err != nil || len(got) != 3 || got[0].Key != "where is x" || got[0].Quality != "weak" ||
		got[0].TopScore == nil || *got[0].TopScore != 0.4 || got[0].TopRaw != nil || got[0].Results != 2 ||
		!got[0].At.Equal(asks[1].At) || got[2].Key != "old" {
		t.Fatalf("asks %+v %v", got, err)
	}

	// Marks: a person's dismissal, an agent's teach, and a restore of the dismissal.
	if err = s.MarkQuestion(ctx, d.ID, "where is x", QuestionMark{State: "dismissed", At: now}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkQuestion(ctx, d.ID, "where is x", QuestionMark{State: "taught", At: now.Add(time.Second),
		By: "c9", Text: "X is in Y", Memory: "m1"}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkQuestion(ctx, d.ID, "nothing like it", QuestionMark{State: "dismissed", At: now}); !errors.Is(err,
		ErrNotFound) {
		t.Errorf("an unknown question: %v", err)
	}
	if err = s.AnswerQuestion(ctx, d.ID, "where is x", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	qs, err := s.Questions(ctx, d.ID)
	if err != nil || len(qs) != 2 {
		t.Fatalf("questions %+v %v", qs, err)
	}
	var q Question
	for _, x := range qs {
		if x.Key == "where is x" {
			q = x
		}
	}
	if q.DismissedAt == nil || q.DismissedBy != "" || q.TaughtAt == nil || q.TaughtBy != "c9" ||
		q.TaughtText != "X is in Y" || q.TaughtMemory != "m1" || q.AnsweredAt == nil || q.OpenedAt != nil {
		t.Errorf("marks %+v", q)
	}
	days, err := s.QuestionDays(ctx, now.Add(-24*time.Hour))
	if err != nil || len(days) != 1 || days[0].Empty != 1 || days[0].Weak != 1 || days[0].Dismissed != 1 ||
		days[0].Taught != 0 {
		t.Errorf("days %+v %v (an agent's teach isn't a person's)", days, err)
	}
	err = s.MarkQuestion(ctx, d.ID, "where is x", QuestionMark{State: "open", At: now.Add(3 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if days, _ = s.QuestionDays(ctx, now.Add(-24*time.Hour)); len(days) != 1 || days[0].Dismissed != 0 {
		t.Errorf("a restore takes back a person's dismissal: %+v", days)
	}
	qs, _ = s.Questions(ctx, d.ID)
	for _, x := range qs {
		if x.Key == "where is x" && (x.DismissedAt != nil || x.OpenedAt == nil || x.TaughtAt == nil) {
			t.Errorf("restored %+v: the dismissal cleared, what was taught kept", x)
		}
	}

	// A Meko Cloud datapack's asks (no datapacks row) aren't this Meko's usage.
	if err = s.AddAsk(ctx, Ask{DatapackID: "cloud-dp", Key: "k", At: now, Query: "k", Quality: "empty",
		Tool: "context_search"}); err != nil {
		t.Fatal(err)
	}
	if days, _ = s.QuestionDays(ctx, now.Add(-24*time.Hour)); len(days) != 1 || days[0].Empty != 1 {
		t.Errorf("a Meko Cloud datapack's ask counted: %+v", days)
	}

	// Forgetting one conversation's asks drops a question left with none, unless someone marked it.
	if err = s.AddAsk(ctx, Ask{DatapackID: d.ID, Key: "seeded", At: now.Add(-time.Minute), Query: "seeded",
		Quality: "empty", Tool: "context_search", ConversationID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkQuestion(ctx, d.ID, "seeded", QuestionMark{State: "taught", At: now, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteAsks(ctx, d.ID, "c1", now)
	if err != nil || n != 3 {
		t.Fatalf("deleted %d %v", n, err)
	}
	if qs, _ = s.Questions(ctx, d.ID); len(qs) != 2 {
		t.Errorf("after forgetting c1, the taught question stays: %+v", qs)
	}
	if n, err = s.PruneAsks(ctx, now); err != nil || n != 1 {
		t.Fatalf("pruned %d %v", n, err)
	}
	if qs, _ = s.Questions(ctx, d.ID); len(qs) != 0 {
		t.Errorf("after pruning: %+v", qs)
	}

	// Deleting the datapack deletes its questions and their asks, unless it was moved to Meko Cloud,
	// whose datapacks keep their questions here.
	if err = s.AddAsk(ctx, asks[0]); err != nil {
		t.Fatal(err)
	}
	moved, err := s.CreateDatapack(ctx, "moved", "")
	if err != nil {
		t.Fatal(err)
	}
	kept := asks[0]
	kept.DatapackID = moved.ID
	if err = s.AddAsk(ctx, kept); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteDatapack(ctx, moved.ID, true); err != nil {
		t.Fatal(err)
	}
	if qs, _ = s.Questions(ctx, moved.ID); len(qs) != 1 {
		t.Errorf("a datapack moved to Meko Cloud lost its questions: %+v", qs)
	}
	if err = s.DeleteDatapack(ctx, d.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Asks(ctx, d.ID); len(got) != 0 {
		t.Errorf("a deleted datapack's asks: %+v", got)
	}
	if qs, _ = s.Questions(ctx, d.ID); len(qs) != 0 {
		t.Errorf("a deleted datapack's questions: %+v", qs)
	}
}
