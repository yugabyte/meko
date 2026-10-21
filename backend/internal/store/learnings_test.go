package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestKeepAndPromote(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	for _, id := range []string{"m1", "m2", "m3"} {
		insert(t, s, Memories, "a", id, unit(1, 0, 0), map[string]any{"data": "text of " + id, "user_id": "u",
			"agent_id": "ag", "meko_conversation_id": "conv", "createdAt": "2026-10-01T10:00:00.000Z", "tag": "t"})
	}
	insert(t, s, Memories, "a", "theirs", unit(1, 0, 0), map[string]any{"data": "x", "user_id": "other"})

	c, err := s.CountDatapack(ctx, "a", "u")
	if err != nil || c.Memories != 3 || c.Learnings != 3 || c.Promoted != 0 {
		t.Fatalf("counts %+v, %v", c, err)
	}
	// Keep: only the user's learnings, once.
	n, err := s.KeepMemories(ctx, "a", "u", []string{"m1", "theirs", "nope"})
	if err != nil || n != 1 {
		t.Fatalf("kept %d, %v", n, err)
	}
	if n, _ = s.KeepMemories(ctx, "a", "u", []string{"m1"}); n != 0 {
		t.Errorf("kept twice: %d", n)
	}
	page, total, err := s.ListMemoryPage(ctx, MemoryPage{DatapackID: "a", UserID: "u", Scope: ScopeLearnings, Limit: 10})
	if err != nil || total != 2 || len(page) != 2 {
		t.Errorf("learnings %d %v", total, err)
	}
	if _, total, _ = s.ListMemoryPage(ctx, MemoryPage{DatapackID: "a", UserID: "u", Limit: 10}); total != 3 {
		t.Errorf("memories %d", total)
	}

	// Promote copies a learning with its provenance and dates; a kept one isn't a learning.
	p, err := s.CopyToPromoted(ctx, "a", "u", "m2", "promoter")
	if err != nil || p.ID != "m2" || p.Text != "text of m2" || p.AgentID != "ag" || p.ConversationID != "conv" ||
		p.Payload["tag"] != "t" || p.PromotedBy != "promoter" || p.CreatedAt.Day() != 1 || p.ConnectorID != "" {
		t.Fatalf("copy %+v, %v", p, err)
	}
	for _, id := range []string{"m1", "m2", "theirs", "nope"} {
		if _, err = s.CopyToPromoted(ctx, "a", "u", id, "p"); !errors.Is(err, ErrNotFound) {
			t.Errorf("copy %s: %v", id, err)
		}
	}
	if yes, _ := s.IsPromoted(ctx, "a", "m2"); !yes {
		t.Error("m2 should be promoted")
	}
	ps, total, err := s.PromotedMemories(ctx, "a", 10, 0)
	if err != nil || total != 1 || ps[0].ID != "m2" {
		t.Errorf("promoted %+v %d %v", ps, total, err)
	}
	if err = s.DeletePromoted(ctx, "a", "m2"); err != nil {
		t.Fatal(err)
	}
	if err = s.DeletePromoted(ctx, "a", "m2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}
}

func TestArtifacts(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	body := []byte("<h1>design</h1>")
	a, created, err := s.PutArtifact(ctx, "a", "ag", "design.html", "text/html", body)
	if err != nil || !created || len(a.ContentHash) != 64 || a.SizeBytes != len(body) || a.LastAccessedAt != nil {
		t.Fatalf("put %+v %v %v", a, created, err)
	}
	again, created, err := s.PutArtifact(ctx, "a", "other", "renamed.html", "text/html", body)
	if err != nil || created || again.ID != a.ID || again.Filename != "design.html" {
		t.Errorf("same bytes again: %+v %v %v", again, created, err)
	}
	if _, _, err = s.PutArtifact(ctx, "nope", "ag", "x", "text/plain", body); !errors.Is(err, ErrNoDatapack) {
		t.Errorf("no datapack: %v", err)
	}
	got, err := s.GetArtifact(ctx, "a", a.ContentHash)
	if err != nil || !bytes.Equal(got.Bytes, body) || got.LastAccessedAt == nil {
		t.Fatalf("get %+v %v", got, err)
	}
	if _, err = s.GetArtifact(ctx, "b", a.ContentHash); !errors.Is(err, ErrNotFound) {
		t.Errorf("another datapack's: %v", err)
	}
	list, total, err := s.ListArtifacts(ctx, "a", "DESIGN", 10, 0)
	if err != nil || total != 1 || len(list) != 1 || list[0].Bytes != nil {
		t.Errorf("list %+v %d %v", list, total, err)
	}
	if _, total, _ = s.ListArtifacts(ctx, "a", "nothing", 10, 0); total != 0 {
		t.Errorf("filtered list %d", total)
	}
}
