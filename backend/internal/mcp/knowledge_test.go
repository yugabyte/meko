package mcp_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/testenv"
)

// postFile posts a file to an upload link as a browser posts one to S3: the fields, then the file.
func postFile(t *testing.T, url string, fields map[string]any, filename string, data []byte) int {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := w.WriteField(k, str(v)); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(data)
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, w.FormDataContentType(), &body) //nolint:gosec,noctx // a test server's URL
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// uploadArgs are knowledgebase_upload_document's arguments for a file.
func uploadArgs(name string, data []byte) map[string]any {
	head := data[:min(len(data), 3072)]
	return map[string]any{"filename": name, "size": len(data), "content_hash": sha(data),
		"content_head_base64": base64.StdEncoding.EncodeToString(head), "conversation_id": "c",
		"agent_id": "claude_code:kb"}
}

// waitIndexed polls knowledgebase_document_status until the file's state is final.
func waitIndexed(t *testing.T, e *testenv.Env, key string) callResult {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		r := ok(t, e, "knowledgebase_document_status", map[string]any{"document_key": key, "conversation_id": "c"})
		if s := r.JSON["state"]; s == "completed" || s == "failed" || time.Now().After(deadline) {
			return r
		}
	}
}

func TestKnowledgeFiles(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	doc := []byte("# Release runbook\n\nThe release train leaves on Tuesdays.\n\n## Rollback\n\n" +
		"To roll back a bad deploy, run the rollback script with the previous version tag.\n")

	// Ask for a link, post the file to it (no token: the ticket is the credential), follow it.
	r := ok(t, e, "knowledgebase_upload_document", uploadArgs("runbook.md", doc))
	url, key := str(r.JSON["url"]), str(r.JSON["key"])
	if r.JSON["already_uploaded"] != false || !strings.HasPrefix(url, e.URL+"/api/v1/knowledge-uploads/") ||
		str(obj(r.JSON["fields"])["key"]) != key || r.JSON["datapack_id"] != e.DatapackID ||
		r.JSON["status_url"] != nil || !strings.HasSuffix(key, "/runbook.md") {
		t.Fatalf("upload offer: %s", r.Text)
	}
	r = ok(t, e, "knowledgebase_document_status", map[string]any{"document_key": key, "conversation_id": "c"})
	if r.JSON["state"] != "awaiting_upload" || obj(r.JSON["upload"])["expires_at"] == nil || r.JSON["document"] != nil {
		t.Errorf("status before the upload: %s", r.Text)
	}
	if code := postFile(t, url, obj(r.JSON["fields"]), "runbook.md", doc); code != http.StatusNoContent {
		t.Fatalf("upload answered %d", code)
	}
	if code := postFile(t, url, nil, "runbook.md", doc); code != http.StatusForbidden {
		t.Errorf("a used link answered %d", code)
	}
	r = waitIndexed(t, e, key)
	d := obj(r.JSON["document"])
	if r.JSON["state"] != "completed" || r.JSON["document_status"] != "COMPLETED" || d["status"] != "COMPLETED" ||
		num(d["chunks_processed"]) < 2 || num(d["size_bytes"]) != float64(len(doc)) || r.JSON["document_name"] != key {
		t.Fatalf("status: %s", r.Text)
	}

	// Found by knowledgebase_search and by context_search's knowledge_base bucket.
	r = ok(t, e, "knowledgebase_search", map[string]any{"query": "how do I roll back a bad deploy",
		"conversation_id": "c", "datapack_id": ""})
	results := arr(r.JSON["results"])
	if len(results) == 0 || num(r.JSON["count"]) != float64(len(results)) {
		t.Fatalf("knowledgebase_search: %s", r.Text)
	}
	top := obj(results[0])
	if !strings.Contains(str(top["chunk_text"]), "roll back a bad deploy") ||
		!strings.Contains(str(top["chunk_text"]), "Section: Release runbook > Rollback") || top["rerank_score"] == nil ||
		top["distance"] == nil || top["document_id"] == nil || top["match_type"] == nil ||
		top["metadata_filters"] == nil {
		t.Errorf("top result: %v", top)
	}
	docID := str(top["document_id"])
	r = ok(t, e, "context_search", map[string]any{"query": "roll back a bad deploy", "conversation_id": "c",
		"force_source": "knowledge_base"})
	kb := arr(obj(r.JSON["results"])["knowledge_base"])
	savings := obj(r.JSON["token_savings"])
	if len(kb) == 0 || obj(savings["knowledge_base"]) == nil || num(savings["total_tokens"]) != float64(len(kb)*1000) {
		t.Errorf("context_search: %s", r.Text)
	}

	// The same bytes again are already uploaded, under the first name.
	r = ok(t, e, "knowledgebase_upload_document", uploadArgs("copy.md", doc))
	if r.JSON["already_uploaded"] != true || r.JSON["url"] != "" || r.JSON["key"] != key ||
		r.JSON["existing_filename"] != "runbook.md" || r.JSON["fields"] != nil {
		t.Errorf("a second upload: %s", r.Text)
	}

	// Promoted memories join the knowledge base.
	m := ok(t, e, "memory_add", map[string]any{"text": "staging deploys need two approvals", "conversation_id": "c",
		"agent_id": "a"})
	mid := str(obj(arr(m.JSON["results"])[0])["id"])
	ok(t, e, "memory_promote", map[string]any{"memory_ids": []string{mid}, "conversation_id": "c", "agent_id": "a"})
	r = ok(t, e, "knowledgebase_search", map[string]any{"query": "staging deploys need two approvals",
		"conversation_id": "c", "datapack_id": e.DatapackID})
	found := false
	for _, it := range arr(r.JSON["results"]) {
		md := obj(obj(it)["metadata_filters"])
		if md["source"] == "memory" && md["memory_id"] == mid {
			found = true
		}
	}
	if !found {
		t.Errorf("the promoted memory isn't found: %s", r.Text)
	}
	r = ok(t, e, "datapack_describe", map[string]any{"conversation_id": "c"})
	chunks := num(r.JSON["knowledge_chunk_count"])
	if chunks < 3 || num(r.JSON["knowledge_count"]) != chunks || num(r.JSON["knowledge_base_file_count"]) != 1 ||
		num(r.JSON["shared_knowledge_count"]) != 2 || num(r.JSON["collective_memory_count"]) != 1 {
		t.Errorf("describe counts: %s", r.Text)
	}

	// Delete: by its UUID, once.
	wantError(t, e, "knowledgebase_delete_document", map[string]any{"document_id": "nope", "conversation_id": "c"},
		"invalid_document_id")
	r = ok(t, e, "knowledgebase_delete_document", map[string]any{"document_id": docID, "conversation_id": "c"})
	if r.JSON["document_id"] != docID || num(r.JSON["chunks_deleted"]) < 2 || r.JSON["s3_deleted"] != true {
		t.Errorf("delete: %s", r.Text)
	}
	r = call(t, e, "knowledgebase_delete_document", map[string]any{"document_id": docID, "conversation_id": "c"})
	if r.JSON["error"] != "NOT_FOUND" || num(r.JSON["status"]) != 404 {
		t.Errorf("delete twice: %s", r.Text)
	}
	r = ok(t, e, "knowledgebase_search", map[string]any{"query": "roll back a bad deploy", "conversation_id": "c",
		"datapack_id": ""})
	for _, it := range arr(r.JSON["results"]) {
		if obj(it)["document_id"] == docID {
			t.Errorf("a deleted file is still found: %s", r.Text)
		}
	}
	wantError(t, e, "knowledgebase_document_status", map[string]any{"document_key": key, "conversation_id": "c"},
		"NOT_FOUND")
}

