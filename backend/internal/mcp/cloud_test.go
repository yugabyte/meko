package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yugabyte/meko/backend/internal/cloudlink"
	"github.com/yugabyte/meko/backend/internal/store"
	"github.com/yugabyte/meko/backend/internal/testenv"
)

// fakeCloud is a linked Meko Cloud as /mcp sees it: its tools, and the calls sent to it, answered
// by reply (by tool name) or with the tool's name and arguments.
type fakeCloud struct {
	mu      sync.Mutex
	linked  bool
	linkErr error
	tools   *cloudlink.Tools
	calls   []string
	args    []map[string]any
	reply   map[string]string
	err     error
}

func (f *fakeCloud) IsLinked() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.linked, f.linkErr
}

func (f *fakeCloud) CloudTools(context.Context) *cloudlink.Tools {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tools
}

func (f *fakeCloud) Forward(_ context.Context, name string, raw json.RawMessage) (*sdk.CallToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var a map[string]any
	_ = json.Unmarshal(raw, &a)
	f.calls, f.args = append(f.calls, name), append(f.args, a)
	if f.err != nil {
		return nil, f.err
	}
	text, ok := f.reply[name]
	if !ok {
		b, _ := json.Marshal(map[string]any{"cloud_tool": name, "args": a})
		text = string(b)
	}
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}, nil
}

func cloudTool(name string) *sdk.Tool {
	return &sdk.Tool{Name: name, Description: "Meko Cloud's " + name, InputSchema: map[string]any{"type": "object",
		"properties": map[string]any{"datapack_id": map[string]any{"type": "string"}}}}
}

func toolNames(t *testing.T, e *testenv.Env) map[string]bool {
	t.Helper()
	status, reply := rpc(t, e, testenv.ClientToken, map[string]any{"id": 1, "method": "tools/list"})
	res, _ := reply["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	if status != 200 || len(tools) == 0 {
		t.Fatalf("tools/list: %d %v", status, reply)
	}
	out := map[string]bool{}
	for _, tl := range tools {
		m, _ := tl.(map[string]any)
		out[str(m["name"])] = true
	}
	return out
}

