package metrics

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
)

func compiled(t *testing.T, entries ...Entry) []compiledEntry {
	t.Helper()
	out := compileEntries(entries)
	if len(out) != len(entries) {
		t.Fatalf("compiled %d of %d entries", len(out), len(entries))
	}
	return out
}

func TestPromCmdFallsBackFromCurlToWget(t *testing.T) {
	cmd := promCmd(9000, ybPromPath)
	if !strings.Contains(cmd, "curl -sf http://localhost:9000/prometheus-metrics") {
		t.Errorf("cmd missing curl form: %s", cmd)
	}
	if !strings.Contains(cmd, "|| wget -qO- http://localhost:9000/prometheus-metrics") {
		t.Errorf("cmd missing wget fallback: %s", cmd)
	}
	if ps := promCmd(9898, pageserverPromPath); !strings.Contains(ps, "http://localhost:9898/metrics") {
		t.Errorf("pageserver cmd = %s, want the /metrics path on 9898", ps)
	}
}

// An entry's optional fields take their documented defaults, and a latency entry
// gets a derived latency title.
func TestNormalizeDefaults(t *testing.T) {
	g := normalize(Entry{Match: `^x$`, Title: "X"})
	if g.Kind != KindGauge || g.DivideBy != 1 || g.Round != 2 || g.Unit != "" {
		t.Errorf("gauge defaults = %+v", g)
	}
	c := normalize(Entry{Match: `^y$`, Title: "Y", Kind: KindCounter})
	if c.Unit != "/s" {
		t.Errorf("counter unit = %q, want /s", c.Unit)
	}
	l := normalize(Entry{Match: `^z_(count|sum)$`, Title: "Z ops", Kind: KindLatency})
	if l.Unit != "/s" || l.LatencyTitle != "Z ops latency" || l.SumUnit != "us" {
		t.Errorf("latency defaults = %+v", l)
	}
}

// Labels are stripped and every line sharing a name is summed (a YB server
// reports one line per tablet); names no entry matches never enter the map.
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
	entries := compiled(t,
		Entry{Match: `^rocksdb_compact_read_bytes$`, Title: "Compactions read"},
		Entry{Match: `^total_sst_files_size$`, Title: "SST files size"},
		Entry{Match: `^mem_tracker$`, Title: "Memory tracker"},
	)
	got := parseExposition(text, entries)

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

func TestSplitHistAndSumToMS(t *testing.T) {
	base, suffix, ok := splitHist("log_sync_latency_sum")
	if !ok || base != "log_sync_latency" || suffix != "_sum" {
		t.Errorf("splitHist = %q/%q/%v", base, suffix, ok)
	}
	if _, _, ok := splitHist("mem_tracker"); ok {
		t.Errorf("a non-histogram name split as one")
	}
	for unit, want := range map[string]float64{"ns": 1e-6, "us": 1e-3, "ms": 1, "s": 1e3, "": 1e-3} {
		if got := sumToMS(unit); got != want {
			t.Errorf("sumToMS(%q) = %v, want %v", unit, got, want)
		}
	}
}

// A counter reports a per-second rate under its human title, so its first sample
// only seeds the delta; divide_by applies to the rate.
func TestDeriveCounterBecomesTitledRate(t *testing.T) {
	st := newPodState()
	e := normalize(Entry{Match: `^log_bytes_logged$`, Title: "WAL bytes", Kind: KindCounter, Unit: "MiB/s", DivideBy: 1 << 20})
	t0 := time.Unix(1730000000, 0)
	if _, ok := derive("log_bytes_logged", 0, e, t0, time.Time{}, st); ok {
		t.Errorf("first counter sample produced a reading; a rate needs a delta")
	}
	rd, ok := derive("log_bytes_logged", 10*(1<<20), e, t0.Add(10*time.Second), time.Time{}, st)
	if !ok {
		t.Fatalf("second counter sample produced nothing")
	}
	if rd.name != "WAL bytes" || rd.unit != "MiB/s" || math.Abs(rd.value-1) > 1e-9 {
		t.Errorf("reading = %+v, want WAL bytes 1 MiB/s", rd)
	}
	if len(rd.raw) != 1 || rd.raw[0] != "log_bytes_logged" {
		t.Errorf("raw = %v, want the source prometheus name", rd.raw)
	}
}

// A gauge passes through from the very first sample, scaled and titled.
func TestDeriveGaugePassesThroughScaled(t *testing.T) {
	st := newPodState()
	e := normalize(Entry{Match: `^mem_tracker$`, Title: "Memory tracker", Unit: "MiB", DivideBy: 1 << 20})
	t0 := time.Unix(1730000000, 0)
	rd, ok := derive("mem_tracker", 8*(1<<20), e, t0, time.Time{}, st)
	if !ok || rd.name != "Memory tracker" || rd.unit != "MiB" || rd.value != 8 {
		t.Fatalf("first gauge sample = %+v (ok %v), want Memory tracker 8 MiB", rd, ok)
	}
}

