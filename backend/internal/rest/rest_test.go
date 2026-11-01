package rest_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/store"
	"github.com/yugabyte/meko/backend/internal/testenv"
)

// reply is an envelope: data, or error {code, message}.
type reply struct {
	Status int
	Data   any
	Error  *struct{ Code, Message string }
}

// do sends a request with the client token and decodes the envelope.
func do(t *testing.T, e *testenv.Env, method, path string, body any) reply {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.URL+"/api/v1"+path, rd)
	req.Header.Set("Authorization", "Bearer "+testenv.ClientToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out reply
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s %s: not an envelope: %s", method, path, raw)
	}
	out.Status = resp.StatusCode
	return out
}

// okData sends a request and fails the test unless it succeeded with want (200 or 201).
func okData(t *testing.T, e *testenv.Env, method, path string, body any, want int) any {
	t.Helper()
	r := do(t, e, method, path, body)
	if r.Status != want || r.Error != nil {
		t.Fatalf("%s %s: %d %+v", method, path, r.Status, r.Error)
	}
	return r.Data
}

func wantErr(t *testing.T, e *testenv.Env, method, path string, body any, status int, code string) {
	t.Helper()
	r := do(t, e, method, path, body)
	if r.Status != status || r.Error == nil || r.Error.Code != code {
		t.Fatalf("%s %s: want %d %s, got %d %+v", method, path, status, code, r.Status, r.Error)
	}
}

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func arr(v any) []any          { a, _ := v.([]any); return a }
func num(v any) float64        { n, _ := v.(float64); return n }

func TestUserRoutes(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	cfg := obj(okData(t, e, "GET", "/auth/config", nil, 200))
	if cfg["provider"] != "none" || cfg["signup"] != false {
		t.Errorf("config %v", cfg)
	}
	me := obj(okData(t, e, "GET", "/auth/me", nil, 200))
	if me["account_id"] != e.UserID || me["email_verified"] != true || me["has_completed_onboarding"] != true ||
		me["first_name"] != "Local" {
		t.Errorf("me %v", me)
	}
	okData(t, e, "PATCH", "/auth/me/profile", map[string]string{"first_name": "Ada", "last_name": "L"}, 200)
	wantErr(t, e, "PATCH", "/auth/me/profile", map[string]string{"first_name": "", "last_name": "x"}, 400,
		"VALIDATION_ERROR")
	okData(t, e, "PATCH", "/auth/me", map[string]bool{"has_completed_onboarding": false}, 200)
	me = obj(okData(t, e, "GET", "/auth/me", nil, 200))
	if me["first_name"] != "Ada" || me["has_completed_onboarding"] != false {
		t.Errorf("me after changes %v", me)
	}
	if u := obj(okData(t, e, "GET", "/auth/me/usage", nil, 200)); num(u["datapack_count"]) != 1 {
		t.Errorf("usage %v", u)
	}
}

