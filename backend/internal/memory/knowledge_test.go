package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"time"

	"github.com/yugabyte/meko/backend/internal/knowledge"
	"github.com/yugabyte/meko/backend/internal/store"
)

func ptr(f float64) *float64 { return &f }

func TestFuseAndTrim(t *testing.T) {
	r := func() *float64 { v := 1.0; return &v }
	sem := []store.KBHit{{Rowid: 1, ID: "a", Distance: 0.1}, {Rowid: 2, ID: "b", Distance: 0.2}}
	kw := []store.KBHit{{Rowid: 3, ID: "c", Distance: 0.5, Rank: r()}, {Rowid: 2, ID: "b", Distance: 0.2, Rank: r()}}
	got := fuse(sem, kw)
	ids := ""
	for _, g := range got {
		ids += g.ID + ":" + g.MatchType + " "
	}
	// b was found both ways: it's a hybrid match, fused first; then a (semantic first), then c.
	if ids != "b:hybrid a:semantic c:keyword " || got[0].Rank == nil || got[1].Rank != nil ||
		got[0].MetadataFilters == nil {
		t.Errorf("fused %s", ids)
	}

	scored := func(s ...float64) []KBResult {
		out := make([]KBResult, len(s))
		for i, v := range s {
			out[i].RerankScore = ptr(v)
		}
		return out
	}
	for _, c := range []struct {
		scores []float64
		want   int
	}{
		{[]float64{0.9, 0.8, 0.3, 0.2}, 3}, // 0.2 < 0.3 x 0.9
		{[]float64{0.9, 0.2}, 1},           // 0.2 < 0.3 x 0.9
		{[]float64{0.5, 0.4, 0.1}, 2},      // 0.1 < 0.3 x 0.4
		{[]float64{0, 0}, 1},               // nothing scores: keep the best
		{[]float64{0.01}, 1},
	} {
		if n := len(trimResults(scored(c.scores...))); n != c.want {
			t.Errorf("trim %v: kept %d, want %d", c.scores, n, c.want)
		}
	}
}

// upload uploads a file through an upload link, as the REST sink does.
func (h *harness) upload(name string, data []byte) UploadOffer {
	h.t.Helper()
	ctx := context.Background()
	sum := sha256.Sum256(data)
	offer, err := h.svc.UploadURL(ctx, UploadRequest{DatapackID: h.dp, Filename: name, Size: int64(len(data)),
		ContentHash: hex.EncodeToString(sum[:]), AgentID: "test", Head: data[:min(len(data), knowledge.SniffBytes)]})
	if err != nil {
		h.t.Fatal(err)
	}
	if offer.AlreadyUploaded {
		return offer
	}
	token := offer.URL[strings.LastIndex(offer.URL, "/")+1:]
	if err = h.svc.ReceiveUpload(ctx, token, bytes.NewReader(data)); err != nil {
		h.t.Fatal(err)
	}
	return offer
}

