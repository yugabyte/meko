package mcp_test

import (
	"context"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/memory"
	"github.com/yugabyte/meko/src/meko/internal/memoryengine"
	"github.com/yugabyte/meko/src/meko/internal/testenv"
)

// The search tools record the searches Meko answered poorly for Context Gaps, except AgentK's own
// and the web UI's.
func TestSearchesRecordContextGaps(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	open := func() []memory.Question {
		t.Helper()
		qs, err := e.Service.ListQuestions(ctx, e.DatapackID, memory.QuestionFilter{State: "all"})
		if err != nil {
			t.Fatal(err)
		}
		return qs.Questions
	}
	search := func(tool, query, agent string, header ...string) {
		t.Helper()
		status, reply := rpc(t, e, testenv.ClientToken, map[string]any{"id": 1, "method": "tools/call",
			"params": map[string]any{"name": tool, "arguments": map[string]any{"query": query,
				"conversation_id": "c1", "agent_id": agent}}}, header...)
		if status != 200 || reply["result"] == nil {
			t.Fatalf("%s: %d %v", tool, status, reply)
		}
	}

	search("context_search", "Where is the deploy key?", "claude_code:a")
	search("knowledgebase_search", "Who reviews releases?", "kiro:b")
	search("context_search", "AgentK's own search", "claude_code:a", "X-Meko-Client", "agentk")
	search("context_search", "A person's search", memory.WebUIAgent)
	qs := open()
	if len(qs) != 2 || qs[0].Query != "Who reviews releases?" || qs[0].Tool != "knowledgebase_search" ||
		qs[0].Quality != memory.QualityEmpty || qs[1].Query != "Where is the deploy key?" ||
		qs[1].Agents[0] != "claude_code:a" || qs[1].Conversations[0] != "c1" {
		t.Fatalf("questions %+v", qs)
	}

	// A memory that answers it: memory_search finds it, which answers the question.
	ok(t, e, "memory_add", map[string]any{"text": "The deploy key is in the team vault.", "conversation_id": "c2"})
	r := ok(t, e, "memory_search", map[string]any{"query": "Where is the deploy key?", "conversation_id": "c1"})
	if len(arr(r.JSON["results"])) == 0 {
		t.Fatalf("memory_search found nothing: %s", r.Text)
	}
	score := num(obj(arr(r.JSON["results"])[0])["score"])
	qs = open()
	switch {
	case score >= memory.WeakMemoryScore && len(qs) != 1:
		t.Errorf("a relevant answer (score %.2f) leaves the question off: %+v", score, qs)
	case score < memory.WeakMemoryScore && (len(qs) != 2 || qs[0].Quality != memory.QualityWeak):
		t.Errorf("a weak answer (score %.2f) is an ask: %+v", score, qs)
	}
}

