package mcp_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"meko/ycp/internal/mcp"
)

// rpc sends a JSON-RPC request and returns the decoded response.
func rpc(t *testing.T, url, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(url+"/mcp", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e, ok := out["error"]; ok && e != nil {
		t.Fatalf("rpc error: %v", e)
	}
	return out
}

// TestMCPProtocol exercises the real MCP flow end-to-end over HTTP:
// initialize -> tools/list -> tools/call(memory_add) -> tools/call(memory_list).
func TestMCPProtocol(t *testing.T) {
	ts := httptest.NewServer(mcp.Handler(mcp.NewMemStore()))
	defer ts.Close()

	init := rpc(t, ts.URL, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	result, _ := init["result"].(map[string]any)
	if result["protocolVersion"] != "2024-11-05" {
		t.Fatalf("initialize protocolVersion = %v", result["protocolVersion"])
	}

	list := rpc(t, ts.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools, _ := list["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	if !names["memory_add"] || !names["memory_list"] {
		t.Fatalf("tools/list missing tools: %v", names)
	}

	add := rpc(t, ts.URL, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memory_add","arguments":{"text":"hello datapack"}}}`)
	addText := firstText(t, add)
	if !strings.Contains(addText, "stored memory") {
		t.Fatalf("memory_add result = %q", addText)
	}

	got := rpc(t, ts.URL, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"memory_list"}}`)
	if !strings.Contains(firstText(t, got), "hello datapack") {
		t.Fatalf("memory_list did not return the stored memory: %q", firstText(t, got))
	}
}

func TestMCPUnknownMethod(t *testing.T) {
	ts := httptest.NewServer(mcp.Handler(mcp.NewMemStore()))
	defer ts.Close()
	resp, _ := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"bogus"}`))
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if out["error"] == nil {
		t.Fatal("expected error for unknown method")
	}
}

func firstText(t *testing.T, resp map[string]any) string {
	t.Helper()
	content, _ := resp["result"].(map[string]any)["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content in %v", resp)
	}
	return content[0].(map[string]any)["text"].(string)
}
