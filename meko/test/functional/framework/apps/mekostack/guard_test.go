package mekostack

import (
	"strings"
	"testing"
)

func TestRestartFindings(t *testing.T) {
	before, _ := parseRestarts("meko-api-server-a\tmeko-api-server=0=,\nyb-tserver-0\tyb-tserver=1=Error,yb-cleanup=0=,\n")
	after, reasons := parseRestarts("meko-api-server-a\tmeko-api-server=2=OOMKilled,\nyb-tserver-0\tyb-tserver=1=Error,yb-cleanup=0=,\nrag-agent-x\trag-agent=1=Error,\n")
	got := restartFindings(before, after, reasons)
	if len(got) != 2 {
		t.Fatalf("findings = %+v, want the api-server and the new rag-agent pod", got)
	}
	if got[0].Workload != "meko-api-server-a/meko-api-server" || !strings.Contains(got[0].Detail, "2 time(s), last exit OOMKilled") || !got[0].Fatal {
		t.Errorf("api-server finding = %+v", got[0])
	}
	if got[1].Workload != "rag-agent-x/rag-agent" {
		t.Errorf("a pod born during the test with a restart is a finding: %+v", got[1])
	}
}

const metricsBody = `# HELP meko_api_http_requests_total HTTP requests handled.
meko_api_http_requests_total{method="GET",route="/api/v1/health",status="200"} 120
meko_api_http_requests_total{method="DELETE",route="/api/v1/datapacks/:datapack_id",status="500"} 3
meko_api_http_requests_total{method="POST",route="/api/v1/datapacks",status="503"} 1
`

func TestRequestFindings(t *testing.T) {
	now := parseRequests(metricsBody)
	if len(now) != 2 {
		t.Fatalf("parsed %v, want only the two 5xx series", now)
	}
	before := map[string]float64{"pod-a DELETE /api/v1/datapacks/:datapack_id 500": 1}
	after := map[string]float64{}
	for k, v := range now {
		after["pod-a "+k] = v
	}
	after["pod-b DELETE /api/v1/datapacks/:datapack_id 500"] = 2
	got := requestFindings(before, after)
	if len(got) != 2 {
		t.Fatalf("findings = %+v", got)
	}
	if got[0].Detail != "DELETE /api/v1/datapacks/:datapack_id 500 answered 4 time(s)" {
		t.Errorf("deltas across pods should add up: %+v", got[0])
	}
	if f := got[0]; f.Method != "DELETE" || f.Route != "/api/v1/datapacks/:datapack_id" || f.Count != 4 {
		t.Errorf("series = %s %s x%d, want DELETE /api/v1/datapacks/:datapack_id x4", f.Method, f.Route, f.Count)
	}
	if requestFindings(after, after) != nil {
		t.Errorf("no new 5xx is no finding")
	}
}

func TestFindingMatches(t *testing.T) {
	f := Finding{Kind: FindingHTTP5xx, Method: "POST", Route: "/api/v1/datapacks/:datapack_id/knowledge-bases/upload"}
	for path, want := range map[string]bool{
		"/api/v1/datapacks/4483b702/knowledge-bases/upload":     true,
		"/api/v1/datapacks/4483b702/knowledge-bases/upload?x=1": true,
		"/api/v1/datapacks/4483b702":                            false,
		"/api/v1/datapacks/4483b702/knowledge-bases/upload/2":   false,
		"/api/v1/agents/4483b702/knowledge-bases/upload":        false,
	} {
		if got := f.Matches("POST", path); got != want {
			t.Errorf("Matches(POST, %s) = %v, want %v", path, got, want)
		}
	}
	if f.Matches("GET", "/api/v1/datapacks/4483b702/knowledge-bases/upload") {
		t.Errorf("another method matched")
	}
	if !(Finding{Method: "GET", Route: "/api/v1/files/*path"}).Matches("GET", "/api/v1/files/a/b/c") {
		t.Errorf("a * segment should match the rest")
	}
}

func TestRequestFindingsAfterCounterReset(t *testing.T) {
	got := requestFindings(map[string]float64{"p GET /x 500": 9}, map[string]float64{"p GET /x 500": 2})
	if len(got) != 1 || !strings.Contains(got[0].Detail, "answered 2 time(s)") {
		t.Errorf("a restarted pod's counter starts over, its whole value is new: %+v", got)
	}
}

