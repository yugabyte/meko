// The tenant workload mix: which real benchmark each tenant runs, and how a
// tenant's own client pod is stood up to drive it.
//
// Tenants in a multi-tenant deployment do not run the same thing. Measuring N
// copies of one benchmark answers a question nobody has; what matters is
// whether an OLTP tenant keeps its throughput while an analytical tenant reads
// the world and a third hammers key-value writes. So each tenant is assigned a
// different driver from a configurable mix, each gets its OWN client pod (a
// shared pod would make client CPU the thing under test), and each is measured
// alone and then again with all of them running.
package perf

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/apps/benchbase"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/sysbench"
	"github.com/yugabyte/amp/test/functional/framework/artifact"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// The drivers a tenant may be given. The benchbase family covers OLTP (tpcc),
// analytics (tpch), key-value (ycsb) and a mixed HTAP load (chbenchmark); the
// other two are the standard Postgres micro-drivers already wired into this
// tier.
const (
	mixTPCC        = "tpcc"
	mixTPCH        = "tpch"
	mixYCSB        = "ycsb"
	mixCHBenchmark = "chbenchmark"
	mixPgbench     = "pgbench"
	mixSysbench    = "sysbench"
)

// defaultWorkloadMix gives each tenant a different KIND of tenant to be: a
// TPC-C shop (benchbase), a bank moving money between accounts and a
// transactional writer (both yb-stress-sample-apps, the same workloads the
// stress team runs). tpch, chbenchmark, ycsb, pgbench, sysbench and
// data_load_ddl are all selectable via perf.multitenant.workloads; the
// analytical ones stay out of the default because their load phase alone would
// dominate a nightly.
var defaultWorkloadMix = []string{
	mixTPCC,            // OLTP, benchbase
	mixYCSB,            // key-value, benchbase
	mixBankTransfer,    // contended row updates, yb-stress-sample-apps
	mixSqlTransactions, // multi-statement transactions, yb-stress-sample-apps
	mixPgbench,         // the standard Postgres micro-driver
}

// benchbaseMix maps a mix name to its benchbase benchmark, and reports whether
// the name belongs to benchbase at all.
func benchbaseMix(name string) (benchbase.Benchmark, bool) {
	switch name {
	case mixTPCC:
		return benchbase.TPCC, true
	case mixTPCH:
		return benchbase.TPCH, true
	case mixYCSB:
		return benchbase.YCSB, true
	case mixCHBenchmark:
		return benchbase.CHBenchmark, true
	}
	return "", false
}

// knownMixWorkload reports whether a configured mix entry can be built at all,
// so a typo fails the run at once instead of after the tenants are provisioned.
func knownMixWorkload(name string) bool {
	if _, ok := benchbaseMix(name); ok {
		return true
	}
	if _, ok := sampleappMix(name); ok {
		return true
	}
	return name == mixPgbench || name == mixSysbench
}

// allMixWorkloads is every name the mix accepts, for error messages.
func allMixWorkloads() []string {
	return []string{
		mixTPCC, mixTPCH, mixYCSB, mixCHBenchmark,
		mixPgbench, mixSysbench,
		mixBankTransfer, mixSqlTransactions, mixDataLoadDDL,
	}
}

// tenantClient is one tenant's driver: its own pod, and the workload it runs
// against its own endpoint.
type tenantClient struct {
	TenantIndex  int
	WorkloadName string
	Workload     Workload
	Exec         exec.RemoteExecutor
}