func TestDatapackRoutes(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	list := arr(okData(t, e, "GET", "/datapacks", nil, 200))
	if len(list) != 1 || obj(list[0])["grant"] != "owner" || obj(list[0])["datapack_owner_name"] != "Local User" ||
		obj(list[0])["memory_count"] == nil {
		t.Fatalf("list %v", list)
	}
	made := obj(okData(t, e, "POST", "/datapacks", map[string]any{"name": "notes", "user_id": "x", "agent_id": nil}, 201))
	id, _ := made["datapack_id"].(string)
	if id == "" || obj(made["mcp_server"]) == nil {
		t.Fatalf("create %v", made)
	}
	wantErr(t, e, "POST", "/datapacks", map[string]any{"name": "notes"}, 409, "CONFLICT")
	wantErr(t, e, "POST", "/datapacks", map[string]any{"name": "no"}, 400, "VALIDATION_ERROR")
	wantErr(t, e, "GET", "/datapacks/not-a-uuid", nil, 400, "VALIDATION_ERROR")
	wantErr(t, e, "GET", "/datapacks/00000000-0000-0000-0000-000000000000", nil, 404, "NOT_FOUND")
	// Any form of the id Cloud's uuid column takes finds the datapack.
	okData(t, e, "GET", "/datapacks/"+strings.ToUpper(id), nil, 200)
	if list = arr(okData(t, e, "GET", "/datapacks", nil, 200)); len(list) != 2 || obj(list[1])["datapack_id"] != id {
		t.Errorf("by name (default, then notes): %v", list)
	}

	okData(t, e, "PATCH", "/datapacks/"+id, map[string]string{"description": "my notes"}, 200)
	ctx := context.Background()
	add := memory.AddMemory{DatapackID: id, AgentID: "claude_code:x", Text: "one"}
	if _, err := e.Service.AddMemory(ctx, add); err != nil {
		t.Fatal(err)
	}
	d := obj(okData(t, e, "GET", "/datapacks/"+id, nil, 200))
	if d["description"] != "my notes" || num(d["memory_count"]) != 1 || num(d["learnings_count"]) != 1 ||
		d["grant"] != "owner" {
		t.Errorf("describe %v", d)
	}
	ps := obj(okData(t, e, "GET", "/datapacks/"+id+"/provision-status", nil, 200))
	url, _ := ps["mcp_server_url"].(string)
	if ps["mcp_api_key"] != testenv.ClientToken || !strings.HasSuffix(url, "/mcp") {
		t.Errorf("provision-status %v", ps)
	}
	agents := arr(okData(t, e, "GET", "/datapacks/"+id+"/agents", nil, 200))
	if len(agents) != 1 || obj(agents[0])["agent_id"] != "claude_code:x" {
		t.Errorf("agents %v", agents)
	}
	if n := obj(okData(t, e, "GET", "/datapacks/"+id+"/conversations/count", nil, 200)); num(n["count"]) != 0 {
		t.Errorf("count %v", n)
	}
	okData(t, e, "PATCH", "/datapacks/"+id+"/conversation-search-opt-out", map[string]bool{"opted_out": true}, 200)
	if d = obj(okData(t, e, "GET", "/datapacks/"+id, nil, 200)); d["conversation_search_opt_out"] != true {
		t.Errorf("opt-out %v", d)
	}
	okData(t, e, "DELETE", "/datapacks/"+id, nil, 200)
	wantErr(t, e, "GET", "/datapacks/"+id, nil, 404, "NOT_FOUND")
}