// index indexes the queued files and embeds their chunks, as serve's indexer and sweep do.
func (h *harness) index() {
	h.t.Helper()
	ctx := context.Background()
	if err := h.svc.IndexQueued(ctx); err != nil {
		h.t.Fatal(err)
	}
	if err := h.svc.EmbedPending(ctx); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) status(key string) DocumentStatus {
	h.t.Helper()
	st, err := h.svc.DocumentStatus(context.Background(), h.dp, key)
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

func TestKnowledgeFilesThroughTheEngine(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, hashEmbedder)
	doc := []byte("# Ops\n\nThe pager rotation changes every Monday at noon.\n\n## Backups\n\n" +
		"Backups run nightly and are kept for thirty days.\n")
	offer := h.upload("ops.md", doc)
	if offer.AlreadyUploaded || !strings.HasPrefix(offer.Key, h.userID+"/"+h.dp+"/") || offer.Fields["key"] != offer.Key {
		t.Fatalf("offer %+v", offer)
	}
	st := h.status(offer.Key)
	if st.Document == nil || st.Document.Status != store.DocQueued {
		t.Fatalf("queued %+v", st)
	}
	h.index()
	st = h.status(offer.Key)
	if st.Document.Status != store.DocCompleted || st.Document.ChunksProcessed != 2 {
		t.Fatalf("completed %+v", st.Document)
	}
	// Its bytes are in the files folder, owner-only.
	p := filepath.Join(h.l.KnowledgeFiles, h.dp, st.Document.ContentHash)
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file %v %v", fi, err)
	}

	res, err := h.svc.SearchKnowledge(ctx, h.dp, "how long are backups kept", 10)
	if err != nil || len(res) == 0 || !strings.Contains(res[0].ChunkText, "Backups run nightly") ||
		res[0].RerankScore == nil || res[0].DocumentID != st.Document.ID {
		t.Fatalf("search %+v %v", res, err)
	}
	h.svc.kbMaxDistance = 2 // the hashing embedder's distances aren't a model's
	cs, err := h.svc.ContextSearch(ctx, ContextSearch{DatapackID: h.dp, Query: "how long are backups kept",
		Limit: 5, KnowledgeBase: true})
	if err != nil || len(cs.KnowledgeBase) == 0 || cs.KnowledgeChunks != 2 || cs.Memory != nil {
		t.Fatalf("context search %+v %v", cs, err)
	}

	// The same bytes again: nothing to upload.
	again := h.upload("copy.md", doc)
	if !again.AlreadyUploaded || again.ExistingFilename != "ops.md" || again.Key != offer.Key {
		t.Errorf("again %+v", again)
	}
	// A link is spent by a refused file, and the file isn't kept.
	sum := sha256.Sum256([]byte("other"))
	o2, err := h.svc.UploadURL(ctx, UploadRequest{DatapackID: h.dp, Filename: "o.txt", Size: 5,
		ContentHash: hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatal(err)
	}
	token := o2.URL[strings.LastIndex(o2.URL, "/")+1:]
	if w := h.svc.tickets.waiting(h.dp, o2.Key); w == nil {
		t.Error("no waiting link")
	}
	if aw, _ := h.svc.DocumentStatus(ctx, h.dp, o2.Key); aw.WaitingUntil == nil || aw.Document != nil {
		t.Errorf("awaiting %+v", aw)
	}
	if err = h.svc.ReceiveUpload(ctx, token, strings.NewReader("OTHER")); !errors.Is(err, knowledge.ErrInvalid) {
		t.Errorf("wrong bytes: %v", err)
	}
	if err = h.svc.ReceiveUpload(ctx, token, strings.NewReader("other")); !errors.Is(err, ErrTicket) {
		t.Errorf("a spent link: %v", err)
	}
	if _, err = h.svc.DocumentStatus(ctx, h.dp, o2.Key); !errors.Is(err, ErrNotFound) {
		t.Errorf("a spent link's key: %v", err)
	}

	// Sweep: a stray file goes, the document's stays.
	stray := filepath.Join(h.l.KnowledgeFiles, h.dp, strings.Repeat("a", 64))
	if err = os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, serr := h.svc.SweepFiles(ctx); serr != nil || n != 1 {
		t.Errorf("swept %d %v", n, serr)
	}
	if _, err = os.Stat(p); err != nil {
		t.Errorf("the sweep took a document's file: %v", err)
	}

	// Delete takes the bytes along; deleting the datapack takes its folder.
	if n, derr := h.svc.DeleteDocument(ctx, h.dp, st.Document.ID); derr != nil || n != 2 {
		t.Fatalf("delete %d %v", n, derr)
	}
	if _, err = os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("a deleted file's bytes stayed: %v", err)
	}
	h.upload("ops.md", doc)
	if err = h.svc.DeleteDatapack(ctx, h.dp); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(h.l.KnowledgeFiles, h.dp)); !os.IsNotExist(err) {
		t.Errorf("a deleted datapack's folder stayed: %v", err)
	}
}

func TestKnowledgeFilesWaitForTheEngine(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, hashEmbedder)
	h.stopMemoryEngine()
	offer := h.upload("notes.txt", []byte("the office plants are watered on fridays"))
	if err := h.svc.IndexQueued(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.EmbedPending(ctx); err == nil {
		t.Fatal("embedded with the engine down")
	}
	st := h.status(offer.Key)
	if st.Document.Status != store.DocProcessing || st.Document.ChunksProcessed != 0 || *st.Document.ChunksTotal != 1 {
		t.Fatalf("waiting %+v", st.Document)
	}
	// A server stopped here queues it again at the next start.
	if n, err := h.svc.RequeueInterrupted(ctx); err != nil || n != 1 {
		t.Fatalf("requeued %d %v", n, err)
	}
	h.startMemoryEngine()
	h.index()
	if st = h.status(offer.Key); st.Document.Status != store.DocCompleted {
		t.Errorf("after the engine came back %+v", st.Document)
	}
	// A file whose bytes vanished fails, saying why.
	h.upload("gone.txt", []byte("bytes that will vanish"))
	d, _ := h.st.DocumentByKey(ctx, h.dp, h.svc.documentKey(h.dp, shaHex("bytes that will vanish"), "gone.txt"))
	if err := os.Remove(filepath.Join(h.l.KnowledgeFiles, h.dp, d.ContentHash)); err != nil {
		t.Fatal(err)
	}
	h.index()
	if d, _ = h.st.GetDocument(ctx, h.dp, d.ID); d.Status != store.DocFailed || !strings.Contains(d.Error, "missing") {
		t.Errorf("vanished bytes %+v", d)
	}
}

func shaHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// With the real models: the done-when. A file is found by a paraphrased question through
// knowledgebase_search and context_search, a promoted memory is found the same way, the same file
// twice is stored once, and no file text reaches the memory engine's log.
func TestKnowledgeSearchWithTheModels(t *testing.T) {
	if testing.Short() {
		t.Skip("downloads the embedding and reranking models on their first run")
	}
	ctx := context.Background()
	h := newHarness(t, realEmbedder)
	waitReranker(t, h)
	runbook := []byte("# Release runbook\n\n## Schedule\n\nThe release train leaves every Tuesday after the change " +
		"review.\n\n## Rollback\n\nTo undo a bad release, run the rollback script with the previous version tag; it " +
		"redeploys the last good build.\n\n## Lunch\n\nThe cafeteria serves mushroom risotto on Fridays.\n")
	offer := h.upload("runbook.md", runbook)
	h.index()
	if st := h.status(offer.Key); st.Document.Status != store.DocCompleted {
		t.Fatalf("indexed %+v", st.Document)
	}
	// ms-marco-MiniLM scores a paraphrase that shares no words with the text as unrelated ("how do I
	// revert a broken deployment?" got a logit of -11 here), as in Cloud: the query shares a few.
	q := "how do I undo a bad release?"
	res, err := h.svc.SearchKnowledge(ctx, h.dp, q, 10)
	if err != nil || len(res) == 0 {
		t.Fatalf("search %+v %v", res, err)
	}
	for _, r := range res {
		t.Logf("rerank %.3f distance %.3f %s: %.50q", *r.RerankScore, r.Distance, r.MatchType, r.ChunkText)
	}
	if !strings.Contains(res[0].ChunkText, "rollback script") || res[0].Distance > DefaultKBMaxDistance ||
		*res[0].RerankScore < 0.5 || len(res) != 1 {
		t.Errorf("the paraphrase's results: %+v", res)
	}
	cs, err := h.svc.ContextSearch(ctx, ContextSearch{DatapackID: h.dp, Query: q, Limit: 10, KnowledgeBase: true})
	if err != nil || len(cs.KnowledgeBase) == 0 || !strings.Contains(cs.KnowledgeBase[0].ChunkText, "rollback script") {
		t.Errorf("context_search %+v %v", cs.KnowledgeBase, err)
	}
	if again := h.upload("again.md", runbook); !again.AlreadyUploaded {
		t.Error("the same file was stored twice")
	}

	added, err := h.svc.AddMemory(ctx, AddMemory{DatapackID: h.dp, AgentID: "a", ConversationID: "conv",
		Text: "Production database migrations must be reviewed by two people."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.svc.PromoteMemories(ctx, h.dp, "a", []string{added[0].ID}); err != nil {
		t.Fatal(err)
	}
	res, err = h.svc.SearchKnowledge(ctx, h.dp, "who has to approve schema changes in prod?", 10)
	if err != nil || len(res) == 0 || res[0].MetadataFilters["memory_id"] != added[0].ID ||
		res[0].MetadataFilters["source"] != "memory" {
		t.Errorf("the promoted memory: %+v %v", res, err)
	}

	log, err := os.ReadFile(h.l.MemoryEngineLog)
	if err != nil || bytes.Contains(log, []byte("rollback")) || bytes.Contains(log, []byte("migrations")) {
		t.Errorf("file or memory text in memory_engine.log (%v):\n%s", err, log)
	}
}

// waitReranker waits until the engine's reranker has loaded (it loads after the embedder).
func waitReranker(t *testing.T, h *harness) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if st := h.svc.EngineStatus(context.Background()); st.Reranker != nil && st.Reranker.State == "ready" {
			return
		}
	}
	t.Fatalf("the reranker never loaded: %+v", h.svc.EngineStatus(context.Background()))
}