// startTenantClients gives every tenant a client pod and a workload from the
// mix, cycling the mix when there are more tenants than entries. Benchbase is
// delivered only to the pods whose workload needs it.
func startTenantClients(ctx context.Context, t *core.T, cfg perfConfig, kube *k8s.Kubectl, sink apps.ArtifactSink,
	td *teardownStack, tenants []tenant, mix []string) ([]tenantClient, error) {

	if kube == nil {
		return nil, fmt.Errorf("driving real benchmark clients needs a kube context for the control-plane namespace")
	}
	for _, name := range mix {
		if !knownMixWorkload(name) {
			return nil, fmt.Errorf("perf.multitenant.workloads names %q, which is not a driver this tier can build (want any of %s)",
				name, strings.Join(allMixWorkloads(), ", "))
		}
	}

	// 1. Pods, concurrently — pure setup, and doing it one tenant at a time was
	//    most of this test's wall clock.
	names := make([]string, len(tenants))
	execs := make([]exec.RemoteExecutor, len(tenants))
	errs := make([]error, len(tenants))
	var wg sync.WaitGroup
	for i := range tenants {
		names[i] = mix[i%len(mix)]
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Named by the tenant's own Index, not the slice position: a caller
			// adding tenants one stage at a time passes a one-element slice, and
			// slice-positional names would collide on the second stage's pod.
			podName := fmt.Sprintf("ampqa-mt-%s-%d", t.ResourceSuffix(), tenants[i].Index)
			podExec, err := startPerfClientPod(ctx, kube, sink, td, podName, cfg.ClientImage, cfg.ClientPullSecret)
			if err != nil {
				errs[i] = fmt.Errorf("tenant %d client pod: %w", i, err)
				return
			}
			execs[i] = podExec
		}(i)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	// 2. Artifacts, once each, pushed to every pod that needs them.
	jarPath, err := deliverDriversOnce(ctx, t, cfg, names, execs)
	if err != nil {
		return nil, err
	}

	// 3. The drivers themselves, pointed at their own tenant's endpoint Service:
	//    the test process's connection is a port-forward to 127.0.0.1 and means
	//    nothing inside a pod.
	clients := make([]tenantClient, 0, len(tenants))
	for i := range tenants {
		conn := endpointConn(kube.Namespace(), tenants[i].Endpoint.ID, tenants[i].Endpoint.Address)
		w, err := mixWorkloadFor(names[i], cfg, t.Options, conn,
			mixDeps{jarPath: jarPath, marker: fmt.Sprintf("mt-%s-%d", t.ResourceSuffix(), tenants[i].Index), logf: logfFor(t)})
		if err != nil {
			return nil, fmt.Errorf("tenant %d: %w", i, err)
		}
		clients = append(clients, tenantClient{TenantIndex: i, WorkloadName: names[i], Workload: w, Exec: execs[i]})
	}
	return clients, nil
}

