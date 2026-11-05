package rest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/store"
	"github.com/yugabyte/meko/src/meko/internal/testenv"
)

// The Calls page's routes: the call log a page at a time with its filters, one call with its kept
// contents, and "Keep call contents", which only the owner turns on or clears.
func TestCallRoutes(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	const settings = "/settings/call-contents"
	if st := obj(okData(t, e, "GET", settings, nil, 200)); st["keep"] != false || num(st["retention_days"]) != 7 {
		t.Fatalf("on by default: %v", st)
	}
	// The client token (an agent's, or AgentK's) reads calls but can't start keeping them.
	if r := doAs(t, e, testenv.ClientToken, "PUT", settings, map[string]any{"keep": true}); r.Status != 403 {
		t.Errorf("the client token turned it on: %+v", r)
	}
	if r := doAs(t, e, testenv.AdminToken, "PUT", settings, map[string]any{}); r.Status != 400 {
		t.Errorf("no keep: %+v", r)
	}
	if r := doAs(t, e, testenv.AdminToken, "PUT", settings, map[string]any{"keep": true}); r.Status != 200 ||
		obj(r.Data)["keep"] != true {
		t.Fatalf("turned on: %+v", r)
	}

	for i := range 3 {
		e.Service.RecordCall(ctx, store.Call{At: time.Now(), Tool: "context_search", AgentID: "claude_code:a",
			DatapackID: e.DatapackID, OK: i != 1, ErrorCode: map[bool]string{true: "x"}[i == 1],
			Client: map[bool]string{true: "agentk"}[i == 2]},
			json.RawMessage(fmt.Sprintf(`{"query":"q%d"}`, i)), fmt.Sprintf("reply %d", i))
	}
	e.Service.RecordCall(ctx, store.Call{At: time.Now(), Tool: "datapack_list", AgentID: "kiro:b", OK: true}, nil, "")

	page := obj(okData(t, e, "GET", "/calls?agent=claude_code:a&limit=2", nil, 200))
	calls := arr(page["calls"])
	if len(calls) != 2 || page["more"] != true || obj(calls[0])["client"] != "agentk" ||
		obj(calls[0])["has_contents"] != true || obj(calls[0])["datapack_id"] != e.DatapackID {
		t.Fatalf("first page %v", page)
	}
	if _, says := obj(calls[0])["location"]; says {
		t.Errorf("a call says where it ran: %v", calls[0])
	}
	last := int(num(obj(calls[1])["id"]))
	older := obj(okData(t, e, "GET", fmt.Sprintf("/calls?agent=claude_code:a&before=%d", last), nil, 200))
	if len(arr(older["calls"])) != 1 || older["more"] != false {
		t.Errorf("older page %v", older)
	}
	for q, want := range map[string]int{"": 4, "?failed=true": 1, "?client=agentk": 1, "?tool=datapack_list": 1,
		"?datapack=" + e.DatapackID: 3} {
		if n := len(arr(obj(okData(t, e, "GET", "/calls"+q, nil, 200))["calls"])); n != want {
			t.Errorf("/calls%s: %d calls, want %d", q, n, want)
		}
	}
	wantErr(t, e, "GET", "/calls?limit=0", nil, 400, "VALIDATION_ERROR")
	wantErr(t, e, "GET", "/calls?before=x", nil, 400, "VALIDATION_ERROR")

	id := int(num(obj(calls[0])["id"]))
	one := obj(okData(t, e, "GET", fmt.Sprintf("/calls/%d", id), nil, 200))
	if one["args"] != `{"query":"q2"}` || one["reply"] != "reply 2" || one["cut"] != false {
		t.Errorf("one call %v", one)
	}
	wantErr(t, e, "GET", "/calls/99999", nil, 404, "NOT_FOUND")
	wantErr(t, e, "GET", "/calls/x", nil, 400, "VALIDATION_ERROR")

	wantErr(t, e, "DELETE", "/calls/contents", nil, 403, "FORBIDDEN")
	if r := doAs(t, e, testenv.AdminToken, "DELETE", "/calls/contents", nil); r.Status != 200 ||
		num(obj(r.Data)["calls"]) != 0 {
		t.Fatalf("cleared: %+v", r)
	}
	if one = obj(okData(t, e, "GET", fmt.Sprintf("/calls/%d", id), nil, 200)); one["args"] != nil ||
		one["has_contents"] != false {
		t.Errorf("after clearing %v", one)
	}
}