func TestMemoryRoutes(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	ids := make([]string, 0, 3)
	for _, text := range []string{"alpha fact", "beta fact", "gamma fact"} {
		added, err := e.Service.AddMemory(ctx, memory.AddMemory{DatapackID: e.DatapackID, AgentID: "a", Text: text})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, added[0].ID)
	}
	q := "/memories?datapack_id=" + e.DatapackID
	page := obj(okData(t, e, "GET", q+"&page_size=2&scope=learnings", nil, 200))
	ms := arr(page["memories"])
	if num(page["total"]) != 3 || num(page["total_pages"]) != 2 || len(ms) != 2 || obj(ms[0])["data"] != "gamma fact" ||
		obj(ms[0])["is_mine"] != true || obj(ms[0])["user_name"] != "Local User" {
		t.Fatalf("learnings page %v", page)
	}
	if page = obj(okData(t, e, "GET", q+"&q=BETA", nil, 200)); num(page["total"]) != 1 {
		t.Errorf("search %v", page)
	}
	wantErr(t, e, "GET", q+"&scope=everything", nil, 400, "VALIDATION_ERROR")
	wantErr(t, e, "GET", "/memories", nil, 400, "VALIDATION_ERROR")

	if r := obj(okData(t, e, "POST", "/keep-as-memory", map[string]any{"datapack_id": e.DatapackID,
		"memory_ids": []string{ids[0]}}, 200)); num(r["updated"]) != 1 {
		t.Errorf("keep %v", r)
	}
	if page = obj(okData(t, e, "GET", q+"&scope=learnings", nil, 200)); num(page["total"]) != 2 {
		t.Errorf("learnings after keep %v", page)
	}
	if page = obj(okData(t, e, "GET", q, nil, 200)); num(page["total"]) != 3 {
		t.Errorf("memories after keep %v", page)
	}
	r := obj(okData(t, e, "POST", "/promote-memories", map[string]any{"datapack_id": e.DatapackID, "user_id": e.UserID,
		"memory_ids": []string{ids[0], strings.ToUpper(ids[1])}}, 200))
	if ins := arr(r["inserted_ids"]); len(ins) != 1 || ins[0] != ids[1] || len(arr(r["not_found_ids"])) != 1 {
		t.Errorf("promote (a kept one isn't a learning; any case of an id) %v", r)
	}
	wantErr(t, e, "POST", "/promote-memories", map[string]any{"datapack_id": e.DatapackID, "memory_ids": []string{"x"}},
		400, "VALIDATION_ERROR")
	pl := obj(okData(t, e, "GET", "/promoted-memories?datapack_id="+e.DatapackID, nil, 200))
	if num(pl["total"]) != 1 || obj(arr(pl["promoted_memories"])[0])["data"] != "beta fact" {
		t.Errorf("promoted %v", pl)
	}
	okData(t, e, "DELETE", "/promoted-memories/"+ids[1]+"?datapack_id="+e.DatapackID, nil, 200)
	wantErr(t, e, "DELETE", "/promoted-memories/"+ids[1]+"?datapack_id="+e.DatapackID, nil, 404, "NOT_FOUND")
	if d := obj(okData(t, e, "DELETE", "/memories/"+ids[2]+"?datapack_id="+e.DatapackID, nil, 200)); d["id"] != ids[2] {
		t.Errorf("delete %v", d)
	}
	wantErr(t, e, "DELETE", "/memories/nope?datapack_id="+e.DatapackID, nil, 400, "VALIDATION_ERROR")
}