// deliverDriversOnce resolves each needed artifact ONCE and pushes it to every
// pod that needs it in a single Deploy call — the model the stress framework
// uses (one deploy(), given all its target hosts). Delivering per tenant instead
// fetched the same jar several times into one local cache and raced on its temp
// file: two tenants in the default mix (bank_transfer, sql_transactions) share
// the sample-app artifact.
//
// It returns the sample-app jar path, which is the same on every pod because
// they all share cfg.RemoteDir.
func deliverDriversOnce(ctx context.Context, t *core.T, cfg perfConfig, names []string,
	execs []exec.RemoteExecutor) (jarPath string, err error) {

	// Group the pods by the artifact their driver needs.
	byArtifact := map[string][]exec.RemoteExecutor{}
	for i, name := range names {
		switch {
		case isBenchbaseName(name):
			byArtifact["benchbase"] = append(byArtifact["benchbase"], execs[i])
		case isSampleAppName(name):
			byArtifact["sampleapp"] = append(byArtifact["sampleapp"], execs[i])
		case name == mixPgbench:
			byArtifact["pgbench"] = append(byArtifact["pgbench"], execs[i])
		case name == mixSysbench:
			byArtifact["sysbench"] = append(byArtifact["sysbench"], execs[i])
		default:
			return "", fmt.Errorf("no delivery step for %q", name)
		}
	}

	specs, err := artifact.Resolve(t.Options)
	if err != nil {
		return "", fmt.Errorf("resolve artifacts: %w", err)
	}
	dep := &artifact.Deployer{}

	if pods, ok := byArtifact["benchbase"]; ok {
		// The stress fork by default: its workers reconnect and retry when a
		// connection drops, and an amp compute restarting mid-run is exactly the
		// event that otherwise kills a tenant's load for good (observed: TPC-C
		// exited 255 on "This connection has been closed"). Fall back to the
		// public build when the fork is not declared or its secret is absent.
		name := "benchbase_stress"
		spec, declared := specs[name]
		if !declared || !benchbaseForkWanted(t.Options) {
			name = "benchbase"
			spec, declared = specs[name]
		}
		if !declared {
			return "", fmt.Errorf("neither artifacts.benchbase_stress nor artifacts.benchbase is declared")
		}
		opts := artifact.DeployOptions{RemoteDir: cfg.RemoteDir}
		if spec.Secret != "" {
			secrets, serr := core.NewSecrets(t.Options)
			if serr != nil {
				return "", fmt.Errorf("secrets: %w", serr)
			}
			value, serr := secrets.Value(spec.Secret)
			if serr != nil {
				return "", fmt.Errorf("resolve the %s secret: %w", name, serr)
			}
			opts.Secret = value
		}
		if err := dep.Deploy(ctx, pods, spec, opts); err != nil {
			return "", fmt.Errorf("deploy %s into %d client pod(s): %w", name, len(pods), err)
		}
	}
	if pods, ok := byArtifact["sampleapp"]; ok {
		spec, declared := specs["sampleapp"]
		if !declared {
			return "", fmt.Errorf("artifacts.sampleapp is not declared, so no sample-app workload can run")
		}
		secrets, serr := core.NewSecrets(t.Options)
		if serr != nil {
			return "", fmt.Errorf("secrets: %w", serr)
		}
		jarSecret, serr := secrets.Value(sampleAppJarSecret(t.Options))
		if serr != nil {
			return "", fmt.Errorf("resolve the sample-app jar secret: %w", serr)
		}
		if err := dep.Deploy(ctx, pods, spec,
			artifact.DeployOptions{RemoteDir: cfg.RemoteDir, Secret: jarSecret}); err != nil {
			return "", fmt.Errorf("deploy the sample-app jar into %d client pod(s): %w", len(pods), err)
		}
		jarPath = path.Join(cfg.RemoteDir, spec.GlobPath)
	}
	if pods, ok := byArtifact["pgbench"]; ok {
		spec, serr := pgbenchArtifact(t.Options)
		if serr != nil {
			return "", serr
		}
		if err := dep.Deploy(ctx, pods, spec, artifact.DeployOptions{RemoteDir: cfg.RemoteDir}); err != nil {
			return "", fmt.Errorf("deploy pgbench into %d client pod(s): %w", len(pods), err)
		}
		// The shim is per pod: it is a script written inside each container.
		for _, pod := range pods {
			if _, err := deliverPgbench(ctx, dep, pod, spec, cfg.RemoteDir); err != nil {
				return "", err
			}
		}
	}
	if pods, ok := byArtifact["sysbench"]; ok {
		// A distro package, installed in each container rather than fetched.
		for _, pod := range pods {
			if err := sysbench.EnsureInstalled(ctx, pod); err != nil {
				return "", fmt.Errorf("install sysbench: %w", err)
			}
		}
	}
	return jarPath, nil
}

func isBenchbaseName(name string) bool { _, ok := benchbaseMix(name); return ok }
func isSampleAppName(name string) bool { _, ok := sampleappMix(name); return ok }

// benchbaseForkWanted lets a run fall back to the public BenchBase build, for a
// deployment without access to the private stress release.
func benchbaseForkWanted(c *core.Config) bool {
	return c.GetBool("perf.multitenant.benchbase_fork", true)
}

// hasDeliveryStep reports whether deliverDriversOnce knows how to put this
// driver in a pod. A driver that is selectable in the mix but never delivered
// fails deep in prepare with "<tool> not found on target", so the rule is
// asserted in a unit test rather than discovered on a cluster.
func hasDeliveryStep(name string) bool {
	return isBenchbaseName(name) || isSampleAppName(name) || name == mixPgbench || name == mixSysbench
}

// mixDeps are the per-tenant bits a driver may need beyond its connection.// mixDeps are the per-tenant bits a driver may need beyond its connection.
type mixDeps struct {
	jarPath string
	marker  string
	logf    func(string, ...any)
}

