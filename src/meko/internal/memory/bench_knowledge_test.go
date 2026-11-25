//go:build bench

package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/store"
)

// The knowledge base's budget, with the real models: make bench. It indexes a 10 MB text file
// through the real pipeline (chunking, then embedding) for its throughput, times knowledge-base
// searches at 10 MB and, with 40 MB more of chunks loaded straight into the store with random
// vectors, at 50 MB, and times context_search while another file indexes.
func TestKnowledgeBudget(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, realEmbedder)
	waitReranker(t, h)
	rng := rand.New(rand.NewSource(2))
	words := strings.Fields("the release deploy server build database backup rollback schema migration index " +
		"cluster node replica query latency budget review owner team service config token storage cache queue " +
		"worker retry timeout network region zone tenant datapack memory knowledge file upload search vector " +
		"keyword score model engine socket process restart signal log metric alert page rotation incident")
	zipf := rand.NewZipf(rng, 1.2, 1, uint64(len(words)-1))
	text := func(n int) string {
		var b strings.Builder
		for b.Len() < n {
			for i := 0; i < 12 && b.Len() < n; i++ {
				b.WriteString(words[zipf.Uint64()])
				b.WriteByte(' ')
			}
			b.WriteString(". ")
			if rng.Intn(6) == 0 {
				b.WriteString("\n\n")
			}
		}
		return b.String()
	}

	// Ingestion of 10 MB through the real pipeline.
	file := []byte(text(10 << 20))
	h.upload("big.txt", file)
	start := time.Now()
	if err := h.svc.IndexQueued(ctx); err != nil {
		t.Fatal(err)
	}
	chunked := time.Since(start)
	st, _ := h.st.KnowledgeStats(ctx, h.dp)
	start = time.Now()
	if err := h.svc.EmbedPending(ctx); err != nil {
		t.Fatal(err)
	}
	embedded := time.Since(start)
	t.Logf("10 MB file: %d chunks; chunked in %s; embedded in %s (%.0f chunks/s)", st.FileChunks,
		chunked.Round(time.Millisecond), embedded.Round(time.Second), float64(st.FileChunks)/embedded.Seconds())

	queries := make([]string, 30)
	for i := range queries {
		queries[i] = "how do I " + text(40)
	}
	time1 := func(name string, f func(q string) error) {
		var ds []time.Duration
		for _, q := range queries {
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
	search := func(q string) error {
		_, err := h.svc.SearchKnowledge(ctx, h.dp, q, 10)
		return err
	}
	time1("knowledgebase_search at 10 MB", search)

	// 40 MB more, loaded straight into the store with random vectors.
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
	start = time.Now()
	for f := 0; f < 4; f++ {
		sum := sha256.Sum256([]byte(fmt.Sprint("bulk", f)))
		hash := hex.EncodeToString(sum[:])
		if _, _, err := h.st.AddDocument(ctx, store.Document{DatapackID: h.dp, Key: "bulk/" + hash, Filename: "bulk.txt",
			DocumentType: "text/plain", ContentHash: hash, SizeBytes: 10 << 20}); err != nil {
			t.Fatal(err)
		}
		d, err := h.st.ClaimDocument(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var chunks []string
		for n := 0; n < 10<<20; n += 900 {
			chunks = append(chunks, text(1000))
		}
		for i := 0; i < len(chunks); i += 500 {
			end := min(i+500, len(chunks))
			if err = h.st.AddChunks(ctx, *d, i, chunks[i:end], end == len(chunks)); err != nil {
				t.Fatal(err)
			}
		}
		for {
			pending, err := h.st.PendingKBChunks(ctx, 1000)
			if err != nil || len(pending) == 0 {
				break
			}
			ids := make([]int64, len(pending))
			vecs := make([][]float32, len(pending))
			for i, p := range pending {
				ids[i], vecs[i] = p.Rowid, vec()
			}
			if err = h.st.PutKBChunkVectors(ctx, ids, vecs); err != nil {
				t.Fatal(err)
			}
		}
	}
	st, _ = h.st.KnowledgeStats(ctx, h.dp)
	t.Logf("loaded 40 MB more (%d chunks in all) in %s", st.Chunks, time.Since(start).Round(time.Second))
	time1("knowledgebase_search at 50 MB", search)
	qv := vec()
	time1("  vector scan (k=20)", func(string) error {
		_, err := h.st.SearchKBVectors(ctx, h.dp, qv, 20)
		return err
	})
	time1("  keyword search (20)", func(q string) error {
		_, err := h.st.SearchKBKeywords(ctx, h.dp, q, qv, 20)
		return err
	})
	time1("  rerank 20", func(q string) error {
		_, err := h.svc.eng.Rerank(ctx, q, strings.Split(strings.Repeat(text(1000)+"\x00", 20), "\x00")[:20])
		return err
	})

	// context_search while a 5 MB file indexes.
	h.upload("more.txt", []byte(text(5<<20)))
	done := make(chan error, 1)
	go func() {
		if err := h.svc.IndexQueued(ctx); err != nil {
			done <- err
			return
		}
		done <- h.svc.EmbedPending(ctx)
	}()
	time.Sleep(2 * time.Second) // let embedding start
	time1("context_search while a file indexes", func(q string) error {
		_, err := h.svc.ContextSearch(ctx, ContextSearch{DatapackID: h.dp, Query: q, Limit: 10, Memory: true,
			Conversation: true, KnowledgeBase: true})
		return err
	})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	time1("context_search idle", func(q string) error {
		_, err := h.svc.ContextSearch(ctx, ContextSearch{DatapackID: h.dp, Query: q, Limit: 10, Memory: true,
			Conversation: true, KnowledgeBase: true})
		return err
	})
}
