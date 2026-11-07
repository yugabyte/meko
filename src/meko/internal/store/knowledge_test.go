package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

func addDoc(t *testing.T, s *Store, dp, hash, name string) Document {
	t.Helper()
	d, what, err := s.AddDocument(context.Background(), Document{DatapackID: dp, Key: "u/" + dp + "/" + hash + "/" + name,
		Filename: name, DocumentType: "text/plain", ContentHash: hash, SizeBytes: 10, UploadedBy: "ag"})
	if err != nil || what != AddedNew {
		t.Fatalf("add %s: %v %v", name, what, err)
	}
	return d
}

func hashOf(c byte) string { return strings.Repeat(string(c), 64) }

// indexDoc claims the next queued file, chunks it and embeds the chunks with vecs.
func indexDoc(t *testing.T, s *Store, texts []string, vecs [][]float32) Document {
	t.Helper()
	ctx := context.Background()
	d, err := s.ClaimDocument(ctx)
	if err != nil || d == nil {
		t.Fatalf("claim: %v %v", d, err)
	}
	if err = s.AddChunks(ctx, *d, 0, texts, true); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingKBChunks(ctx, len(texts))
	if err != nil {
		t.Fatal(err)
	}
	rowids := make([]int64, 0, len(pending))
	for _, c := range pending {
		rowids = append(rowids, c.Rowid)
	}
	if err = s.PutKBChunkVectors(ctx, rowids, vecs[:len(rowids)]); err != nil {
		t.Fatal(err)
	}
	out, err := s.GetDocument(ctx, d.DatapackID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDocumentsLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	d := addDoc(t, s, "a", hashOf('1'), "notes.txt")
	if d.Status != DocQueued || d.ID == "" || d.ChunksTotal != nil {
		t.Fatalf("added %+v", d)
	}
	// The same bytes again are the same file; another datapack keeps its own copy.
	again, what, err := s.AddDocument(ctx, Document{DatapackID: "a", Key: "other", Filename: "copy.txt",
		DocumentType: "text/plain", ContentHash: hashOf('1'), SizeBytes: 10})
	if err != nil || what != AddedExisting || again.ID != d.ID || again.Filename != "notes.txt" {
		t.Fatalf("again %+v %v %v", again, what, err)
	}
	addDoc(t, s, "b", hashOf('1'), "notes.txt")
	_, _, err = s.AddDocument(ctx, Document{DatapackID: "nope", ContentHash: hashOf('2')})
	if !errors.Is(err, ErrNoDatapack) {
		t.Errorf("a missing datapack: %v", err)
	}

	// Claimed in order; chunks unembedded until their vectors land; COMPLETED with the last one.
	c, err := s.ClaimDocument(ctx)
	if err != nil || c.ID != d.ID || c.Status != DocProcessing {
		t.Fatalf("claim %+v %v", c, err)
	}
	if err = s.AddChunks(ctx, *c, 0, []string{"alpha beta", "gamma delta"}, false); err != nil {
		t.Fatal(err)
	}
	if err = s.AddChunks(ctx, *c, 2, []string{"epsilon"}, true); err != nil {
		t.Fatal(err)
	}
	pending, _ := s.PendingKBChunks(ctx, 2)
	if len(pending) != 2 || pending[0].Text != "alpha beta" {
		t.Fatalf("pending %+v", pending)
	}
	if err = s.PutKBChunkVectors(ctx, []int64{pending[0].Rowid, pending[1].Rowid},
		[][]float32{unit(1, 0, 0), unit(0, 1, 0)}); err != nil {
		t.Fatal(err)
	}
	mid, _ := s.GetDocument(ctx, "a", d.ID)
	if mid.Status != DocProcessing || mid.ChunksProcessed != 2 || *mid.ChunksTotal != 3 {
		t.Fatalf("part way %+v", mid)
	}
	// Only embedded chunks are found, by vector or by word.
	if hits, _ := s.SearchKBKeywords(ctx, "a", "epsilon", unit(1, 0, 0), 10); len(hits) != 0 {
		t.Errorf("an unembedded chunk was found: %+v", hits)
	}
	pending, _ = s.PendingKBChunks(ctx, 10)
	if err = s.PutKBChunkVectors(ctx, []int64{pending[0].Rowid}, [][]float32{unit(0, 0, 1)}); err != nil {
		t.Fatal(err)
	}
	done, _ := s.GetDocument(ctx, "a", d.ID)
	if done.Status != DocCompleted || done.ChunksProcessed != 3 || done.CompletedAt == nil {
		t.Fatalf("done %+v", done)
	}
	if _, what, _ = s.AddDocument(ctx, Document{DatapackID: "a", ContentHash: hashOf('1')}); what != AddedExisting {
		t.Errorf("a completed file again: %v", what)
	}

	// Vector and keyword search, within the datapack, with document ids and distances.
	hits, err := s.SearchKBVectors(ctx, "a", unit(0, 1, 0), 2)
	if err != nil || len(hits) != 2 || hits[0].Text != "gamma delta" || hits[0].DocumentID != d.ID ||
		hits[0].Distance > 1e-6 || hits[0].Rank != nil || hits[1].Distance < 0.99 {
		t.Fatalf("vector hits %+v %v", hits, err)
	}
	hits, err = s.SearchKBKeywords(ctx, "a", "Is there any Gamma?", unit(1, 0, 0), 10)
	if err != nil || len(hits) != 1 || hits[0].Text != "gamma delta" || hits[0].Rank == nil || *hits[0].Rank <= 0 ||
		hits[0].Distance < 0.99 || hits[0].Distance > 1.01 {
		t.Fatalf("keyword hits %+v %v", hits, err)
	}
	// Stemmed: "deltas" finds "delta".
	if hits, _ = s.SearchKBKeywords(ctx, "a", "deltas", unit(1, 0, 0), 10); len(hits) != 1 {
		t.Errorf("stemming: %+v", hits)
	}
	if hits, _ = s.SearchKBKeywords(ctx, "b", "gamma", unit(1, 0, 0), 10); len(hits) != 0 {
		t.Errorf("another datapack's chunks: %+v", hits)
	}
	if hits, _ = s.SearchKBKeywords(ctx, "a", "the and of", unit(1, 0, 0), 10); len(hits) != 0 {
		t.Errorf("stop words matched: %+v", hits)
	}

	// Stats.
	st, err := s.KnowledgeStats(ctx, "a")
	if err != nil || !st.HasBase || st.Files != 1 || st.Chunks != 3 || st.FileChunks != 3 || st.Bytes != 10 ||
		st.Heaviest != "notes.txt" || st.HeaviestChunks != 3 || st.InFlight != 0 || st.LastUploadAt == nil {
		t.Fatalf("stats %+v %v", st, err)
	}

	// A file in flight can't be deleted; a finished one goes with its chunks, vectors and terms.
	inFlight, _, _ := s.AddDocument(ctx, Document{DatapackID: "a", Key: "k2", Filename: "two.md",
		DocumentType: "text/markdown", ContentHash: hashOf('2'), SizeBytes: 5})
	if _, _, err = s.DeleteDocument(ctx, "a", inFlight.ID); !errors.Is(err, ErrDocumentProcessing) {
		t.Errorf("deleting a queued file: %v", err)
	}
	gone, n, err := s.DeleteDocument(ctx, "a", d.ID)
	if err != nil || n != 3 || gone.ContentHash != hashOf('1') {
		t.Fatalf("delete %d %v", n, err)
	}
	if hits, _ = s.SearchKBVectors(ctx, "a", unit(0, 1, 0), 10); len(hits) != 0 {
		t.Errorf("a deleted file's vectors: %+v", hits)
	}
	if hits, _ = s.SearchKBKeywords(ctx, "a", "gamma", unit(1, 0, 0), 10); len(hits) != 0 {
		t.Errorf("a deleted file's terms: %+v", hits)
	}
	if _, _, err = s.DeleteDocument(ctx, "a", d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}
	if ok, _ := s.HasDocumentFile(ctx, "a", hashOf('1')); ok {
		t.Error("a deleted file's row is still there")
	}
	if ok, _ := s.HasDocumentFile(ctx, "a", hashOf('2')); !ok {
		t.Error("a queued file's row is missing")
	}
}

func TestDocumentsFailRequeueAndRecover(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	d := addDoc(t, s, "a", hashOf('1'), "bad.pdf")
	c, _ := s.ClaimDocument(ctx)
	if err := s.AddChunks(ctx, *c, 0, []string{"partial"}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.FailDocument(ctx, c.Rowid, "the PDF is encrypted"); err != nil {
		t.Fatal(err)
	}
	f, _ := s.GetDocument(ctx, "a", d.ID)
	if f.Status != DocFailed || f.Error != "the PDF is encrypted" {
		t.Fatalf("failed %+v", f)
	}
	if p, _ := s.PendingKBChunks(ctx, 10); len(p) != 0 {
		t.Errorf("a failed file's chunks stayed: %+v", p)
	}
	// AddChunks refuses a file that isn't PROCESSING.
	if err := s.AddChunks(ctx, *c, 1, []string{"late"}, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("chunks for a failed file: %v", err)
	}
	// Uploaded again, a failed file is queued again.
	r, what, err := s.AddDocument(ctx, Document{DatapackID: "a", ContentHash: hashOf('1')})
	if err != nil || what != AddedRequeued || r.Status != DocQueued || r.Error != "" {
		t.Fatalf("requeued %+v %v %v", r, what, err)
	}
	// A server stopped part way: PROCESSING goes back to QUEUED without its chunks.
	c, _ = s.ClaimDocument(ctx)
	_ = s.AddChunks(ctx, *c, 0, []string{"half"}, false)
	n, err := s.RequeueProcessing(ctx)
	if err != nil || n != 1 {
		t.Fatalf("requeue %d %v", n, err)
	}
	if p, _ := s.PendingKBChunks(ctx, 10); len(p) != 0 {
		t.Errorf("a requeued file's chunks stayed: %+v", p)
	}
	if c, _ = s.ClaimDocument(ctx); c == nil || c.ID != d.ID {
		t.Errorf("claim after requeue: %+v", c)
	}
	if c, _ = s.ClaimDocument(ctx); c != nil {
		t.Errorf("claimed with nothing queued: %+v", c)
	}
}

func TestListDocumentsAndActivity(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	addDoc(t, s, "a", hashOf('1'), "one.md")
	two := addDoc(t, s, "a", hashOf('2'), "two.TXT")
	addDoc(t, s, "a", hashOf('3'), "50%_off.md")
	addDoc(t, s, "b", hashOf('4'), "elsewhere.md")
	page, total, err := s.ListDocuments(ctx, DocumentPage{DatapackID: "a", Limit: 2})
	if err != nil || total != 3 || len(page) != 2 || page[0].Filename != "50%_off.md" {
		t.Fatalf("page %+v %d %v", page, total, err)
	}
	if _, total, _ = s.ListDocuments(ctx, DocumentPage{DatapackID: "a", TypeSuffix: ".txt", Limit: 10}); total != 1 {
		t.Errorf("by type: %d", total)
	}
	if _, total, _ = s.ListDocuments(ctx, DocumentPage{DatapackID: "a", TypeSuffix: "%", Limit: 10}); total != 0 {
		t.Errorf("a LIKE wildcard in the type matched: %d", total)
	}
	if _, total, _ = s.ListDocuments(ctx, DocumentPage{DatapackID: "a", Query: "ONE", Limit: 10}); total != 1 {
		t.Errorf("by name: %d", total)
	}
	if _, total, _ = s.ListDocuments(ctx, DocumentPage{DatapackID: "a", Status: DocCompleted, Limit: 10}); total != 0 {
		t.Errorf("by status: %d", total)
	}
	if got, _ := s.DocumentByKey(ctx, "a", two.Key); got.ID != two.ID {
		t.Errorf("by key: %+v", got)
	}
	if _, err = s.DocumentByKey(ctx, "b", two.Key); !errors.Is(err, ErrNotFound) {
		t.Errorf("another datapack's key: %v", err)
	}

	// Promotions join the activity feed, newest first, a page at a time.
	insert(t, s, Memories, "a", "m1", unit(1, 0, 0), map[string]any{"data": "a fact", "user_id": "u",
		"agent_id": "ag", "meko_conversation_id": "conv"})
	if _, err = s.CopyToPromoted(ctx, "a", "u", "m1", "ag"); err != nil {
		t.Fatal(err)
	}
	acts, cur, err := s.KBActivity(ctx, "a", nil, 3)
	if err != nil || len(acts) != 3 || acts[0].Type != "memory_promotion" || acts[0].Content != "a fact" ||
		acts[1].Type != "file_upload" {
		t.Fatalf("activity %+v %v", acts, err)
	}
	rest, _, err := s.KBActivity(ctx, "a", cur, 3)
	if err != nil || len(rest) != 1 || rest[0].DocumentKey == "" {
		t.Fatalf("next page %+v %v", rest, err)
	}
	if err = s.MarkKnowledgeBase(ctx, "nope"); !errors.Is(err, ErrNoDatapack) {
		t.Errorf("mark a missing datapack: %v", err)
	}
}

func TestPromotedMemoriesJoinTheKnowledgeBase(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	insert(t, s, Memories, "a", "m1", unit(0, 1, 0), map[string]any{"data": "deploys need two approvals",
		"user_id": "u", "agent_id": "ag", "meko_conversation_id": "conv1"})
	p, err := s.CopyToPromoted(ctx, "a", "u", "m1", "ag")
	if err != nil || p.PromotedToKBAt == nil {
		t.Fatalf("promoted %+v %v", p, err)
	}
	// The memory's own vector came along: found at once, with its memory id and source.
	if err = s.DeleteVector(ctx, Memories, "a", "m1"); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchKBVectors(ctx, "a", unit(0, 1, 0), 5)
	if err != nil || len(hits) != 1 || hits[0].Text != "deploys need two approvals" || hits[0].DocumentID != "conv1" ||
		hits[0].Metadata["source"] != "memory" || hits[0].Metadata["memory_id"] != "m1" ||
		hits[0].Metadata["agent_id"] != "ag" || hits[0].ID == "m1" || hits[0].Distance > 1e-6 {
		t.Fatalf("hits %+v %v", hits, err)
	}
	if hits, _ = s.SearchKBKeywords(ctx, "a", "approvals", unit(1, 0, 0), 5); len(hits) != 1 {
		t.Errorf("keyword: %+v", hits)
	}
	st, _ := s.KnowledgeStats(ctx, "a")
	if st.Chunks != 1 || st.FileChunks != 0 || st.HasBase {
		t.Errorf("stats %+v", st)
	}
	// Deleting the promoted memory takes its chunk along.
	if err = s.DeletePromoted(ctx, "a", "m1"); err != nil {
		t.Fatal(err)
	}
	if hits, _ = s.SearchKBVectors(ctx, "a", unit(0, 1, 0), 5); len(hits) != 0 {
		t.Errorf("a deleted promoted memory is still found: %+v", hits)
	}
	if hits, _ = s.SearchKBKeywords(ctx, "a", "approvals", unit(1, 0, 0), 5); len(hits) != 0 {
		t.Errorf("a deleted promoted memory's terms: %+v", hits)
	}
}

func TestKBTerms(t *testing.T) {
	got := strings.Join(kbTerms("How do I deploy the Meko server? Don't restart it; deploy-it twice."), ",")
	if got != "deploy,meko,server,restart,twice" {
		t.Errorf("terms %s", got)
	}
}

func TestKeywordsDropTermsInHalfTheChunks(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	addDoc(t, s, "a", hashOf('1'), "a.txt")
	texts := make([]string, minWeighableRows)
	vecs := make([][]float32, len(texts))
	for i := range texts {
		texts[i], vecs[i] = "common filler", unit(1, 0, 0)
	}
	texts[0] = "common rare"
	indexDoc(t, s, texts, vecs)
	hits, err := s.SearchKBKeywords(ctx, "a", "common rare", unit(1, 0, 0), 10)
	if err != nil || len(hits) != 1 || hits[0].Text != "common rare" {
		t.Errorf("hits %+v %v", hits, err)
	}
	// Below the threshold, every term counts.
	small := openMem(t)
	addDoc(t, small, "a", hashOf('1'), "a.txt")
	indexDoc(t, small, []string{"common rare", "common"}, [][]float32{unit(1, 0, 0), unit(1, 0, 0)})
	if hits, _ = small.SearchKBKeywords(ctx, "a", "common", unit(1, 0, 0), 10); len(hits) != 2 {
		t.Errorf("small knowledge base: %+v", hits)
	}
}

// Memories promoted before 0006_knowledge.sql were text only: the migration gives each its chunk, which
// the embedder sweep embeds.
func TestMigrationGivesEarlierPromotionsTheirChunks(t *testing.T) {
	ctx := context.Background()
	full := migrationSource
	t.Cleanup(func() { migrationSource = full })
	upTo5 := fstest.MapFS{}
	for i := 1; i <= 5; i++ {
		matches, err := fs.Glob(full, fmt.Sprintf("%04d_*.sql", i))
		if err != nil || len(matches) != 1 {
			t.Fatal(matches, err)
		}
		b, _ := fs.ReadFile(full, matches[0])
		upTo5[matches[0]] = &fstest.MapFile{Data: b}
	}
	migrationSource = upTo5
	s := open(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// The context engine said hello on this database from before 0006, so its model is recorded;
	// there's no kb_chunks_vec, which the store makes for 0006's chunks.
	for k, v := range map[string]string{keyEmbedderProvider: testModel.Provider, keyEmbedderModel: testModel.Model,
		keyEmbeddingDims: fmt.Sprint(testModel.Dims)} {
		if _, err := s.w.Exec("INSERT INTO settings(key, value) VALUES (?, ?)", k, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.w.Exec("INSERT INTO datapacks(id, name, created_at, updated_at) " +
		"VALUES ('a', 'dp-a', '', '')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec("INSERT INTO promoted_memories(id, datapack_id, agent_id, conversation_id, text, created_at, "+
		"updated_at, promoted_at) VALUES ('m1', 'a', 'ag', 'conv', 'an old promotion', ?1, ?1, ?1)",
		"2026-10-01T00:00:00.000000000Z"); err != nil {
		t.Fatal(err)
	}
	migrationSource = full
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// The upgrade made kb_chunks_vec at once, with no hello: a promotion before the context engine is
	// back copies its vector instead of failing on a missing table.
	var n int
	err := s.r.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'kb_chunks_vec'").Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("kb_chunks_vec after the upgrade: %d %v", n, err)
	}
	pending, err := s.PendingKBChunks(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Text != "an old promotion" {
		t.Fatalf("pending %+v %v", pending, err)
	}
	if err = s.PutKBChunkVectors(ctx, []int64{pending[0].Rowid}, [][]float32{unit(1, 0, 0)}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchKBVectors(ctx, "a", unit(1, 0, 0), 5)
	if err != nil || len(hits) != 1 || hits[0].Metadata["memory_id"] != "m1" || hits[0].DocumentID != "conv" ||
		!regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(hits[0].ID) {
		t.Fatalf("hits %+v %v", hits, err)
	}
	ps, _, err := s.PromotedMemories(ctx, "a", 10, 0)
	if err != nil || len(ps) != 1 || ps[0].PromotedToKBAt == nil {
		t.Errorf("promoted_to_kb_at wasn't set: %+v %v", ps, err)
	}
}
