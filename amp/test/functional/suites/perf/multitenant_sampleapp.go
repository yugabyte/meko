// The yb-stress-sample-apps driver, adapted to the perf tier's Workload
// contract so a tenant can be given one of the workloads the stress team
// already uses — SqlBankTransfer, SqlTransactions, SqlDataLoadWithDDL — instead
// of only a benchmark-suite load.
//
// Where it comes from: the jar is a GitHub release asset of the PRIVATE
// yugabyte/yb-stress-test repo (tag ssa_<version>, artifacts.sampleapp),
// fetched with the gh_package secret and delivered into the tenant's client
// pod. apps/sampleapp wraps running it; this file only adapts it.
//
// The adaptation that matters: benchbase and pgbench print a summary at the end
// and exit, while the sample-app is a continuous generator that prints a
// periodic progress line and keeps going. So a measured run bounds it with
// --run_time, waits for it to finish, then reads the throughput out of the last
// progress line it printed.
package perf

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/sampleapp"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// The mix names for the sample-app workloads.
const (
	mixBankTransfer    = "bank_transfer"
	mixSqlTransactions = "sql_transactions"
	mixDataLoadDDL     = "data_load_ddl"
)

// sampleappMix maps a mix name to its sample-app workload.
func sampleappMix(name string) (sampleapp.Workload, bool) {
	switch name {
	case mixBankTransfer:
		return sampleapp.SqlBankTransfer, true
	case mixSqlTransactions:
		return sampleapp.SqlTransactions, true
	case mixDataLoadDDL:
		return sampleapp.SqlDataLoadWithDDL, true
	}
	return "", false
}

// sampleAppJarSecret is the secret name the private jar is fetched with. It
// follows the artifact's own declaration, so a deployment that renames the
// secret does not have to patch this tier too.
func sampleAppJarSecret(c *core.Config) string {
	return c.GetString("artifacts.sampleapp.secret", "gh_package")
}

// sampleappWorkload drives one sample-app workload as a measured benchmark.
type sampleappWorkload struct {
	name         string
	workload     sampleapp.Workload
	jarPath      string
	conn         pg.ConnInfo
	tables       int
	readers      int
	writers      int
	marker       string
	props        map[string]string
	readyTimeout time.Duration
	logf         func(string, ...any)
}

func (w *sampleappWorkload) Name() string { return "sampleapp." + w.name }

// Prepare is a no-op: these workloads create the schema they need when they
// start, which is exactly how the combined stress scenario runs them. The
// two-phase create-then-load pattern belongs to SqlDataLoad, which is not in
// this mix.
func (w *sampleappWorkload) Prepare(context.Context, exec.RemoteExecutor) error { return nil }

// RunOnce measures the workload for exactly p.Duration.
//
// The window is bounded HERE rather than by waiting for the jar to exit. It is
// given --run_time as well, but that only makes its worker threads stop: the
// process itself can linger afterwards, and a run that waits for the process to
// disappear waits forever (observed: a SqlBankTransfer jar still alive 65
// minutes into a 60-second measurement). Stopping it ourselves makes the
// measured window exact and the phase's cost predictable.
func (w *sampleappWorkload) RunOnce(ctx context.Context, ex exec.RemoteExecutor, p RunParams) (Sample, error) {
	spec := sampleapp.RunSpec{
		JarPath:         w.jarPath,
		Workload:        w.workload,
		Hosts:           []string{w.conn.Host},
		YSQLPort:        w.conn.Port,
		NumTables:       w.tables,
		NumThreadsRead:  w.readers,
		NumThreadsWrite: w.writers,
		RunTime:         p.Duration,
		Marker:          w.marker,
		Props:           w.props,
	}
	// WaitReady's default readiness is "the log file is not empty", which is true
	// the instant the JVM prints its banner — long before a single operation has
	// run. Waiting for the first metrics line instead means the measured window
	// starts when the workload is actually working: SqlBankTransfer seeds its
	// accounts first, and a 60-second window that began at process start expired
	// during that seeding, leaving a log with no throughput line in it at all.
	app := sampleapp.New(ex, spec,
		sampleapp.WithLogf(w.logf),
		sampleapp.WithLoadMarker("ops/sec"),
		sampleapp.WithReadyTimeout(w.readyTimeout))
	if err := app.Start(ctx); err != nil {
		return Sample{}, fmt.Errorf("start %s: %w", w.Name(), err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = app.Stop(context.WithoutCancel(ctx))
		}
	}
	defer stop()

	if err := app.WaitReady(ctx); err != nil {
		return Sample{}, fmt.Errorf("%s never started generating load: %w", w.Name(), err)
	}
	if err := sleepCtx(ctx, p.Duration); err != nil {
		return Sample{}, err
	}
	stop()

	log, err := captureAppLog(ctx, app)
	if err != nil {
		return Sample{}, err
	}
	sample, err := parseSampleAppMetrics(log)
	if err != nil {
		return Sample{}, fmt.Errorf("%s: %w", w.Name(), err)
	}
	// The jar's own log, which is what gets attached for a human to read. The
	// parser cannot supply it: it is handed the text and returns measurements.
	sample.Raw = log
	return sample, nil
}

