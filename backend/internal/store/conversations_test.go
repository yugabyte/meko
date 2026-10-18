package store

import (
	"context"
	"errors"
	"testing"
)

func TestConversations(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	c, created, err := s.CreateConversation(ctx, Conversation{ID: "c1", DatapackID: "a", AgentID: "ag", SessionID: "s",
		Title: "First", Metadata: map[string]any{"k": "v"}})
	if err != nil || !created || c.Title != "First" || c.Metadata["k"] != "v" || c.MessageCount != 0 {
		t.Fatalf("create: %+v %v %v", c, created, err)
	}
	// The same id again in the datapack is the same conversation, owner and all.
	again, created, err := s.CreateConversation(ctx, Conversation{ID: "c1", DatapackID: "a", AgentID: "other",
		Title: "Second"})
	if err != nil || created || again.AgentID != "ag" || again.Title != "First" {
		t.Fatalf("create again: %+v %v %v", again, created, err)
	}
	// Another datapack, or none, has its own.
	for _, dp := range []string{"b", ""} {
		if _, made, cerr := s.CreateConversation(ctx, Conversation{ID: "c1", DatapackID: dp, AgentID: "ag",
			Title: "t"}); cerr != nil || !made {
			t.Errorf("datapack %q: %v %v", dp, made, cerr)
		}
	}
	if _, made, _ := s.CreateConversation(ctx, Conversation{ID: "c1", AgentID: "ag", Title: "t"}); made {
		t.Error("a second conversation c1 in no datapack")
	}
	_, _, err = s.CreateConversation(ctx, Conversation{ID: "x", DatapackID: "nope", AgentID: "ag", Title: "t"})
	if !errors.Is(err, ErrNoDatapack) {
		t.Errorf("unknown datapack: %v", err)
	}

	m, created, err := s.AddMessage(ctx, "a", Message{ID: "m1", ConversationID: "c1", Input: "in", Output: "out",
		Plan: []string{"step"}}, []string{"in out"})
	if err != nil || !created || m.CreatedAt.IsZero() {
		t.Fatalf("add message: %+v %v %v", m, created, err)
	}
	dup, created, err := s.AddMessage(ctx, "a", Message{ID: "m1", ConversationID: "c1", Input: "changed"}, nil)
	if err != nil || created || dup.Input != "in" {
		t.Errorf("a repeated message: %+v %v %v", dup, created, err)
	}
	if _, _, err = s.AddMessage(ctx, "b", Message{ID: "m2", ConversationID: "nope"}, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("message to a missing conversation: %v", err)
	}
	if _, _, err = s.AddMessage(ctx, "a", Message{ID: "m2", ConversationID: "c1", Input: "second"}, nil); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages(ctx, "a", "c1", 10, 0)
	if err != nil || len(msgs) != 2 || msgs[0].ID != "m1" || msgs[0].Plan[0] != "step" || msgs[1].ID != "m2" {
		t.Fatalf("messages: %+v %v", msgs, err)
	}
	if msgs, _ := s.Messages(ctx, "a", "c1", 1, 1); len(msgs) != 1 || msgs[0].ID != "m2" {
		t.Errorf("a page of messages: %+v", msgs)
	}
	if c, _ := s.GetConversation(ctx, "a", "c1"); c.MessageCount != 2 {
		t.Errorf("message count %d", c.MessageCount)
	}

	title := "Renamed"
	if u, uerr := s.UpdateConversation(ctx, "a", "c1", &title, nil); uerr != nil || u.Title != "Renamed" ||
		u.Metadata["k"] != "v" {
		t.Errorf("update: %+v %v", u, uerr)
	}
	if _, err = s.UpdateConversation(ctx, "b", "nope", &title, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("update of a missing conversation: %v", err)
	}
	list, more, err := s.ListConversations(ctx, "a", "", 10, 0)
	if err != nil || more || len(list) != 1 || list[0].ID != "c1" {
		t.Errorf("list: %+v %v %v", list, more, err)
	}
	if list, _, _ := s.ListConversations(ctx, "a", "other", 10, 0); len(list) != 0 {
		t.Errorf("list by another agent: %+v", list)
	}
	if err := s.DeleteConversation(ctx, "a", "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetConversation(ctx, "a", "c1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete: %v", err)
	}
	if err := s.DeleteConversation(ctx, "a", "c1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}
	if _, err := s.GetConversation(ctx, "", "c1"); err != nil {
		t.Errorf("the conversation in no datapack went too: %v", err)
	}
}

func TestChunkSearch(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	for _, c := range []struct{ dp, id string }{{"a", "c1"}, {"a", "c2"}, {"b", "c3"}, {"", "c4"}} {
		conv := Conversation{ID: c.id, DatapackID: c.dp, AgentID: "ag", Title: "t"}
		if _, _, err := s.CreateConversation(ctx, conv); err != nil {
			t.Fatal(err)
		}
	}
	add := func(dp, conv, id string, chunks ...string) {
		if _, _, err := s.AddMessage(ctx, dp, Message{ID: id, ConversationID: conv}, chunks); err != nil {
			t.Fatal(err)
		}
	}
	add("a", "c1", "m1", "near", "far")
	add("a", "c2", "m2", "own conversation")
	add("b", "c3", "m3", "other datapack")
	add("", "c4", "m4", "no datapack")
	pending, err := s.PendingChunks(ctx, 100)
	if err != nil || len(pending) != 5 {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	vecs := map[string][]float32{"near": unit(1, 0.1, 0), "far": unit(0, 1, 0), "own conversation": unit(1, 0, 0),
		"other datapack": unit(1, 0, 0), "no datapack": unit(1, 0, 0)}
	ids := make([]int64, 0, len(pending))
	vs := make([][]float32, 0, len(pending))
	for _, c := range pending {
		ids, vs = append(ids, c.Rowid), append(vs, vecs[c.Text])
	}
	if err = s.PutChunkVectors(ctx, ids, vs); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.PendingChunks(ctx, 100); len(p) != 0 {
		t.Errorf("still pending: %+v (the chunk in no datapack is dropped)", p)
	}
	hits, err := s.SearchChunks(ctx, "a", unit(1, 0, 0), 10, "c2")
	if err != nil || len(hits) != 2 || hits[0].Text != "near" || hits[0].MessageID != "m1" || hits[1].Text != "far" {
		t.Fatalf("hits: %+v %v", hits, err)
	}
	if hits[0].Distance > 0.01 || hits[1].Distance < 0.99 {
		t.Errorf("distances %f %f", hits[0].Distance, hits[1].Distance)
	}
	if hits, _ := s.SearchChunks(ctx, "a", unit(1, 0, 0), 10, ""); len(hits) != 3 {
		t.Errorf("excluding nothing: %d hits", len(hits))
	}
	if err := s.DeleteConversation(ctx, "a", "c1"); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.SearchChunks(ctx, "a", unit(1, 0, 0), 10, ""); len(hits) != 1 || hits[0].ConversationID != "c2" {
		t.Errorf("after deleting c1: %+v", hits)
	}
	var n int
	if err := s.r.QueryRow("SELECT count(*) FROM message_chunks_vec").Scan(&n); err != nil || n != 2 {
		t.Errorf("%d chunk vectors left, want 2: %v", n, err)
	}
}

func TestConversationSearchOptOut(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	if out, err := s.ConversationSearchOptOut(ctx, "a"); err != nil || out {
		t.Fatalf("default: %v %v", out, err)
	}
	if err := s.SetConversationSearchOptOut(ctx, "a", true); err != nil {
		t.Fatal(err)
	}
	if out, _ := s.ConversationSearchOptOut(ctx, "a"); !out {
		t.Error("not opted out")
	}
	if err := s.SetConversationSearchOptOut(ctx, "nope", true); !errors.Is(err, ErrNoDatapack) {
		t.Errorf("unknown datapack: %v", err)
	}
}