func TestConversationRoutes(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	conv, err := e.Service.CreateConversation(ctx, memory.CreateConversation{AgentID: "a", DatapackID: e.DatapackID,
		SessionID: "s1", Title: "release planning"})
	if err != nil {
		t.Fatal(err)
	}
	remember := func(text string) {
		t.Helper()
		time.Sleep(5 * time.Millisecond)
		if _, err := e.Service.AddMemory(ctx, memory.AddMemory{DatapackID: e.DatapackID, AgentID: "a",
			ConversationID: conv.ID, Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	turn := func(input string) {
		t.Helper()
		time.Sleep(5 * time.Millisecond)
		if _, err := e.Service.AddMessage(ctx, memory.AddMessage{ConversationID: conv.ID, AgentID: "a",
			DatapackID: e.DatapackID, Input: input, Output: "ok", Seed: input}); err != nil {
			t.Fatal(err)
		}
	}
	remember("saved during the first turn")
	turn("when do we release?")
	remember("saved during the second turn")
	turn("and who signs off?")

	base := "/datapacks/" + e.DatapackID + "/conversations"
	l := obj(okData(t, e, "GET", base, nil, 200))
	cs := arr(l["conversations"])
	if len(cs) != 1 || obj(cs[0])["first_message"] != "when do we release?" || num(obj(cs[0])["messages"]) != 2 ||
		num(obj(cs[0])["memories_added"]) != 2 || obj(cs[0])["last_activity_at"] == nil {
		t.Fatalf("list %v", l)
	}
	if l = obj(okData(t, e, "GET", base+"?q=SIGNS", nil, 200)); len(arr(l["conversations"])) != 1 {
		t.Errorf("search by turn text %v", l)
	}
	if l = obj(okData(t, e, "GET", base+"?q=nothing-like-this", nil, 200)); len(arr(l["conversations"])) != 0 {
		t.Errorf("search misses %v", l)
	}

	d := obj(obj(okData(t, e, "GET", base+"/"+conv.ID, nil, 200))["conversation"])
	thread := arr(d["thread"])
	if len(thread) != 2 || d["thread_has_more"] != false || d["can_index"] != false {
		t.Fatalf("thread %v", d)
	}
	first, second := obj(thread[0]), obj(thread[1])
	if first["input"] != "when do we release?" || len(arr(first["memory_texts"])) != 1 ||
		arr(first["memory_texts"])[0] != "saved during the first turn" || num(first["memories_extracted"]) != 1 ||
		arr(second["memory_texts"])[0] != "saved during the second turn" {
		t.Errorf("each turn's memories: %v", thread)
	}
	// The second page's first turn still gets only the memories after the turn before it.
	page2 := arr(obj(obj(okData(t, e, "GET", base+"/"+conv.ID+"?messages_limit=1&messages_offset=1", nil,
		200))["conversation"])["thread"])
	if len(page2) != 1 || arr(obj(page2[0])["memory_texts"])[0] != "saved during the second turn" {
		t.Errorf("second page %v", page2)
	}
	// A page that the remaining turns exactly fill is the last.
	for q, more := range map[string]bool{"?messages_limit=1": true, "?messages_limit=1&messages_offset=1": false,
		"?messages_limit=2": false} {
		if d = obj(obj(okData(t, e, "GET", base+"/"+conv.ID+q, nil, 200))["conversation"]); d["thread_has_more"] != more {
			t.Errorf("%s: thread_has_more %v", q, d["thread_has_more"])
		}
	}
	wantErr(t, e, "GET", base+"/"+strings.Repeat("0", 32), nil, 404, "NOT_FOUND")
	wantErr(t, e, "GET", base+"/-", nil, 404, "NOT_FOUND") // not the latest conversation
}

func TestMetricsAndArtifacts(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	add := memory.AddMemory{DatapackID: e.DatapackID, Text: "the cache is cold"}
	if _, err := e.Service.AddMemory(ctx, add); err != nil {
		t.Fatal(err)
	}
	// One search through MCP, which the call log records.
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
		"name": "context_search", "arguments": map[string]any{"query": "the cache is cold", "conversation_id": "c",
			"agent_id": "claude_code:x"}}})
	req, _ := http.NewRequest("POST", e.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+testenv.ClientToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	m := obj(okData(t, e, "GET", "/datapacks/"+e.DatapackID+"/metrics", nil, 200))
	days := arr(m["daily_activity"])
	if num(m["total_requests"]) != 1 || num(m["agent_count"]) != 1 || m["last_accessed_at"] == nil || len(days) != 1 ||
		num(obj(days[0])["hybrid"]) != 1 || num(m["memory_chunk_count"]) != 1 || num(m["tokens_saved_total"]) == 0 ||
		num(obj(m["context_size"])["tokens"]) != 200 || m["artifact_count"] == nil {
		t.Errorf("metrics %v", m)
	}
	u := obj(okData(t, e, "GET", "/datapacks/"+e.DatapackID+"/usage", nil, 200))
	tools, agents := arr(u["tools"]), arr(u["agents"])
	if num(u["calls"]) != 1 || len(tools) != 1 || obj(tools[0])["tool"] != "context_search" ||
		len(agents) != 1 || obj(agents[0])["agent_id"] != "claude_code:x" || len(arr(u["recent"])) != 1 ||
		len(arr(u["steps"])) == 0 {
		t.Errorf("usage %v", u)
	}
	if r := obj(arr(u["recent"])[0]); r["id"] == nil || len(arr(r["steps"])) == 0 {
		t.Errorf("recent call %v", r)
	}
	if w := obj(okData(t, e, "GET", "/datapacks/"+e.DatapackID+"/usage?days=7", nil, 200)); num(w["calls"]) != 1 {
		t.Errorf("usage?days=7 %v", w)
	}
	wantErr(t, e, "GET", "/datapacks/"+e.DatapackID+"/metrics?days=week", nil, 400, "VALIDATION_ERROR")
	ts := obj(okData(t, e, "GET", "/datapacks/"+e.DatapackID+"/token-savings", nil, 200))
	if num(ts["total_requests"]) != 1 || len(arr(ts["by_datapack"])) != 1 {
		t.Errorf("token-savings %v", ts)
	}

	art, err := e.Service.PutArtifact(ctx, e.DatapackID, "a", "notes.md", "text/markdown", []byte("# notes"))
	if err != nil {
		t.Fatal(err)
	}
	base := "/datapacks/" + e.DatapackID + "/artifacts"
	l := obj(okData(t, e, "GET", base+"?q=NOTES", nil, 200))
	if num(l["total"]) != 1 || l["has_more"] != false || obj(arr(l["items"])[0])["content_base64"] != nil {
		t.Errorf("list %v", l)
	}
	if u := obj(okData(t, e, "POST", base+"/"+art.ContentHash+"/download-url", nil, 200)); u["url"] != nil ||
		u["stored_in"] != "db" {
		t.Errorf("download-url %v", u)
	}
	g := obj(okData(t, e, "GET", base+"/"+art.ContentHash, nil, 200))
	if g["content_base64"] != base64.StdEncoding.EncodeToString([]byte("# notes")) {
		t.Errorf("get %v", g)
	}
	wantErr(t, e, "GET", base+"/nothex", nil, 400, "INVALID_PARAM")
	wantErr(t, e, "GET", base+"/"+strings.Repeat("0", 64), nil, 404, "ARTIFACT_NOT_FOUND")
}

func TestRESTNeedsACredential(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	resp, err := http.Get(e.URL + "/api/v1/datapacks")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("no token: %d", resp.StatusCode)
	}
}