// mixWorkloadFor builds the driver for one mix entry against a tenant's own
// endpoint connection.
func mixWorkloadFor(name string, cfg perfConfig, opts *core.Config, conn pg.ConnInfo, deps mixDeps) (Workload, error) {
	if bench, ok := benchbaseMix(name); ok {
		c := cfg
		c.Benchmark = string(bench)
		c.Terminals = mixTerminals(opts)
		return &benchbaseSeriesWorkload{Workload: tpccWorkloadFor(c, conn), runDir: benchbaseDirFor(cfg)}, nil
	}
	if wl, ok := sampleappMix(name); ok {
		if deps.jarPath == "" {
			return nil, fmt.Errorf("%q needs the sample-app jar, which was not delivered", name)
		}
		props := sampleAppProps(name, conn)
		// Schema-shaping flags, forwarded only when they carry a real value. The
		// embedded config ships num_value_columns: 0, and passing that through
		// made SqlTransactions emit "INSERT INTO SqlTransactions(k, ) VALUES
		// (..., )" — a syntax error on every operation, reported as a driver
		// that ran and committed nothing.
		if v := opts.GetInt("apps.sampleapp.num_value_columns", 0); v > 0 {
			props["num_value_columns"] = strconv.Itoa(v)
		}
		if v := opts.GetString("apps.sampleapp.use_datatypes", ""); v != "" && v != "false" {
			props["use_datatypes"] = v
		}
		return &sampleappWorkload{
			name: name, workload: wl, jarPath: deps.jarPath, conn: conn,
			// The jar connects from a client pod, i.e. remotely, and an amp compute
			// demands cloud_admin's password for anything that is not loopback.
			// Without these it retries the connection forever and says nothing —
			// the jar hides connection errors by default — so the workload simply
			// never starts and the log holds only its config dump.
			props:  props,
			tables: opts.GetInt("apps.sampleapp.num_tables", 1),
			// Its OWN knob, not apps.sampleapp.num_threads_*: those are 0 in the
			// embedded config (meaning "let the jar decide"), which is fine for a
			// stress soak but leaves a perf measurement with a concurrency nobody
			// chose and cannot compare across runs.
			readers: opts.GetInt("perf.multitenant.sampleapp_threads", 8),
			writers: opts.GetInt("perf.multitenant.sampleapp_threads", 8),
			marker:  deps.marker, logf: deps.logf,
			// Seeding before the first operation is part of these workloads, so
			// readiness gets a real budget rather than the measurement's.
			readyTimeout: opts.GetDuration("perf.multitenant.sampleapp_ready_timeout", 10*time.Minute),
		}, nil
	}
	switch name {
	case mixPgbench:
		return pgbenchWorkloadFor(pgbenchParamsFromConfig(opts), conn), nil
	case mixSysbench:
		return sysbenchWorkloadFor(sysbenchParamsFromConfig(opts), conn), nil
	}
	return nil, fmt.Errorf("no driver for %q", name)
}

// sampleAppProps are the extra jar flags a tenant's driver needs.
//
// The jar connects from a client pod, i.e. remotely, and an amp compute demands
// cloud_admin's password for anything that is not loopback. Without these it
// retries forever and says nothing, because the jar hides connection errors by
// default — the workload simply never starts and its log holds only the config
// dump.
//
// It also pins off every knob that makes the apps emit YugabyteDB-only DDL.
// An amp compute is stock PostgreSQL, so one YSQL-ism in setup kills the
// driver before its first operation and the tenant is dropped from the
// measurement. The key counts in the config dump (2,000,000 writes) are the
// app's OPERATION budget, not its data size, and stay untouched: capping them
// would end the workload early, mid-measurement.
func sampleAppProps(name string, conn pg.ConnInfo) map[string]string {
	props := map[string]string{
		"username":                  conn.User,
		"password":                  conn.Password,
		"default_postgres_database": conn.Database,
	}
	for k, v := range pgOnlyProps {
		props[k] = v
	}
	// One row per second, matching benchbase's -im and pgbench's -P, so all five
	// workloads share an x-resolution and their timelines can be read against
	// each other. The jar's own default is 5000ms.
	props["metrics_trace_timeout"] = strconv.Itoa(int(sampleAppTraceInterval / time.Millisecond))
	return props
}

