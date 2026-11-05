package memory

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/store"
)

// The search map draws the nearest memories, knowledge-base chunks and conversation turns to a
// question, each by its source, a turn once however many chunks it has, at most searchMapPoints of
// them, with the question placed among them; hits are drawn even past the nearest.
func TestSearchMap(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, hashEmbedder)
	const question = "when do releases ship"
	if m, err := h.svc.SearchMap(ctx, h.dp, "", nil, nil); err != nil || len(m.Points) != 0 || m.Query != nil {
		t.Fatalf("no question: %+v %v", m, err)
	}
	mem, err := h.svc.AddMemory(ctx, AddMemory{DatapackID: h.dp, AgentID: "a", Text: "releases ship on Tuesdays"})
	if err != nil || len(mem) != 1 {
		t.Fatal(mem, err)
	}
	h.upload("releases.txt", []byte("Release process. Releases ship every Tuesday after two approvals."))
	h.index()
	c, err := h.svc.CreateConversation(ctx, CreateConversation{DatapackID: h.dp, SessionID: "s", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	// A turn long enough for several chunks, and more turns than the map draws.
	if _, err = h.svc.AddMessage(ctx, AddMessage{ConversationID: c.ID, DatapackID: h.dp, Seed: "long",
		Input: question + "? " + strings.Repeat("releases ship on tuesdays. ", 200), Output: "ok"}); err != nil {
		t.Fatal(err)
	}
	for i := range searchMapPoints + 20 {
		if _, err = h.svc.AddMessage(ctx, AddMessage{ConversationID: c.ID, DatapackID: h.dp, Seed: fmt.Sprint(i),
			Input:  fmt.Sprintf("filler %d: lunch, coffee and parking", i),
			Output: "Noted, lunch and parking are sorted for the week."}); err != nil {
			t.Fatal(err)
		}
	}
	if err = h.svc.EmbedPending(ctx); err != nil {
		t.Fatal(err)
	}

	all := []string{store.MapMemory, store.MapKnowledge, store.MapConversation}
	m, err := h.svc.SearchMap(ctx, h.dp, question, all, nil)
	if err != nil {
		t.Fatal(err)
	}
	bySource := map[string]int{}
	turns := map[string]int{}
	for _, p := range m.Points {
		bySource[p.Source]++
		if p.Source == store.MapConversation {
			turns[p.MessageID]++
		}
		if math.Abs(p.X) > 1.0001 || math.Abs(p.Y) > 1.0001 || p.Similarity == nil || p.State != "" {
			t.Errorf("point %+v", p)
		}
	}
	if len(m.Points) != searchMapPoints || bySource[store.MapMemory] != 1 || bySource[store.MapKnowledge] == 0 ||
		m.Query == nil || len(m.Links) != 0 {
		t.Errorf("%d points by source %v, query %v, %d links", len(m.Points), bySource, m.Query, len(m.Links))
	}
	for id, n := range turns {
		if n != 1 {
			t.Errorf("turn %s drawn %d times", id, n)
		}
	}

	// One source draws only its own; a hit past the nearest is drawn too.
	m, err = h.svc.SearchMap(ctx, h.dp, question, []string{store.MapMemory}, nil)
	if err != nil || len(m.Points) != 1 || m.Points[0].ID != mem[0].ID {
		t.Errorf("memories only: %+v %v", m.Points, err)
	}
	far, err := h.svc.SearchMap(ctx, h.dp, "filler 3: lunch, coffee and parking", []string{store.MapConversation}, nil)
	if err != nil || len(far.Points) != searchMapPoints {
		t.Fatalf("filler map: %d %v", len(far.Points), err)
	}
	drawn := map[string]bool{}
	for _, p := range far.Points {
		drawn[p.ID] = true
	}
	var missing string
	for i := range searchMapPoints + 20 {
		if id := messageID(fmt.Sprint(i)); !drawn[id] {
			missing = id
			break
		}
	}
	if missing == "" {
		t.Fatal("every turn is among the nearest: nothing to ask for")
	}
	m, err = h.svc.SearchMap(ctx, h.dp, "filler 3: lunch, coffee and parking", []string{store.MapConversation},
		[]string{missing})
	if err != nil || len(m.Points) != searchMapPoints+1 || m.Points[len(m.Points)-1].ID != missing {
		t.Errorf("a hit past the nearest: %d points %v", len(m.Points), err)
	}
}
