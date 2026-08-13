package pageserver

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

const exposition = `# HELP pageserver_yb_payload_logical_bytes_total Uncompressed payload bytes
# TYPE pageserver_yb_payload_logical_bytes_total counter
pageserver_yb_payload_logical_bytes_total{table="pages"} 8192000
pageserver_yb_payload_logical_bytes_total{table="meta"} 1024
pageserver_yb_payload_stored_bytes_total{table="pages"} 409600
pageserver_yb_payload_rows_total{outcome="lz4",table="pages"} 1000
pageserver_bare_counter 7
garbage line without a value
`

func TestParseSamplesAndLookup(t *testing.T) {
	s := ParseSamples(exposition)

	if v, ok := s.Value("pageserver_yb_payload_logical_bytes_total", map[string]string{"table": "pages"}); !ok || v != 8192000 {
		t.Fatalf("logical bytes for table=pages = (%v, %v), want (8192000, true)", v, ok)
	}
	if v, ok := s.Value("pageserver_yb_payload_rows_total", map[string]string{"table": "pages", "outcome": "lz4"}); !ok || v != 1000 {
		t.Fatalf("lz4 rows for table=pages = (%v, %v), want (1000, true)", v, ok)
	}
	if v, ok := s.Value("pageserver_bare_counter", nil); !ok || v != 7 {
		t.Fatalf("unlabelled counter = (%v, %v), want (7, true)", v, ok)
	}
	// A label mismatch must report absence, not fall through to another series.
	if _, ok := s.Value("pageserver_yb_payload_stored_bytes_total", map[string]string{"table": "slru_pages"}); ok {
		t.Fatal("stored bytes for table=slru_pages reported present, want absent (the label filter fell through to another series)")
	}
	if _, ok := s.Value("pageserver_missing_metric_total", nil); ok {
		t.Fatal("a metric absent from the scrape reported present")
	}
}

func TestMetricsAndFlushScripts(t *testing.T) {
	var got []string
	c := New(func(_ context.Context, script string) (string, error) {
		got = append(got, script)
		return exposition, nil
	}, 0)

	if _, err := c.Metrics(context.Background()); err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if err := c.FlushBranch(context.Background(), "b1", "0/16B3748"); err != nil {
		t.Fatalf("FlushBranch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ran %d commands, want 2", len(got))
	}
	// The endpoint image is bare Debian — the request must need nothing beyond
	// bash (/dev/tcp), and must check the status line rather than pass an error
	// body to the parser.
	for i, script := range got {
		if strings.Contains(script, "python3") || strings.Contains(script, "curl") || strings.Contains(script, "wget") {
			t.Fatalf("script %d needs a tool the endpoint image lacks: %s", i, script)
		}
		if !strings.Contains(script, "/dev/tcp/127.0.0.1/9898") {
			t.Fatalf("script %d does not target the default port over /dev/tcp: %s", i, script)
		}
		if !strings.Contains(script, "exit 22") {
			t.Fatalf("script %d does not fail on a non-2xx status: %s", i, script)
		}
	}
	if !strings.Contains(got[0], "GET /metrics HTTP/1.0") {
		t.Fatalf("metrics script is not a GET of /metrics: %s", got[0])
	}
	if !strings.Contains(got[1], "PUT /v1/branch/b1/flush?lsn=0/16B3748 HTTP/1.0") {
		t.Fatalf("flush script is not a PUT at the branch flush route: %s", got[1])
	}
	// A request with no body must not announce one.
	for i, script := range got {
		if strings.Contains(script, "Content-Length") {
			t.Fatalf("script %d sends a Content-Length for a request with no body: %s", i, script)
		}
	}
	if !strings.Contains(got[0], `printf "%s: %s\n" "$status" "$body" >&2`) {
		t.Fatalf("script does not report the status line AND the response body on a non-2xx: %s", got[0])
	}
	if !strings.Contains(got[0], "exit 7") {
		t.Fatalf("script does not exit ConnectRefusedExit when the port refuses: %s", got[0])
	}
}