// pgOnlyProps switch off every sample-app knob that appends YSQL syntax to a
// CREATE TABLE. bank_number_of_tablets is the one that actually bit us — it
// defaults to 10, so SqlBankTransfer emitted "SPLIT INTO 10 TABLETS" and died
// in createTablesIfNeeded with "syntax error at or near SPLIT". The other two
// are off by default today; they are pinned so a change to those defaults
// cannot silently drop a tenant again.
// mixTerminals is a benchbase tenant's client concurrency. Its own knob rather
// than perf.terminals, because a multitenant tenant is sized to help SATURATE a
// shared deployment, while the single-tenant perf tests size for a clean number
// from one client. At 8 the whole population left the storage layer at ~1% CPU.
func mixTerminals(opts *core.Config) int {
	return opts.GetInt("perf.multitenant.terminals", 16)
}

// sampleAppTraceInterval is how often the yb-stress-sample-apps tracker prints a
// window — the resolution of those drivers' time series.
const sampleAppTraceInterval = time.Second

var pgOnlyProps = map[string]string{
	"bank_number_of_tablets":   "0",     // else: SPLIT INTO n TABLETS
	"num_tablets":              "0",     // else: SPLIT INTO n TABLETS
	"disable_table_colocation": "false", // true emits WITH (COLOCATION=FALSE)
}

// logfFor is the masked logger the drivers write through.
func logfFor(t *core.T) func(string, ...any) { return t.Masker().Logf(log.Printf) }

// benchbaseDirFor is where deliverBenchbase puts the distribution in a pod.
func benchbaseDirFor(cfg perfConfig) string {
	return path.Join(cfg.RemoteDir, benchbase.DefaultSubdir)
}

// --- charts ---
//
// The datapoints carry the numbers and the portal charts each one's history;
// these are the within-run pictures history cannot show: what a workload lost
// the moment its neighbours appeared, and where the control plane spent time.

// throughputChart puts each workload's solo and mixed throughput side by side.
// One series per bar, which is how the portal renders a categorical chart.
func throughputChart(clients []tenantClient, solo, mixed map[int]Sample) core.Chart {
	var series []core.ChartSeries
	for i, c := range clients {
		series = append(series,
			core.ChartSeries{Name: c.WorkloadName + " alone", Points: []core.ChartPoint{{X: float64(i + 1), Y: solo[c.TenantIndex].TPS}}},
			core.ChartSeries{Name: c.WorkloadName + " sharing", Points: []core.ChartPoint{{X: float64(i + 1), Y: mixed[c.TenantIndex].TPS}}},
		)
	}
	return core.Chart{
		Title: "Throughput: alone vs sharing the deployment", Kind: core.ChartBar,
		XLabel: "workload", YLabel: "tps", Series: series,
	}
}

// interferenceChart shows what each tenant lost to its neighbours. 1.0 means it
// lost nothing. A tenant that produced no mixed throughput charts as 0 rather
// than an infinity.
func interferenceChart(clients []tenantClient, solo, mixed map[int]Sample) core.Chart {
	var series []core.ChartSeries
	for i, c := range clients {
		var ratio float64
		if m := mixed[c.TenantIndex].TPS; m > 0 {
			ratio = solo[c.TenantIndex].TPS / m
		}
		series = append(series, core.ChartSeries{
			Name: c.WorkloadName, Points: []core.ChartPoint{{X: float64(i + 1), Y: ratio}},
		})
	}
	return core.Chart{
		Title: "Interference (alone ÷ sharing; 1.0 = neighbours cost nothing)", Kind: core.ChartBar,
		XLabel: "workload", YLabel: "ratio", Series: series,
	}
}

// timelineChart draws one line per workload from its per-window series: the
// within-run picture a summary cannot give, because a tenant that collapses
// halfway through still averages out to something respectable. pick chooses
// throughput or latency; drivers that report no series contribute no line.
func timelineChart(title, yLabel string, clients []tenantClient, samples map[int]Sample,
	pick func(SamplePoint) float64) (core.Chart, bool) {

	var series []core.ChartSeries
	for _, c := range clients {
		pts := samples[c.TenantIndex].Series
		if len(pts) == 0 {
			continue
		}
		points := make([]core.ChartPoint, 0, len(pts))
		for _, p := range pts {
			points = append(points, core.ChartPoint{X: p.ElapsedSec, Y: pick(p)})
		}
		series = append(series, core.ChartSeries{Name: c.WorkloadName, Points: points})
	}
	if len(series) == 0 {
		return core.Chart{}, false
	}
	return core.Chart{Title: title, Kind: core.ChartLine, XLabel: "seconds", YLabel: yLabel, Series: series}, true
}