func instructions(t *testing.T, e *testenv.Env) string {
	t.Helper()
	_, reply := rpc(t, e, testenv.ClientToken, map[string]any{"id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "1"}}})
	res, _ := reply["result"].(map[string]any)
	return str(res["instructions"])
}

// While Meko Cloud is linked, /mcp offers its tools and instructions (plus this Meko's
// knowledgebase_upload_file) and sends a call that names none of this Meko's datapacks there as it
// came, recording it as Meko Cloud's; unlinked, it's this Meko's again.
func TestCloudPassThrough(t *testing.T) {
	cloud := &fakeCloud{tools: &cloudlink.Tools{Instructions: "Meko Cloud's own instructions.",
		Tools: []*sdk.Tool{cloudTool("memory_add"), cloudTool("amp_cluster_list"), cloudTool("knowledgebase_upload_file")}}}
	e := testenv.Start(t, testenv.Options{MCPCloud: cloud})

	if n := toolNames(t, e); !n["datapack_metrics"] || n["amp_cluster_list"] {
		t.Fatalf("unlinked tools/list: %v", n)
	}
	if !strings.Contains(instructions(t, e), "Meko running on this machine") {
		t.Errorf("unlinked instructions: %s", instructions(t, e))
	}

	cloud.linked = true
	n := toolNames(t, e)
	if len(n) != 3 || !n["memory_add"] || !n["amp_cluster_list"] || !n["knowledgebase_upload_file"] {
		t.Fatalf("linked tools/list: %v", n)
	}
	if got := instructions(t, e); got != "Meko Cloud's own instructions." {
		t.Errorf("linked instructions: %q", got)
	}

	// A datapack this Meko doesn't have goes to Meko Cloud, with the arguments as they came.
	r := call(t, e, "memory_add", map[string]any{"text": "a fact", "datapack_id": cloudDP,
		"conversation_id": "c0ffee00-0000-4000-8000-00000000000a", "agent_id": "claude_code:demo", "extra": 1})
	if r.IsError || str(r.JSON["cloud_tool"]) != "memory_add" {
		t.Fatalf("memory_add: %s", r.Text)
	}
	if a := cloud.args[0]; a["extra"] != float64(1) || a["datapack_id"] != cloudDP {
		t.Errorf("the arguments sent on: %v", a)
	}
	if r = call(t, e, "amp_cluster_list", map[string]any{}); str(r.JSON["cloud_tool"]) != "amp_cluster_list" {
		t.Fatalf("amp_cluster_list: %s", r.Text)
	}

	// Forwarded calls are in the call log as Meko Cloud's, and this Meko's reads leave them out.
	rows := cloudRows(t, e)
	if len(rows) != 2 || rows[0].Tool != "memory_add" || rows[0].DatapackID != cloudDP ||
		rows[0].AgentID != "claude_code:demo" || rows[0].Credential != "client" || !rows[0].OK ||
		rows[0].Location != store.LocationCloud {
		t.Fatalf("the call log's Meko Cloud rows: %+v", rows)
	}
	if calls, _ := e.Store.Calls(context.Background(), "", time.Time{}); len(calls) != 0 {
		t.Errorf("this Meko's calls hold Meko Cloud's: %+v", calls)
	}

	// An error reply comes back as it is, and is the row's code.
	cloud.reply = map[string]string{"memory_add": `{"error":"datapack_access_denied","detail":"no"}`}
	if r = call(t, e, "memory_add", map[string]any{"text": "x"}); str(r.JSON["error"]) != "datapack_access_denied" {
		t.Fatalf("an error reply: %s", r.Text)
	}
	if rows = cloudRows(t, e); rows[len(rows)-1].ErrorCode != "datapack_access_denied" || rows[len(rows)-1].OK {
		t.Errorf("the error reply's row: %+v", rows[len(rows)-1])
	}

	// A call that doesn't reach Meko Cloud says what's wrong, without falling back to this Meko.
	cloud.err = &cloudlink.CallError{Code: cloudlink.CodeUnreachable}
	if r = call(t, e, "memory_add", map[string]any{"text": "x"}); str(r.JSON["error"]) != "cloud_unreachable" {
		t.Fatalf("Meko Cloud unreachable: %s", r.Text)
	}
	cloud.err = nil

	// Without Meko Cloud's list, the tools this Meko shares with it are sent on.
	cloud.tools = nil
	if n = toolNames(t, e); !n["context_search"] || !n["knowledgebase_upload_file"] || n["datapack_metrics"] {
		t.Fatalf("linked tools/list without Meko Cloud's: %v", n)
	}

	// A link that can't be read may be a linked one: calls fail rather than land here.
	cloud.linked, cloud.linkErr = false, errors.New("can't read the Meko Cloud key: the Keychain refused")
	cloud.err = cloud.linkErr
	if n = toolNames(t, e); n["datapack_metrics"] {
		t.Fatalf("an unreadable link serves this Meko's tools: %v", n)
	}
	if r = call(t, e, "datapack_create", map[string]any{"name": "x"}); str(r.JSON["error"]) != "cloud_call_failed" ||
		strings.Contains(r.Text, "Keychain") {
		t.Fatalf("an unreadable link: %s", r.Text)
	}
	cloud.linkErr, cloud.err = nil, nil

	if r = call(t, e, "datapack_describe", map[string]any{"conversation_id": "x"}); str(r.JSON["datapack_id"]) !=
		e.DatapackID {
		t.Fatalf("unlinked, this Meko's again: %s", r.Text)
	}
}

// cloudDP is a datapack only Meko Cloud has.
const cloudDP = "c10bd000-0000-4000-8000-000000000001"

// While Meko Cloud is linked, a call that names a datapack this Meko has runs here, recorded as this
// Meko's, and the listings add this Meko's datapacks to Meko Cloud's, one row per id.
func TestCloudServesLocalDatapacks(t *testing.T) {
	cloudRow := `{"datapack_id":"` + cloudDP + `","datapack_name":"cloud-notes","grant":"owner"}`
	cloud := &fakeCloud{linked: true, tools: &cloudlink.Tools{Tools: []*sdk.Tool{cloudTool("memory_add"),
		cloudTool("datapack_describe"), cloudTool("datapack_list"), cloudTool("datapack_search"),
		cloudTool("datapack_create"), cloudTool("amp_cluster_list")}}}
	e := testenv.Start(t, testenv.Options{MCPCloud: cloud})
	ctx := context.Background()
	here, err := e.Service.Datapack(ctx, e.DatapackID)
	if err != nil {
		t.Fatal(err)
	}
	// Meko Cloud has this Meko's datapack too, as after an export: this Meko's copy is served.
	twin := `{"datapack_id":"` + e.DatapackID + `","datapack_name":"cloud-twin","grant":"owner"}`
	cloud.reply = map[string]string{
		"datapack_list":   "[" + cloudRow + "," + twin + "]",
		"datapack_search": `{"datapacks":[` + cloudRow + "," + twin + `],"next_cursor":"c2","has_more":true,"total":7}`,
	}

	r := call(t, e, "datapack_describe", map[string]any{"datapack_id": " " + e.DatapackID + " ",
		"conversation_id": "c0ffee00-0000-4000-8000-00000000000a"})
	if str(r.JSON["datapack_id"]) != e.DatapackID || r.JSON["cloud_tool"] != nil {
		t.Fatalf("describe of this Meko's datapack: %s", r.Text)
	}
	r = call(t, e, "memory_add", map[string]any{"text": "deploys go out on Tuesdays", "datapack_id": e.DatapackID,
		"conversation_id": "c0ffee00-0000-4000-8000-00000000000a"})
	if r.JSON["error"] != nil || r.JSON["cloud_tool"] != nil {
		t.Fatalf("memory_add to this Meko's datapack: %s", r.Text)
	}
	if len(cloud.calls) != 0 {
		t.Fatalf("Meko Cloud was called for this Meko's datapack: %v", cloud.calls)
	}
	time.Sleep(50 * time.Millisecond) // recorded after the reply
	if calls, _ := e.Store.Calls(ctx, e.DatapackID, time.Time{}); len(calls) != 2 {
		t.Fatalf("this Meko's call log: %+v", calls)
	}

	// A blank id, an id only Meko Cloud has, a create and a tool only Meko Cloud has go there.
	for _, c := range []struct {
		tool string
		args map[string]any
	}{{"memory_add", map[string]any{"text": "x"}}, {"memory_add", map[string]any{"text": "x", "datapack_id": cloudDP}},
		{"datapack_create", map[string]any{"name": "new-one", "datapack_id": e.DatapackID}},
		{"amp_cluster_list", map[string]any{"datapack_id": e.DatapackID}}} {
		if r = call(t, e, c.tool, c.args); str(r.JSON["cloud_tool"]) != c.tool {
			t.Errorf("%s %v wasn't sent to Meko Cloud: %s", c.tool, c.args, r.Text)
		}
	}

	// datapack_list: Meko Cloud's rows, then this Meko's, one row per id.
	r = call(t, e, "datapack_list", map[string]any{})
	var list []map[string]any
	if err = json.Unmarshal([]byte(r.Text), &list); err != nil || len(list) != 2 ||
		list[0]["datapack_id"] != cloudDP || list[1]["datapack_id"] != e.DatapackID ||
		list[1]["datapack_name"] != here.Name || list[1]["location"] != nil || list[1]["user_id"] != "" {
		t.Fatalf("datapack_list: %s", r.Text)
	}
	// Meko Cloud's row of an id this Meko has stays when this Meko's copy doesn't match the query.
	if r = call(t, e, "datapack_list", map[string]any{"query": "cloud"}); !strings.Contains(r.Text, "cloud-twin") {
		t.Errorf("datapack_list with this Meko's copy left out: %s", r.Text)
	}
	if r = call(t, e, "datapack_list", map[string]any{"limit": 1}); !strings.Contains(r.Text, cloudDP) ||
		strings.Contains(r.Text, e.DatapackID) {
		t.Errorf("datapack_list cut to limit: %s", r.Text)
	}

	// datapack_search: this Meko's matches first on the first page, and none on a cursor's page.
	r = call(t, e, "datapack_search", map[string]any{"name": here.Name})
	rows, _ := r.JSON["datapacks"].([]any)
	if len(rows) != 2 || obj(rows[0])["datapack_id"] != e.DatapackID || obj(rows[1])["datapack_id"] != cloudDP ||
		r.JSON["total"] != float64(7) || r.JSON["next_cursor"] != "c2" {
		t.Fatalf("datapack_search's first page: %s", r.Text)
	}
	r = call(t, e, "datapack_search", map[string]any{"name": "no-such-name"})
	if rows, _ = r.JSON["datapacks"].([]any); len(rows) != 2 || r.JSON["total"] != float64(7) {
		t.Errorf("datapack_search with no match here: %s", r.Text)
	}
	// A cursor's page adds none of this Meko's, and still leaves out Meko Cloud's row of one.
	r = call(t, e, "datapack_search", map[string]any{"name": here.Name, "cursor": "c2"})
	if rows, _ = r.JSON["datapacks"].([]any); len(rows) != 1 || obj(rows[0])["datapack_id"] != cloudDP {
		t.Fatalf("datapack_search's cursor page: %s", r.Text)
	}
	// With no total from Meko Cloud (include_total off), there's none in the reply.
	cloud.reply["datapack_search"] = `{"datapacks":[` + twin + `],"next_cursor":"","has_more":false,"total":null}`
	if r = call(t, e, "datapack_search", map[string]any{"query": "nothing-here"}); r.JSON["total"] != nil ||
		!strings.Contains(r.Text, `"total":null`) {
		t.Errorf("datapack_search without a total: %s", r.Text)
	}

	// Meko Cloud's error replies and failures pass as they are: the listings fail, while calls on
	// this Meko's datapacks still work.
	cloud.reply["datapack_list"] = `{"error":"list_failed","detail":"no"}`
	if r = call(t, e, "datapack_list", map[string]any{}); str(r.JSON["error"]) != "list_failed" {
		t.Errorf("Meko Cloud's error reply: %s", r.Text)
	}
	cloud.err = &cloudlink.CallError{Code: cloudlink.CodeUnreachable}
	if r = call(t, e, "datapack_list", map[string]any{}); str(r.JSON["error"]) != "cloud_unreachable" {
		t.Errorf("datapack_list with Meko Cloud unreachable: %s", r.Text)
	}
	if r = call(t, e, "datapack_describe", map[string]any{"datapack_id": e.DatapackID,
		"conversation_id": "c0ffee00-0000-4000-8000-00000000000a"}); str(r.JSON["datapack_id"]) != e.DatapackID {
		t.Errorf("this Meko's datapack with Meko Cloud unreachable: %s", r.Text)
	}

	// knowledgebase_upload_file to this Meko's datapack indexes the file here.
	path := filepath.Join(t.TempDir(), "notes.md")
	if err = os.WriteFile(path, []byte("# Notes\n\nDeploys go out on Tuesdays.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cloud.calls = nil
	r = call(t, e, "knowledgebase_upload_file", map[string]any{"path": path, "datapack_id": e.DatapackID,
		"conversation_id": "c0ffee00-0000-4000-8000-00000000000a"})
	if r.JSON["error"] != nil || r.JSON["already_uploaded"] != false || len(cloud.calls) != 0 {
		t.Fatalf("upload_file to this Meko's datapack: %s (Meko Cloud: %v)", r.Text, cloud.calls)
	}

	// When the store can't say whether it has the datapack, the call fails here, not in Meko Cloud.
	cloud.err = nil
	_ = e.Store.Close()
	if r = call(t, e, "memory_add", map[string]any{"text": "x", "datapack_id": e.DatapackID}); str(r.JSON["error"]) !=
		"memory_add_failed" || len(cloud.calls) != 0 {
		t.Errorf("a failed lookup: %s (Meko Cloud: %v)", r.Text, cloud.calls)
	}
}

func cloudRows(t *testing.T, e *testenv.Env) []store.Call {
	t.Helper()
	time.Sleep(50 * time.Millisecond) // recorded after the reply
	rows, err := e.Store.CloudCalls(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// knowledgebase_upload_file, linked, reads the file here, takes an upload link from Meko Cloud's
// knowledgebase_upload_document, posts the file there, and answers with its status.
func TestCloudUploadFile(t *testing.T) {
	var posted string
	link := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		posted = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(link.Close)
	cloud := &fakeCloud{linked: true,
		tools: &cloudlink.Tools{Tools: []*sdk.Tool{cloudTool("knowledgebase_upload_document")}},
		reply: map[string]string{
			"knowledgebase_upload_document": `{"url":"` + link.URL + `","fields":{"key":"k1"},"key":"k1"}`,
			"knowledgebase_document_status": `{"document_key":"k1","state":"indexing"}`,
		}}
	e := testenv.Start(t, testenv.Options{MCPCloud: cloud})
	path := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(path, []byte("# Notes\n\nDeploys go out on Tuesdays.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := call(t, e, "knowledgebase_upload_file", map[string]any{"path": path, "datapack_id": "dp-1",
		"conversation_id": "c0ffee00-0000-4000-8000-00000000000a"})
	if r.IsError || str(r.JSON["state"]) != "indexing" || r.JSON["already_uploaded"] != false {
		t.Fatalf("upload_file: %s", r.Text)
	}
	if strings.Join(cloud.calls, ",") != "knowledgebase_upload_document,knowledgebase_document_status" {
		t.Fatalf("calls: %v", cloud.calls)
	}
	offer := cloud.args[0]
	if offer["filename"] != "notes.md" || offer["datapack_id"] != "dp-1" || len(str(offer["content_hash"])) != 64 ||
		offer["content_head_base64"] == nil {
		t.Errorf("the offer's arguments: %v", offer)
	}
	if !strings.Contains(posted, "Deploys go out on Tuesdays.") || !strings.Contains(posted, "name=\"key\"") {
		t.Errorf("posted: %s", posted)
	}
	if r = call(t, e, "knowledgebase_upload_file", map[string]any{"path": "relative.md"}); str(r.JSON["error"]) !=
		"VALIDATION_ERROR" {
		t.Errorf("a relative path: %s", r.Text)
	}
}