// A counter that goes backwards (process restart) yields no point rather than a
// negative rate.
func TestDeriveSkipsCounterReset(t *testing.T) {
	st := newPodState()
	e := normalize(Entry{Match: `^log_bytes_logged$`, Title: "WAL bytes", Kind: KindCounter})
	t0 := time.Unix(1730000000, 0)
	derive("log_bytes_logged", 5000, e, t0, time.Time{}, st)
	if _, ok := derive("log_bytes_logged", 10, e, t0.Add(10*time.Second), time.Time{}, st); ok {
		t.Errorf("counter reset produced a reading")
	}
	rd, ok := derive("log_bytes_logged", 110, e, t0.Add(20*time.Second), time.Time{}, st)
	if !ok || rd.value != 10 {
		t.Errorf("post-reset rate = %+v (ok %v), want 10 /s", rd, ok)
	}
}

// A pod that appeared after sampling began started counting from zero, so its
// first sample is measured from there rather than only seeding the delta. The
// reset rule is unchanged: a sum over label sets can drop with no restart.
func TestDeriveMeasuresANewPodsFirstCounterSampleFromZero(t *testing.T) {
	e := normalize(Entry{Match: `^tasks_reclaimed_total$`, Title: "Tasks reclaimed", Kind: KindCounter})
	t0 := time.Unix(1730000000, 0)
	st := newPodState()
	st.newSince = t0

	rd, ok := derive("tasks_reclaimed_total", 20, e, t0.Add(10*time.Second), time.Time{}, st)
	if !ok || rd.value != 2 {
		t.Fatalf("first sample on a new pod = %+v (ok %v), want 2 /s since the listing before it", rd, ok)
	}
	if _, ok := derive("tasks_reclaimed_total", 5, e, t0.Add(20*time.Second), time.Time{}, st); ok {
		t.Errorf("counter reset on a new pod produced a reading")
	}
}

// A labelled counter is exported from its first increment, so a series missing
// from the previous scrape of a running pod was zero then.
func TestCounterAbsentFromThePreviousScrapeIsMeasuredFromZero(t *testing.T) {
	entries := compiled(t, Entry{Match: `^errors_total$`, Title: "Errors", Kind: KindCounter})
	ex := &fakeRunner{out: []execpkg.Result{
		{Stdout: "other_total 1\n"},
		{Stdout: "other_total 1\nerrors_total 30\n"},
	}}
	st := newPodState()
	t0 := time.Unix(1730000000, 0)
	logf := func(string, ...any) {}

	if got := samplePrometheus(context.Background(), ex, 9898, pageserverPromPath, entries, st, t0, logf); len(got) != 0 {
		t.Fatalf("first scrape = %+v, want nothing", got)
	}
	got := samplePrometheus(context.Background(), ex, 9898, pageserverPromPath, entries, st, t0.Add(15*time.Second), logf)
	if len(got) != 1 || math.Abs(got[0].value-2) > 1e-9 {
		t.Fatalf("scrape where errors_total first appears = %+v, want 2 /s since the previous scrape", got)
	}
}

func TestDerivePairMeasuresANewPodsFirstSampleFromZero(t *testing.T) {
	entries := compiled(t, Entry{Match: `^q_(count|sum)$`, Title: "Q", Kind: KindLatency, LatencyTitle: "Q latency", SumUnit: "s"})
	// 20 calls in the 10s since the pod was last listed without it, 0.1s in all → 5 ms each.
	ex := &fakeRunner{out: []execpkg.Result{{Stdout: "q_count 20\nq_sum 0.1\n"}}}
	st := newPodState()
	t0 := time.Unix(1730000000, 0)
	st.newSince = t0

	got := samplePrometheus(context.Background(), ex, 9898, pageserverPromPath, entries, st, t0.Add(10*time.Second), func(string, ...any) {})
	if len(got) != 2 || math.Abs(got[0].value-2) > 1e-9 || math.Abs(got[1].value-5) > 1e-9 {
		t.Fatalf("first scrape of a new pod = %+v, want 2 /s at 5 ms", got)
	}
}

