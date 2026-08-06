package metrics

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
)

func mustInclude(t *testing.T, pats ...string) []*regexp.Regexp {
	t.Helper()
	var out []*regexp.Regexp
	for _, p := range pats {
		out = append(out, regexp.MustCompile(p))
	}
	return out
}

func TestPromCmdFallsBackFromCurlToWget(t *testing.T) {
	cmd := promCmd(9000)
	if !strings.Contains(cmd, "curl -sf http://localhost:9000/prometheus-metrics") {
		t.Errorf("cmd missing curl form: %s", cmd)
	}
	if !strings.Contains(cmd, "|| wget -qO- http://localhost:9000/prometheus-metrics") {
		t.Errorf("cmd missing wget fallback: %s", cmd)
	}
}

// Labels are stripped and every line sharing a name is summed (a YB server
// reports one line per tablet); names outside include never enter the map.
func TestParseExpositionAggregatesLabelsAndFilters(t *testing.T) {
	text := `# HELP rocksdb_compact_read_bytes some help
# TYPE rocksdb_compact_read_bytes counter
rocksdb_compact_read_bytes{table_id="a",tablet_id="t1"} 100
rocksdb_compact_read_bytes{table_id="a",tablet_id="t2"} 250 1730000000000
total_sst_files_size{table_id="a"} 4096
not_included_metric{x="y"} 999
mem_tracker 8192
mem_tracker_Call 64
rocksdb_compact_read_bytes{bad="}brace"} 5
garbage line with no value here
broken_metric{unterminated 12
`
	include := mustInclude(t, `^rocksdb_compact_read_bytes$`, `^total_sst_files_size$`, `^mem_tracker$`)
	got := parseExposition(text, include)

	if got["rocksdb_compact_read_bytes"] != 355 {
		t.Errorf("summed compact_read_bytes = %v, want 355", got["rocksdb_compact_read_bytes"])
	}
	if got["total_sst_files_size"] != 4096 {
		t.Errorf("total_sst_files_size = %v", got["total_sst_files_size"])
	}
	if got["mem_tracker"] != 8192 {
		t.Errorf("mem_tracker = %v, want 8192 (the root tracker only)", got["mem_tracker"])
	}
	for _, unwanted := range []string{"not_included_metric", "mem_tracker_Call", "broken_metric"} {
		if _, ok := got[unwanted]; ok {
			t.Errorf("%s should not have been kept: %v", unwanted, got)
		}
	}
}

// A _count/_sum suffix makes a series a counter regardless of the configured
// patterns; anything else is a counter only when apps.metrics.counters says so.
func TestIsCounter(t *testing.T) {
	counters := mustInclude(t, `^rocksdb_(compact|flush)_`)
	for _, name := range []string{"log_append_latency_count", "log_append_latency_sum", "rocksdb_compact_read_bytes"} {
		if !isCounter(name, counters) {
			t.Errorf("%s not treated as a counter", name)
		}
	}
	// A YB gauge that also rises under load must NOT become a rate.
	for _, name := range []string{"mem_tracker", "total_sst_files_size"} {
		if isCounter(name, counters) {
			t.Errorf("%s treated as a counter", name)
		}
	}
}

// A counter reports a per-second rate, so its first sample only seeds the delta.
func TestDeriveCounterBecomesRate(t *testing.T) {
	st := newPodState()
	t0 := time.Unix(1730000000, 0)
	if _, ok := derive("log_append_latency_count", 1000, true, t0, st); ok {
		t.Errorf("first counter sample produced a reading; a rate needs a delta")
	}
	rd, ok := derive("log_append_latency_count", 1300, true, t0.Add(10*time.Second), st)
	if !ok {
		t.Fatalf("second counter sample produced nothing")
	}
	if rd.value != 30 || rd.unit != "/s" {
		t.Errorf("rate = %v %s, want 30 /s", rd.value, rd.unit)
	}
}

// A gauge passes through unchanged from the very first sample, rising or falling.
func TestDeriveGaugePassesThrough(t *testing.T) {
	st := newPodState()
	t0 := time.Unix(1730000000, 0)
	rd, ok := derive("mem_tracker", 8192, false, t0, st)
	if !ok || rd.value != 8192 || rd.unit != "" {
		t.Fatalf("first gauge sample = %+v (ok %v), want the raw 8192", rd, ok)
	}
	rd, _ = derive("mem_tracker", 16384, false, t0.Add(10*time.Second), st)
	if rd.value != 16384 || rd.unit != "" {
		t.Errorf("a rising gauge became %+v, want the raw 16384", rd)
	}
}

// A counter that goes backwards (process restart) yields no point rather than a
// negative rate.
func TestDeriveSkipsCounterReset(t *testing.T) {
	st := newPodState()
	t0 := time.Unix(1730000000, 0)
	derive("log_bytes_logged", 5000, true, t0, st)
	if _, ok := derive("log_bytes_logged", 10, true, t0.Add(10*time.Second), st); ok {
		t.Errorf("counter reset produced a reading")
	}
	// The new baseline is the post-reset value, so the next tick resumes.
	rd, ok := derive("log_bytes_logged", 110, true, t0.Add(20*time.Second), st)
	if !ok || rd.value != 10 {
		t.Errorf("post-reset rate = %+v (ok %v), want 10 /s", rd, ok)
	}
}

func TestSamplePrometheusReturnsSortedReadings(t *testing.T) {
	body := "total_sst_files_size{t=\"1\"} 100\nmem_tracker 2048\nlog_bytes_logged 500\n"
	ex := &fakeRunner{out: []execpkg.Result{{Stdout: body}, {Stdout: body}}}
	include := mustInclude(t, `^total_sst_files_size$`, `^mem_tracker$`, `^log_bytes_logged$`)
	counters := mustInclude(t, `^log_bytes_logged$`)
	st := newPodState()
	t0 := time.Unix(1730000000, 0)
	quiet := func(string, ...any) {}

	// First scrape: the two gauges report, the counter only seeds its delta.
	first := samplePrometheus(context.Background(), ex, 9000, include, counters, st, t0, quiet)
	if len(first) != 2 {
		t.Fatalf("first scrape = %+v, want the 2 gauges only", first)
	}
	got := samplePrometheus(context.Background(), ex, 9000, include, counters, st, t0.Add(10*time.Second), quiet)
	if len(got) != 3 {
		t.Fatalf("readings = %+v, want 3", got)
	}
	names := []string{got[0].name, got[1].name, got[2].name}
	want := []string{"log_bytes_logged", "mem_tracker", "total_sst_files_size"}
	if names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Errorf("readings not name-sorted: %q, want %q", names, want)
	}
}

// An empty body (neither curl nor wget present) is logged and skipped.
func TestSamplePrometheusSkipsEmptyBody(t *testing.T) {
	ex := &fakeRunner{out: []execpkg.Result{{Stdout: "  \n"}}}
	var logged int
	got := samplePrometheus(context.Background(), ex, 7000, mustInclude(t, `.`), nil, newPodState(),
		time.Now(), func(string, ...any) { logged++ })
	if got != nil || logged != 1 {
		t.Errorf("readings = %+v, logged = %d", got, logged)
	}
}
