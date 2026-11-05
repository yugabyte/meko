package store

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// recount sums what content_usage counts from the rows themselves.
func recount(t *testing.T, s *Store) ContentUsage {
	t.Helper()
	var u ContentUsage
	if err := s.r.QueryRow("SELECT "+
		"(SELECT coalesce(sum(octet_length(text) + octet_length(payload)), 0) FROM memories) + "+
		"(SELECT coalesce(sum(octet_length(text) + octet_length(payload)), 0) FROM promoted_memories), "+
		"(SELECT coalesce(sum(octet_length(input) + octet_length(output) + octet_length(reasoning) + "+
		"octet_length(plan) + octet_length(metadata)), 0) FROM messages), "+
		"(SELECT coalesce(sum(size_bytes), 0) FROM kb_documents), "+
		"(SELECT coalesce(sum(size_bytes), 0) FROM artifacts)").
		Scan(&u.Memories, &u.Conversations, &u.Knowledge, &u.Artifacts); err != nil {
		t.Fatal(err)
	}
	return u
}

// sameUsage checks the kept counts against a recount, and returns them.
func sameUsage(t *testing.T, s *Store, step string) ContentUsage {
	t.Helper()
	got, err := s.ContentUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := recount(t, s); got != want {
		t.Fatalf("%s: kept %+v, rows hold %+v", step, got, want)
	}
	return got
}

func TestContentUsageFollowsWrites(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	if u := sameUsage(t, s, "empty"); u.Total() != 0 {
		t.Fatalf("empty: %+v", u)
	}

	// Memories: added by the memory engine through the store API, updated, promoted, deleted.
	insert(t, s, Memories, "a", "m1", unit(1, 0, 0), map[string]any{"data": "naïve text", "user_id": "u", "tag": "t"})
	insert(t, s, Memories, "a", "m2", unit(0, 1, 0), map[string]any{"data": "second", "user_id": "u"})
	insert(t, s, Memories, "b", "m3", unit(0, 0, 1), map[string]any{"data": "in b", "user_id": "u"})
	if u := sameUsage(t, s, "memories added"); u.Memories == 0 {
		t.Fatal("memories added count nothing")
	}
	if err := s.UpdateVector(ctx, Memories, "a", "m1", unit(1, 1, 0),
		map[string]any{"data": strings.Repeat("longer text ", 10), "user_id": "u"}); err != nil {
		t.Fatal(err)
	}
	sameUsage(t, s, "memory updated")
	if _, err := s.CopyToPromoted(ctx, "a", "u", "m2", "p"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteVector(ctx, Memories, "a", "m2"); err != nil {
		t.Fatal(err)
	}
	sameUsage(t, s, "memory promoted")

	// A conversation's turns, then the conversation deleted (a cascade).
	for _, dp := range []string{"a", "b"} {
		c := Conversation{ID: "c1", DatapackID: dp, AgentID: "ag", Title: "t"}
		if _, _, err := s.CreateConversation(ctx, c); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"t1", "t2"} {
			if _, _, err := s.AddMessage(ctx, dp, Message{ID: id, ConversationID: "c1", Input: "question " + id,
				Output: "answer " + id, Reasoning: "why", Metadata: map[string]any{"k": "v"}}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if u := sameUsage(t, s, "turns added"); u.Conversations == 0 {
		t.Fatal("turns count nothing")
	}
	if err := s.DeleteConversation(ctx, "b", "c1"); err != nil {
		t.Fatal(err)
	}
	sameUsage(t, s, "conversation deleted")

	// Artifacts: the same bytes again add nothing.
	for range 2 {
		if _, _, err := s.PutArtifact(ctx, "a", "ag", "f.txt", "text/plain", []byte("artifact bytes")); err != nil {
			t.Fatal(err)
		}
	}
	if u := sameUsage(t, s, "artifact put"); u.Artifacts != int64(len("artifact bytes")) {
		t.Fatalf("artifact counted %d", u.Artifacts)
	}

	// Knowledge files, by their size.
	doc, _, err := s.AddDocument(ctx, Document{DatapackID: "a", Key: "u/a/h/f.md", Filename: "f.md",
		DocumentType: "text/markdown", ContentHash: strings.Repeat("ab", 32), SizeBytes: 1234})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.AddDocument(ctx, Document{DatapackID: "b", Key: "u/b/h/g.md", Filename: "g.md",
		DocumentType: "text/markdown", ContentHash: strings.Repeat("cd", 32), SizeBytes: 99}); err != nil {
		t.Fatal(err)
	}
	if u := sameUsage(t, s, "documents added"); u.Knowledge != 1234+99 {
		t.Fatalf("knowledge counted %d", u.Knowledge)
	}
	// Indexed (an update that leaves the size as it is), then deleted.
	if _, err = s.w.ExecContext(ctx, "UPDATE kb_documents SET status = 'COMPLETED' WHERE id = ?", doc.ID); err != nil {
		t.Fatal(err)
	}
	sameUsage(t, s, "document indexed")
	if _, _, err = s.DeleteDocument(ctx, "a", doc.ID); err != nil {
		t.Fatal(err)
	}
	sameUsage(t, s, "document deleted")

	// Deleting a datapack takes everything in it off, through the cascades.
	if err = s.DeleteDatapack(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	sameUsage(t, s, "datapack a deleted")
	if err = s.DeleteDatapack(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if u := sameUsage(t, s, "every datapack deleted"); u.Total() != 0 {
		t.Fatalf("left %+v", u)
	}
}

// An install upgraded from before content_usage counts what it already holds.
func TestContentUsageFilledOnUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "meko.db")
	latest, err := LatestSchema()
	if err != nil {
		t.Fatal(err)
	}
	old := migrationSource
	t.Cleanup(func() { migrationSource = old })
	before := fstest.MapFS{}
	ms, _ := fs.ReadDir(old, ".")
	for _, e := range ms {
		if strings.HasPrefix(e.Name(), "0014_") {
			break
		}
		b, _ := fs.ReadFile(old, e.Name())
		before[e.Name()] = &fstest.MapFile{Data: b}
	}
	migrationSource = before
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO datapacks(id, name, created_at, updated_at) VALUES ('a', 'a', '', '')",
		"INSERT INTO memories(id, datapack_id, text, payload, created_at, updated_at) " +
			"VALUES ('m1', 'a', 'twelve bytes', '{\"k\":1}', '', '')",
		"INSERT INTO conversations(id, datapack_id, agent_id, title, created_at, updated_at) " +
			"VALUES ('c1', 'a', 'ag', 't', '', '')",
		"INSERT INTO messages(id, conversation_rowid, input, output, created_at) VALUES ('t1', 1, 'in', 'out', '')",
		"INSERT INTO artifacts(id, datapack_id, content_hash, filename, content_type, size_bytes, bytes, created_at) " +
			"VALUES ('x', 'a', 'h', 'f', 'text/plain', 5, x'0102030405', '')",
		"INSERT INTO kb_documents(id, datapack_id, document_key, filename, document_type, content_hash, size_bytes, " +
			"uploaded_at) VALUES ('d', 'a', 'k', 'f.md', 'text/markdown', 'h', 700, '')",
	} {
		if _, err = s.w.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	migrationSource = old
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.SchemaVersion(ctx); v != latest {
		t.Fatalf("version %d, want %d", v, latest)
	}
	u := sameUsage(t, s, "upgraded")
	if want := (ContentUsage{Memories: 12 + 7, Conversations: 2 + 3 + 2 + 2, Knowledge: 700, Artifacts: 5}); u != want {
		t.Fatalf("filled %+v, want %+v", u, want)
	}
}
