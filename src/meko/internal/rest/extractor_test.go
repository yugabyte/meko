package rest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/agents"
	"github.com/yugabyte/meko/src/meko/internal/memory"
	"github.com/yugabyte/meko/src/meko/internal/testenv"
)

// fakeHarness answers extraction calls with what answer returns, recording each call.
type fakeHarness struct {
	mu      sync.Mutex
	systems []string
	inputs  []string
	answer  func(input string) ([]memory.ExtractedFact, error)
}

func (h *fakeHarness) Extract(_ context.Context, system, input string) ([]memory.ExtractedFact, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.systems = append(h.systems, system)
	h.inputs = append(h.inputs, input)
	if h.answer == nil {
		return nil, nil
	}
	return h.answer(input)
}

func (h *fakeHarness) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.systems, h.inputs = nil, nil
}

func (h *fakeHarness) last() (string, string, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.inputs) == 0 {
		return "", "", 0
	}
	return h.systems[len(h.systems)-1], h.inputs[len(h.inputs)-1], len(h.inputs)
}

// fakeClaude puts a claude first on PATH and hides this computer's agents (no home,
// system folders or login shell), so the server finds that claude and only it. It returns
// its path.
func fakeClaude(t *testing.T) string {
	t.Helper()
	folders, shell := agents.SystemFolders, agents.LoginShell
	agents.SystemFolders, agents.LoginShell = nil, ""
	t.Cleanup(func() { agents.SystemFolders, agents.LoginShell = folders, shell })
	t.Setenv("HOME", t.TempDir())
	bin := t.TempDir()
	claude := filepath.Join(bin, "claude")
	//nolint:gosec // G306: a test script
	if err := os.WriteFile(claude, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH")) // the context engine's Node too
	return claude
}

// asAdmin sends a PUT with the CLI's admin token.
func asAdmin(t *testing.T, e *testenv.Env, path string, body any) reply {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPut, e.URL+"/api/v1"+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+testenv.AdminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out reply
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("not an envelope: %s", raw)
	}
	out.Status = resp.StatusCode
	return out
}

