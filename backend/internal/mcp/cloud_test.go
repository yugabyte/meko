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
// knowledgebase_upload_file) and sends every call there as it came, whatever datapack it names,
// recording it as Meko Cloud's; unlinked, it's this Meko's again.
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

	// A datapack this Meko has goes to Meko Cloud too, with the arguments as they came.
	r := call(t, e, "memory_add", map[string]any{"text": "a fact", "datapack_id": e.DatapackID,
		"conversation_id": "c0ffee00-0000-4000-8000-00000000000a", "agent_id": "claude_code:demo", "extra": 1})
	if r.IsError || str(r.JSON["cloud_tool"]) != "memory_add" {
		t.Fatalf("memory_add: %s", r.Text)
	}
	if a := cloud.args[0]; a["extra"] != float64(1) || a["datapack_id"] != e.DatapackID {
		t.Errorf("the arguments sent on: %v", a)
	}
	if r = call(t, e, "amp_cluster_list", map[string]any{}); str(r.JSON["cloud_tool"]) != "amp_cluster_list" {
		t.Fatalf("amp_cluster_list: %s", r.Text)
	}

	// Forwarded calls are in the call log as Meko Cloud's, and this Meko's reads leave them out.
	rows := cloudRows(t, e)
	if len(rows) != 2 || rows[0].Tool != "memory_add" || rows[0].DatapackID != e.DatapackID ||
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
