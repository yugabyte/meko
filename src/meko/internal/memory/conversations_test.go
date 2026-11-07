package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/contextengine"
)

func TestIDs(t *testing.T) {
	if got := hexID("0123456789ABCDEF0123456789abcdef"); got != "0123456789abcdef0123456789abcdef" {
		t.Errorf("a 32-hex session id: %s", got)
	}
	if got := hexID("01234567-89ab-cdef-0123-456789abcdef"); got != "0123456789abcdef0123456789abcdef" {
		t.Errorf("a UUID session id: %s", got)
	}
	// Any other session id is the first 32 hex of its SHA-256, as AgentK's own fallback computes.
	sum := sha256.Sum256([]byte("agentk-service"))
	if got := hexID("agentk-service"); got != hex.EncodeToString(sum[:])[:32] {
		t.Errorf("a hashed session id: %s", got)
	}
	s1, s2, r1, r2 := hexID("s"), hexID("s"), hexID(""), hexID("")
	if s1 != s2 || r1 == r2 || len(r1) != 32 {
		t.Error("session ids aren't stable, or random ones repeat")
	}
	hexSeed := "0123456789abcdef0123456789abcdef"
	x1, x2 := messageID("x"), messageID("x")
	if messageID(hexSeed) == hexSeed || x1 != x2 || len(messageID("")) != 32 {
		t.Error("message ids from seeds")
	}
}

func TestChunkAndSubstantive(t *testing.T) {
	if c := chunk("short"); len(c) != 1 || c[0] != "short" {
		t.Errorf("short text: %q", c)
	}
	long := strings.Repeat("é", 2000) // runes, not bytes
	c := chunk(long)
	if len(c) != 3 || len([]rune(c[0])) != 1024 || len([]rune(c[1])) != 1024 || len([]rune(c[2])) != 2000-1536 {
		t.Errorf("chunks of 2000 runes: %d, sizes %d", len(c), len([]rune(c[len(c)-1])))
	}
	if isSubstantive(strings.Repeat("a", 39), " ") || !isSubstantive("", strings.Repeat("b", 40)) {
		t.Error("the 40-character gate")
	}
}

const longTurn = "We decided the vector index lives in sqlite-vec with the datapack as its partition key."

