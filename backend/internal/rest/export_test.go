package rest_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/store"
	"github.com/yugabyte/meko/backend/internal/testenv"
)

// An export is one zip of a datapack, ids and dates kept: a JSON Lines file per kind (memories with
// their state, history and vector; collective memories; conversations with every turn and its
// chunks; knowledge files with their chunks; artifacts; the call log), the knowledge files' and
// artifacts' own bytes, and datapack.json, which counts them and names the embedding model.
func TestExportDatapack(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	add := func(text string) string {
		t.Helper()
		a, err := e.Service.AddMemory(ctx, memory.AddMemory{DatapackID: e.DatapackID, AgentID: "claude_code:x",
			Text: text})
		if err != nil || len(a) != 1 {
			t.Fatal(a, err)
		}
		return a[0].ID
	}
	learning, kept, promoted := add("a learning"), add("kept for later"), add("shared with every agent")
	halfway := add("promotion stopped partway")
	// A promotion that copied the memory but never deleted it leaves it in both tables.
	if _, err := e.Store.CopyToPromoted(ctx, e.DatapackID, e.UserID, halfway, "a"); err != nil {
		t.Fatal(err)
	}
	if err := e.Service.UpdateMemory(ctx, e.DatapackID, learning, "a learning, edited"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Service.KeepMemories(ctx, e.DatapackID, []string{kept}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Service.PromoteMemories(ctx, e.DatapackID, "", []string{promoted}); err != nil {
		t.Fatal(err)
	}
	conv, err := e.Service.CreateConversation(ctx, memory.CreateConversation{AgentID: "claude_code:x",
		DatapackID: e.DatapackID, SessionID: "s", Title: "release planning"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Service.AddMessage(ctx, memory.AddMessage{ConversationID: conv.ID, AgentID: "claude_code:x",
		DatapackID: e.DatapackID, Input: "when do we ship releases?", // long enough to be chunked
		Output:    "Releases ship every Tuesday, once the release team signs off on the build.",
		Reasoning: "the calendar", Plan: []string{"look"}, Seed: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Service.PutArtifact(ctx, e.DatapackID, "claude_code:x", "notes.md", "text/markdown",
		[]byte("# notes")); err != nil {
		t.Fatal(err)
	}
	doc := []byte("# Deploys\n\nProduction deploys need two approvals from the release team.\n")
	uploadFile(t, e, "deploys.md", doc)
	// A file whose bytes are gone (deleted after the snapshot listed it) goes out without them.
	gone := []byte("# Gone\n\nThis file's bytes were removed from disk.\n")
	uploadFile(t, e, "gone.md", gone)
	if err = os.Remove(filepath.Join(e.Layout.KnowledgeFiles, e.DatapackID, sha(gone))); err != nil {
		t.Fatal(err)
	}
	// The sweep embeds turns and promoted memories in the background: wait for both.
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		cs, cerr := e.Store.MessageChunks(ctx, e.DatapackID, conv.ID)
		v, verr := e.Store.PromotedEmbedding(ctx, e.DatapackID, promoted)
		if cerr != nil || verr != nil {
			t.Fatal(cerr, verr)
		}
		turn := false
		for _, chunks := range cs {
			turn = turn || len(chunks) > 0 && chunks[0].Embedding != nil
		}
		if turn && v != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sweep never embedded the turn and the collective memory: %v %v", cs, v != nil)
		}
	}
	// A call's kept contents are never exported.
	if err = e.Service.SetKeepCallContents(ctx, true); err != nil {
		t.Fatal(err)
	}
	e.Service.RecordCall(ctx, store.Call{At: time.Now(), Tool: "memory_search", DatapackID: e.DatapackID,
		AgentID: "claude_code:x", OK: true, Ms: 12}, json.RawMessage(`{"query":"kept-query-text"}`),
		"kept-reply-text")

	req, _ := http.NewRequest("GET", e.URL+"/api/v1/datapacks/"+e.DatapackID+"/export", nil)
	req.Header.Set("Authorization", "Bearer "+testenv.ClientToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), `filename="meko-default-`) ||
		!strings.HasSuffix(resp.Header.Get("Content-Disposition"), `.zip"`) {
		t.Fatalf("%d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition"))
	}
	z, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	files := map[string][]byte{}
	for _, f := range z.File {
		rc, _ := f.Open()
		files[f.Name], _ = io.ReadAll(rc)
		_ = rc.Close()
	}
	lines := func(name string) []map[string]any {
		t.Helper()
		var out []map[string]any
		for _, l := range strings.Split(strings.TrimSpace(string(files[name])), "\n") {
			if l == "" {
				continue
			}
			var m map[string]any
			if jerr := json.Unmarshal([]byte(l), &m); jerr != nil {
				t.Fatalf("%s: %v\n%s", name, jerr, l)
			}
			out = append(out, m)
		}
		return out
	}
	var x map[string]any
	if err = json.Unmarshal(files["datapack.json"], &x); err != nil {
		t.Fatalf("datapack.json: %v\n%s", err, files["datapack.json"])
	}
	dims := int(num(obj(x["embedding"])["dims"]))
	counts := obj(x["counts"])
	if x["format"] != "meko-datapack-export" || num(x["version"]) != 2 || dims == 0 ||
		obj(x["datapack"])["id"] != e.DatapackID || num(counts["memories"]) != 2 || num(counts["calls"]) != 1 {
		t.Errorf("header %v", x)
	}
	// Every vector is the model's length.
	vector := func(what string, v any) {
		t.Helper()
		if len(arr(v)) != dims {
			t.Errorf("%s: a %d-long vector, want %d", what, len(arr(v)), dims)
		}
	}
	byID := map[string]map[string]any{}
	for _, m := range lines("memories.jsonl") {
		byID[str(m["id"])] = m
		vector("memory", m["embedding"])
	}
	if len(byID) != 2 || byID[learning]["state"] != "learning" || byID[kept]["state"] != "kept" ||
		byID[learning]["text"] != "a learning, edited" || byID[learning]["agent_id"] != "claude_code:x" ||
		len(arr(byID[learning]["history"])) != 2 {
		t.Errorf("memories %s", files["memories.jsonl"])
	}
	if byID[halfway] != nil {
		t.Error("a half-promoted memory is exported twice")
	}
	collective := map[string]map[string]any{}
	for _, c := range lines("collective_memories.jsonl") {
		collective[str(c["id"])] = c
	}
	vector("collective memory", collective[promoted]["embedding"])
	if len(collective) != 2 || collective[promoted]["promoted_at"] == nil || collective[halfway] == nil {
		t.Errorf("collective %s", files["collective_memories.jsonl"])
	}
	cs := lines("conversations.jsonl")
	if len(cs) != 1 || cs[0]["title"] != "release planning" || len(arr(cs[0]["turns"])) != 1 ||
		obj(arr(cs[0]["turns"])[0])["reasoning"] != "the calendar" {
		t.Errorf("conversations %s", files["conversations.jsonl"])
	} else if chunks := arr(obj(arr(cs[0]["turns"])[0])["chunks"]); len(chunks) == 0 {
		t.Errorf("a turn without chunks: %s", files["conversations.jsonl"])
	} else {
		vector("turn chunk", obj(chunks[0])["embedding"])
	}
	kb := map[string]map[string]any{}
	for _, k := range lines("knowledge.jsonl") {
		kb[str(k["filename"])] = k
	}
	if k := kb["deploys.md"]; len(kb) != 2 || k == nil || len(arr(k["chunks"])) == 0 ||
		!bytes.Equal(files[str(k["path"])], doc) {
		t.Errorf("knowledge %s", files["knowledge.jsonl"])
	} else {
		vector("knowledge chunk", obj(arr(k["chunks"])[0])["embedding"])
	}
	if k := kb["gone.md"]; k == nil || k["path"] != "" || files["knowledge/"+sha(gone)+".md"] != nil {
		t.Errorf("a file whose bytes are gone: %v", k)
	}
	a := lines("artifacts.jsonl")
	if len(a) != 1 || a[0]["filename"] != "notes.md" || string(files[str(a[0]["path"])]) != "# notes" ||
		!strings.HasPrefix(str(a[0]["path"]), "artifacts/") {
		t.Errorf("artifacts %s", files["artifacts.jsonl"])
	}
	for name, b := range files {
		if bytes.Contains(b, []byte("kept-query-text")) || bytes.Contains(b, []byte("kept-reply-text")) {
			t.Errorf("%s holds a call's contents", name)
		}
	}
	if calls := lines("call_log.jsonl"); len(calls) != 1 || calls[0]["tool"] != "memory_search" {
		t.Errorf("call log %s", files["call_log.jsonl"])
	}
	wantErr(t, e, "GET", "/datapacks/00000000-0000-0000-0000-000000000000/export", nil, 404, "NOT_FOUND")

	// By name too, on a route of its own (meko-cli's way, which never asks Meko Cloud): the :id
	// routes still take only a UUID.
	if _, err := e.Service.CreateDatapack(context.Background(), "team/notes 50%", ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"default", "team/notes 50%"} {
		req, _ := http.NewRequest("GET", e.URL+"/api/v1/datapacks/export?name="+url.QueryEscape(name), nil)
		req.Header.Set("Authorization", "Bearer "+testenv.AdminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" {
			t.Errorf("export by name %s: %d", name, resp.StatusCode)
		}
	}
	wantErr(t, e, "GET", "/datapacks/export?name=no-such-name", nil, 404, "NOT_FOUND")
	wantErr(t, e, "GET", "/datapacks/export", nil, 400, "VALIDATION_ERROR")
	wantErr(t, e, "GET", "/datapacks/default/export", nil, 400, "VALIDATION_ERROR")
}