func TestLogFindings(t *testing.T) {
	logs := strings.Join([]string{
		`[pod/meko-api-server-a/meko-api-server] time=x level=INFO msg="ok"`,
		`[pod/meko-api-server-a/meko-api-server] time=x level=ERROR msg="langfuse mapping provisioning failed"`,
		`[pod/meko-api-server-a/meko-api-server] panic: runtime error: invalid memory address or nil pointer dereference`,
		`[pod/rag-agent-x/rag-agent] Traceback (most recent call last):`,
		`[pod/rag-agent-x/rag-agent] 2026-10-03 ERROR: embed failed`,
	}, "\n")
	got := logFindings("app=x", logs)
	if len(got) != 2 {
		t.Fatalf("findings = %+v", got)
	}
	if got[0].Kind != FindingCrashLog || !got[0].Fatal || !strings.Contains(got[0].Detail, "panic: runtime error") || !strings.Contains(got[0].Detail, "Traceback") {
		t.Errorf("crash finding = %+v", got[0])
	}
	if got[1].Kind != FindingErrorLogs || got[1].Fatal || !strings.HasPrefix(got[1].Detail, "2 ERROR line(s)") {
		t.Errorf("error lines are counted, not fatal: %+v", got[1])
	}
	py := strings.Join([]string{
		`[pod/mcp-a/meko-mcp] Traceback (most recent call last):`,
		`[pod/mcp-b/meko-mcp] INFO: interleaved from another pod`,
		`[pod/mcp-a/meko-mcp]   File "/app/server.py", line 10, in handle`,
		`[pod/mcp-a/meko-mcp]     return tool(**args)`,
		`[pod/mcp-a/meko-mcp]   File "/app/tools.py", line 42, in memory_get_all`,
		`[pod/mcp-a/meko-mcp]     raise KeyError(key)`,
		`[pod/mcp-a/meko-mcp] KeyError: 'limit'`,
	}, "\n")
	if got := logFindings("app=x", py); len(got) != 1 || !strings.HasSuffix(got[0].Detail, `-> File "/app/tools.py", line 42, in memory_get_all -> KeyError: 'limit'`) {
		t.Errorf("a Python traceback is reported with the frame that raised and the exception: %+v", got)
	}
	box := "[pod/mcp-a/meko-mcp]                     "
	richLog := strings.Join([]string{
		box + "Error calling tool 'memory_search'",
		box + "╭─────── Traceback (most recent call last) ───────╮",
		box + "│ /app/.venv/lib/python3.10/site-packages/fastmcp/s │",
		box + "│ erver.py:1240 in call_tool                        │",
		box + "│ /app/src/tools.py:134 in _tool_trace              │",
		box + "│ ❱  134 │   trace_id=int(parent_trace_id, 16),     │",
		box + "╰─────────────────────────────────────────────────╯",
		box + "ValueError: invalid literal for int() with base 16: ''",
	}, "\n")
	if got := logFindings("app=x", richLog); len(got) != 1 || !strings.HasSuffix(got[0].Detail, "-> /app/src/tools.py:134 in _tool_trace -> ValueError: invalid literal for int() with base 16: ''") {
		t.Errorf("a rich traceback is reported with the frame that raised and the exception: %+v", got)
	}
	mcp := "[pod/mcp-a/meko-mcp] "
	chained := strings.Join([]string{
		mcp + "Traceback (most recent call last):",
		mcp + `  File "/app/.venv/lib/python3.10/site-packages/httpcore/_exceptions.py", line 14, in map_exceptions`,
		mcp + "    raise to_exc(exc) from exc",
		mcp + "httpcore.ReadTimeout: timed out",
		mcp,
		mcp + "The above exception was the direct cause of the following exception:",
		mcp,
		mcp + "Traceback (most recent call last):",
		mcp + `  File "/app/src/langfuse_store.py", line 231, in _get_full_trace_locked`,
		mcp + "    return api.trace.get(trace_id)",
		mcp + `  File "/app/.venv/lib/python3.10/site-packages/httpx/_transports/default.py", line 118, in map_httpcore_exceptions`,
		mcp + "    raise mapped_exc(message) from exc",
		mcp + "httpx.ReadTimeout: timed out",
		mcp + "2026-10-06 02:42:47 [INFO] meko-mcp.tools: next",
	}, "\n")
	if got := logFindings("app=x", chained); len(got) != 1 || strings.Contains(got[0].Detail, " | ") || !strings.HasSuffix(got[0].Detail, `-> File "/app/src/langfuse_store.py", line 231, in _get_full_trace_locked -> httpx.ReadTimeout: timed out`) {
		t.Errorf("a chained traceback is reported with the service's own frame and the outermost exception: %+v", got)
	}
	refusedArgs := strings.Join([]string{
		"[pod/mcp-a/meko-mcp] [10/05/26 23:50:57] Error validating tool 'memory_get_all'",
		box + "╭─────── Traceback (most recent call last) ───────╮",
		box + "╰─────────────────────────────────────────────────╯",
		box + "ValidationError: 1 validation error for call[memory_get_all]",
	}, "\n")
	if got := logFindings("app=x", refusedArgs); got != nil {
		t.Errorf("a traceback for refused tool arguments is the server working, not a crash: %+v", got)
	}
	if logFindings("app=x", "level=INFO msg=fine\n") != nil {
		t.Errorf("a clean log is no finding")
	}
}