// timelineCharts is the full over-time set: throughput and latency, each with
// the tenant alone and then with every tenant running, so the two conditions
// can be read against each other rather than as one blended number.
func timelineCharts(clients []tenantClient, solo, mixed map[int]Sample) []core.Chart {
	tps := func(p SamplePoint) float64 { return p.TPS }
	lat := func(p SamplePoint) float64 { return p.LatencyMS }
	var out []core.Chart
	for _, c := range []struct {
		title  string
		yLabel string
		data   map[int]Sample
		pick   func(SamplePoint) float64
	}{
		{"Throughput over time — tenant alone", "tps", solo, tps},
		{"Throughput over time — all tenants running", "tps", mixed, tps},
		{"Latency over time — tenant alone", "ms", solo, lat},
		{"Latency over time — all tenants running", "ms", mixed, lat},
	} {
		if chart, ok := timelineChart(c.title, c.yLabel, clients, c.data, c.pick); ok {
			out = append(out, chart)
		}
	}
	out = append(out, perWorkloadCharts(clients, solo, mixed)...)
	return out
}

// perWorkloadCharts put one workload's alone and sharing runs on the SAME axes,
// which is the comparison the test exists to make. The all-workloads timelines
// above cannot show it: ycsb at 1700 tps and tpcc at 90 flatten each other, and
// a tenant's two runs land on different charts where nobody can line them up.
func perWorkloadCharts(clients []tenantClient, solo, mixed map[int]Sample) []core.Chart {
	var out []core.Chart
	for _, c := range clients {
		for _, m := range []struct {
			label  string
			yLabel string
			pick   func(SamplePoint) float64
		}{
			{"Throughput", "tps", func(p SamplePoint) float64 { return p.TPS }},
			{"Latency", "ms", func(p SamplePoint) float64 { return p.LatencyMS }},
		} {
			chart := core.Chart{
				Title:  fmt.Sprintf("%s — %s: alone vs sharing", c.WorkloadName, m.label),
				Kind:   core.ChartLine,
				XLabel: "elapsed s", YLabel: m.yLabel,
			}
			for _, run := range []struct {
				name string
				data map[int]Sample
			}{{"alone", solo}, {"sharing", mixed}} {
				pts := seriesPoints(run.data[c.TenantIndex], m.pick)
				if len(pts) == 0 {
					continue
				}
				chart.Series = append(chart.Series, core.ChartSeries{Name: run.name, Points: pts})
			}
			if len(chart.Series) > 0 {
				out = append(out, chart)
			}
		}
	}
	return out
}

// seriesPoints projects one sample's time series onto a chart axis.
func seriesPoints(s Sample, pick func(SamplePoint) float64) []core.ChartPoint {
	var pts []core.ChartPoint
	for _, p := range s.Series {
		pts = append(pts, core.ChartPoint{X: p.ElapsedSec, Y: pick(p)})
	}
	return pts
}

// opLatencyChart is where the control plane spent its time standing tenants up.
func opLatencyChart(clusterMS, endpointMedianMS, endpointMaxMS, firstSQLMS float64) core.Chart {
	return core.Chart{
		Title: "Control-plane operation latency", Kind: core.ChartBar,
		XLabel: "operation", YLabel: "ms",
		Series: []core.ChartSeries{
			{Name: "create cluster", Points: []core.ChartPoint{{X: 1, Y: clusterMS}}},
			{Name: "create endpoint (median)", Points: []core.ChartPoint{{X: 2, Y: endpointMedianMS}}},
			{Name: "create endpoint (worst)", Points: []core.ChartPoint{{X: 3, Y: endpointMaxMS}}},
			{Name: "first SQL", Points: []core.ChartPoint{{X: 4, Y: firstSQLMS}}},
		},
	}
}