func TestKnowledgeFileRefusals(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	doc := []byte("plain text for the knowledge base")
	for name, args := range map[string]map[string]any{
		"a type Meko doesn't take": uploadArgs("slides.pptx", doc),
		"a file over 50 MiB": func() map[string]any {
			a := uploadArgs("big.txt", doc)
			a["size"] = 50<<20 + 1
			return a
		}(),
		"a hash that isn't one": func() map[string]any {
			a := uploadArgs("a.txt", doc)
			a["content_hash"] = "abc"
			return a
		}(),
		"a head that doesn't fit the type": uploadArgs("a.pdf", doc),
		"a head shorter than the file's first 3 KB": func() map[string]any {
			a := uploadArgs("a.txt", doc)
			a["content_head_base64"] = base64.StdEncoding.EncodeToString(doc[:3])
			return a
		}(),
	} {
		r := call(t, e, "knowledgebase_upload_document", args)
		if r.JSON["error"] != "VALIDATION_ERROR" || r.JSON["detail"] == "" {
			t.Errorf("%s: %s", name, r.Text)
		}
	}
	a := uploadArgs("a.txt", doc)
	delete(a, "content_head_base64")
	wantError(t, e, "knowledgebase_upload_document", a, "content_head_base64_required")
	a = uploadArgs("a.txt", doc)
	delete(a, "conversation_id")
	wantError(t, e, "knowledgebase_upload_document", a, "conversation_id_required")

	// The sink refuses bytes that aren't the ones the link was asked for, and the link is spent.
	r := ok(t, e, "knowledgebase_upload_document", uploadArgs("a.txt", doc))
	if code := postFile(t, str(r.JSON["url"]), nil, "a.txt", []byte("other bytes of another size")); code != 400 {
		t.Errorf("wrong bytes answered %d", code)
	}
	if code := postFile(t, str(r.JSON["url"]), nil, "a.txt", doc); code != http.StatusForbidden {
		t.Errorf("a spent link answered %d", code)
	}
	// A file with no text fails, saying why.
	blank := []byte("   \n\n   ")
	r = ok(t, e, "knowledgebase_upload_document", uploadArgs("blank.txt", blank))
	if code := postFile(t, str(r.JSON["url"]), nil, "blank.txt", blank); code != http.StatusNoContent {
		t.Fatalf("upload answered %d", code)
	}
	st := waitIndexed(t, e, str(r.JSON["key"]))
	if st.JSON["state"] != "failed" || !strings.Contains(str(st.JSON["message"]), "no text") {
		t.Errorf("a blank file: %s", st.Text)
	}
	// Uploading it again queues it again.
	r = ok(t, e, "knowledgebase_upload_document", uploadArgs("blank.txt", blank))
	if r.JSON["already_uploaded"] != true {
		t.Errorf("a failed file again: %s", r.Text)
	}
}

