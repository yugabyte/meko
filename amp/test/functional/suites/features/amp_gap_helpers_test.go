package features

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
)

func TestExternalLBVerdict(t *testing.T) {
	ok := lbService{Type: "LoadBalancer", Ports: []string{"5432"}, Annotations: `{"a":"b"}`}
	if err := externalLBVerdict(ok, "192.0.2.1/32", "aws", "5432", "192.0.2.1/32"); err != nil {
		t.Errorf("a compliant external Service was refused: %v", err)
	}
	cases := map[string]struct {
		svc    lbService
		ranges string
	}{
		"no source ranges":    {ok, ""},
		"extra source range":  {ok, "192.0.2.1/32 0.0.0.0/0"},
		"internal annotation": {lbService{Type: "LoadBalancer", Ports: []string{"5432"}, Annotations: internalLBAnnotation["aws"].json()}, "192.0.2.1/32"},
		"second port exposed": {lbService{Type: "LoadBalancer", Ports: []string{"5432", "9898"}}, "192.0.2.1/32"},
		"not a load balancer": {lbService{Type: "ClusterIP", Ports: []string{"5432"}}, "192.0.2.1/32"},
	}
	for name, tc := range cases {
		if err := externalLBVerdict(tc.svc, tc.ranges, "aws", "5432", "192.0.2.1/32"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func fakeTools(names ...string) map[string]mcpTool {
	out := map[string]mcpTool{}
	for _, n := range names {
		desc := "does " + n
		if strings.HasPrefix(n, "create_") || strings.HasPrefix(n, "delete_") || strings.HasPrefix(n, "resize_") {
			desc += "; poll get_task"
		}
		if strings.HasPrefix(n, "start_migration") {
			desc += "; poll get_migration"
		}
		out[n] = mcpTool{Name: n, Description: desc, InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)}
	}
	return out
}

func fullCatalog() map[string]mcpTool {
	names := append([]string{}, mcpExpectedTools...)
	for i := len(names); i < mcpMinTools; i++ {
		names = append(names, "get_filler_"+string(rune('a'+i)))
	}
	return fakeTools(names...)
}

func TestMCPCatalogVerdict(t *testing.T) {
	if err := mcpCatalogVerdict(fullCatalog()); err != nil {
		t.Fatalf("a documented catalog was refused: %v", err)
	}
	withUsers := fullCatalog()
	withUsers["create_user"] = mcpTool{Name: "create_user", Description: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}
	if err := mcpCatalogVerdict(withUsers); err == nil {
		t.Error("create_user in the catalog was accepted")
	}
	quiet := fullCatalog()
	tl := quiet["create_cluster"]
	tl.Description = "creates a cluster"
	quiet["create_cluster"] = tl
	if err := mcpCatalogVerdict(quiet); err == nil {
		t.Error("an async tool that does not mention get_task was accepted")
	}
	small := fakeTools(mcpExpectedTools...)
	if err := mcpCatalogVerdict(small); err == nil {
		t.Error("a catalog under the documented size was accepted")
	}
}

func TestMCPArgsFollowsTheSchema(t *testing.T) {
	tool := mcpTool{Name: "create_branch", InputSchema: json.RawMessage(`{"type":"object","required":["pool_id","cluster_id","name"],"properties":{"pool_id":{},"cluster_id":{},"name":{},"parent_branch_id":{},"note":{}}}`)}
	known := map[string]any{"pool_id": "p", "cluster_id": "c", "name": "n", "parent_branch_id": "b", "note": "ignored"}
	args, err := mcpArgs(tool, known)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"pool_id", "cluster_id", "name", "parent_branch_id"} {
		if args[k] != known[k] {
			t.Errorf("%s = %v", k, args[k])
		}
	}
	if _, ok := args["note"]; ok {
		t.Error("an optional non-id property was sent")
	}
	if _, err := mcpArgs(tool, map[string]any{"pool_id": "p"}); err == nil {
		t.Error("a required argument with no known value did not fail")
	}
}

func TestMCPPayloadLiftsSSE(t *testing.T) {
	sse := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"
	if got := string(mcpPayload("text/event-stream", []byte(sse))); got != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Errorf("SSE payload = %q", got)
	}
	if got := string(mcpPayload("application/json", []byte(`{"a":1}`))); got != `{"a":1}` {
		t.Errorf("JSON payload = %q", got)
	}
}

func TestForkParentNotReadyOnlyMatchesTheRetryable503(t *testing.T) {
	retryable := &controller.APIError{StatusCode: 503, Code: "service_unavailable",
		Message: "the parent branch's endpoint is not serving its safekeeper API yet; retry the fork"}
	if !forkParentNotReady(retryable) {
		t.Error("the documented retryable fork 503 was not recognised")
	}
	for _, e := range []error{
		&controller.APIError{StatusCode: 503, Code: "service_unavailable", Message: "state store unavailable"},
		&controller.APIError{StatusCode: 400, Code: "invalid_request", Message: "cannot determine a fork point"},
		&controller.APIError{StatusCode: 404},
		errors.New("connection refused"),
	} {
		if forkParentNotReady(e) {
			t.Errorf("retried a non-retryable answer: %v", e)
		}
	}
}

// The exact shape step_plan_endpoint_upgrades.go renders for a roll that did
// not converge, so a change to that sentence fails here rather than in a run
// that costs 35 minutes to reach.
func TestParseRollTallyReadsTheProductsError(t *testing.T) {
	msg := "endpoint upgrade roll to 0.0.0-missing-abc did not converge: 0 upgraded, 0 skipped, " +
		"2 failed, 1 unattempted (see the task's subtasks for each endpoint); first failure: " +
		"endpoint 88e0c8f7-0f88-4648-b358-40a676c6bb0f (subtask 1f0d): redeploy endpoint: exit status 1"
	got, err := parseRollTally(msg)
	if err != nil {
		t.Fatalf("the product's own error was unreadable: %v", err)
	}
	for k, want := range map[string]int{"upgraded": 0, "skipped": 0, "failed": 2, "unattempted": 1} {
		if got[k] != want {
			t.Errorf("%s = %d, want %d", k, got[k], want)
		}
	}
	if _, err := parseRollTally("roll failed"); err == nil {
		t.Error("an error with no tally was accepted")
	}
}
