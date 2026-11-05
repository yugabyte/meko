package store

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
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
		days[0].Taught != 0 || days[0].AgentTaught != 1 || days[0].AgentDismissed != 0 {
		t.Errorf("days %+v %v (an agent's teach isn't a person's, nor a person's dismissal an agent's)", days, err)
	}
	err = s.MarkQuestion(ctx, d.ID, "where is x", QuestionMark{State: "open", At: now.Add(3 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if days, _ = s.QuestionDays(ctx, now.Add(-24*time.Hour)); len(days) != 1 || days[0].Dismissed != 0 ||
		days[0].AgentTaught != 1 {
		t.Errorf("a restore takes back a person's dismissal, and keeps what an agent taught: %+v", days)
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

// Agents' marks (with a conversation) count apart from people's, each kept from the other's later
// mark, on Meko Cloud's datapacks too.
func TestQuestionDaysAgentMarks(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDatapack(ctx, "project", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(24 * time.Hour).Add(12 * time.Hour)
	for _, a := range []Ask{
		{DatapackID: d.ID, Key: "a", At: now.Add(-time.Hour), Query: "a", Quality: "empty", Tool: "context_search"},
		{DatapackID: d.ID, Key: "b", At: now.Add(-time.Hour), Query: "b", Quality: "empty", Tool: "context_search"},
		{DatapackID: "cloud-dp", Key: "c", At: now.Add(-time.Hour), Query: "c", Quality: "weak", Tool: "context_search"},
		{DatapackID: "cloud-dp", Key: "d", At: now.Add(-time.Hour), Query: "d", Quality: "weak", Tool: "context_search"},
	} {
		if err = s.AddAsk(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	mark := func(dp, key string, m QuestionMark) {
		t.Helper()
		if err := s.MarkQuestion(ctx, dp, key, m); err != nil {
			t.Fatal(err)
		}
	}
	day := func(what string, want QuestionDay) {
		t.Helper()
		days, err := s.QuestionDays(ctx, now.Add(-24*time.Hour))
		want.Day = now.Format("2006-01-02")
		if err != nil || len(days) != 1 || days[0] != want {
			t.Errorf("%s: %+v %v, want %+v", what, days, err, want)
		}
	}
	// An agent dismisses a, then a person dismisses it too: both count.
	mark(d.ID, "a", QuestionMark{State: "dismissed", At: now, By: "c1"})
	mark(d.ID, "a", QuestionMark{State: "dismissed", At: now.Add(time.Second)})
	day("both dismissed a", QuestionDay{Empty: 2, Dismissed: 1, AgentDismissed: 1})
	// Restoring the person's dismissal leaves the agent's.
	mark(d.ID, "a", QuestionMark{State: "open", At: now.Add(2 * time.Second)})
	day("the person's restored", QuestionDay{Empty: 2, AgentDismissed: 1})
	// An agent dismisses b and teaches it; restoring its dismissal takes that back, not the teach.
	mark(d.ID, "b", QuestionMark{State: "dismissed", At: now, By: "c1"})
	mark(d.ID, "b", QuestionMark{State: "taught", At: now.Add(time.Second), By: "c1", Text: "B"})
	day("an agent marked b", QuestionDay{Empty: 2, AgentDismissed: 2, AgentTaught: 1})
	mark(d.ID, "b", QuestionMark{State: "open", At: now.Add(2 * time.Second)})
	day("the agent's restored", QuestionDay{Empty: 2, AgentDismissed: 1, AgentTaught: 1})
	// On a Meko Cloud datapack only an agent's marks count: not the asks, nor a person's marks.
	mark("cloud-dp", "c", QuestionMark{State: "taught", At: now, By: "c2", Text: "C"})
	mark("cloud-dp", "d", QuestionMark{State: "dismissed", At: now})
	day("Meko Cloud's", QuestionDay{Empty: 2, AgentDismissed: 1, AgentTaught: 2})
}

// An install upgraded from before agents' marks were kept apart counts the ones still standing.
func TestAgentMarksFilledOnUpgrade(t *testing.T) {
	ctx := context.Background()
	old := migrationSource
	t.Cleanup(func() { migrationSource = old })
	before := fstest.MapFS{}
	ms, _ := fs.ReadDir(old, ".")
	for _, e := range ms {
		if strings.HasPrefix(e.Name(), "0016_") {
			break
		}
		b, _ := fs.ReadFile(old, e.Name())
		before[e.Name()] = &fstest.MapFile{Data: b}
	}
	migrationSource = before
	s := open(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(24 * time.Hour).Add(12 * time.Hour)
	at := now.Format(timeLayout)
	if _, err := s.w.ExecContext(ctx, "INSERT INTO questions(datapack_id, query_key, dismissed_at, dismissed_by, "+
		"taught_at, taught_by, person_dismissed_at, person_taught_at) VALUES "+
		"('dp', 'agent', ?, 'c1', ?, 'c1', NULL, NULL), ('dp', 'person', ?, NULL, ?, NULL, ?, ?)",
		at, at, at, at, at, at); err != nil {
		t.Fatal(err)
	}
	migrationSource = old
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	days, err := s.QuestionDays(ctx, now.Add(-24*time.Hour))
	if err != nil || len(days) != 1 || days[0].AgentDismissed != 1 || days[0].AgentTaught != 1 {
		t.Errorf("days %+v %v: the agent's marks filled in, the person's not", days, err)
	}
}