// The memory extractor: off until the admin sets it up; then turns are queued, a run sends each
// conversation's in one call (trivial ones left out, the user's messages as the new turns), and
// the facts it returns are saved with their turn, near-duplicates dropped. A failed call keeps the
// turns queued; an opted-out datapack queues nothing.
func TestMemoryExtractor(t *testing.T) {
	h := &fakeHarness{}
	claude := fakeClaude(t)
	e := testenv.Start(t, testenv.Options{ExtractorHarness: h})
	ctx := context.Background()
	const settings = "/settings/memory-extractor"
	if st := obj(okData(t, e, "GET", settings, nil, 200)); st["enabled"] != false {
		t.Fatalf("on by default: %v", st)
	}
	conv, err := e.Service.CreateConversation(ctx, memory.CreateConversation{AgentID: "claude_code:x",
		DatapackID: e.DatapackID, SessionID: "s", Title: "setup"})
	if err != nil {
		t.Fatal(err)
	}
	turn := func(seed, input string) string {
		t.Helper()
		m, aerr := e.Service.AddMessage(ctx, memory.AddMessage{ConversationID: conv.ID, AgentID: "claude_code:x",
			DatapackID: e.DatapackID, Input: input, Output: "Noted, I'll keep that in mind for this project.",
			Seed: seed})
		if aerr != nil {
			t.Fatal(aerr)
		}
		return m.MessageID
	}
	turn("0", "We deploy with helm on GKE in us-west1.")
	if st := obj(okData(t, e, "GET", settings, nil, 200)); num(st["queued"]) != 0 {
		t.Errorf("queued while off: %v", st)
	}

	wantErr(t, e, "PUT", settings, map[string]any{"harness": "claude"}, 403, "FORBIDDEN")
	if r := asAdmin(t, e, settings, map[string]any{"harness": "codex"}); r.Status != 400 {
		t.Errorf("an unknown harness: %+v", r)
	}
	r := asAdmin(t, e, settings, map[string]any{"harness": "claude", "interval_seconds": 120})
	if r.Status != 200 || obj(r.Data)["enabled"] != true || obj(r.Data)["command"] != claude ||
		obj(r.Data)["model"] != "haiku" || num(obj(r.Data)["interval_seconds"]) != 120 {
		t.Fatalf("set up: %+v", r)
	}
	if _, _, n := h.last(); n != 1 {
		t.Errorf("setting up made %d test calls, want 1", n)
	}
	h.reset()

	trivialID := turn("1", "ok")
	helm := turn("2", "I always deploy with helm on GKE in us-west1, never with kubectl apply.")
	pnpm := turn("3", "We use pnpm in this repo, never npm, and tests live in __tests__.")
	if st := obj(okData(t, e, "GET", settings, nil, 200)); num(st["queued"]) != 3 {
		t.Fatalf("queued %v", st)
	}
	h.answer = func(string) ([]memory.ExtractedFact, error) {
		return []memory.ExtractedFact{{Text: "The user deploys with helm on GKE in us-west1.", Turn: 0},
			{Text: "The repo uses pnpm, never npm.", Turn: 1}, {Text: "out of range", Turn: 7}}, nil
	}
	run := obj(okData(t, e, "POST", settings+"/run", nil, 200))
	if num(run["memories"]) != 2 || num(run["turns"]) != 3 || num(run["conversations"]) != 1 {
		t.Fatalf("run %v", run)
	}
	system, input, calls := h.last()
	if calls != 1 || !strings.Contains(input, "[turn 0]") || !strings.Contains(input, "helm on GKE") ||
		!strings.Contains(input, "[turn 1]") || strings.Contains(input, "[turn 2]") ||
		!strings.Contains(input, "Earlier turns (context only)") || !strings.Contains(system, "USER's messages") {
		t.Errorf("call %d:\n%s\n%s", calls, system, input)
	}
	byText := map[string]map[string]any{}
	for _, m := range arr(obj(okData(t, e, "GET", "/memories?datapack_id="+e.DatapackID, nil, 200))["memories"]) {
		byText[str(obj(m)["data"])] = obj(m)
	}
	if m := byText["The user deploys with helm on GKE in us-west1."]; m == nil || m["source"] != "extract" ||
		m["message_id"] != helm || m["conversation_id"] != conv.ID || m["agent_id"] != "claude_code:x" {
		t.Errorf("helm memory %v", m)
	}
	if m := byText["The repo uses pnpm, never npm."]; m == nil || m["message_id"] != pnpm {
		t.Errorf("pnpm memory %v", m)
	}
	// In the thread, each extracted memory is on the turn it names, though it was saved after later turns.
	for _, tn := range arr(obj(okData(t, e, "GET", "/datapacks/"+e.DatapackID+"/conversations/"+conv.ID, nil,
		200))["thread"]) {
		texts := arr(obj(tn)["memory_texts"])
		switch obj(tn)["message_id"] {
		case helm:
			if len(texts) != 1 || texts[0] != "The user deploys with helm on GKE in us-west1." {
				t.Errorf("helm turn's memories %v", texts)
			}
		case trivialID:
			if len(texts) != 0 {
				t.Errorf("the trivial turn got %v", texts)
			}
		}
	}

	// Nothing waiting: no call. The same fact again is dropped as a duplicate.
	okData(t, e, "POST", settings+"/run", nil, 200)
	if _, _, n := h.last(); n != 1 {
		t.Errorf("a run with nothing waiting made a call: %d", n)
	}
	turn("4", "Remember again that we deploy with helm on GKE in us-west1.")
	h.answer = func(string) ([]memory.ExtractedFact, error) {
		return []memory.ExtractedFact{{Text: "The user deploys with helm on GKE in us-west1.", Turn: 0}}, nil
	}
	if run = obj(okData(t, e, "POST", settings+"/run", nil, 200)); num(run["memories"]) != 0 {
		t.Errorf("a duplicate was saved: %v", run)
	}

	// A failed call keeps its turns queued and says why, and the run goes on to the next conversation.
	turn("5", "The release train leaves every Tuesday at 10am Pacific.")
	other, err := e.Service.CreateConversation(ctx, memory.CreateConversation{AgentID: "claude_code:x",
		DatapackID: e.DatapackID, SessionID: "s2", Title: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Service.AddMessage(ctx, memory.AddMessage{ConversationID: other.ID, AgentID: "claude_code:x",
		DatapackID: e.DatapackID, Input: "Our staging database is called orchid.", Output: "Noted.",
		Seed: "o"}); err != nil {
		t.Fatal(err)
	}
	h.answer = func(input string) ([]memory.ExtractedFact, error) {
		if strings.Contains(input, "release train") {
			return nil, errors.New("not signed in")
		}
		return []memory.ExtractedFact{{Text: "The staging database is called orchid.", Turn: 0}}, nil
	}
	run = obj(okData(t, e, "POST", settings+"/run", nil, 200))
	if num(run["failed"]) != 1 || num(run["memories"]) != 1 || !strings.Contains(str(run["last_error"]), "not signed in") {
		t.Errorf("a run with one failing conversation %v", run)
	}
	if st := obj(okData(t, e, "GET", settings, nil, 200)); num(st["queued"]) != 1 ||
		!strings.Contains(str(st["last_error"]), "not signed in") {
		t.Errorf("after a failure %v", st)
	}
	// A turn that keeps failing leaves the queue after its fifth attempt.
	for range 4 {
		okData(t, e, "POST", settings+"/run", nil, 200)
	}
	okData(t, e, "POST", settings+"/run", nil, 200)
	if st := obj(okData(t, e, "GET", settings, nil, 200)); num(st["queued"]) != 0 {
		t.Errorf("a turn that kept failing is still queued: %v", st)
	}

	// A datapack's profile and instructions reach the prompt; an opted-out one queues nothing.
	dp := "/datapacks/" + e.DatapackID + "/memory-extraction"
	wantErr(t, e, "PATCH", dp, map[string]any{"profile": "poetry"}, 400, "VALIDATION_ERROR")
	okData(t, e, "PATCH", dp, map[string]any{"profile": "coding", "instructions": "Keep ticket keys."}, 200)
	h.answer = nil
	turn("7", "Every PR title starts with its ticket key, such as MEKO-640.")
	okData(t, e, "POST", settings+"/run", nil, 200)
	if system, _, _ = h.last(); !strings.Contains(system, "coding conversations") ||
		!strings.Contains(system, "Keep ticket keys.") {
		t.Errorf("profile and instructions missing:\n%s", system)
	}
	if d := obj(okData(t, e, "PATCH", dp, map[string]any{"opt_out": true}, 200)); d["opt_out"] != true ||
		d["profile"] != "coding" {
		t.Errorf("opt out %v", d)
	}
	turn("6", "This turn is in a datapack that opted out of extraction.")
	if st := obj(okData(t, e, "GET", settings, nil, 200)); num(st["queued"]) != 0 {
		t.Errorf("an opted-out datapack queued a turn: %v", st)
	}

	// Off: a run is refused.
	if r = asAdmin(t, e, settings, map[string]any{"harness": ""}); r.Status != 200 ||
		obj(r.Data)["enabled"] != false {
		t.Errorf("off: %+v", r)
	}
	wantErr(t, e, "POST", settings+"/run", nil, 409, "EXTRACTOR_OFF")
}

// A turn replayed with metadata.session_date is extracted as said on that date: the call shows
// that date, and the memory is backdated to it, as Cloud backdates memories from the same key.
func TestMemoryExtractorSessionDate(t *testing.T) {
	h := &fakeHarness{}
	fakeClaude(t)
	e := testenv.Start(t, testenv.Options{ExtractorHarness: h})
	ctx := context.Background()
	if r := asAdmin(t, e, "/settings/memory-extractor", map[string]any{"harness": "claude"}); r.Status != 200 {
		t.Fatalf("set up: %+v", r)
	}
	h.reset()
	conv, err := e.Service.CreateConversation(ctx, memory.CreateConversation{AgentID: "claude_code:x",
		DatapackID: e.DatapackID, SessionID: "s", Title: "replay"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Service.AddMessage(ctx, memory.AddMessage{ConversationID: conv.ID, AgentID: "claude_code:x",
		DatapackID: e.DatapackID, Input: "I bought five tops at H&M last weekend for the trip.", Seed: "0",
		Metadata: map[string]any{"session_date": "2023-05-20T22:00:00-07:00"}}); err != nil {
		t.Fatal(err)
	}
	h.answer = func(string) ([]memory.ExtractedFact, error) {
		return []memory.ExtractedFact{{Text: "The user bought five tops at H&M.", Turn: 0}}, nil
	}
	if run := obj(okData(t, e, "POST", "/settings/memory-extractor/run", nil, 200)); num(run["memories"]) != 1 {
		t.Fatalf("run %v", run)
	}
	if _, input, _ := h.last(); !strings.Contains(input, "[turn 0] (2023-05-20)") {
		t.Errorf("the call doesn't date the turn by its session:\n%s", input)
	}
	found, err := e.Service.SearchMemories(ctx, memory.Search{DatapackID: e.DatapackID, Query: "H&M tops", Limit: 5})
	if err != nil || len(found) != 1 || found[0].CreatedAt != "2023-05-21T05:00:00.000Z" {
		t.Errorf("memory %+v, %v: want created_at at the session's instant, 2023-05-21T05:00Z", found, err)
	}
}