// The headline derivation: a _count/_sum pair becomes throughput AND a mean
// latency in ms — NOT two raw rates. The µs → ms conversion is what makes the
// latency number readable.
func TestDerivePairThroughputAndMeanLatency(t *testing.T) {
	entries := compiled(t, Entry{
		Match: `^handler_latency_yb_tserver_TabletServerService_Write_(count|sum)$`,
		Title: "Write RPCs", Kind: KindLatency, LatencyTitle: "Write latency", SumUnit: "us",
	})
	// 1000 writes over 10s (100/s), 2_500_000 µs of total time → 2500 µs = 2.5 ms each.
	body1 := "handler_latency_yb_tserver_TabletServerService_Write_count 0\n" +
		"handler_latency_yb_tserver_TabletServerService_Write_sum 0\n"
	body2 := "handler_latency_yb_tserver_TabletServerService_Write_count 1000\n" +
		"handler_latency_yb_tserver_TabletServerService_Write_sum 2500000\n"
	ex := &fakeRunner{out: []execpkg.Result{{Stdout: body1}, {Stdout: body2}}}
	st := newPodState()
	t0 := time.Unix(1730000000, 0)
	quiet := func(string, ...any) {}

	if first := samplePrometheus(context.Background(), ex, 9000, ybPromPath, entries, st, t0, quiet); len(first) != 0 {
		t.Fatalf("first scrape = %+v, want nothing (both deltas unseeded)", first)
	}
	got := samplePrometheus(context.Background(), ex, 9000, ybPromPath, entries, st, t0.Add(10*time.Second), quiet)
	if len(got) != 2 {
		t.Fatalf("readings = %+v, want throughput + mean latency", got)
	}
	tp, lat := got[0], got[1]
	if tp.name != "Write RPCs" || tp.unit != "/s" || math.Abs(tp.value-100) > 1e-9 {
		t.Errorf("throughput = %+v, want Write RPCs 100 /s", tp)
	}
	if lat.name != "Write latency" || lat.unit != "ms" || math.Abs(lat.value-2.5) > 1e-9 {
		t.Errorf("latency = %+v, want Write latency 2.5 ms", lat)
	}
	if lat.round != latencyRound {
		t.Errorf("latency round = %d, want %d", lat.round, latencyRound)
	}
	for _, rd := range got {
		if len(rd.raw) != 2 ||
			rd.raw[0] != "handler_latency_yb_tserver_TabletServerService_Write_count" ||
			rd.raw[1] != "handler_latency_yb_tserver_TabletServerService_Write_sum" {
			t.Errorf("raw = %v, want both histogram components", rd.raw)
		}
	}
}

// A seconds-unit histogram (the pageserver's) converts to ms too.
func TestDerivePairSecondsSumConvertsToMS(t *testing.T) {
	entries := compiled(t, Entry{
		Match: `^pageserver_smgr_query_seconds_(count|sum)$`,
		Title: "Page requests", Kind: KindLatency, LatencyTitle: "Page request latency", SumUnit: "s",
	})
	// 200 requests, 0.4s total → 2 ms each.
	body1 := "pageserver_smgr_query_seconds_count 0\npageserver_smgr_query_seconds_sum 0\n"
	body2 := "pageserver_smgr_query_seconds_count 200\npageserver_smgr_query_seconds_sum 0.4\n"
	ex := &fakeRunner{out: []execpkg.Result{{Stdout: body1}, {Stdout: body2}}}
	st := newPodState()
	t0 := time.Unix(1730000000, 0)
	quiet := func(string, ...any) {}

	samplePrometheus(context.Background(), ex, 9898, pageserverPromPath, entries, st, t0, quiet)
	got := samplePrometheus(context.Background(), ex, 9898, pageserverPromPath, entries, st, t0.Add(10*time.Second), quiet)
	if len(got) != 2 || got[1].name != "Page request latency" || math.Abs(got[1].value-2) > 1e-9 {
		t.Fatalf("readings = %+v, want a 2 ms Page request latency", got)
	}
}

// An idle interval (no new calls) reports the throughput zero but NO latency —
// a mean over zero calls is a division by zero, not a number.
func TestDerivePairIdleIntervalEmitsNoLatency(t *testing.T) {
	entries := compiled(t, Entry{Match: `^q_(count|sum)$`, Title: "Q", Kind: KindLatency})
	body := "q_count 5\nq_sum 100\n"
	ex := &fakeRunner{out: []execpkg.Result{{Stdout: body}, {Stdout: body}}}
	st := newPodState()
	t0 := time.Unix(1730000000, 0)
	quiet := func(string, ...any) {}

	samplePrometheus(context.Background(), ex, 9000, ybPromPath, entries, st, t0, quiet)
	got := samplePrometheus(context.Background(), ex, 9000, ybPromPath, entries, st, t0.Add(10*time.Second), quiet)
	if len(got) != 1 || got[0].name != "Q" || got[0].value != 0 {
		t.Errorf("readings = %+v, want only a zero throughput", got)
	}
}

