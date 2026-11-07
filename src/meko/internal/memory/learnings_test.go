package memory

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/contextengine"
	"github.com/yugabyte/meko/src/meko/internal/store"
)

func TestPromoteMemories(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, hashEmbedder)
	add := func(text string) string {
		added, err := h.svc.AddMemory(ctx, AddMemory{DatapackID: h.dp, AgentID: "a", Text: text})
		if err != nil || len(added) != 1 {
			t.Fatalf("add: %+v %v", added, err)
		}
		return added[0].ID
	}
	first, second, third := add("the team uses trunk-based development"), add("releases go out on tuesdays"),
		add("the staging database is called orchid")

	// An id given twice is promoted once, and not also reported missing.
	p, err := h.svc.PromoteMemories(ctx, h.dp, "a", []string{first, first})
	if err != nil || !slices.Equal(p.Inserted, []string{first}) || len(p.NotFound) != 0 {
		t.Fatalf("promote a duplicate: %+v %v", p, err)
	}

	// With the context engine down the call fails, so the caller can retry, and the memory stays a learning.
	h.stopContextEngine()
	p, err = h.svc.PromoteMemories(ctx, h.dp, "a", []string{second})
	if !errors.Is(err, contextengine.ErrUnavailable) || len(p.Inserted) != 0 || len(p.NotFound) != 0 {
		t.Fatalf("promote with no context engine: %+v %v", p, err)
	}
	if yes, _ := h.svc.IsPromoted(ctx, h.dp, second); yes {
		t.Error("the copy of a memory the context engine didn't delete was kept")
	}
	if rec, _ := h.st.GetVector(ctx, store.Memories, h.dp, second); rec == nil {
		t.Error("the memory is gone")
	}
	h.startContextEngine()

	// A promotion cut short (copied, never deleted) finishes when it's promoted again.
	if _, err = h.st.CopyToPromoted(ctx, h.dp, h.userID, third, "a"); err != nil {
		t.Fatal(err)
	}
	p, err = h.svc.PromoteMemories(ctx, h.dp, "a", []string{third, second})
	if err != nil || !slices.Equal(p.Inserted, []string{third, second}) {
		t.Fatalf("promote again: %+v %v", p, err)
	}
	for _, id := range []string{third, second} {
		if rec, _ := h.st.GetVector(ctx, store.Memories, h.dp, id); rec != nil {
			t.Errorf("%s is still a memory", id)
		}
	}
}