func TestConversationsThroughTheContextEngine(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, hashEmbedder)
	go h.svc.RunEmbedder(ctx)

	c, err := h.svc.CreateConversation(ctx, CreateConversation{AgentID: "claude_code:x", DatapackID: h.dp,
		SessionID: "session-1", Title: "Design talk"})
	if err != nil || len(c.ID) != 32 {
		t.Fatalf("create: %+v %v", c, err)
	}
	again, err := h.svc.CreateConversation(ctx, CreateConversation{AgentID: "claude_code:x", DatapackID: h.dp,
		SessionID: "session-1", Title: "Other title"})
	if err != nil || again.ID != c.ID || again.Title != "Design talk" {
		t.Errorf("create is idempotent per session: %+v %v", again, err)
	}
	if _, err = h.svc.CreateConversation(ctx, CreateConversation{AgentID: "someone-else", DatapackID: h.dp,
		SessionID: "session-1", Title: "Mine"}); !errors.Is(err, ErrAgentMismatch) {
		t.Errorf("another agent's session: %v", err)
	}
	_, err = h.svc.CreateConversation(ctx, CreateConversation{DatapackID: h.dp, Title: " "})
	if !errors.Is(err, ErrBadRequest) {
		t.Errorf("no title: %v", err)
	}

	add := func(conv, agent, seed, input, output string) AddedMessage {
		t.Helper()
		r, aerr := h.svc.AddMessage(ctx, AddMessage{ConversationID: conv, AgentID: agent, DatapackID: h.dp, Input: input,
			Output: output, Seed: seed})
		if aerr != nil {
			t.Fatal(aerr)
		}
		return r
	}
	r := add(c.ID, "claude_code:x", "turn-1", "Where does the vector index live?", longTurn)
	if r.Status != "accepted" || r.MessageID == "" || r.SearchSkipped != "" {
		t.Errorf("add: %+v", r)
	}
	if again := add(c.ID, "claude_code:x", "turn-1", "x", "y"); again.MessageID != r.MessageID {
		t.Errorf("a repeated seed made another turn: %+v", again)
	}
	add(c.ID, "claude_code:x", "turn-2", "ok", "thanks") // too short to search
	if _, err = h.svc.AddMessage(ctx, AddMessage{ConversationID: c.ID, AgentID: "someone-else", DatapackID: h.dp,
		Input: "x", Output: "y"}); !errors.Is(err, ErrAgentMismatch) {
		t.Errorf("another agent's conversation: %v", err)
	}
	if _, err = h.svc.AddMessage(ctx, AddMessage{ConversationID: "ffffffffffffffffffffffffffffffff", DatapackID: h.dp,
		Input: "x"}); !errors.Is(err, ErrNoConversation) || err.Error() != "no such conversation" {
		t.Errorf("a missing conversation: %v", err)
	}

	got, msgs, err := h.svc.GetConversation(ctx, h.dp, strings.ToUpper(c.ID), "claude_code:x", true, 0, 0)
	if err != nil || got.MessageCount != 2 || len(msgs) != 2 || msgs[0].Output != longTurn {
		t.Errorf("get: %+v %+v %v", got, msgs, err)
	}
	list, more, err := h.svc.ListConversations(ctx, h.dp, "", 0, 0)
	if err != nil || more || len(list) != 1 {
		t.Errorf("list: %+v %v %v", list, more, err)
	}
	title := "Vector index"
	if u, err := h.svc.UpdateConversation(ctx, h.dp, c.ID, "claude_code:x", &title, nil); err != nil || u.Title != title {
		t.Errorf("update: %+v %v", u, err)
	}
	if _, err := h.svc.UpdateConversation(ctx, h.dp, c.ID, "claude_code:x", nil, nil); !errors.Is(err, ErrBadRequest) {
		t.Errorf("update with nothing: %v", err)
	}

	// Searched from another conversation, the turn is a hit and its conversation comes back whole
	// (minus the turn too short to search).
	res := waitForHits(t, h, c.ID[:0], "where does the vector index live")
	if res.Hits[0].ConversationID != c.ID || res.Hits[0].MessageID != r.MessageID || res.Hits[0].UserID != h.userID ||
		res.Hits[0].RerankScore != nil {
		t.Errorf("hit: %+v", res.Hits[0])
	}
	if len(res.Thread) != 1 || res.Thread[0].MessageID != r.MessageID || res.Thread[0].Distance == nil {
		t.Errorf("thread: %+v", res.Thread)
	}
	if own, _ := h.svc.SearchConversations(ctx, h.dp, c.ID, "where does the vector index live"); len(own.Hits) != 0 {
		t.Errorf("the caller's own conversation was searched: %+v", own.Hits)
	}
	if res, _ := h.svc.SearchConversations(ctx, h.dp, "", "bananas mangoes pineapples"); len(res.Hits) != 0 {
		t.Errorf("an unrelated query hit: %+v", res.Hits)
	}

	// Opted out, new turns aren't embedded; old ones stay searchable.
	if err := h.svc.SetConversationSearchOptOut(ctx, h.dp, true); err != nil {
		t.Fatal(err)
	}
	r = add(c.ID, "claude_code:x", "turn-3", "Which tokenizer does FTS5 use here?", strings.Repeat("unicode61 ", 8))
	if r.SearchSkipped != "datapack_opted_out" {
		t.Errorf("opted out: %+v", r)
	}
	if res, _ := h.svc.SearchConversations(ctx, h.dp, "", "where does the vector index live"); len(res.Hits) == 0 {
		t.Error("opting out hid turns already embedded")
	}

	if err := h.svc.DeleteConversation(ctx, h.dp, c.ID, "claude_code:x"); err != nil {
		t.Fatal(err)
	}
	if res, _ := h.svc.SearchConversations(ctx, h.dp, "", "where does the vector index live"); len(res.Hits) != 0 {
		t.Errorf("a deleted conversation's turns were found: %+v", res.Hits)
	}
}

