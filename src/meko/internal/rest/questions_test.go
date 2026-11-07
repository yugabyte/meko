package rest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/secrets"
	"github.com/yugabyte/meko/src/meko/internal/store"
	"github.com/yugabyte/meko/src/meko/internal/testenv"
)

// status posts with the client token and returns only its status, for a 204.
func status(t *testing.T, e *testenv.Env, path string, body any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, e.URL+"/api/v1"+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+testenv.ClientToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestContextGaps(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	dp := e.DatapackID
	base := "/datapacks/" + dp + "/questions"
	search := func(query, agent, conv string) {
		t.Helper()
		okData(t, e, http.MethodPost, "/datapacks/"+dp+"/search", map[string]any{"query": query, "agent_id": agent,
			"conversation_id": conv}, http.StatusOK)
	}
	list := func(q string) map[string]any {
		t.Helper()
		return obj(okData(t, e, http.MethodGet, base+q, nil, http.StatusOK))
	}

	// An empty datapack answers nothing: each search is an ask of its question.
	search("Where do the release notes come from?", "claude_code:a", "c1")
	search("where do the  release notes come from?", "kiro:b", "c2")
	search("Who owns the deploy key?", "claude_code:a", "c1")
	page := list("")
	qs := arr(page["questions"])
	if len(qs) != 2 || num(obj(page["counts"])["open"]) != 2 || num(obj(page["open"])["empty"]) != 2 ||
		num(page["searches"]) != 3 || page["since"] == nil {
		t.Fatalf("page %v", page)
	}
	first := obj(qs[1])
	if first["key"] != "where do the release notes come from?" ||
		first["query"] != "where do the  release notes come from?" ||
		num(first["asks"]) != 2 || first["quality"] != "empty" || first["tool"] != "context_search" ||
		first["state"] != "open" || len(arr(first["agents"])) != 2 || len(arr(first["conversations"])) != 2 {
		t.Errorf("question %v", first)
	}
	if mine := arr(list("?state=all&conversation_id=c2")["questions"]); obj(mine[0])["asked_by_conversation"] != false ||
		obj(mine[1])["asked_by_conversation"] != true {
		t.Errorf("asked by c2: %v", mine)
	}
	key := str(first["key"])

	// Paging after the last one shown.
	last := obj(qs[0])
	rest := arr(list("?after=" + url.QueryEscape(str(last["last_at"])) + "&after_key=" +
		url.QueryEscape(str(last["key"])))["questions"])
	if len(rest) != 1 || obj(rest[0])["key"] != key {
		t.Errorf("the next page: %v", rest)
	}

	// Dismiss, then restore.
	if s := status(t, e, base+"/mark", map[string]any{"key": key, "state": "dismissed"}); s != 204 {
		t.Fatalf("dismiss: %d", s)
	}
	d := arr(list("?state=dismissed")["questions"])
	if len(d) != 1 || obj(d[0])["dismissal"] == nil || obj(obj(d[0])["dismissal"])["conversation_id"] != nil {
		t.Errorf("dismissed by a person: %v", d)
	}
	if s := status(t, e, base+"/mark", map[string]any{"key": key, "state": nil}); s != 204 {
		t.Fatalf("restore: %d", s)
	}
	if n := num(obj(list("")["counts"])["open"]); n != 2 {
		t.Errorf("restored: %v open", n)
	}

	// Teach: a memory saved, the question taught.
	q := obj(okData(t, e, http.MethodPost, base+"/teach", map[string]any{"key": key,
		"text": "The release notes come from the commit subjects."}, http.StatusOK))
	taught := obj(q["taught"])
	if q["state"] != "taught" || taught["text"] != "The release notes come from the commit subjects." ||
		taught["memory_id"] == nil || taught["conversation_id"] != nil {
		t.Errorf("taught %v", q)
	}
	mem := arr(obj(okData(t, e, http.MethodGet, "/memories?datapack_id="+dp, nil, http.StatusOK))["memories"])
	if len(mem) != 1 || obj(mem[0])["data"] != "The release notes come from the commit subjects." {
		t.Errorf("the memory saved: %v", mem)
	}

	// An agent's marks name its conversation.
	other := str(obj(arr(list("")["questions"])[0])["key"])
	if s := status(t, e, base+"/mark", map[string]any{"key": other, "state": "taught",
		"text": "Ops owns it.", "memory_id": "m-1", "conversation_id": "c9"}); s != 204 {
		t.Fatalf("an agent's teach: %d", s)
	}
	tq := arr(list("?state=taught")["questions"])
	if len(tq) != 2 {
		t.Fatalf("taught %v", tq)
	}
	for _, x := range tq {
		if obj(x)["key"] == other && (obj(obj(x)["taught"])["conversation_id"] != "c9" ||
			obj(obj(x)["taught"])["memory_id"] != "m-1") {
			t.Errorf("an agent's teach %v", x)
		}
	}

	// An agent's dismissal names the memory that already said it, with its text.
	mid := str(obj(arr(obj(okData(t, e, http.MethodPost, "/datapacks/"+dp+"/memories",
		map[string]any{"text": "Lunch is at noon on Fridays."}, http.StatusOK))["results"])[0])["id"])
	if s := status(t, e, base+"/mark", map[string]any{"key": other, "state": "dismissed",
		"memory_id": mid, "conversation_id": "c9"}); s != 204 {
		t.Fatalf("an agent's dismissal: %d", s)
	}
	dq := obj(arr(list("?state=dismissed")["questions"])[0])
	if dm := obj(dq["dismissal"]); dm["memory_id"] != mid || dm["memory_text"] != "Lunch is at noon on Fridays." ||
		dm["conversation_id"] != "c9" {
		t.Errorf("an agent's dismissal %v", dq)
	}

	// The usage stats count the person's teach as a person's and the agent's marks as an agent's (the
	// person's dismissal was restored). Summed over days, in case the marks straddle UTC midnight.
	days, err := e.Store.QuestionDays(context.Background(), time.Now().Add(-24*time.Hour))
	var sum store.QuestionDay
	for _, d := range days {
		sum.Dismissed, sum.Taught = sum.Dismissed+d.Dismissed, sum.Taught+d.Taught
		sum.AgentDismissed, sum.AgentTaught = sum.AgentDismissed+d.AgentDismissed, sum.AgentTaught+d.AgentTaught
	}
	if err != nil || sum != (store.QuestionDay{Taught: 1, AgentDismissed: 1, AgentTaught: 1}) {
		t.Errorf("usage stats' marks %+v %v", sum, err)
	}

	// Asked again after a mark, a question is open again.
	search("Who owns the deploy key?", "claude_code:a", "c3")
	if n := num(obj(list("")["counts"])["open"]); n != 1 {
		t.Errorf("asked again: %v open", n)
	}

	// Errors.
	wantErr(t, e, http.MethodGet, base+"?state=closed", nil, 400, "VALIDATION_ERROR")
	wantErr(t, e, http.MethodGet, base+"?after=yesterday", nil, 400, "VALIDATION_ERROR")
	wantErr(t, e, http.MethodPost, base+"/mark", map[string]any{"key": "nothing like it", "state": "dismissed"}, 404,
		"NOT_FOUND")
	wantErr(t, e, http.MethodPost, base+"/mark", map[string]any{"key": key, "state": "taught"}, 400, "VALIDATION_ERROR")
	wantErr(t, e, http.MethodPost, base+"/mark", map[string]any{"key": key, "state": "closed"}, 400, "VALIDATION_ERROR")
	wantErr(t, e, http.MethodPost, base+"/teach", map[string]any{"key": key, "text": " "}, 400, "VALIDATION_ERROR")
	wantErr(t, e, http.MethodPost, base+"/teach", map[string]any{"key": key, "text": strings.Repeat("x", 2001)}, 400,
		"VALIDATION_ERROR")
	wantErr(t, e, http.MethodGet, "/datapacks/00000000-0000-0000-0000-000000000000/questions", nil, 404, "NOT_FOUND")
	wantErr(t, e, http.MethodDelete, base+"/asks", nil, 400, "VALIDATION_ERROR")

	// Forgetting a conversation's asks: c3's ask goes, and with it the question it reopened stays
	// taught; c1 and c2's go, and the questions left with none.
	del := obj(okData(t, e, http.MethodDelete, base+"/asks?conversation_id=c3&before="+
		url.QueryEscape(time.Now().UTC().Format(time.RFC3339Nano)), nil, http.StatusOK))
	if num(del["deleted"]) != 1 {
		t.Errorf("deleted %v", del)
	}
	if n := num(obj(list("")["counts"])["open"]); n != 0 {
		t.Errorf("after forgetting c3: %v open", n)
	}
	okData(t, e, http.MethodDelete, base+"/asks?conversation_id=c1", nil, http.StatusOK)
	okData(t, e, http.MethodDelete, base+"/asks?conversation_id=c2", nil, http.StatusOK)
	if n := len(arr(list("?state=all")["questions"])); n != 0 {
		t.Errorf("after forgetting every ask: %d questions", n)
	}
}

// A Meko Cloud datapack's questions are kept on this Meko (its searches pass through /mcp): any
// credential reads and marks them, and teaching one writes the memory to Meko Cloud, for the owner.
func TestContextGapsCloud(t *testing.T) {
	cloud := newFakeCloud(t)
	dir := t.TempDir()
	link := &cloudlink.Service{
		Store:    cloudlink.Store{File: filepath.Join(dir, "cloud.toml"), Secrets: secrets.NewFiles(dir)},
		Check:    func(context.Context, string, string) error { return nil },
		CheckAPI: func(context.Context, string) error { return nil },
	}
	e := testenv.Start(t, testenv.Options{Cloud: link})
	base := "/datapacks/" + cloudDP + "/questions"
	wantErr(t, e, http.MethodGet, base, nil, 404, "NOT_FOUND")
	wantErr(t, e, http.MethodGet, base+"?location=cloud", nil, 409, "CLOUD_NOT_LINKED")
	if _, err := link.Link(context.Background(), cloudlink.Update{URL: ptr(cloud.URL + "/mcp"),
		APIURL: ptr(cloud.URL + "/api/v1"), Key: ptr(cloudKey)}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.AddAsk(context.Background(), store.Ask{DatapackID: cloudDP, Key: "who owns the pager?",
		At: time.Now(), Query: "Who owns the pager?", Quality: "empty", Tool: "context_search",
		AgentID: "claude_code:a", ConversationID: "c1"}); err != nil {
		t.Fatal(err)
	}
	qs := arr(obj(okData(t, e, http.MethodGet, base, nil, http.StatusOK))["questions"])
	if pg := obj(okData(t, e, http.MethodGet, base, nil, http.StatusOK)); pg["location"] != "cloud" {
		t.Errorf("location %v", pg["location"])
	}
	if len(qs) != 1 || obj(qs[0])["query"] != "Who owns the pager?" {
		t.Fatalf("Meko Cloud's questions, with the client token: %v", qs)
	}
	if s := status(t, e, base+"/mark", map[string]any{"key": "who owns the pager?", "state": "dismissed",
		"conversation_id": "c9"}); s != 204 {
		t.Fatalf("an agent's dismissal: %d", s)
	}
	q := obj(okData(t, e, http.MethodPost, base+"/teach",
		map[string]any{"key": "who owns the pager?", "text": "Ops owns the pager."}, http.StatusOK))
	if q["state"] != "taught" || obj(q["taught"])["memory_id"] != "m-1" {
		t.Fatalf("taught in Meko Cloud with the client token: %v", q)
	}
	if a := cloud.args["memory_add"]; a["datapack_id"] != cloudDP || a["text"] != "Ops owns the pager." {
		t.Errorf("memory_add sent to Meko Cloud: %v", a)
	}
}