func TestForceGCSendsJSONBodyAndParsesResult(t *testing.T) {
	var got string
	c := New(func(_ context.Context, script string) (string, error) {
		got = script
		return `{"candidates_dequeued":42,"rows_deleted":7,"elapsed":153}`, nil
	}, 0)

	res, err := c.ForceGC(context.Background(), "b1", 1024)
	if err != nil {
		t.Fatalf("ForceGC: %v", err)
	}
	if res.CandidatesDequeued != 42 || res.RowsDeleted != 7 || res.ElapsedMS != 153 {
		t.Fatalf("GC result = %+v, want 42 candidates, 7 rows, 153 ms", res)
	}
	if !strings.Contains(got, "PUT /v1/branch/b1/do_gc HTTP/1.0") {
		t.Fatalf("not a PUT at the do_gc route: %s", got)
	}
	// The route decodes a JSON body unconditionally, so it must arrive with a
	// matching Content-Length and its quotes escaped for the shell printf.
	body := `{"gc_horizon":1024}`
	if !strings.Contains(got, fmt.Sprintf("Content-Length: %d", len(body))) {
		t.Fatalf("Content-Length does not match the %d-byte body: %s", len(body), got)
	}
	if !strings.Contains(got, "Content-Type: application/json") {
		t.Fatalf("body sent without a JSON content type: %s", got)
	}
	if !strings.Contains(got, `{\"gc_horizon\":1024}`) {
		t.Fatalf("body is not escaped for the shell printf: %s", got)
	}
}

func TestBranchInfoReadsTheAppliedGCCutoff(t *testing.T) {
	var got string
	c := New(func(_ context.Context, script string) (string, error) {
		got = script
		return `{"branch_id":"b1","last_record_lsn":"0/3000000","applied_gc_cutoff_lsn":"0/2FFFFF0","min_readable_lsn":"0/2FFFFF0","initdb_lsn":"0/1000000"}`, nil
	}, 0)

	info, err := c.BranchInfo(context.Background(), "b1")
	if err != nil {
		t.Fatalf("BranchInfo: %v", err)
	}
	if info.AppliedGCCutoffLSN != "0/2FFFFF0" || info.LastRecordLSN != "0/3000000" {
		t.Fatalf("branch info = %+v, want the cutoff and last-record LSNs from the response", info)
	}
	if !strings.Contains(got, "GET /v1/branch/b1 HTTP/1.0") {
		t.Fatalf("not a GET of the branch detail route: %s", got)
	}
}

func TestBranchInfoQuotesAnUnparseableResponse(t *testing.T) {
	c := New(func(context.Context, string) (string, error) { return "<html>404</html>", nil }, 0)
	_, err := c.BranchInfo(context.Background(), "b1")
	if err == nil {
		t.Fatal("BranchInfo must error on a response that is not the branch detail")
	}
	if !strings.Contains(err.Error(), "<html>404</html>") {
		t.Errorf("error does not carry the response it could not parse: %v", err)
	}
}

func TestParseLSN(t *testing.T) {
	got, err := ParseLSN("1/16B3748")
	if err != nil {
		t.Fatalf("ParseLSN: %v", err)
	}
	if want := uint64(1)<<32 | 0x16B3748; got != want {
		t.Fatalf("ParseLSN(\"1/16B3748\") = %d, want %d", got, want)
	}
	// Comparison across the hi/lo boundary is the whole point: a plain string
	// compare would order "0/FFFFFFF" above "1/1".
	low, _ := ParseLSN("0/FFFFFFF")
	high, _ := ParseLSN("1/1")
	if !(high > low) {
		t.Errorf("1/1 (%d) must be above 0/FFFFFFF (%d)", high, low)
	}
	for _, bad := range []string{"", "16B3748", "z/1", "0/zz"} {
		if _, err := ParseLSN(bad); err == nil {
			t.Errorf("ParseLSN(%q) must error", bad)
		}
	}
}
