//go:build bench

package memory

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/store"
)

// The search budget at Meko's 200 MB target, in one datapack: make bench. Memories and turns
// are loaded straight into the store with random vectors (loading 200 MB through the model would
// take an hour and time the same searches); the searches then run through the real context engine, model
// included. Each conversation search's query has a chunk planted near it in threadConversations
// conversations, so it gets hits and assembles a full thread.
const (
	memoryBytes = 50 << 20
	turnBytes   = 150 << 20
	turnSize    = 2000
	searches    = 50
)

func TestSearchBudget(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, realEmbedder)
	rng := rand.New(rand.NewSource(1))
	vocab := make([]string, 20000)
	for i := range vocab {
		vocab[i] = fmt.Sprintf("w%s", strings.ToLower(fmt.Sprintf("%x", i*7919)))
	}
	zipf := rand.NewZipf(rng, 1.1, 1, uint64(len(vocab)-1))
	text := func(n int) string {
		var b strings.Builder
		for b.Len() < n {
			b.WriteString(vocab[zipf.Uint64()])
			b.WriteByte(' ')
		}
		return b.String()
	}
	vec := func() []float32 {
		v := make([]float32, 384)
		var n float64
		for i := range v {
			v[i] = float32(rng.NormFloat64())
			n += float64(v[i] * v[i])
		}
		for i := range v {
			v[i] /= float32(math.Sqrt(n))
		}
		return v
	}

	queries := make([]string, searches)
	for i := range queries {
		queries[i] = text(40) // about six words
	}
	qvecs, err := h.svc.eng.Embed(ctx, queries)
	if err != nil {
		t.Fatal(err)
	}
	near := func(q []float32) []float32 { // about 0.04 in cosine distance
		v := vec()
		var n float64
		for i := range v {
			v[i] = q[i] + 0.3*v[i]
			n += float64(v[i] * v[i])
		}
		for i := range v {
			v[i] /= float32(math.Sqrt(n))
		}
		return v
	}

	start := time.Now()
	var loaded int
	for batch := 0; loaded < memoryBytes; batch++ {
		var ids []string
		var vecs [][]float32
		var payloads []map[string]any
		for i := 0; i < 1000; i++ {
			s := text(250)
			loaded += len(s)
			ids = append(ids, fmt.Sprintf("m-%d-%d", batch, i))
			vecs = append(vecs, vec())
			payloads = append(payloads, map[string]any{"data": s, "textLemmatized": s, "user_id": h.userID})
		}
		if err := h.st.InsertVectors(ctx, store.Memories, h.dp, ids, vecs, payloads); err != nil {
			t.Fatal(err)
		}
	}
	memories := time.Since(start)

	start = time.Now()
	loaded = 0
	for conv := 0; loaded < turnBytes; conv++ {
		c, err := h.svc.CreateConversation(ctx, CreateConversation{DatapackID: h.dp, SessionID: fmt.Sprint(conv), Title: "t"})
		if err != nil {
			t.Fatal(err)
		}
		for turn := 0; turn < 75 && loaded < turnBytes; turn++ {
			in, out := text(turnSize/4), text(turnSize*3/4)
			loaded += len(in) + len(out)
			if _, _, err := h.st.AddMessage(ctx, h.dp, store.Message{ID: fmt.Sprintf("%d-%d", conv, turn),
				ConversationID: c.ID, Input: in, Output: out}, chunk(in+"\n\n"+out)); err != nil {
				t.Fatal(err)
			}
		}
		for {
			pending, err := h.st.PendingChunks(ctx, 1000)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) == 0 {
				break
			}
			ids := make([]int64, len(pending))
			vecs := make([][]float32, len(pending))
			for i, p := range pending {
				ids[i], vecs[i] = p.Rowid, vec()
			}
			if conv < searches*threadConversations {
				vecs[0] = near(qvecs[conv/threadConversations])
			}
			if err := h.st.PutChunkVectors(ctx, ids, vecs); err != nil {
				t.Fatal(err)
			}
		}
	}
	turns := time.Since(start)
	t.Logf("loaded %d MB of memories in %s and %d MB of turns in %s", memoryBytes>>20, memories.Round(time.Second),
		turnBytes>>20, turns.Round(time.Second))

	time1 := func(name string, f func(q string) error) {
		var ds []time.Duration
		for i := 0; i < searches; i++ {
			q := queries[i]
			s := time.Now()
			if err := f(q); err != nil {
				t.Fatal(err)
			}
			ds = append(ds, time.Since(s))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		t.Logf("%s: p50 %s, p95 %s, max %s", name, ds[len(ds)/2].Round(time.Millisecond),
			ds[len(ds)*95/100].Round(time.Millisecond), ds[len(ds)-1].Round(time.Millisecond))
	}
	// Where a memory search's time goes: the parts mem0 asks the store for, and the query's embedding.
	qv := vec()
	time1("  vector scan (k=240)", func(string) error {
		_, err := h.st.SearchVectors(ctx, store.Memories, h.dp, qv, 240, store.Filter{"user_id": h.userID})
		return err
	})
	time1("  keyword search (240)", func(q string) error {
		_, err := h.st.KeywordSearch(ctx, h.dp, q, 240, store.Filter{"user_id": h.userID})
		return err
	})
	time1("  embed the query", func(q string) error {
		_, err := h.svc.eng.Embed(ctx, []string{q})
		return err
	})
	time1("memory search", func(q string) error {
		_, err := h.svc.SearchMemories(ctx, Search{DatapackID: h.dp, Query: q, Limit: 10})
		return err
	})
	time1("conversation search", func(q string) error {
		res, err := h.svc.SearchConversations(ctx, h.dp, "", q)
		if err == nil && (len(res.Hits) < threadConversations || len(res.Thread) != threadTurns) {
			err = fmt.Errorf("%d hits and %d thread turns: the planted chunks weren't found", len(res.Hits),
				len(res.Thread))
		}
		return err
	})
}
