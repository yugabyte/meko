package mcp_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/testenv"
)

// words is about n bytes of text.
func words(seed string, n int) string {
	return strings.Repeat(seed+" ", n/(len(seed)+1)+1)[:n]
}

// wantFull calls a tool and fails the test unless Meko refused it as full, saying what to do.
func wantFull(t *testing.T, e *testenv.Env, name string, args map[string]any) {
	t.Helper()
	wantFullSaying(t, e, name, args, "link Meko Cloud for larger datasets")
}

func wantFullSaying(t *testing.T, e *testenv.Env, name string, args map[string]any, next string) {
	t.Helper()
	r := call(t, e, name, args)
	detail := str(r.JSON["detail"])
	if r.JSON["error"] != "storage_limit_exceeded" || !strings.Contains(detail, "storage limit is 8 KB") ||
		!strings.Contains(detail, next) || strings.Contains(detail, "max_content_mb") {
		t.Fatalf("%s: want storage_limit_exceeded saying %q, got %s", name, next, r.Text)
	}
}

// The storage limit: every tool that adds content is refused once it would go past it, with an
// error that says so; reads, deletes, a shorter memory and the same artifact bytes again still work,
// and deleting makes room. Upload links hold their file's size.
func TestStorageLimit(t *testing.T) {
	e := testenv.Start(t, testenv.Options{MaxContentBytes: 8 << 10})
	kept := []byte(words("kept", 100))
	ok(t, e, "artifact_put", map[string]any{"filename": "kept.txt", "content_type": "text/plain",
		"content_base64": base64.StdEncoding.EncodeToString(kept), "conversation_id": "c"})
	r := ok(t, e, "memory_add", map[string]any{"text": words("alpha", 6000), "conversation_id": "c"})
	id := str(obj(arr(r.JSON["results"])[0])["id"])
	small := str(obj(arr(ok(t, e, "memory_add", map[string]any{"text": "Deploys go out on Tuesdays.",
		"conversation_id": "c"}).JSON["results"])[0])["id"])
	if id == "" || small == "" {
		t.Fatalf("memory_add: %s", r.Text)
	}

	wantFull(t, e, "memory_add", map[string]any{"text": words("beta", 3000), "conversation_id": "c"})
	conv := str(ok(t, e, "conversation_create", map[string]any{"agent_id": "a", "title": "t",
		"datapack_id": e.DatapackID}).JSON["id"])
	turn := map[string]any{"conversation_id": conv, "agent_id": "a", "datapack_id": e.DatapackID,
		"input": "We deploy on Tuesdays.", "output": "Noted.", "seed": "turn-1"}
	stored := str(ok(t, e, "conversation_add_message", turn).JSON["message_id"])
	wantFull(t, e, "conversation_add_message", map[string]any{"conversation_id": conv, "agent_id": "a",
		"datapack_id": e.DatapackID, "input": words("gamma", 3000), "output": "ok"})
	wantFull(t, e, "artifact_put", map[string]any{"filename": "new.txt", "content_type": "text/plain",
		"content_base64": base64.StdEncoding.EncodeToString([]byte(words("delta", 3000))), "conversation_id": "c"})
	doc := []byte("# Runbook\n\n" + words("epsilon", 3000))
	wantFull(t, e, "knowledgebase_upload_document", uploadArgs("runbook.md", doc))
	wantFull(t, e, "memory_update", map[string]any{"memory_id": id, "text": words("alpha", 9000), "conversation_id": "c"})

	// Filled to exactly the limit: one byte more is refused.
	st, err := e.Service.Storage(context.Background())
	if err != nil || st.LimitBytes != 8<<10 || st.UsedBytes >= st.LimitBytes {
		t.Fatalf("storage %+v, %v", st, err)
	}
	ok(t, e, "artifact_put", map[string]any{"filename": "fill.txt", "content_type": "text/plain",
		"content_base64":  base64.StdEncoding.EncodeToString([]byte(words("fill", int(st.LimitBytes-st.UsedBytes)))),
		"conversation_id": "c"})
	wantFull(t, e, "memory_add", map[string]any{"text": "x", "conversation_id": "c"})
	// A turn sent again (its seed) is answered as the one stored; a new one is refused.
	if again := str(ok(t, e, "conversation_add_message", turn).JSON["message_id"]); again != stored {
		t.Errorf("the same turn again: %s, want %s", again, stored)
	}
	turn["seed"] = "turn-2"
	wantFull(t, e, "conversation_add_message", turn)

	// Still allowed when full: the same artifact again, reads, a promotion, and a memory made shorter.
	ok(t, e, "artifact_put", map[string]any{"filename": "again.txt", "content_type": "text/plain",
		"content_base64": base64.StdEncoding.EncodeToString(kept), "conversation_id": "c"})
	ok(t, e, "memory_search", map[string]any{"query": "alpha", "conversation_id": "c"})
	// Promoting moves a memory's bytes rather than adding them.
	if r = ok(t, e, "memory_promote", map[string]any{"memory_ids": []string{small}, "conversation_id": "c"}); len(
		arr(r.JSON["inserted_ids"])) != 1 {
		t.Fatalf("promote when full: %s", r.Text)
	}
	ok(t, e, "memory_update", map[string]any{"memory_id": id, "text": words("alpha", 5000), "conversation_id": "c"})

	// Deleting makes room for one file. Its upload link holds its size, so a second file doesn't fit,
	// though nothing has been uploaded yet; asking again for the first holds its size once.
	ok(t, e, "memory_delete_by_id", map[string]any{"memory_id": id, "conversation_id": "c"})
	if st, err = e.Service.Storage(context.Background()); err != nil || st.UsedBytes >= st.LimitBytes {
		t.Fatalf("after the delete: %+v, %v", st, err)
	}
	ok(t, e, "knowledgebase_upload_document", uploadArgs("runbook.md", doc))
	ok(t, e, "knowledgebase_upload_document", uploadArgs("runbook.md", doc))
	ok(t, e, "knowledgebase_upload_document", uploadArgs("runbook-copy.md", doc))
	wantFull(t, e, "knowledgebase_upload_document", uploadArgs("second.md", append(doc, " second"...)))
}

// While Meko Cloud is linked, the limit still holds for this Meko's datapacks, and the error says to
// keep new work in Meko Cloud.
func TestStorageLimitWhileLinked(t *testing.T) {
	e := testenv.Start(t, testenv.Options{MaxContentBytes: 8 << 10, MCPCloud: &fakeCloud{linked: true}})
	wantFullSaying(t, e, "memory_add", map[string]any{"text": words("alpha", 9000), "datapack_id": e.DatapackID,
		"conversation_id": "c"}, "keep new work in a Meko Cloud datapack")
}
