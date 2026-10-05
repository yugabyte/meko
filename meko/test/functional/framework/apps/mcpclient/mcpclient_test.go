package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type seenReq struct {
	method, auth, session, protocol string
	rpc                             rpcReq
}

// fakeServer answers initialize and notifications/initialized as plain JSON,
// tools/list as an event stream with a progress notification ahead of the
// reply, and tools/call as JSON (an auth failure for the revoked token).
func fakeServer(t *testing.T) (*httptest.Server, *[]seenReq) {
	t.Helper()
	var mu sync.Mutex
	var seen []seenReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		var req rpcReq
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		mu.Lock()
		seen = append(seen, seenReq{r.Method, r.Header.Get("Authorization"), r.Header.Get("Mcp-Session-Id"), r.Header.Get("MCP-Protocol-Version"), req})
		mu.Unlock()
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Header.Get("Authorization") == "Bearer revoked" && req.Method == "tools/call" {
			http.Error(w, `{"error":"token revoked"}`, http.StatusUnauthorized)
			return
		}
		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "sess-1")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"fake","version":"9"}}}`, *req.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"tools\":[{\"name\":\"memory_add\",\"description\":\"d\",\"inputSchema\":{\"type\":\"object\"}}],\"nextCursor\":\"p2\"}}\n\n", *req.ID)
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"ok"}],"isError":false}}`, *req.ID)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"no such method"}}`, *req.ID)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestInitializeCarriesSessionAndProtocolOnLaterCalls(t *testing.T) {
	srv, seen := fakeServer(t)
	c := New(srv.URL+"/mcp", "tok")
	ctx := context.Background()
	init, err := c.Initialize(ctx)
	if err != nil || init.ServerInfo.Name != "fake" || init.ProtocolVersion != "2025-06-18" {
		t.Fatalf("Initialize = %+v, %v", init, err)
	}
	if c.SessionID() != "sess-1" {
		t.Errorf("SessionID = %q", c.SessionID())
	}
	res, err := c.CallTool(ctx, "memory_add", map[string]any{"text": "x"})
	if err != nil || res.IsError || res.Text() != "ok" {
		t.Fatalf("CallTool = %+v, %v", res, err)
	}
	if got := (*seen)[0]; got.auth != "Bearer tok" || got.session != "" {
		t.Errorf("initialize request = %+v", got)
	}
	last := (*seen)[len(*seen)-1]
	if last.session != "sess-1" || last.protocol != "2025-06-18" || last.rpc.Method != "tools/call" {
		t.Errorf("tools/call request = %+v", last)
	}
	if (*seen)[1].rpc.Method != "notifications/initialized" || (*seen)[1].rpc.ID != nil {
		t.Errorf("second request = %+v, want the initialized notification with no id", (*seen)[1])
	}
}

func TestListToolsReadsAnEventStreamAndFollowsCursors(t *testing.T) {
	srv, seen := fakeServer(t)
	c := New(srv.URL+"/mcp", "tok")
	if _, err := c.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	tools, err := c.ListTools(context.Background())
	if err == nil {
		t.Fatalf("ListTools = %v, want the endless cursor to stop it", tools)
	}
	var lists int
	for _, s := range *seen {
		if s.rpc.Method == "tools/list" {
			lists++
		}
	}
	if lists < 2 {
		t.Errorf("tools/list posted %d times, want the cursor followed", lists)
	}
}

func TestRevokedTokenIsAnAuthRejection(t *testing.T) {
	srv, _ := fakeServer(t)
	c := New(srv.URL+"/mcp", "revoked")
	ctx := context.Background()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	_, err := c.CallTool(ctx, "memory_add", nil)
	if err == nil || !IsAuthRejection(err) {
		t.Fatalf("CallTool err = %v, want an auth rejection", err)
	}
}

func TestRPCErrorIsReturned(t *testing.T) {
	srv, _ := fakeServer(t)
	c := New(srv.URL+"/mcp", "tok")
	var out any
	err := c.call(context.Background(), "bogus/method", nil, &out)
	re, ok := err.(*RPCError)
	if !ok || re.Code != -32601 {
		t.Fatalf("err = %#v", err)
	}
}

func TestInitializeTriesTheOtherSlashForm(t *testing.T) {
	srv, _ := fakeServer(t)
	c := New(srv.URL+"/mcp/", "tok")
	if _, err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if c.URL() != srv.URL+"/mcp" {
		t.Errorf("URL = %q, want it to settle on the form that answered", c.URL())
	}
}

func TestCloseDeletesTheSession(t *testing.T) {
	srv, seen := fakeServer(t)
	c := New(srv.URL+"/mcp", "tok")
	ctx := context.Background()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)
	last := (*seen)[len(*seen)-1]
	if last.method != http.MethodDelete || last.session != "sess-1" {
		t.Errorf("last request = %+v", last)
	}
}

func TestReadSSEJoinsMultiLineDataAndIgnoresOtherIDs(t *testing.T) {
	stream := "data: {\"jsonrpc\":\"2.0\",\"id\":9,\"result\":{}}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\r\ndata:  \"id\":3,\"result\":{\"x\":1}}\r\n\r\n"
	msg, err := readSSE(strings.NewReader(stream), 3)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Result struct{ X int } `json:"result"`
	}
	if err := json.Unmarshal(msg, &m); err != nil || m.Result.X != 1 {
		t.Fatalf("msg = %s, %v", msg, err)
	}
	if _, err := readSSE(strings.NewReader("data: {\"jsonrpc\":\"2.0\",\"id\":4,\"result\":{}}\n\n"), 3); err == nil {
		t.Error("a stream with no reply to the id returned no error")
	}
}

func TestPickReplyFromABatch(t *testing.T) {
	msg, err := pickReply([]byte(`[{"jsonrpc":"2.0","id":1,"result":{"a":1}},{"jsonrpc":"2.0","id":2,"result":{"b":2}}]`), 2)
	if err != nil || !strings.Contains(string(msg), `"b"`) {
		t.Fatalf("pickReply = %s, %v", msg, err)
	}
}

func TestArgsFromSchema(t *testing.T) {
	schema := json.RawMessage(`{
	  "type":"object",
	  "properties":{
	    "conversation_id":{"type":"string"},
	    "query":{"type":"string"},
	    "limit":{"type":"integer","minimum":5},
	    "flag":{"type":"boolean"},
	    "mode":{"type":"string","enum":["fast","slow"]},
	    "tags":{"type":"array"},
	    "optional":{"type":"string"}
	  },
	  "required":["conversation_id","query","limit","flag","mode","tags"]}`)
	args := ArgsFromSchema(schema)
	if id, _ := args["conversation_id"].(string); len(id) != 36 || id[14] != '4' {
		t.Errorf("conversation_id = %v, want a uuid", args["conversation_id"])
	}
	if args["query"] != "mekoqa" || args["limit"] != 5.0 || args["flag"] != false || args["mode"] != "fast" {
		t.Errorf("args = %v", args)
	}
	if _, ok := args["optional"]; ok {
		t.Errorf("an optional property was filled: %v", args)
	}
	if got := ArgsFromSchema(json.RawMessage(`not json`)); len(got) != 0 {
		t.Errorf("a bad schema produced %v", got)
	}
}

func TestLooksLikeAuthFailure(t *testing.T) {
	for _, s := range []string{"Unauthorized", "token has been revoked", "HTTP 401", "Invalid or expired token"} {
		if !LooksLikeAuthFailure(s) {
			t.Errorf("%q not recognised as an auth failure", s)
		}
	}
	if LooksLikeAuthFailure("conversation_id is required") {
		t.Error("a validation message was read as an auth failure")
	}
}

// A dropped connection on initialize, what a port-forward does now and then,
// is retried instead of failing the test.
func TestInitializeRetriesADroppedConnection(t *testing.T) {
	srv, _ := fakeServer(t)
	var dropped atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dropped.CompareAndSwap(false, true) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		srv.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	c := New(proxy.URL+"/mcp", "tok")
	if _, err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize after a dropped connection: %v", err)
	}
	if !dropped.Load() {
		t.Fatal("the first connection was never dropped")
	}
}