// With auth off, the Connect page doesn't hand the client token to callers that sent none.
func TestProvisionStatusWithAuthOff(t *testing.T) {
	e := testenv.Start(t, testenv.Options{Auth: "none"})
	resp, err := http.Get(e.URL + "/api/v1/datapacks/" + e.DatapackID + "/provision-status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r struct{ Data map[string]any }
	if err = json.NewDecoder(resp.Body).Decode(&r); err != nil || resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, err)
	}
	if r.Data["mcp_api_key"] != "" || r.Data["mcp_server_url"] == "" {
		t.Errorf("provision-status with no credential: %v", r.Data)
	}
}

// A turn's decision trace is the conversation's calls since the turn before it, what it read and
// what it wrote, without the calls that store the conversation, next to the reasoning and plan it
// sent. Calls from before the conversation existed (one deleted, then made again under the same
// id) aren't in it; calls that failed before their datapack was known are.
func TestDecisionTrace(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	const convID = "5a3c4b0f6d3e9b8a7c2d1e0f9a8b7c6d"
	stale := store.Call{At: time.Now(), Tool: "memory_add", DatapackID: e.DatapackID, ConversationID: convID, OK: true}
	e.Service.RecordCall(ctx, stale) // the earlier conversation's
	time.Sleep(5 * time.Millisecond)
	conv, err := e.Service.CreateConversation(ctx, memory.CreateConversation{AgentID: "a", DatapackID: e.DatapackID,
		SessionID: convID, Title: "trace"})
	if err != nil || conv.ID != convID {
		t.Fatal(conv.ID, err)
	}
	call := func(tool string, ok bool, datapack string) {
		t.Helper()
		time.Sleep(5 * time.Millisecond)
		e.Service.RecordCall(ctx, store.Call{At: time.Now(), Tool: tool, DatapackID: datapack, AgentID: "a",
			ConversationID: conv.ID, OK: ok, Ms: 12, MemoryN: 2,
			Steps: []store.Step{{Step: "memory_search", Ms: 9, N: 2}}})
	}
	turn := func(input, reasoning string, plan []string) {
		t.Helper()
		time.Sleep(5 * time.Millisecond)
		if _, err := e.Service.AddMessage(ctx, memory.AddMessage{ConversationID: conv.ID, AgentID: "a",
			DatapackID: e.DatapackID, Input: input, Output: "ok", Seed: input, Reasoning: reasoning,
			Plan: plan}); err != nil {
			t.Fatal(err)
		}
	}
	call("context_search", true, e.DatapackID)
	call("memory_add", false, "")                        // failed before its datapack was known
	call("conversation_add_message", true, e.DatapackID) // stores the turn: not part of its trace
	turn("when do we release?", "The calendar memory says Tuesdays.", []string{"Search memories", "Answer"})
	call("knowledgebase_search", true, e.DatapackID)
	call("memory_add", true, "other-datapack") // the same id in another datapack
	turn("who signs off?", "", nil)

	reply := obj(okData(t, e, "GET", "/datapacks/"+e.DatapackID+"/conversations/"+conv.ID, nil, 200))
	th := arr(obj(reply["conversation"])["thread"])
	if len(th) != 2 {
		t.Fatalf("thread %v", th)
	}
	first, second := obj(th[0]), obj(th[1])
	tools := func(tr map[string]any) []string {
		calls := arr(tr["calls"])
		out := make([]string, 0, len(calls))
		for _, c := range calls {
			out = append(out, str(obj(c)["tool"]))
		}
		return out
	}
	if got := tools(first); len(got) != 2 || got[0] != "context_search" || got[1] != "memory_add" {
		t.Errorf("first turn's calls %v", got)
	}
	if c := obj(arr(first["calls"])[0]); c["ok"] != true || num(c["ms"]) != 12 || num(c["memory_n"]) != 2 ||
		len(arr(c["steps"])) != 1 || c["id"] == nil || c["read_only"] != true {
		t.Errorf("a call %v", c)
	}
	if c := obj(arr(first["calls"])[1]); c["ok"] != false || c["read_only"] != false {
		t.Errorf("the failed memory_add %v", c)
	}
	if first["reasoning"] != "The calendar memory says Tuesdays." || len(arr(first["plan"])) != 2 {
		t.Errorf("first turn's reasoning and plan %v %v", first["reasoning"], first["plan"])
	}
	if got := tools(second); len(got) != 1 || got[0] != "knowledgebase_search" {
		t.Errorf("second turn's calls %v", got)
	}
	if second["reasoning"] != nil || arr(second["calls"]) == nil {
		t.Errorf("second turn %v", second)
	}
}

