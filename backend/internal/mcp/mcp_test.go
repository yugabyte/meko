package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yugabyte/meko/backend/internal/testenv"
)

// rpc posts one JSON-RPC message to /mcp, as a 2025-06-18 client (Claude Code, AgentK's) sends it,
// and returns the HTTP status and the reply's JSON (nil for none).
func rpc(t *testing.T, e *testenv.Env, token string, msg map[string]any, header ...string) (int, map[string]any) {
	t.Helper()
	msg["jsonrpc"] = "2.0"
	b, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, e.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if len(bytes.TrimSpace(body)) == 0 {
		return resp.StatusCode, nil
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return resp.StatusCode, map[string]any{"raw": string(body)}
	}
	return resp.StatusCode, out
}

// callResult is a tools/call result: its text, the text parsed as JSON (when it is), and isError.
type callResult struct {
	Text    string
	JSON    map[string]any
	List    []any
	IsError bool
	Raw     map[string]any
}

// call calls a tool with the client token, as AgentK's client does: one stateless POST.
func call(t *testing.T, e *testenv.Env, name string, args map[string]any) callResult {
	t.Helper()
	status, reply := rpc(t, e, testenv.ClientToken, map[string]any{"id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	if status != 200 || reply["result"] == nil {
		t.Fatalf("%s: status %d, reply %v", name, status, reply)
	}
	res, _ := reply["result"].(map[string]any)
	out := callResult{Raw: res}
	out.IsError, _ = res["isError"].(bool)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("%s: no content: %v", name, res)
	}
	first, _ := content[0].(map[string]any)
	out.Text = str(first["text"])
	_ = json.Unmarshal([]byte(out.Text), &out.JSON)
	_ = json.Unmarshal([]byte(out.Text), &out.List)
	return out
}

// ok calls a tool and fails the test unless it succeeded.
func ok(t *testing.T, e *testenv.Env, name string, args map[string]any) callResult {
	t.Helper()
	r := call(t, e, name, args)
	if r.IsError || r.JSON["error"] != nil {
		t.Fatalf("%s(%v): %s", name, args, r.Text)
	}
	return r
}

// wantError calls a tool and fails the test unless it returned the error code, in band.
func wantError(t *testing.T, e *testenv.Env, name string, args map[string]any, code string) {
	t.Helper()
	r := call(t, e, name, args)
	if r.IsError || r.JSON["error"] != code {
		t.Fatalf("%s(%v): want error %q, got %s (isError %v)", name, args, code, r.Text, r.IsError)
	}
}

func TestClaudeCodesHandshake(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	// Claude Code probes server/discover first; an old-protocol server answers -32601 with HTTP 200.
	status, reply := rpc(t, e, testenv.ClientToken, map[string]any{"id": "server-discover-probe-1",
		"method": "server/discover", "params": map[string]any{}})
	if status != 200 || num(obj(reply["error"])["code"]) != -32601 {
		t.Fatalf("discover: %d %v", status, reply)
	}
	status, reply = rpc(t, e, testenv.ClientToken, map[string]any{"id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "claude-code", "version": "2"}}})
	res, _ := reply["result"].(map[string]any)
	if status != 200 || res["protocolVersion"] != "2025-06-18" ||
		obj(res["serverInfo"])["name"] != "Meko" || !strings.Contains(str(res["instructions"]), "context_search") {
		t.Fatalf("initialize: %d %v", status, reply)
	}
	status, reply = rpc(t, e, testenv.ClientToken, map[string]any{"method": "notifications/initialized"})
	if status != 202 {
		t.Errorf("notification: %d %v", status, reply)
	}
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		req, _ := http.NewRequest(m, e.URL+"/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+testenv.ClientToken)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /mcp: %d", m, resp.StatusCode)
		}
	}
	status, reply = rpc(t, e, testenv.ClientToken, map[string]any{"id": 2, "method": "tools/list"})
	tools := arr(obj(reply["result"])["tools"])
	names := map[string]map[string]any{}
	for _, tl := range tools {
		m := obj(tl)
		names[str(m["name"])] = m
	}
	for _, want := range []string{"context_search", "memory_add", "memory_search", "memory_get_all",
		"memory_get_by_id", "memory_update", "memory_delete_by_id", "memory_delete_all", "conversation_create",
		"conversation_add_message", "conversation_get", "conversation_list", "conversation_update",
		"conversation_delete", "datapack_list", "datapack_search", "datapack_create", "datapack_describe",
		"datapack_update", "datapack_delete", "track_token_usage", "memory_promote", "artifact_put", "artifact_get",
		"knowledgebase_search", "knowledgebase_upload_document", "knowledgebase_upload_file",
		"knowledgebase_document_status", "knowledgebase_delete_document"} {
		if names[want] == nil {
			t.Errorf("tools/list lacks %s", want)
		}
	}
	if status != 200 {
		t.Errorf("tools/list answered %d", status)
	}
	add := names["memory_add"]
	schema := obj(add["inputSchema"])
	if add["title"] != "Add Memory" || obj(add["annotations"])["readOnlyHint"] != false ||
		obj(obj(schema["properties"])["agent_id"])["anyOf"] == nil ||
		!strings.Contains(strings.Join(toStrings(schema["required"]), ","), "text") {
		t.Errorf("memory_add's description: %v", add)
	}
	// Cloud's internal arguments are read but not advertised.
	if obj(obj(names["conversation_add_message"]["inputSchema"])["properties"])["skip_search_embed"] != nil {
		t.Errorf("conversation_add_message advertises skip_search_embed")
	}
}

func toStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func TestCredentialsAndGuards(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	list := map[string]any{"id": 1, "method": "tools/list"}
	if status, _ := rpc(t, e, "", list); status != 401 {
		t.Errorf("no token: %d", status)
	}
	if status, _ := rpc(t, e, "wrong", list); status != 401 {
		t.Errorf("wrong token: %d", status)
	}
	if status, _ := rpc(t, e, testenv.AdminToken, list); status != 200 {
		t.Errorf("admin token: %d", status)
	}
	if status, _ := rpc(t, e, testenv.ClientToken, list, "Origin", "http://example.com"); status != 403 {
		t.Errorf("a browser page: %d", status)
	}
	// Each call is logged with the credential it came in with, and nothing it carried.
	ok(t, e, "memory_add", map[string]any{"text": "the secret is plum", "conversation_id": "c1"})
	calls, err := e.Store.Calls(context.Background(), "", time.Time{})
	if err != nil || len(calls) == 0 {
		t.Fatalf("calls %v %v", calls, err)
	}
	last := calls[len(calls)-1]
	if last.Tool != "memory_add" || last.Credential != "client" || !last.OK || last.DatapackID != e.DatapackID ||
		last.ConversationID != "c1" || last.AgentID != "meko_agent" {
		t.Errorf("call log row %+v", last)
	}
}

func TestAuthOff(t *testing.T) {
	e := testenv.Start(t, testenv.Options{Auth: "none"})
	status, reply := rpc(t, e, "", map[string]any{"id": 1, "method": "tools/list"})
	if status != 200 || reply["result"] == nil {
		t.Fatalf("no token with auth off: %d %v", status, reply)
	}
}

func TestResultsAndArguments(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	r := ok(t, e, "memory_add", map[string]any{"text": "Migrations are append-only.", "conversation_id": "c1",
		"agent_id": "claude_code:a", "metadata": `{"kind":"gotcha"}`, "unknown_argument": "ignored"})
	// The reply is JSON text, and structuredContent wraps that same text, as Cloud's server does.
	if obj(r.Raw["structuredContent"])["result"] != r.Text {
		t.Errorf("structuredContent: %v", r.Raw["structuredContent"])
	}
	results := arr(r.JSON["results"])
	added := obj(results[0])
	if added["event"] != "ADD" || added["memory"] != "Migrations are append-only." || added["role"] != "user" {
		t.Errorf("memory_add reply: %s", r.Text)
	}
	// Numbers may come as strings.
	r = ok(t, e, "memory_search", map[string]any{"query": "migrations append-only", "conversation_id": "c1",
		"limit": "5"})
	if len(arr(r.JSON["results"])) == 0 {
		t.Errorf("search: %s", r.Text)
	}
	// A value that can't be read as its type fails the call.
	if r = call(t, e, "memory_search", map[string]any{"query": "x", "conversation_id": "c1",
		"limit": map[string]any{}}); !r.IsError || !strings.Contains(r.Text, "limit") {
		t.Errorf("a bad limit: %s %v", r.Text, r.IsError)
	}
	// Cloud's plain-text replies for bad JSON strings.
	r = call(t, e, "memory_add", map[string]any{"text": "x", "conversation_id": "c1", "metadata": "{nope"})
	if r.IsError || r.Text != "Error: metadata must be a valid JSON string." {
		t.Errorf("bad metadata: %s", r.Text)
	}
	wantError(t, e, "memory_add", map[string]any{"conversation_id": "c1"}, "messages_missing")
	wantError(t, e, "memory_add", map[string]any{"text": "x"}, "conversation_id_required")
	wantError(t, e, "memory_add", map[string]any{"text": "x", "conversation_id": "c1",
		"datapack_id": "00000000-0000-0000-0000-000000000000"}, "datapack_access_denied")
	wantError(t, e, "context_search", map[string]any{"query": "x", "conversation_id": "c1",
		"force_source": "everything"}, "invalid_force_source")
	// An unknown tool is an MCP error, as in Cloud.
	_, reply := rpc(t, e, testenv.ClientToken, map[string]any{"id": 1, "method": "tools/call",
		"params": map[string]any{"name": "no_such_tool", "arguments": map[string]any{}}})
	if reply["error"] == nil {
		t.Errorf("unknown tool: %v", reply)
	}
}

// The SDK's own client works too: a newer protocol, through the same handler.
func TestSDKClient(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	sess, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: e.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{testenv.ClientToken}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: "datapack_list",
		Arguments: map[string]any{"conversation_id": "c1"}})
	if err != nil || res.IsError {
		t.Fatalf("datapack_list: %v %+v", err, res)
	}
	text, _ := res.Content[0].(*sdk.TextContent)
	if text == nil || !strings.Contains(text.Text, e.DatapackID) {
		t.Errorf("datapack_list: %+v", res.Content)
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// Safe accessors for decoded JSON: the zero value when v isn't that type.
func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func arr(v any) []any          { a, _ := v.([]any); return a }
func str(v any) string         { s, _ := v.(string); return s }
func num(v any) float64        { n, _ := v.(float64); return n }
