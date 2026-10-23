package rest_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/testenv"
)

// An export is one JSON file of a datapack's text and metadata, ids and dates kept: memories with
// their state and history, collective memories, conversations with every turn, and the artifact
// list. It says what it leaves out, and carries no vectors or file bytes.
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
		DatapackID: e.DatapackID, Input: "when?", Output: "Tuesday", Reasoning: "the calendar", Plan: []string{"look"},
		Seed: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Service.PutArtifact(ctx, e.DatapackID, "claude_code:x", "notes.md", "text/markdown",
		[]byte("# notes")); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("GET", e.URL+"/api/v1/datapacks/"+e.DatapackID+"/export", nil)
	req.Header.Set("Authorization", "Bearer "+testenv.ClientToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), `filename="meko-default-`) {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Disposition"))
	}
	var x map[string]any
	if err = json.Unmarshal(body, &x); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	if x["format"] != "meko-datapack-export" || num(x["version"]) != 1 || x["embedding_model"] == "" ||
		obj(x["datapack"])["id"] != e.DatapackID || len(arr(x["left_out"])) == 0 {
		t.Errorf("header %v", x)
	}
	byID := map[string]map[string]any{}
	for _, m := range arr(x["memories"]) {
		byID[str(obj(m)["id"])] = obj(m)
	}
	if len(byID) != 2 || byID[learning]["state"] != "learning" || byID[kept]["state"] != "kept" ||
		byID[learning]["text"] != "a learning, edited" || byID[learning]["agent_id"] != "claude_code:x" ||
		len(arr(byID[learning]["history"])) != 2 {
		t.Errorf("memories %v", arr(x["memories"]))
	}
	collective := map[string]map[string]any{}
	for _, c := range arr(x["collective_memories"]) {
		collective[str(obj(c)["id"])] = obj(c)
	}
	if len(collective) != 2 || collective[promoted]["promoted_at"] == nil || collective[halfway] == nil {
		t.Errorf("collective %v", arr(x["collective_memories"]))
	}
	if byID[halfway] != nil {
		t.Error("a half-promoted memory is exported twice")
	}
	cs := arr(x["conversations"])
	if len(cs) != 1 || obj(cs[0])["title"] != "release planning" || len(arr(obj(cs[0])["turns"])) != 1 ||
		obj(arr(obj(cs[0])["turns"])[0])["reasoning"] != "the calendar" {
		t.Errorf("conversations %v", cs)
	}
	a := arr(x["artifacts"])
	if len(a) != 1 || obj(a[0])["filename"] != "notes.md" || len(str(obj(a[0])["sha256"])) != 64 {
		t.Errorf("artifacts %v", a)
	}
	if x["knowledge_files"] == nil {
		t.Error("no knowledge_files list")
	}
	for _, leak := range []string{"# notes", `"content_base64"`, `"embedding":`, `"vector":`, `"vectors":`} {
		if strings.Contains(string(body), leak) {
			t.Errorf("the export carries %q", leak)
		}
	}
	wantErr(t, e, "GET", "/datapacks/00000000-0000-0000-0000-000000000000/export", nil, 404, "NOT_FOUND")
}