func TestAnswerQuality(t *testing.T) {
	low, high := 0.5, 0.8
	for _, c := range []struct {
		name string
		a    memory.Answer
		want string
	}{
		{"nothing", memory.Answer{}, memory.QualityEmpty},
		{"only low memories", memory.Answer{Memories: mems(&low, &low)}, memory.QualityWeak},
		{"a high memory", memory.Answer{Memories: mems(&low, &high)}, ""},
		{"a memory with no score", memory.Answer{Memories: mems(&low, nil)}, ""},
		{"a knowledge-base chunk", memory.Answer{Memories: mems(&low), Strong: 1}, ""},
		{"knowledgebase_search's weak chunk", memory.Answer{Strong: 1, OnlyEmpty: true}, ""},
		{"knowledgebase_search's nothing", memory.Answer{OnlyEmpty: true}, memory.QualityEmpty},
	} {
		if got := c.a.Quality(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if k := memory.QuestionKey("  Where IS\tthe key?\n"); k != "where is the key?" {
		t.Errorf("key %q", k)
	}
}

func mems(scores ...*float64) []memoryengine.Memory {
	out := make([]memoryengine.Memory, len(scores))
	for i, sc := range scores {
		out[i] = memoryengine.Memory{ID: "m", Score: sc}
	}
	return out
}

// While Meko Cloud is linked, the searches /mcp passes on to it are recorded for Context Gaps too,
// judged from Meko Cloud's reply, under its datapack's id.
func TestCloudSearchesRecordContextGaps(t *testing.T) {
	cloud := &fakeCloud{linked: true, tools: &cloudlink.Tools{Tools: []*sdk.Tool{cloudTool("context_search"),
		cloudTool("memory_search"), cloudTool("knowledgebase_search")}},
		reply: map[string]string{
			"context_search":       `{"results": {"memory": [], "knowledge_base": [], "conversation": {"hits": []}}}`,
			"memory_search":        `{"results": [{"id": "m1", "memory": "noise", "score": 0.31}]}`,
			"knowledgebase_search": `{"results": [{"chunk_text": "a hit", "distance": 0.2}], "count": 1}`,
		}}
	e := testenv.Start(t, testenv.Options{MCPCloud: cloud})
	const dp = "0a1b2c3d-0000-4000-8000-00000000c10d"
	search := func(tool, query string, args map[string]any, header ...string) {
		t.Helper()
		a := map[string]any{"query": query, "conversation_id": "c1", "agent_id": "claude_code:a", "datapack_id": dp}
		for k, v := range args {
			a[k] = v
		}
		status, reply := rpc(t, e, testenv.ClientToken, map[string]any{"id": 1, "method": "tools/call",
			"params": map[string]any{"name": tool, "arguments": a}}, header...)
		if status != 200 || reply["result"] == nil {
			t.Fatalf("%s: %d %v", tool, status, reply)
		}
	}
	search("context_search", "Who owns the pager?", nil)
	search("memory_search", "Where are the runbooks?", nil)
	search("knowledgebase_search", "What does the deploy do?", nil)
	search("context_search", "AgentK's own search", nil, "X-Meko-Client", "agentk")
	search("context_search", "Cloud's default datapack", map[string]any{"datapack_id": ""})
	qs, err := e.Service.ListQuestions(context.Background(), dp, memory.QuestionFilter{State: "all", Cloud: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(qs.Questions) != 2 || qs.Questions[0].Query != "Where are the runbooks?" ||
		qs.Questions[0].Quality != memory.QualityWeak || qs.Questions[1].Quality != memory.QualityEmpty ||
		qs.Questions[1].Agents[0] != "claude_code:a" || qs.Searches != 4 {
		t.Fatalf("Meko Cloud's questions %+v (searches %d)", qs.Questions, qs.Searches)
	}
	// This Meko's own datapack has none of them.
	if local, _ := e.Service.ListQuestions(context.Background(), e.DatapackID,
		memory.QuestionFilter{State: "all"}); len(local.Questions) != 0 {
		t.Errorf("local questions %+v", local.Questions)
	}
}

func TestAnswerOf(t *testing.T) {
	for _, c := range []struct {
		tool, text, want string
		ok               bool
	}{
		{"context_search", `{"results": {"memory": [], "knowledge_base": [], "conversation": null}}`,
			memory.QualityEmpty, true},
		{"context_search", `{"memory": [{"score": 0.2}], "conversation": [{"distance": 0.1}]}`, "", true},
		{"context_search", `{"results": {"memory": [{"score": 0.2}, {"score": 0.5}]}}`, memory.QualityWeak, true},
		{"memory_search", `[{"score": 0.9}]`, "", true},
		{"memory_search", `{"memories": []}`, memory.QualityEmpty, true},
		{"knowledgebase_search", `{"results": []}`, memory.QualityEmpty, true},
		{"context_search", `{"error": "search_failed"}`, "", false},
		{"context_search", `not json`, "", false},
		{"memory_add", `{"results": []}`, "", false},
	} {
		a, ok := memory.AnswerOf(c.tool, c.text)
		if ok != c.ok || (ok && a.Quality() != c.want) {
			t.Errorf("%s %s: %q %v, want %q %v", c.tool, c.text, a.Quality(), ok, c.want, c.ok)
		}
	}
}
