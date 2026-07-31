package pageserver

import (
	"context"
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
	if !strings.Contains(got[0], "127.0.0.1:9898/metrics") {
		t.Fatalf("metrics script does not target the default port: %s", got[0])
	}
	if !strings.Contains(got[1], "/v1/branch/b1/flush?lsn=0/16B3748") || !strings.Contains(got[1], `method="PUT"`) {
		t.Fatalf("flush script is not a PUT at the branch flush route: %s", got[1])
	}
}
