package memory

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/contextengine"
	"github.com/yugabyte/meko/src/meko/internal/store"
	"github.com/yugabyte/meko/src/meko/internal/supervisor"
)

func TestMemoriesThroughTheContextEngine(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, hashEmbedder)
	const secret = "the zebra password is periwinkle"

	added, err := h.svc.AddMemory(ctx, AddMemory{DatapackID: h.dp, AgentID: "claude_code:x", ConversationID: "c1",
		Text: secret, Metadata: map[string]any{"tag": "t1"}})
	if err != nil || len(added) != 1 || added[0].Event != "ADD" || added[0].Role != "user" {
		t.Fatalf("add: %+v %v", added, err)
	}
	id := added[0].ID
	more, err := h.svc.AddMemory(ctx, AddMemory{DatapackID: h.dp, Messages: []contextengine.Message{
		{Role: "system", Content: "skipped"},
		{Role: "user", Content: "the rate limit is five hundred"},
		{Role: "assistant", Content: "bananas are yellow", Name: "helper"},
	}})
	if err != nil || len(more) != 2 || more[1].ActorID == nil || *more[1].ActorID != "helper" {
		t.Fatalf("add messages: %+v %v", more, err)
	}

	m, err := h.svc.GetMemory(ctx, h.dp, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.Memory != secret || m.UserID != h.userID || m.AgentID != "claude_code:x" || m.RunID != "c1" ||
		m.Role != "user" || m.Metadata["tag"] != "t1" || m.Metadata["meko_datapack_id"] != h.dp || m.CreatedAt == "" {
		t.Errorf("get: %+v", m)
	}

	res, err := h.svc.SearchMemories(ctx, Search{DatapackID: h.dp, Query: "what is the zebra password", Limit: 5})
	if err != nil || len(res) == 0 || res[0].ID != id {
		t.Fatalf("search: %+v %v", res, err)
	}
	for _, r := range res {
		if *r.Score < 0 || *r.Score > 1 || *r.RawSimilarityScore < 0.1 {
			t.Errorf("scores %v %v", *r.Score, *r.RawSimilarityScore)
		}
	}
	res, err = h.svc.SearchMemories(ctx, Search{DatapackID: h.dp, ConversationID: "other", Query: "zebra"})
	if err != nil || len(res) != 0 {
		t.Errorf("conversation filter: %+v %v", res, err)
	}

	list, total, err := h.svc.ListMemories(ctx, h.dp, store.Filter{"role": "assistant"}, 0)
	if err != nil || total != 1 || list[0].Memory != "bananas are yellow" || list[0].ActorID != "helper" {
		t.Errorf("list: %+v %d %v", list, total, err)
	}

	if err = h.svc.UpdateMemory(ctx, h.dp, id, "the zebra password is now crimson"); err != nil {
		t.Fatal(err)
	}
	if m, _ := h.svc.GetMemory(ctx, h.dp, id); m.Memory != "the zebra password is now crimson" || m.RunID != "c1" {
		t.Errorf("after update: %+v", m)
	}
	if err = h.svc.DeleteMemory(ctx, h.dp, id); err != nil {
		t.Fatal(err)
	}
	if _, err = h.svc.GetMemory(ctx, h.dp, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete: %v", err)
	}
	if res, err = h.svc.SearchMemories(ctx, Search{DatapackID: h.dp, Query: "zebra password crimson"}); err != nil ||
		len(res) > 0 && res[0].ID == id {
		t.Errorf("a deleted memory was found, or the search failed: %v", err)
	}
	if err = h.svc.DeleteMemory(ctx, h.dp, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete of a deleted memory: %v", err)
	}
	// Another user's memory (written by mem0 for another user_id) is out of reach.
	vec := make([]float32, 384)
	vec[0] = 1
	if err = h.st.InsertVectors(ctx, store.Memories, h.dp, []string{"theirs"}, [][]float32{vec},
		[]map[string]any{{"data": "their memory", "user_id": "someone-else"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = h.svc.GetMemory(ctx, h.dp, "theirs"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get of another user's memory: %v", err)
	}
	if err = h.svc.UpdateMemory(ctx, h.dp, "theirs", "mine now"); !errors.Is(err, ErrNotFound) {
		t.Errorf("update of another user's memory: %v", err)
	}
	if err = h.svc.DeleteMemory(ctx, h.dp, "theirs"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete of another user's memory: %v", err)
	}
	if err = h.st.AddHistory(ctx, h.dp, []store.HistoryEntry{{MemoryID: "theirs", Action: "ADD",
		CreatedAt: "2026-10-22T00:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = h.svc.MemoryHistory(ctx, h.dp, "theirs"); !errors.Is(err, ErrNotFound) {
		t.Errorf("history of another user's memory: %v", err)
	}
	if _, err = h.svc.AddMemory(ctx, AddMemory{DatapackID: "nope", Text: "x"}); !errors.Is(err, ErrNoDatapack) {
		t.Errorf("an unknown datapack: %v", err)
	}
	if _, err = h.svc.SearchMemories(ctx, Search{DatapackID: h.dp, Query: " "}); !errors.Is(err, ErrBadRequest) {
		t.Errorf("an empty query: %v", err)
	}

	// Memory text never reaches the context engine's log.
	log, err := os.ReadFile(h.l.ContextEngineLog)
	if err != nil || strings.Contains(string(log), "zebra") || strings.Contains(string(log), "bananas") {
		t.Errorf("memory text in context-engine.log (%v):\n%s", err, log)
	}
}

// The context engine going away gives ErrUnavailable; started again, it serves the same memories.
func TestContextEngineDownAndBack(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, hashEmbedder)
	kept := AddMemory{DatapackID: h.dp, Text: "kept across context engine restarts"}
	if _, err := h.svc.AddMemory(ctx, kept); err != nil {
		t.Fatal(err)
	}
	h.stopContextEngine()
	_, err := h.svc.SearchMemories(ctx, Search{DatapackID: h.dp, Query: "restarts"})
	if !errors.Is(err, contextengine.ErrUnavailable) {
		t.Errorf("with no context engine: %v", err)
	}
	if st := h.svc.EngineStatus(ctx); st.State == "ready" {
		t.Errorf("status with no context engine: %+v", st)
	}
	h.startContextEngine()
	res, err := h.svc.SearchMemories(ctx, Search{DatapackID: h.dp, Query: "context engine restarts"})
	if err != nil || len(res) != 1 {
		t.Errorf("after a restart: %+v %v", res, err)
	}
}

// A context engine whose model isn't the one the vectors were made with stays up, says why, and serves
// nothing.
func TestContextEngineWithAnotherModelIsRefused(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, hashEmbedder)
	h.stopContextEngine()
	// Pretend the vectors were made with another model: rewrite the recorded one.
	if err := h.st.SetSetting(ctx, "embedder_model", "another-model"); err != nil {
		t.Fatal(err)
	}
	h.launchContextEngine()
	st := h.waitContextEngine("error")
	if !strings.Contains(st.Reason, "re-embedding") {
		t.Errorf("status: %+v", st)
	}
	var notReady *contextengine.NotReadyError
	if _, err := h.svc.SearchMemories(ctx, Search{DatapackID: h.dp, Query: "x"}); !errors.As(err, &notReady) {
		t.Errorf("search on a refused context engine: %v", err)
	}
}

// The end-to-end check, with the real model: a paraphrased question finds its memory, with a 0-1
// score and the cosine as raw_similarity_score, and an exact identifier clears Kode's 0.75
// recall cut once BM25 has a datapack's worth of memories to weigh terms against.
func TestParaphraseAndIdentifierSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("downloads the embedding model on its first run")
	}
	ctx := context.Background()
	h := newHarness(t, realEmbedder)
	words := strings.Fields("server client session datapack context engine socket token port worker model vector " +
		"search keyword config release build test deploy branch commit review design approve merge cache index query")
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // G404: filler text
	filler := make([]contextengine.Message, 200)
	for i := range filler {
		var b strings.Builder
		for j := 0; j < 10; j++ {
			b.WriteString(words[rng.Intn(len(words))] + " ")
		}
		filler[i] = contextengine.Message{Role: "user", Content: fmt.Sprintf("The %snote %d.", b.String(), i)}
	}
	if _, err := h.svc.AddMemory(ctx, AddMemory{DatapackID: h.dp, Messages: filler}); err != nil {
		t.Fatal(err)
	}
	facts := map[string]string{
		"restarts":   "Kode sessions survive server restarts because the session host keeps each pty alive.",
		"identifier": "MIN_MEMORY_SCORE is 0.75 and drops weaker memories from recall.",
	}
	ids := map[string]string{}
	for k, text := range facts {
		added, err := h.svc.AddMemory(ctx, AddMemory{DatapackID: h.dp, Text: text})
		if err != nil {
			t.Fatal(err)
		}
		ids[k] = added[0].ID
	}
	for _, c := range []struct {
		query, want string
		minScore    float64
	}{
		{"Do sessions keep running when the server restarts?", "restarts", 0},
		{"MIN_MEMORY_SCORE", "identifier", 0.75},
	} {
		res, err := h.svc.SearchMemories(ctx, Search{DatapackID: h.dp, Query: c.query, Limit: 3})
		if err != nil || len(res) == 0 {
			t.Fatalf("%q: %v %v", c.query, res, err)
		}
		top := res[0]
		t.Logf("%q: score %.3f, cosine %.3f", c.query, *top.Score, *top.RawSimilarityScore)
		if top.ID != ids[c.want] {
			t.Errorf("%q found %q first", c.query, top.Memory)
		}
		if *top.Score < c.minScore || *top.Score > 1 || *top.RawSimilarityScore < 0.5 || *top.RawSimilarityScore > 1 {
			t.Errorf("%q: score %.3f, cosine %.3f", c.query, *top.Score, *top.RawSimilarityScore)
		}
	}
}

// A crashed context engine's status says what the supervisor does about it, not "starting".
func TestContextEngineStatusOfACrashedContextEngine(t *testing.T) {
	child := supervisor.Status{Name: contextengine.Name, State: supervisor.Failed, LastError: "exited: exit status 1"}
	s := New(nil, contextengine.NewClient("/nonexistent/ctx_eng.sock", "tok"), Options{
		ContextEngineChild: func() (supervisor.Status, bool) { return child, true },
	})
	if st := s.EngineStatus(context.Background()); st.State != "failed" || st.Reason != child.LastError {
		t.Errorf("failed: %+v", st)
	}
	child.State = supervisor.Backoff
	if st := s.EngineStatus(context.Background()); st.State != "restarting" {
		t.Errorf("in backoff: %+v", st)
	}
	child.State = supervisor.Starting
	if st := s.EngineStatus(context.Background()); st.State != "starting" {
		t.Errorf("starting: %+v", st)
	}
}
