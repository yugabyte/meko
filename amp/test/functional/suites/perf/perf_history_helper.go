package perf

import (
	"fmt"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/suites/perf/history"
)

// Historical perf tracking. Each perf run APPENDS its aggregate to a run-history
// store (separate from the baseline, which holds one reference value) and the
// report renders a TREND of median TPS (and latency) across the recent runs via
// the same T.Chart primitive the per-run graphs use — no new chart code. The
// history file is environment-specific like the baseline and gitignored.

// History defaults (perf.history.*). The store is on by default and bounded to
// the most-recent runs so the trend stays readable and the file stays small.
const (
	DefaultHistoryEnabled = true
	DefaultHistoryPath    = "baselines/perf-history.json"
	DefaultHistoryMaxRuns = 20
)

// historyConfig is the resolved perf.history.* configuration.
type historyConfig struct {
	Enabled bool
	Path    string
	MaxRuns int
}

// historyConfigFromConfig reads perf.history.* into a historyConfig.
func historyConfigFromConfig(c *core.Config) historyConfig {
	return historyConfig{
		Enabled: c.GetBool("perf.history.enabled", DefaultHistoryEnabled),
		Path:    c.GetString("perf.history.path", DefaultHistoryPath),
		MaxRuns: c.GetInt("perf.history.max_runs", DefaultHistoryMaxRuns),
	}
}

// historyRecord builds this run's history record from the aggregate and the
// run identity. The timestamp and seed are injected (never read from the clock
// here) so the record path is deterministic under test.
func historyRecord(testName, version string, res BenchResult, id Identity, seed int64, now time.Time) history.Record {
	return history.Record{
		Test:               testName,
		Version:            version,
		Timestamp:          now.UTC().Format(time.RFC3339),
		MedianTPS:          res.TPS.Median,
		LatencyAvgMS:       res.LatencyAvg.Median,
		LatencyP95MS:       res.LatencyP95.Median,
		LatencyP99MS:       res.LatencyP99.Median,
		CVTPS:              res.TPS.CV,
		Seed:               seed,
		YBImage:            id.YBImage,
		AmpControllerImage: id.AmpControllerImage,
		AmpEndpointImage:   id.AmpEndpointImage,
		Commit:             id.Commit,
		LoadSignature:      id.LoadSignature,
	}
}

// trendCharts turns the recent run history into the trend charts a perf run
// reports: median TPS across runs (X = chronological run index) and, when any
// run recorded a latency, median latency across runs. Both are line charts built
// with the shared T.Chart primitive. Fewer than two records is not a trend
// (nothing to draw a line between), so it yields no charts rather than a
// one-point line. Pure — unit-tested without a live run.
func trendCharts(recs []history.Record) []core.Chart {
	if len(recs) < 2 {
		return nil
	}
	n := len(recs)
	var tps, lat []core.ChartPoint
	hasLat := false
	for i, r := range recs {
		x := float64(i + 1)
		tps = append(tps, core.ChartPoint{X: x, Y: r.MedianTPS})
		lat = append(lat, core.ChartPoint{X: x, Y: r.LatencyAvgMS})
		if r.LatencyAvgMS != 0 {
			hasLat = true
		}
	}
	charts := []core.Chart{{
		Title: fmt.Sprintf("TPS trend (last %d runs)", n), Kind: core.ChartLine,
		XLabel: "run", YLabel: "median tps",
		Series: []core.ChartSeries{{Name: "median tps", Points: tps}},
	}}
	if hasLat {
		charts = append(charts, core.Chart{
			Title: fmt.Sprintf("Latency trend (last %d runs)", n), Kind: core.ChartLine,
			XLabel: "run", YLabel: "latency (ms)",
			Series: []core.ChartSeries{{Name: "median latency", Points: lat}},
		})
	}
	return charts
}

// recordAndTrend appends this run's aggregate to the history store, reads back
// the recent history for this test+version, and returns the trend charts to
// emit (empty when the history now holds fewer than two records — a first run).
// It is the testable core of the perf run's history wiring: file IO plus the
// pure trend build, with the timestamp and seed injected. The caller emits the
// returned charts via T.Chart.
func recordAndTrend(hc historyConfig, testName, version string, res BenchResult, id Identity, seed int64, now time.Time) ([]core.Chart, error) {
	if err := history.Append(hc.Path, historyRecord(testName, version, res, id, seed, now)); err != nil {
		return nil, fmt.Errorf("append perf history %s: %w", hc.Path, err)
	}
	f, err := history.Load(hc.Path)
	if err != nil {
		return nil, fmt.Errorf("reload perf history %s: %w", hc.Path, err)
	}
	return trendCharts(history.Recent(f.ForTest(testName, version), hc.MaxRuns)), nil
}

// emitHistoryTrend records this run in the history store and emits the trend
// chart(s) onto the test's report. History is best-effort telemetry: a store IO
// failure is logged, not fatal, so it never fails an otherwise-passing perf run.
// Disabled (perf.history.enabled=false) is a no-op.
func emitHistoryTrend(t *core.T, hc historyConfig, testName, version string, res BenchResult, id Identity, logf func(string, ...any)) {
	if !hc.Enabled {
		return
	}
	charts, err := recordAndTrend(hc, testName, version, res, id, seedValue(t), time.Now())
	if err != nil {
		logf("perf: history record failed (best-effort): %v", err)
		return
	}
	for _, c := range charts {
		t.Chart(c)
	}
}

// seedValue is the run seed as an int64 for the history record, 0 when unset.
func seedValue(t *core.T) int64 {
	if t.Seed == nil {
		return 0
	}
	return t.Seed.Value()
}
