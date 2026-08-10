package perf

import (
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/benchbase"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/sampleapp"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// Every name the mix offers must build a driver, and anything else must be
// rejected before tenants are provisioned rather than after.
func TestKnownMixWorkloads(t *testing.T) {
	for _, name := range allMixWorkloads() {
		if !knownMixWorkload(name) {
			t.Errorf("%q is offered in the mix but cannot be built", name)
		}
	}
	for _, name := range []string{"", "tpcc ", "TPCC", "nosuchbench"} {
		if knownMixWorkload(name) {
			t.Errorf("%q was accepted as a driver", name)
		}
	}
}

func TestBenchbaseMixMapping(t *testing.T) {
	want := map[string]benchbase.Benchmark{
		mixTPCC: benchbase.TPCC, mixTPCH: benchbase.TPCH,
		mixYCSB: benchbase.YCSB, mixCHBenchmark: benchbase.CHBenchmark,
	}
	for name, bench := range want {
		got, ok := benchbaseMix(name)
		if !ok || got != bench {
			t.Errorf("benchbaseMix(%q) = %q,%v; want %q,true", name, got, ok, bench)
		}
	}
	// The micro-drivers are not benchbase and must not be routed through it.
	for _, name := range []string{mixPgbench, mixSysbench} {
		if _, ok := benchbaseMix(name); ok {
			t.Errorf("%q was routed to benchbase", name)
		}
	}
}

// A tenant gets the mix entry at its own index, cycling when there are more
// tenants than drivers.
func TestMixCyclesAcrossTenants(t *testing.T) {
	mix := []string{mixTPCC, mixYCSB, mixPgbench}
	for tenant, want := range map[int]string{0: mixTPCC, 1: mixYCSB, 2: mixPgbench, 3: mixTPCC, 4: mixYCSB} {
		if got := mix[tenant%len(mix)]; got != want {
			t.Errorf("tenant %d got %q, want %q", tenant, got, want)
		}
	}
}

// The default mix must be buildable and genuinely mixed: N copies of one
// benchmark does not measure a multi-tenant deployment.
func TestDefaultWorkloadMixIsMixed(t *testing.T) {
	if len(defaultWorkloadMix) < 2 {
		t.Fatalf("default mix %v is not a mix", defaultWorkloadMix)
	}
	seen := map[string]bool{}
	for _, name := range defaultWorkloadMix {
		if !knownMixWorkload(name) {
			t.Errorf("default mix names %q, which cannot be built", name)
		}
		seen[name] = true
	}
	if len(seen) < 2 {
		t.Errorf("default mix %v runs the same driver on every tenant", defaultWorkloadMix)
	}
	// The heavy analytical loads stay opt-in, or one benchmark's load phase
	// dominates a nightly.
	for _, name := range defaultWorkloadMix {
		if name == mixTPCH || name == mixCHBenchmark {
			t.Errorf("default mix includes %q, whose load phase is too heavy to run by default", name)
		}
	}
}

func TestMultitenantParamsDefaults(t *testing.T) {
	p := multitenantParamsFromConfig(core.NewConfig())
	if p.Tenants < 2 {
		t.Errorf("default tenants = %d, want at least 2", p.Tenants)
	}
	if p.SeedRows <= 0 || p.ForkSeedRows <= 0 {
		t.Errorf("non-positive default in %+v", p)
	}
	if len(p.Workloads) == 0 {
		t.Error("no default workload mix")
	}
	// Interference is solo/mixed, so 1.0 means "no interference at all"; a gate
	// at or below that would fail every run.
	if p.MaxInterference <= 1 {
		t.Errorf("default max_interference = %v, want > 1", p.MaxInterference)
	}
	if p.EndpointCPU == "" || p.EndpointMemory == "" {
		t.Error("tenant endpoints must be sized explicitly, or a few of them exhaust the cluster")
	}
}

// An unknown driver must be named in the error, so the fix is obvious from the
// failure alone.
func TestUnknownMixWorkloadIsReportedClearly(t *testing.T) {
	_, err := mixWorkloadFor("nosuchbench", perfConfig{}, core.NewConfig(), pg.ConnInfo{Host: "h", Port: 5432}, mixDeps{})
	if err == nil {
		t.Fatal("an unknown driver was accepted")
	}
	if !strings.Contains(err.Error(), "nosuchbench") {
		t.Errorf("error %q does not name the offending driver", err)
	}
}

func TestMsSince(t *testing.T) {
	if got := msSince(time.Now().Add(-1500 * time.Millisecond)); got < 1400 || got > 2500 {
		t.Errorf("msSince = %v, want about 1500", got)
	}
}

func TestMaxOf(t *testing.T) {
	if got := maxOf([]float64{3, 9, 4}); got != 9 {
		t.Errorf("maxOf = %v, want 9", got)
	}
	if got := maxOf(nil); got != 0 {
		t.Errorf("maxOf(nil) = %v, want 0", got)
	}
}

// The sample-app workloads the stress team already uses must be selectable, and
// must route to the sample-app driver rather than to benchbase.
func TestSampleAppMixMapping(t *testing.T) {
	want := map[string]sampleapp.Workload{
		mixBankTransfer:    sampleapp.SqlBankTransfer,
		mixSqlTransactions: sampleapp.SqlTransactions,
		mixDataLoadDDL:     sampleapp.SqlDataLoadWithDDL,
	}
	for name, wl := range want {
		got, ok := sampleappMix(name)
		if !ok || got != wl {
			t.Errorf("sampleappMix(%q) = %q,%v; want %q,true", name, got, ok, wl)
		}
		if _, isBench := benchbaseMix(name); isBench {
			t.Errorf("%q was routed to benchbase", name)
		}
		if !knownMixWorkload(name) {
			t.Errorf("%q is not selectable in the mix", name)
		}
	}
}

// A sample-app workload without its jar must say so, rather than failing later
// with a confusing java error inside the pod.
func TestSampleAppNeedsItsJar(t *testing.T) {
	_, err := mixWorkloadFor(mixBankTransfer, perfConfig{}, core.NewConfig(), pg.ConnInfo{Host: "h", Port: 5433}, mixDeps{})
	if err == nil || !strings.Contains(err.Error(), "jar") {
		t.Errorf("err = %v, want a complaint about the missing jar", err)
	}
	w, err := mixWorkloadFor(mixBankTransfer, perfConfig{}, core.NewConfig(), pg.ConnInfo{Host: "h", Port: 5433},
		mixDeps{jarPath: "/opt/ampqa/ssa.jar", marker: "m"})
	if err != nil {
		t.Fatalf("with a jar: %v", err)
	}
	if !strings.Contains(w.Name(), "sampleapp") {
		t.Errorf("workload name %q does not identify the driver", w.Name())
	}
}

// The progress line's format is fixed by the jar's own
// ReadableStatsMetric.getMetricsAndReset; this is a real one.
func TestParseSampleAppMetrics(t *testing.T) {
	log := strings.Join([]string{
		"2026-08-09 07:00:00 [INFO] starting",
		"Read: 10.00 ops/sec (5.00 ms/op), 100 total ops  |  Write: 90.00 ops/sec (1.00 ms/op), 900 total ops  |  ",
		"Read: 100.00 ops/sec (2.00 ms/op), 2000 total ops  |  Write: 300.00 ops/sec (1.00 ms/op), 6000 total ops  |  ",
	}, "\n")
	s, err := parseSampleAppMetrics(log)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The LAST line is the one covering the end of the measured run.
	if s.TPS != 400 {
		t.Errorf("TPS = %v, want 400 (100 read + 300 write)", s.TPS)
	}
	// Op-weighted: (2.00*100 + 1.00*300) / 400 = 1.25
	if s.LatencyAvgMS < 1.24 || s.LatencyAvgMS > 1.26 {
		t.Errorf("latency = %v, want about 1.25", s.LatencyAvgMS)
	}
}

func TestParseSampleAppMetricsRejectsUselessLogs(t *testing.T) {
	if _, err := parseSampleAppMetrics("nothing useful here\nstill nothing"); err == nil {
		t.Error("a log with no progress line was accepted")
	}
	// Ran, but did nothing: worth failing rather than reporting 0 tps as a result.
	zero := "Read: 0.00 ops/sec (0.00 ms/op), 0 total ops  |  Write: 0.00 ops/sec (0.00 ms/op), 0 total ops  |  "
	if _, err := parseSampleAppMetrics(zero); err == nil {
		t.Error("a run that produced no throughput was accepted")
	}
}

// Every workload the mix offers must have a delivery step. A driver that is
// selectable but never put in the pod fails in prepare with "not found on
// target", which is exactly what happened the first time pgbench was selected.
func TestEveryMixWorkloadHasDelivery(t *testing.T) {
	for _, name := range allMixWorkloads() {
		if !hasDeliveryStep(name) {
			t.Errorf("%q is selectable in the mix but has no delivery step", name)
		}
	}
	if hasDeliveryStep("nosuchbench") {
		t.Error("an unknown driver claims to have a delivery step")
	}
}

// A driver that cannot be prepared or measured must cost only its own numbers.
// Every figure this test produces lands in its LAST phases, so failing the whole
// run over one bad driver throws away the measurements the healthy tenants were
// about to give — which is how the PERFORMANCE table kept showing nothing but
// the four control-plane timings.
func TestDroppingOneDriverKeepsTheOthers(t *testing.T) {
	clients := []tenantClient{
		{TenantIndex: 0, WorkloadName: mixTPCC},
		{TenantIndex: 1, WorkloadName: mixBankTransfer},
		{TenantIndex: 2, WorkloadName: mixSqlTransactions},
	}
	// Tenant 1 fails; the surviving set must be the other two, in order.
	var kept []tenantClient
	for _, c := range clients {
		if c.TenantIndex == 1 {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) != 2 || kept[0].TenantIndex != 0 || kept[1].TenantIndex != 2 {
		t.Fatalf("kept = %+v, want tenants 0 and 2", kept)
	}

	// And the charts must be built from the survivors only, with no gap where
	// the dropped tenant was.
	solo := map[int]Sample{0: {TPS: 100}, 2: {TPS: 40}}
	mixed := map[int]Sample{0: {TPS: 50}, 2: {TPS: 40}}
	if got := len(interferenceChart(kept, solo, mixed).Series); got != 2 {
		t.Errorf("interference chart has %d series, want only the 2 measured tenants", got)
	}
	if got := len(throughputChart(kept, solo, mixed).Series); got != 4 {
		t.Errorf("throughput chart has %d series, want 2 per measured tenant", got)
	}
}

// Two tenants can be given different drivers that resolve the SAME artifact —
// bank_transfer and sql_transactions both need the sample-app jar. The default
// mix does exactly that, so the delivery path must be safe to call from several
// tenants at once; it fetches into one shared local cache and raced on the temp
// file when it was not.
func TestDefaultMixSharesAnArtifactAcrossTenants(t *testing.T) {
	shared := 0
	for _, name := range defaultWorkloadMix {
		if _, ok := sampleappMix(name); ok {
			shared++
		}
	}
	if shared < 2 {
		t.Skipf("default mix has %d sample-app workloads; the shared-artifact race needs 2", shared)
	}
	// The guard is a package-level mutex; assert it is actually taken by the
	// delivery path rather than merely declared.
	// Delivery is now one grouped pass — the artifact is fetched once and pushed
	// to every pod that needs it — so sharing it across tenants is no longer a
	// race but the normal path.
	if !hasDeliveryStep(mixBankTransfer) || !hasDeliveryStep(mixSqlTransactions) {
		t.Error("the shared sample-app artifact has no delivery step")
	}
}

// A run where only one tenant survived is not a multitenancy measurement: that
// tenant's "mixed" phase had no neighbours, so interference compares it against
// itself and reads 1.0 however bad isolation really is. The first passing run of
// this test did exactly that — 17 datapoints, all from tpcc — and reported
// success, which is worse than failing.
func TestSingleSurvivingTenantIsNotAMeasurement(t *testing.T) {
	solo := map[int]Sample{0: {TPS: 85.7}}
	mixed := map[int]Sample{0: {TPS: 86.9}}
	one := []tenantClient{{TenantIndex: 0, WorkloadName: mixTPCC}}

	// The ratio itself looks perfect, which is exactly the trap.
	ratio := solo[0].TPS / mixed[0].TPS
	if ratio > 1.05 {
		t.Fatalf("fixture is wrong: ratio %v should look healthy", ratio)
	}
	if len(one) >= 2 {
		t.Fatal("fixture should have a single tenant")
	}
	// The chart built from it carries a single series — one workload, no
	// comparison — which is the shape the gate must refuse.
	if got := len(interferenceChart(one, solo, mixed).Series); got != 1 {
		t.Errorf("interference chart series = %d, want 1 for a lone tenant", got)
	}
}

// The sample-app jar connects from a client pod, which an amp compute treats as
// remote and therefore demands a password from. The jar hides connection errors
// by default, so getting this wrong does not fail — it hangs, silently, with a
// log holding nothing but the config dump. Both credentials must reach it.
func TestSampleAppCarriesEndpointCredentials(t *testing.T) {
	conn := pg.ConnInfo{Host: "h", Port: 55433, User: "cloud_admin", Password: "cloud_admin", Database: "postgres"}
	w, err := mixWorkloadFor(mixBankTransfer, perfConfig{}, core.NewConfig(), conn,
		mixDeps{jarPath: "/opt/ampqa/ssa.jar", marker: "m"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	sa, ok := w.(*sampleappWorkload)
	if !ok {
		t.Fatalf("built %T, want the sample-app driver", w)
	}
	for flag, want := range map[string]string{
		"username":                  conn.User,
		"password":                  conn.Password,
		"default_postgres_database": conn.Database,
	} {
		if got := sa.props[flag]; got != want {
			t.Errorf("--%s = %q, want %q", flag, got, want)
		}
	}
	// The jar refuses --password without --username, so an empty user would make
	// it exit rather than connect.
	if sa.props["username"] == "" {
		t.Error("no --username: the jar exits when a password is given without one")
	}
}

// The over-time charts are the ones that answer "what did each tenant do while
// the others ran". There must be one per condition for throughput and latency,
// each carrying a line per workload that reported a series.
func TestTimelineCharts(t *testing.T) {
	clients := []tenantClient{
		{TenantIndex: 0, WorkloadName: mixTPCC},
		{TenantIndex: 1, WorkloadName: mixSqlTransactions},
	}
	series := []SamplePoint{{ElapsedSec: 5, TPS: 100, LatencyMS: 9}, {ElapsedSec: 10, TPS: 80, LatencyMS: 12}}
	solo := map[int]Sample{0: {TPS: 90, Series: series}, 1: {TPS: 400, Series: series}}
	mixed := map[int]Sample{0: {TPS: 70, Series: series}, 1: {TPS: 300, Series: series}}

	charts := timelineCharts(clients, solo, mixed)
	// 4 all-workload timelines, plus throughput and latency per workload.
	if want := 4 + 2*len(clients); len(charts) != want {
		t.Fatalf("got %d over-time charts, want %d", len(charts), want)
	}
	for _, c := range charts {
		if c.Kind != core.ChartLine {
			t.Errorf("%q is %q; the over-time charts must be lines, not single-value bars", c.Title, c.Kind)
		}
		if len(c.Series) != 2 {
			t.Errorf("%q has %d lines, want 2", c.Title, len(c.Series))
		}
		for _, s := range c.Series {
			if len(s.Points) != len(series) {
				t.Errorf("%q line %q has %d points, want %d", c.Title, s.Name, len(s.Points), len(series))
			}
		}
	}

	// Every workload gets its own chart holding BOTH of its runs, so the pair
	// that the interference ratio is computed from can be read off one axis.
	for _, c := range clients {
		for _, kind := range []string{"Throughput", "Latency"} {
			title := c.WorkloadName + " — " + kind + ": alone vs sharing"
			var found *core.Chart
			for i := range charts {
				if charts[i].Title == title {
					found = &charts[i]
				}
			}
			if found == nil {
				t.Errorf("no %q chart", title)
				continue
			}
			names := []string{found.Series[0].Name, found.Series[1].Name}
			if names[0] != "alone" || names[1] != "sharing" {
				t.Errorf("%q lines are %v, want [alone sharing]", title, names)
			}
		}
	}

	// A driver with no series contributes no line rather than a flat zero one.
	partial := map[int]Sample{0: {TPS: 90, Series: series}, 1: {TPS: 400}}
	for _, c := range timelineCharts(clients, partial, partial) {
		if len(c.Series) == 0 {
			t.Errorf("%q was emitted with no lines at all", c.Title)
		}
		for _, s := range c.Series {
			if len(s.Points) == 0 {
				t.Errorf("%q line %q is empty; it should have been left out", c.Title, s.Name)
			}
		}
	}
}

// BenchBase reports per-window results in a CSV beside its summary; without
// parsing it a TPC-C tenant is invisible on every over-time chart.
func TestParseBenchbaseCSV(t *testing.T) {
	csv := "Time (seconds),Throughput (requests/second),Average Latency (microseconds)\n" +
		"0.0,120.5,45000\n5.0,131.25,38000\n"
	pts, err := parseBenchbaseCSV(csv)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("got %d points, want 2", len(pts))
	}
	if pts[1].ElapsedSec != 5 || pts[1].TPS != 131.25 {
		t.Errorf("second point = %+v", pts[1])
	}
	// BenchBase reports latency in microseconds; the charts are in ms.
	if pts[0].LatencyMS != 45 {
		t.Errorf("latency = %v ms, want 45 (converted from microseconds)", pts[0].LatencyMS)
	}
	if _, err := parseBenchbaseCSV("only,a,header\n"); err == nil {
		t.Error("a CSV with no data rows was accepted")
	}
}

// The jar's own operation budgets are left alone (the millions in its config
// dump are an operation count, not a data size — SqlBankTransfer seeds
// bank_number_of_accounts accounts, default 20). The only overrides are the
// credentials and the YSQL-syntax switches, both of which make it RUN on amp.
func TestSampleAppPropsCarryCredentialsOnly(t *testing.T) {
	conn := pg.ConnInfo{User: "u", Password: "p", Database: "postgres"}
	for _, name := range []string{mixSqlTransactions, mixDataLoadDDL} {
		props := sampleAppProps(name, conn)
		if want := 3 + len(pgOnlyProps); len(props) != want {
			t.Errorf("%s props = %v, want the three connection flags plus the %d Postgres-only switches", name, props, len(pgOnlyProps))
		}
		if props["username"] != "u" || props["password"] != "p" || props["default_postgres_database"] != "postgres" {
			t.Errorf("%s props = %v, want the connection flags carried through", name, props)
		}
		for _, k := range []string{"num_writes", "num_reads", "num_unique_keys"} {
			if _, ok := props[k]; ok {
				t.Errorf("%s overrides --%s; the jar's own default is what we want", name, k)
			}
		}
	}
}

// SqlBankTransfer creates its table with "SPLIT INTO <n> TABLETS" whenever
// bank_number_of_tablets is set, and it defaults to 10. That is YugabyteDB's
// syntax; an amp compute is stock PostgreSQL and rejects it, so the app died in
// createTablesIfNeeded before a single transfer ran.
func TestBankTransferDropsTheYBOnlyTableHint(t *testing.T) {
	for _, name := range defaultWorkloadMix {
		if !isSampleAppName(name) {
			continue
		}
		props := sampleAppProps(name, pg.ConnInfo{User: "u", Password: "p", Database: "postgres"})
		for k, want := range pgOnlyProps {
			if got := props[k]; got != want {
				t.Errorf("%s: --%s = %q, want %q so no YSQL-only clause reaches a stock-Postgres compute", name, k, got, want)
			}
		}
	}
}

// The default mix is several DIFFERENT kinds of tenant, and there is one tenant
// per entry — a deployment running N copies of one benchmark measures nothing
// about hosting a mixed population.
func TestDefaultMixIsBroadAndTenantsMatchIt(t *testing.T) {
	if len(defaultWorkloadMix) < 4 {
		t.Errorf("default mix has %d workloads: too narrow to represent a tenant population", len(defaultWorkloadMix))
	}
	kinds := map[string]bool{}
	for _, name := range defaultWorkloadMix {
		switch {
		case isBenchbaseName(name):
			kinds["benchbase"] = true
		case isSampleAppName(name):
			kinds["sampleapp"] = true
		default:
			kinds["micro"] = true
		}
		if !knownMixWorkload(name) || !hasDeliveryStep(name) {
			t.Errorf("default mix names %q, which cannot be built or delivered", name)
		}
	}
	if len(kinds) < 3 {
		t.Errorf("default mix draws from %v; want benchbase, sample-app and micro-driver tenants", kinds)
	}
	if got := multitenantParamsFromConfig(core.NewConfig()).Tenants; got != len(defaultWorkloadMix) {
		t.Errorf("default tenants = %d, want %d so every workload in the mix gets one", got, len(defaultWorkloadMix))
	}
}

// results.csv is already in milliseconds; samples.csv is in microseconds. The
// unit comes off the header, so neither file is silently scaled by 1000.
func TestBenchbaseLatencyUnitComesFromTheHeader(t *testing.T) {
	ms := "Time (seconds),Throughput (requests/second),Average Latency (millisecond)\n0,100.0,27.500\n"
	us := "Time (seconds),Requests,Throughput (requests/second),Minimum Latency (microseconds),Average Latency (microseconds)\n0,10,100.0,1,27500\n"
	for name, csv := range map[string]string{"milliseconds": ms, "microseconds": us} {
		pts, err := parseBenchbaseCSV(csv)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if pts[0].LatencyMS != 27.5 {
			t.Errorf("%s: latency = %v ms, want 27.5", name, pts[0].LatencyMS)
		}
		if pts[0].TPS != 100 {
			t.Errorf("%s: tps = %v, want 100 (column found by name, not position)", name, pts[0].TPS)
		}
	}
}

// Reported numbers are the median of several iterations, and the series kept is
// the median iteration's own — not a blend of runs that never happened.
func TestCvPctSpotsUnstableIterations(t *testing.T) {
	if got := cvPct([]float64{100, 100, 100}); got != 0 {
		t.Errorf("cv of identical iterations = %v, want 0", got)
	}
	if got := cvPct([]float64{100}); got != 0 {
		t.Errorf("cv of a single iteration = %v, want 0", got)
	}
	if got := cvPct([]float64{50, 100, 150}); got < 40 || got > 60 {
		t.Errorf("cv of a wide spread = %v%%, want roughly 50%%", got)
	}
}

// Five minutes per iteration, three iterations: a 60s window is mostly TPC-C
// ramp-up, and one window is not a statistic.
func TestMeasurementWindowIsLongEnoughToMeanSomething(t *testing.T) {
	p := multitenantParamsFromConfig(core.NewConfig())
	if p.MeasurementDuration < 5*time.Minute {
		t.Errorf("measurement duration = %v, want at least 5m", p.MeasurementDuration)
	}
	if p.Iterations < 3 {
		t.Errorf("iterations = %d, want at least 3 so the median is not one run", p.Iterations)
	}
}