// A memory's history is each change mem0 recorded, oldest first, its text before and after. Only a
// memory the local user can get has one: a deleted or promoted memory, or an unknown id, is 404.
func TestMemoryHistory(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	added, err := e.Service.AddMemory(ctx, memory.AddMemory{DatapackID: e.DatapackID, Text: "ship on Tuesdays"})
	if err != nil || len(added) != 1 {
		t.Fatal(added, err)
	}
	id := added[0].ID
	if err = e.Service.UpdateMemory(ctx, e.DatapackID, id, "ship on Wednesdays"); err != nil {
		t.Fatal(err)
	}
	path := "/memories/" + id + "/history?datapack_id=" + e.DatapackID
	h := arr(obj(okData(t, e, "GET", path, nil, 200))["history"])
	if len(h) != 2 || obj(h[0])["action"] != "ADD" || obj(h[0])["new_value"] != "ship on Tuesdays" ||
		obj(h[1])["action"] != "UPDATE" || obj(h[1])["previous_value"] != "ship on Tuesdays" ||
		obj(h[1])["new_value"] != "ship on Wednesdays" {
		t.Fatalf("history %v", h)
	}
	upper := "/memories/" + strings.ToUpper(id) + "/history?datapack_id=" + e.DatapackID
	if u := obj(okData(t, e, "GET", upper, nil, 200)); u["memory_id"] != id || len(arr(u["history"])) != 2 {
		t.Errorf("an uppercase id %v", u)
	}
	if _, err = e.Service.PromoteMemories(ctx, e.DatapackID, "", []string{id}); err != nil {
		t.Fatal(err)
	}
	wantErr(t, e, "GET", path, nil, 404, "NOT_FOUND") // promoted: in the collective memory now
	wantErr(t, e, "GET", "/memories/00000000-0000-0000-0000-000000000000/history?datapack_id="+e.DatapackID, nil,
		404, "NOT_FOUND")
	wantErr(t, e, "GET", "/memories/nope/history?datapack_id="+e.DatapackID, nil, 400, "VALIDATION_ERROR")
}