// knowledgebase_upload_file reads a local file itself: one call, then the same indexing as an
// upload link's.
func TestUploadLocalFile(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	dir := t.TempDir()
	doc := []byte("# Fleet\n\nThe depot keeps twelve trucks for towing.\n")
	path := filepath.Join(dir, "fleet.md")
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	args := func(p string) map[string]any {
		return map[string]any{"path": p, "conversation_id": "c", "agent_id": "claude_code:kb"}
	}
	r := ok(t, e, "knowledgebase_upload_file", args(path))
	key := str(r.JSON["document_key"])
	if r.JSON["already_uploaded"] != false || r.JSON["datapack_id"] != e.DatapackID ||
		!strings.HasSuffix(key, "/"+sha(doc)+"/fleet.md") || r.JSON["filename"] != "fleet.md" ||
		(r.JSON["state"] != "indexing" && r.JSON["state"] != "completed") {
		t.Fatalf("upload: %s", r.Text)
	}
	if st := waitIndexed(t, e, key); st.JSON["state"] != "completed" {
		t.Fatalf("status: %s", st.Text)
	}
	r = ok(t, e, "knowledgebase_search", map[string]any{"query": "how many trucks for towing", "conversation_id": "c",
		"datapack_id": ""})
	if res := arr(r.JSON["results"]); len(res) == 0 || !strings.Contains(str(obj(res[0])["chunk_text"]), "twelve trucks") {
		t.Errorf("search: %s", r.Text)
	}
	if r = ok(t, e, "knowledgebase_upload_file", args(path)); r.JSON["already_uploaded"] != true ||
		r.JSON["document_key"] != key {
		t.Errorf("the same file again: %s", r.Text)
	}

	if err := os.WriteFile(filepath.Join(dir, "slides.pptx"), doc, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{
		"a relative path":          "fleet.md",
		"a missing file":           filepath.Join(dir, "gone.md"),
		"a folder":                 dir + "/sub.md",
		"a type Meko doesn't take": filepath.Join(dir, "slides.pptx"),
	} {
		if name == "a folder" {
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if r := call(t, e, "knowledgebase_upload_file", args(p)); r.JSON["error"] != "VALIDATION_ERROR" ||
			r.JSON["detail"] == "" {
			t.Errorf("%s: %s", name, r.Text)
		}
	}
	wantError(t, e, "knowledgebase_upload_file", args(""), "VALIDATION_ERROR")
	a := args(path)
	delete(a, "conversation_id")
	wantError(t, e, "knowledgebase_upload_file", a, "conversation_id_required")
}