// A latency entry that only ever sees one half of the pair derives nothing.
func TestDerivePairNeedsBothComponents(t *testing.T) {
	entries := compiled(t, Entry{Match: `^q_(count|sum)$`, Title: "Q", Kind: KindLatency})
	body := "q_count 5\n"
	ex := &fakeRunner{out: []execpkg.Result{{Stdout: body}, {Stdout: "q_count 15\n"}}}
	st := newPodState()
	t0 := time.Unix(1730000000, 0)
	quiet := func(string, ...any) {}
	samplePrometheus(context.Background(), ex, 9000, ybPromPath, entries, st, t0, quiet)
	if got := samplePrometheus(context.Background(), ex, 9000, ybPromPath, entries, st, t0.Add(10*time.Second), quiet); len(got) != 0 {
		t.Errorf("readings = %+v, want none without a _sum", got)
	}
}

// An empty body (neither curl nor wget present) is logged and skipped.
func TestSamplePrometheusSkipsEmptyBody(t *testing.T) {
	ex := &fakeRunner{out: []execpkg.Result{{Stdout: "  \n"}}}
	var logged int
	got := samplePrometheus(context.Background(), ex, 7000, ybPromPath,
		compiled(t, Entry{Match: `.`, Title: "Anything"}), newPodState(),
		time.Now(), func(string, ...any) { logged++ })
	if got != nil || logged != 1 {
		t.Errorf("readings = %+v, logged = %d", got, logged)
	}
}

// The exposition's own comment lines are the product describing itself: # HELP
// becomes the series description and # TYPE counter upgrades an unclassified
// entry to rate conversion.
func TestExpositionMetaHelpAndType(t *testing.T) {
	entries := compileEntries([]Entry{
		{Match: `^rpc_inbound_calls_created$`, Title: "Inbound RPCs", Unit: "/s", Kind: KindCounter},
		{Match: `^some_gauge$`, Title: "Some gauge"},
	})
	text := "# HELP rpc_inbound_calls_created Number of created RPC inbound calls.\n" +
		"# TYPE rpc_inbound_calls_created counter\n" +
		"rpc_inbound_calls_created{exported_instance=\"n1\"} 10\n" +
		"# HELP some_gauge A gauge the config did not classify.\n" +
		"# TYPE some_gauge counter\n" +
		"some_gauge 5\n" +
		"# HELP unmatched_metric Should not be kept.\n" +
		"unmatched_metric 1\n"
	meta := parseExpositionMeta(text, entries)
	if got := meta["rpc_inbound_calls_created"].desc; got != "Number of created RPC inbound calls." {
		t.Errorf("desc = %q", got)
	}
	if !meta["some_gauge"].counter {
		t.Error("TYPE counter must be read for a matched entry")
	}
	if _, ok := meta["unmatched_metric"]; ok {
		t.Error("unmatched metrics must not be kept")
	}
}

// promCmd must survive an image with neither curl nor wget: the /dev/tcp arm is
// present and the comment lines are no longer stripped in-pod.
func TestPromCmdFallsBackToDevTCP(t *testing.T) {
	cmd := promCmd(9898, "/metrics")
	for _, want := range []string{"curl -sf", "wget -qO-", "/dev/tcp/localhost/9898", "GET /metrics HTTP/1.0"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("promCmd missing %q in %q", want, cmd)
		}
	}
	if strings.Contains(cmd, "grep -v") {
		t.Error("comment lines must reach the parser (# HELP / # TYPE carry metadata)")
	}
}

func TestAbsentBeforeNeedsAnAnsweredScrapeWithoutTheSeries(t *testing.T) {
	t0 := time.Unix(1730000000, 0)
	fault := t0.Add(time.Minute)
	pod := func(answered, seen time.Time) *podState {
		st := newPodState()
		st.firstAnswer = answered
		if !seen.IsZero() {
			st.firstSeen["errors_total"] = seen
		}
		return st
	}
	cases := []struct {
		name   string
		states map[string]*podState
		want   bool
	}{
		{"appeared mid-fault", map[string]*podState{"ps/a": pod(t0, fault.Add(time.Second))}, true},
		{"exported on the seeding scrape", map[string]*podState{"ps/a": pod(t0, t0)}, false},
		{"exposition first answered mid-fault", map[string]*podState{"ps/a": pod(fault.Add(time.Second), fault.Add(2*time.Second))}, false},
		{"never exported", map[string]*podState{"ps/a": pod(t0, time.Time{})}, false},
		{"one pod of two exported it from the start", map[string]*podState{
			"ps/a": pod(t0, fault.Add(time.Second)), "ps/b": pod(t0, t0),
		}, false},
		{"another role's pod", map[string]*podState{"other/a": pod(t0, fault.Add(time.Second))}, false},
	}
	for _, tc := range cases {
		s := &Sampler{states: tc.states}
		if got := s.AbsentBefore("ps", "errors_total", fault); got != tc.want {
			t.Errorf("%s: AbsentBefore = %v, want %v", tc.name, got, tc.want)
		}
	}
}
