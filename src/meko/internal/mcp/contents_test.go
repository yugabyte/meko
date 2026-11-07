package mcp_test

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/store"
	"github.com/yugabyte/meko/src/meko/internal/testenv"
)

// While "Keep call contents" is on, each call keeps what it sent and got back beside its call log
// row, an error's reply too, and the row says which client sent it.
func TestCallContentsKept(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	call := func(tool string, args map[string]any, header ...string) {
		t.Helper()
		if status, reply := rpc(t, e, testenv.ClientToken, map[string]any{"id": 1, "method": "tools/call",
			"params": map[string]any{"name": tool, "arguments": args}}, header...); status != 200 ||
			reply["result"] == nil {
			t.Fatalf("%s: %d %v", tool, status, reply)
		}
	}
	newest := func() (store.Call, *store.CallContents) {
		t.Helper()
		cs, _, err := e.Service.ListCalls(ctx, store.CallFilter{}, 1)
		if err != nil || len(cs) != 1 {
			t.Fatalf("calls %v %v", cs, err)
		}
		c, cc, err := e.Service.Call(ctx, cs[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		return c, cc
	}

	call("memory_get_all", map[string]any{"agent_id": "claude_code:a", "conversation_id": "c1"})
	if _, cc := newest(); cc != nil {
		t.Fatalf("kept while off: %+v", cc)
	}
	if err := e.Service.SetKeepCallContents(ctx, true); err != nil {
		t.Fatal(err)
	}
	call("memory_get_all", map[string]any{"agent_id": "claude_code:a", "conversation_id": "c1"}, "X-Meko-Client", "kode")
	c, cc := newest()
	if c.Client != "kode" || c.AgentID != "claude_code:a" || cc == nil ||
		!strings.Contains(cc.Args, `"agent_id":"claude_code:a"`) || !strings.Contains(cc.Reply, `"memories":[]`) {
		t.Fatalf("kept %+v %+v", c, cc)
	}
	call("memory_get_by_id", map[string]any{"memory_id": "nope"})
	c, cc = newest()
	if c.OK || c.Client != "" || cc == nil || cc.Reply == "" {
		t.Fatalf("a failed call %+v %+v", c, cc)
	}
}

// A call /mcp sends on to the linked Meko Cloud keeps its contents too.
func TestCloudCallContentsKept(t *testing.T) {
	cloud := &fakeCloud{linked: true, tools: &cloudlink.Tools{Tools: []*sdk.Tool{cloudTool("memory_search")}},
		reply: map[string]string{"memory_search": `{"results": [{"id": "m1", "memory": "from Cloud", "score": 0.9}]}`}}
	e := testenv.Start(t, testenv.Options{MCPCloud: cloud})
	ctx := context.Background()
	if err := e.Service.SetKeepCallContents(ctx, true); err != nil {
		t.Fatal(err)
	}
	if status, reply := rpc(t, e, testenv.ClientToken, map[string]any{"id": 1, "method": "tools/call",
		"params": map[string]any{"name": "memory_search", "arguments": map[string]any{"query": "who",
			"datapack_id": "0a1b2c3d-0000-4000-8000-00000000c10d"}}}); status != 200 || reply["result"] == nil {
		t.Fatalf("%d %v", status, reply)
	}
	cs, _, err := e.Service.ListCalls(ctx, store.CallFilter{}, 1)
	if err != nil || len(cs) != 1 || cs[0].Location != store.LocationCloud || !cs[0].HasContents {
		t.Fatalf("calls %+v %v", cs, err)
	}
	if _, cc, _ := e.Service.Call(ctx, cs[0].ID); cc == nil || !strings.Contains(cc.Reply, "from Cloud") ||
		!strings.Contains(cc.Args, `"query":"who"`) {
		t.Errorf("contents %+v", cc)
	}
}
