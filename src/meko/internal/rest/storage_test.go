package rest_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/memory"
	"github.com/yugabyte/meko/src/meko/internal/server"
	"github.com/yugabyte/meko/src/meko/internal/testenv"
)

// The storage limit on REST: a write past it is 402 STORAGE_LIMIT_EXCEEDED, an upload link holds its
// file's size only until the file is stored, the status reply says how full Meko is, and the memory
// extractor makes no call while Meko is full, keeping the turns queued.
func TestStorageLimitRoutes(t *testing.T) {
	h := &fakeHarness{}
	fakeClaude(t)
	const limit = 8 << 10
	e := testenv.Start(t, testenv.Options{ExtractorHarness: h, MaxContentBytes: limit})
	ctx := context.Background()
	const settings = "/settings/memory-extractor"
	if r := asAdmin(t, e, settings, map[string]any{"harness": "claude"}); r.Status != 200 {
		t.Fatalf("set up the extractor: %+v", r)
	}
	h.reset()
	conv, err := e.Service.CreateConversation(ctx, memory.CreateConversation{AgentID: "claude_code:x",
		DatapackID: e.DatapackID, SessionID: "s", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Service.AddMessage(ctx, memory.AddMessage{ConversationID: conv.ID, AgentID: "claude_code:x",
		DatapackID: e.DatapackID, Input: "We deploy with helm on GKE in us-west1, never kubectl apply.",
		Output: "Noted.", Seed: "1"}); err != nil {
		t.Fatal(err)
	}

	// Two files fit, one after the other: the first link's hold ends once its file is stored.
	file := func(seed string) []byte { return []byte(strings.Repeat(seed+" ", 3000/(len(seed)+1))) }
	uploadFile(t, e, "one.md", file("one"))
	uploadFile(t, e, "two.md", file("two"))
	three := file("three")
	wantErr(t, e, "POST", "/datapacks/"+e.DatapackID+"/knowledge-bases/upload-url", map[string]any{
		"filename": "three.md", "size": len(three), "content_hash": sha(three)}, http.StatusPaymentRequired,
		"STORAGE_LIMIT_EXCEEDED")

	// Short of full by less than a fact needs: the extractor makes no call, and says why.
	st, err := e.Service.Storage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Service.PutArtifact(ctx, e.DatapackID, "a", "fill.txt", "text/plain",
		[]byte(strings.Repeat("x", int(limit-st.UsedBytes)-500))); err != nil {
		t.Fatal(err)
	}
	wantErr(t, e, "POST", settings+"/run", nil, http.StatusPaymentRequired, "STORAGE_LIMIT_EXCEEDED")
	wantErr(t, e, "POST", "/datapacks/"+e.DatapackID+"/memories", map[string]any{"text": strings.Repeat("y", 600)},
		http.StatusPaymentRequired, "STORAGE_LIMIT_EXCEEDED")
	if _, _, n := h.last(); n != 0 {
		t.Errorf("a full Meko called the extractor %d times", n)
	}
	if s := obj(okData(t, e, "GET", settings, nil, 200)); num(s["queued"]) != 1 ||
		!strings.Contains(str(s["last_error"]), "Meko is full") {
		t.Errorf("extractor status %v", s)
	}

	// The status reply.
	req, _ := http.NewRequest(http.MethodGet, e.URL+"/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+testenv.AdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var status struct{ Data server.StatusReply }
	if err = json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("status: %s", raw)
	}
	s := status.Data.Storage
	if s == nil || s.UsedBytes != limit-500 || s.LimitBytes != limit || s.Knowledge != int64(len(file("one"))*2) ||
		s.Artifacts != limit-st.UsedBytes-500 || s.Conversations == 0 || s.Memories != 0 {
		t.Errorf("status storage %s", raw)
	}
}