// Turns added while the context engine is down wait, and are embedded once it's back.
func TestTurnsWaitForTheContextEngine(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, hashEmbedder)
	c, err := h.svc.CreateConversation(ctx, CreateConversation{DatapackID: h.dp, SessionID: "s", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	h.stopContextEngine()
	if _, err = h.svc.AddMessage(ctx, AddMessage{ConversationID: c.ID, DatapackID: h.dp,
		Input: "Where does the vector index live?", Output: longTurn}); err != nil {
		t.Fatalf("adding a turn needs no context engine: %v", err)
	}
	if err = h.svc.EmbedPending(ctx); !errors.Is(err, contextengine.ErrUnavailable) {
		t.Errorf("embedding with no context engine: %v", err)
	}
	h.startContextEngine()
	if err = h.svc.EmbedPending(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := h.svc.SearchConversations(ctx, h.dp, "", "where does the vector index live")
	if err != nil || len(res.Hits) == 0 {
		t.Errorf("after the context engine came back: %+v %v", res, err)
	}
}

// A substantive turn that won't be searchable says why, and leaves no chunks to embed.
func TestSearchSkipped(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, hashEmbedder)
	inDatapack, err := h.svc.CreateConversation(ctx, CreateConversation{DatapackID: h.dp, SessionID: "s1", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	inNone, err := h.svc.CreateConversation(ctx, CreateConversation{SessionID: "s2", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		r    AddMessage
		want string
	}{
		{AddMessage{ConversationID: inNone.ID, Input: "Where does the vector index live?", Output: longTurn}, "no_datapack"},
		{AddMessage{ConversationID: inDatapack.ID, DatapackID: h.dp, Input: "Where does the vector index live?",
			Output: longTurn, SkipSearchEmbed: true}, "caller_skip_search_embed"},
		{AddMessage{ConversationID: inDatapack.ID, DatapackID: h.dp, Input: "ok", Output: "thanks"}, ""},
	} {
		r, err := h.svc.AddMessage(ctx, c.r)
		if err != nil || r.SearchSkipped != c.want {
			t.Errorf("%q: %+v %v", c.want, r, err)
		}
	}
	if p, err := h.st.PendingChunks(ctx, 10); err != nil || len(p) != 0 {
		t.Errorf("chunks to embed: %+v %v", p, err)
	}
}

// The thread holds the turns of the best threadConversations conversations: each one's most recent
// searchable turns, threadTurns in all, oldest first.
func TestConversationThread(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, hashEmbedder)
	const query = "where does the vector index live"
	conv := func(session string) string {
		c, err := h.svc.CreateConversation(ctx, CreateConversation{DatapackID: h.dp, SessionID: session, Title: "t"})
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	add := func(c, seed, input, output string) {
		if _, err := h.svc.AddMessage(ctx, AddMessage{ConversationID: c, DatapackID: h.dp, Seed: seed, Input: input,
			Output: output}); err != nil {
			t.Fatal(err)
		}
	}
	long := conv("long")
	add(long, "hit", query+"?", longTurn)
	// More turns than one page of the store's reads (1000), so the newest are past the first page.
	for i := 0; i < 1010; i++ {
		add(long, fmt.Sprint("filler-", i), fmt.Sprintf("filler %04d: lunch plans, coffee, bicycles and parking", i), "ok")
	}
	for _, s := range []string{"b", "c", "d"} {
		add(conv(s), "hit", "Where is the vector index?", longTurn+" ("+s+")")
	}
	if err := h.svc.EmbedPending(ctx); err != nil {
		t.Fatal(err)
	}

	res, err := h.svc.SearchConversations(ctx, h.dp, "", query)
	if err != nil || len(res.Hits) < 4 {
		t.Fatalf("hits: %+v %v", res.Hits, err)
	}
	if len(res.Thread) != threadTurns {
		t.Errorf("%d turns in the thread, want %d", len(res.Thread), threadTurns)
	}
	convs := map[string]bool{}
	contents := make([]string, 0, len(res.Thread))
	var fillers []string
	for _, turn := range res.Thread {
		convs[turn.ConversationID] = true
		contents = append(contents, turn.Content)
		if strings.HasPrefix(turn.Content, "filler") {
			fillers = append(fillers, turn.Content)
		}
	}
	if !sort.StringsAreSorted(fillers) {
		t.Errorf("turns not oldest first: %v", fillers)
	}
	all := strings.Join(contents, "\n")
	if len(convs) > threadConversations || !convs[long] {
		t.Errorf("conversations in the thread: %v", convs)
	}
	if !strings.Contains(all, "filler 1009") || strings.Contains(all, "filler 0959") {
		t.Errorf("the long conversation's newest turns weren't the ones kept:\n%s", all)
	}

	// The cutoff comes from Options.
	strict := New(h.st, contextengine.NewClient(h.l.ContextEngineSock, h.token),
		Options{UserID: h.userID, MaxDistance: 1e-6})
	if res, err := strict.SearchConversations(ctx, h.dp, "", query); err != nil || len(res.Hits) != 0 {
		t.Errorf("hits past a 1e-6 cutoff: %+v %v", res.Hits, err)
	}
}

// waitForHits searches until the embedder sweep has caught up.
func waitForHits(t *testing.T, h *harness, exclude, query string) ConversationResults {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		res, err := h.svc.SearchConversations(context.Background(), h.dp, exclude, query)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Hits) > 0 {
			return res
		}
		if time.Now().After(deadline) {
			t.Fatalf("no hits for %q", query)
		}
	}
}

// With the real model: a paraphrased question finds the turn that answered it within the cutoff,
// and turns about other things stay outside it.
func TestConversationSearchCutoff(t *testing.T) {
	if testing.Short() {
		t.Skip("downloads the embedding model on its first run")
	}
	ctx := context.Background()
	h := newHarness(t, realEmbedder)
	turns := []struct{ session, input, output string }{
		{"a", "Where should the vector index live?",
			"We decided the vector index lives in sqlite-vec, with the datapack as its partition key, so a search " +
				"only scans one datapack's vectors."},
		{"b", "What's for lunch today?", "The cafeteria is serving mushroom risotto and a tomato soup, with fruit after."},
		{"c", "How do I rotate the TLS certificate?",
			"Run the cert command with --renew; the server picks the new certificate up without a restart."},
	}
	for _, tr := range turns {
		c, err := h.svc.CreateConversation(ctx, CreateConversation{DatapackID: h.dp, SessionID: tr.session, Title: "t"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.AddMessage(ctx, AddMessage{ConversationID: c.ID, DatapackID: h.dp, Input: tr.input,
			Output: tr.output}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.svc.EmbedPending(ctx); err != nil {
		t.Fatal(err)
	}
	vecs, err := h.svc.eng.Embed(ctx, []string{"which store holds the embeddings, and is it split by datapack?"})
	if err != nil {
		t.Fatal(err)
	}
	all, err := h.st.SearchChunks(ctx, h.dp, vecs[0], 10, "")
	if err != nil || len(all) != 3 {
		t.Fatalf("candidates: %+v %v", all, err)
	}
	for _, c := range all {
		t.Logf("distance %.3f: %.40s", c.Distance, c.Text)
	}
	res, err := h.svc.SearchConversations(ctx, h.dp, "", "which store holds the embeddings, and is it split by datapack?")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || !strings.Contains(res.Hits[0].Content, "sqlite-vec") {
		t.Errorf("hits with the %.2f cutoff: %+v", DefaultMaxDistance, res.Hits)
	}
}