// captureAppLog collects the app's own log into memory. Collect is the public
// way to get at it, so the sink here just keeps the bytes instead of writing
// them to the run's artifacts.
func captureAppLog(ctx context.Context, app *sampleapp.App) (string, error) {
	var buf bytes.Buffer
	sink := funcSink(func(name, kind string, content []byte) error {
		buf.Write(content)
		buf.WriteByte('\n')
		return nil
	})
	if err := app.Collect(ctx, sink); err != nil {
		return "", fmt.Errorf("collect the sample-app log: %w", err)
	}
	if buf.Len() == 0 {
		return "", fmt.Errorf("the sample-app produced no log to read throughput from")
	}
	return buf.String(), nil
}

// funcSink adapts a function to the ArtifactSink interface.
type funcSink func(name, kind string, content []byte) error

func (f funcSink) Add(name, kind string, content []byte) error { return f(name, kind, content) }

var _ apps.ArtifactSink = funcSink(nil)

// sampleAppMetricRe matches one metric in the jar's progress line, whose format
// is fixed by ReadableStatsMetric.getMetricsAndReset:
//
//	Read: 1234.56 ops/sec (0.81 ms/op), 74073 total ops  |  Write: ...
var sampleAppMetricRe = regexp.MustCompile(`(Read|Write): ([0-9.]+) ops/sec \(([0-9.]+) ms/op\), ([0-9]+) total ops`)

// sampleAppStampRe matches the log4j timestamp every line carries (pattern
// "%d [%t] %p %c{1} - %m", whose %d default is yyyy-MM-dd HH:mm:ss,SSS).
var sampleAppStampRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}),(\d{3})`)

const sampleAppStampLayout = "2006-01-02 15:04:05"

// parseSampleAppMetrics turns the jar's periodic progress lines into a Sample
// WITH its time series.
//
// The tracker prints every few seconds and RESETS its counters each time, so
// each line is that window's own rate — a genuine series, not a running total.
// Keeping all of it matters: a summary number cannot show a tenant that
// collapsed halfway through, which is exactly what a multitenancy run needs to
// make visible.
//
// The reported TPS is the mean of the windows rather than the last one (a
// single window is noise), and latency is weighted by each window's throughput.
func parseSampleAppMetrics(log string) (Sample, error) {
	type window struct {
		elapsed float64
		tps     float64
		latency float64
	}
	var windows []window
	var first time.Time

	for _, line := range strings.Split(log, "\n") {
		matches := sampleAppMetricRe.FindAllStringSubmatch(line, -1)
		if len(matches) == 0 {
			continue
		}
		var tps, weighted float64
		for _, m := range matches {
			v, err1 := strconv.ParseFloat(m[2], 64)
			lat, err2 := strconv.ParseFloat(m[3], 64)
			if err1 != nil || err2 != nil {
				continue
			}
			tps += v
			weighted += lat * v
		}
		w := window{tps: tps}
		if tps > 0 {
			w.latency = weighted / tps
		}
		if stamp, ok := sampleAppLineTime(line); ok {
			if first.IsZero() {
				first = stamp
			}
			w.elapsed = stamp.Sub(first).Seconds()
		} else {
			w.elapsed = float64(len(windows))
		}
		windows = append(windows, w)
	}
	if len(windows) == 0 {
		return Sample{}, fmt.Errorf("no %q progress line in the sample-app log, so its throughput cannot be read", "ops/sec")
	}

	// The first window spans process start, so it is series data but not a fair
	// contribution to the summary.
	summary := windows
	if len(windows) > 1 {
		summary = windows[1:]
	}
	var totalTPS, weighted float64
	points := make([]SamplePoint, 0, len(windows))
	for _, w := range windows {
		points = append(points, SamplePoint{ElapsedSec: w.elapsed, TPS: w.tps, LatencyMS: w.latency})
	}
	for _, w := range summary {
		totalTPS += w.tps
		weighted += w.latency * w.tps
	}
	mean := totalTPS / float64(len(summary))
	if mean <= 0 {
		return Sample{}, fmt.Errorf("the sample-app ran but reported no throughput in %d window(s)", len(windows))
	}
	return Sample{
		TPS:          mean,
		LatencyAvgMS: weighted / totalTPS,
		Series:       points,
	}, nil
}

// sampleAppLineTime reads a log line's timestamp, so elapsed seconds come from
// the run itself rather than from an assumed print interval.
func sampleAppLineTime(line string) (time.Time, bool) {
	m := sampleAppStampRe.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(sampleAppStampLayout, m[1])
	if err != nil {
		return time.Time{}, false
	}
	ms, err := strconv.Atoi(m[2])
	if err == nil {
		t = t.Add(time.Duration(ms) * time.Millisecond)
	}
	return t, true
}