// forkChart contrasts forking an empty branch with forking a seeded one.
func forkChart(emptyMS, seededMS float64, seedRows int) core.Chart {
	return core.Chart{
		Title: "Fork time by parent size", Kind: core.ChartBar,
		XLabel: "parent", YLabel: "ms",
		Series: []core.ChartSeries{
			{Name: "empty parent", Points: []core.ChartPoint{{X: 1, Y: emptyMS}}},
			{Name: fmt.Sprintf("%d-row parent", seedRows), Points: []core.ChartPoint{{X: 2, Y: seededMS}}},
		},
	}
}

// --- per-window series for the benchbase drivers ---

// benchbaseSeriesWorkload wraps a benchbase-backed workload and attaches the
// per-window series BenchBase writes alongside its summary. Without it a
// benchbase tenant contributes no line to the over-time charts — the shared
// driver parses only the summary, so its Sample.Series is empty and TPC-C is
// invisible on exactly the graphs that show a tenant faltering mid-run.
type benchbaseSeriesWorkload struct {
	Workload
	runDir string
}

func (w *benchbaseSeriesWorkload) RunOnce(ctx context.Context, ex exec.RemoteExecutor, p RunParams) (Sample, error) {
	sample, err := w.Workload.RunOnce(ctx, ex, p)
	if err != nil {
		return sample, err
	}
	if series, serr := benchbaseSeries(ctx, ex, w.runDir); serr == nil {
		sample.Series = series
	}
	return sample, nil
}

// benchbaseSeries reads the newest results/*.results.csv BenchBase wrote. Its
// columns are documented by BenchBase itself: time in seconds, then throughput,
// then the latency columns. Best effort — a missing file only costs the chart a
// line, never the run.
func benchbaseSeries(ctx context.Context, ex exec.RemoteExecutor, runDir string) ([]SamplePoint, error) {
	dir := path.Join(runDir, "results")
	cmd := fmt.Sprintf("cat $(ls -t %s/*.results.csv 2>/dev/null | head -n1) 2>/dev/null", shellQuoteArg(dir))
	res, err := ex.Run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return parseBenchbaseCSV(res.Stdout)
}

// parseBenchbaseCSV turns BenchBase's per-window CSV into series points.
//
// Columns are located by header name, and the latency unit is read off that
// name. BenchBase writes two different per-window files from the same shape of
// data: results.csv carries "Average Latency (millisecond)" while samples.csv
// carries "(microseconds)". Reading column 2 positionally and assuming
// microseconds silently divided every results.csv latency by 1000 — the charts
// showed tpcc at 0.06 ms while its summary said 27.8 ms.
//
// Unparseable rows are skipped rather than failing the run.
func parseBenchbaseCSV(csv string) ([]SamplePoint, error) {
	lines := strings.Split(strings.TrimSpace(csv), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("no benchbase per-window results")
	}
	head := strings.Split(lines[0], ",")
	col := func(want string) int {
		for i, h := range head {
			if strings.Contains(strings.ToLower(strings.TrimSpace(h)), want) {
				return i
			}
		}
		return -1
	}
	secCol, tpsCol := col("time (seconds)"), col("throughput")
	latCol := col("average latency")
	if secCol < 0 || tpsCol < 0 {
		return nil, fmt.Errorf("benchbase csv header has no time/throughput column: %q", lines[0])
	}
	latDiv := 1.0
	if latCol >= 0 && strings.Contains(strings.ToLower(head[latCol]), "microsecond") {
		latDiv = 1000
	}
	get := func(cols []string, i int) (float64, bool) {
		if i < 0 || i >= len(cols) {
			return 0, false
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(cols[i]), 64)
		return v, err == nil
	}
	var points []SamplePoint
	for _, line := range lines[1:] {
		cols := strings.Split(line, ",")
		sec, okSec := get(cols, secCol)
		tps, okTPS := get(cols, tpsCol)
		if !okSec || !okTPS {
			continue
		}
		pt := SamplePoint{ElapsedSec: sec, TPS: tps}
		if lat, ok := get(cols, latCol); ok {
			pt.LatencyMS = lat / latDiv
		}
		points = append(points, pt)
	}
	if len(points) == 0 {
		return nil, fmt.Errorf("benchbase results csv had no usable rows")
	}
	return points, nil
}

// shellQuoteArg single-quotes a path for the remote shell.
func shellQuoteArg(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
