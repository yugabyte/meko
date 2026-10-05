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
	if requestFindings(after, after) != nil {
		t.Errorf("no new 5xx is no finding")
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
	if logFindings("app=x", "level=INFO msg=fine\n") != nil {
		t.Errorf("a clean log is no finding")
	}
}
