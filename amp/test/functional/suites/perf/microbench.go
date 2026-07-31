// microbench is the PERFORMANCE tier's phase-timed DDL/bulk-DML workload. It is
// deliberately unlike the other perf tests in two ways.
//
// WHAT it measures: labelled SQL PHASES, not steady-state throughput. Each phase
// is one timed Exec through apps/pg — create, a cold bulk insert, then
// truncate + re-insert cycles, a full count, a PK lookup, a bulk update, a bulk
// delete, drop — and each becomes a `microbench.<phase>` datapoint in ms. The
// GATE is a ratio computed INSIDE the run: median(insert_after_truncate) /
// insert_cold, emitted as its own datapoint and asserted against
// perf.microbench.max_phase_ratio. Because the reference is another phase of the
// same run and not a stored baseline, it asserts on the first run in a fresh CI
// workspace, where the baseline gate can only establish and WARN.
//
// WHERE it measures: the amp COMPUTE ENDPOINT the env provider hands the test
// (the pgEndpointEnv capability — docker single-container, sharedk8s, and
// universe envs all serve it), NOT a tserver. The rest of the perf tier
// self-provisions a co-located YB-in-k8s backing and connects to its tserver
// YSQL port, so amp is not in the request path there and an amp-layer
// regression is structurally invisible. This test therefore takes a
// functional-style environment and applies perf-style measurement and gating to
// it: no client pod, no benchmark driver, no self-provisioned backing — the
// timing, the datapoints and the baseline gate are the same plumbing every other
// perf test uses. Registered by init.
package perf

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// testNameMicrobench is the registered name and the baseline key.
const testNameMicrobench = "perf/microbench"

// Defaults for the phase load (perf.microbench.*).
const (
	// DefaultMicrobenchRows is per bulk statement. The gate is a dimensionless
	// RATIO between two phases of the same run, so it holds at any row count —
	// which frees the default to be what CI can afford rather than what the
	// original report used (1M). 200k keeps the six bulk statements (one cold
	// insert, three re-inserts, one update, one delete) in the low minutes on a
	// single-container env, comfortably inside the 60m perf timeout, while still
	// being large enough that per-statement fixed costs do not dominate.
	// -o perf.microbench.rows=int:1000000 reproduces the reported shape exactly.
	DefaultMicrobenchRows = 200000
	// DefaultMicrobenchTruncateCycles is how many truncate + re-insert cycles run
	// after the cold insert. Three is the minimum that shows a degradation is
	// PERSISTENT (and lets the gate take a median rather than a single value).
	DefaultMicrobenchTruncateCycles = 3
	// DefaultMicrobenchMaxPhaseRatio is the allowed
	// median(insert_after_truncate) / insert_cold. A re-insert into a truncated
	// table doing twice the work of the cold insert is already a finding; the
	// shape this gate was written for is ~5x. It needs calibration against a run
	// on real hardware — the first runs should be treated as establishing what
	// the healthy ratio is.
	DefaultMicrobenchMaxPhaseRatio = 2.0
)

// Phase groups. A phase's group is its datapoint prefix and the key the ratio
// gate aggregates on (the truncate cycles produce several phases per group).
const (
	phaseCreate               = "create"
	phaseInsertCold           = "insert_cold"
	phaseTruncate             = "truncate"
	phaseInsertAfterTruncate  = "insert_after_truncate"
	phaseCount                = "count"
	phasePKLookup             = "pk_lookup"
	phaseUpdate               = "update"
	phaseDelete               = "delete"
	phaseDrop                 = "drop"
	microbenchDatapointPrefix = "microbench."
	// ratioDatapoint is the gated intra-run ratio.
	ratioDatapoint = microbenchDatapointPrefix + "insert_after_truncate_ratio"
)

// microbenchParams is the resolved perf.microbench.* load shape.
type microbenchParams struct {
	Rows           int
	TruncateCycles int
	MaxPhaseRatio  float64
}

// microbenchParamsFromConfig reads perf.microbench.* into a microbenchParams.
func microbenchParamsFromConfig(c *core.Config) microbenchParams {
	return microbenchParams{
		Rows:           c.GetInt("perf.microbench.rows", DefaultMicrobenchRows),
		TruncateCycles: c.GetInt("perf.microbench.truncate_cycles", DefaultMicrobenchTruncateCycles),
		MaxPhaseRatio:  c.GetFloat("perf.microbench.max_phase_ratio", DefaultMicrobenchMaxPhaseRatio),
	}
}

// pgEndpointEnv is the capability this test needs: an env that serves an amp
// compute endpoint's Postgres. Declared locally (like the features suite does)
// so the test runs on any env able to hand out an endpoint — docker
// single-container, sharedk8s project-per-test, or a universe env — and skips
// cleanly on one that cannot (e.g. --fake-env).
type pgEndpointEnv interface {
	PGEndpoint() pg.ConnInfo
}

func init() {
	core.Register(core.Test{
		Name:       testNameMicrobench,
		Suite:      "perf",
		Category:   core.Perf,
		Env:        core.EnvSpec{Sharing: core.Exclusive, Shape: core.SingleContainer},
		Tags:       []string{"perf", "nightly"},
		OwningTeam: core.TeamCompute,
		Description: "Time labelled DDL/bulk-DML phases (create, cold 200k insert, truncate + re-insert cycles, count, PK lookup, update, delete, drop) " +
			"through an amp compute endpoint, and gate median(insert_after_truncate)/insert_cold against perf.microbench.max_phase_ratio — an intra-run ratio, so it asserts with no stored baseline.",
		Params: []string{"perf.microbench", "perf.baseline_path", "perf.update_baseline", "perf.latency_rise_pct", "perf.ready_timeout"},
		Run:    runMicrobench,
	})
}

// runMicrobench is the live body: connect to the endpoint the env serves → time
// each phase → emit the phase datapoints (baseline-gated) plus the intra-run
// ratio → assert the ratio.
func runMicrobench(t *core.T) error {
	cfg := perfFromConfig(t.Options)
	params := microbenchParamsFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)

	env, ok := t.Env.(pgEndpointEnv)
	if !ok {
		t.Skip("perf/microbench measures the amp compute endpoint: needs an env that serves one (docker / sharedk8s / universe)")
		return nil
	}

	id := identityFromConfig(t.Options, "", frameworkCommit(),
		LoadSignature(loadFieldsMicrobench(params)))
	t.SetPerfIdentity(id.core())

	ci := env.PGEndpoint()
	if err := pg.WaitReady(t.Context, ci, cfg.ReadyTimeout); err != nil {
		return err
	}
	client, err := pg.Connect(t.Context, ci)
	if err != nil {
		return err
	}
	defer client.Close()

	table := "mb_" + t.ResourceSuffix()
	phases := microbenchPhases(table, params)
	var results []phaseResult

	if err := t.Step("run_phases", func(s *core.StepHandle) error {
		// The phase list ends in DROP, but a phase failing part-way leaves the
		// table behind on a shared env.
		defer func() {
			if len(results) < len(phases) {
				_, _ = client.Exec(context.WithoutCancel(t.Context), "DROP TABLE IF EXISTS "+table)
			}
		}()
		r, err := runPhases(t.Context, client, phases, logf)
		results = r
		s.Set("phases", len(results))
		s.Set("rows", params.Rows)
		return err
	}); err != nil {
		attachPhases(t, results)
		return err
	}
	attachPhases(t, results)
	t.Chart(phaseChart(results))

	return t.Step("gate_phase_ratio", func(s *core.StepHandle) error {
		gated, info, ratioErr := microbenchDatapoints(results, params)
		if r, ok := phaseRatio(results, phaseInsertAfterTruncate, phaseInsertCold); ok {
			s.Set("insert_after_truncate_ratio", fmt.Sprintf("%.2fx", r))
		}
		gateErr := emitAndGate(t, cfg, microbenchVersion(t), testNameMicrobench, gated, info, id, logf)
		return errors.Join(ratioErr, gateErr)
	})
}

// phaseResult is one timed phase: its datapoint name, the group it aggregates
// into, and how long the statement took.
type phaseResult struct {
	Name  string
	Group string
	MS    float64
}

// phase is one labelled statement to time.
type phase struct {
	Name  string // unique within the run (a repeated group's phases are numbered)
	Group string
	SQL   string
}

// microbenchPhases builds the phase list: create → cold bulk insert → N
// (truncate, re-insert) cycles → count → PK lookup → bulk update → bulk delete →
// drop. The cycles carry the whole point of the test: the same INSERT statement,
// re-run against a table that has been truncated. Pure — unit-tested without a
// live endpoint.
func microbenchPhases(table string, params microbenchParams) []phase {
	rows := params.Rows
	if rows < 1 {
		rows = 1
	}
	insert := fmt.Sprintf("INSERT INTO %s(k, v) SELECT g, g FROM generate_series(1, %d) AS g", table, rows)
	phases := []phase{
		{phaseCreate, phaseCreate, fmt.Sprintf("CREATE TABLE %s(k integer PRIMARY KEY, v integer)", table)},
		{phaseInsertCold, phaseInsertCold, insert},
	}
	for i := 1; i <= params.TruncateCycles; i++ {
		phases = append(phases,
			phase{fmt.Sprintf("%s_%d", phaseTruncate, i), phaseTruncate, "TRUNCATE " + table},
			phase{fmt.Sprintf("%s_%d", phaseInsertAfterTruncate, i), phaseInsertAfterTruncate, insert},
		)
	}
	return append(phases,
		phase{phaseCount, phaseCount, fmt.Sprintf("SELECT count(*) FROM %s", table)},
		phase{phasePKLookup, phasePKLookup, fmt.Sprintf("SELECT v FROM %s WHERE k = %d", table, rows/2)},
		phase{phaseUpdate, phaseUpdate, fmt.Sprintf("UPDATE %s SET v = v + 1", table)},
		phase{phaseDelete, phaseDelete, fmt.Sprintf("DELETE FROM %s", table)},
		phase{phaseDrop, phaseDrop, fmt.Sprintf("DROP TABLE %s", table)},
	)
}

// pgExecer is the client surface runPhases needs; injectable so the phase loop
// is unit-tested without a live endpoint.
type pgExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (int64, error)
}

// runPhases executes each phase in order and records its wall time. It returns
// the phases completed so far alongside any error, so a partial run still
// reports its evidence.
func runPhases(ctx context.Context, client pgExecer, phases []phase, logf func(string, ...any)) ([]phaseResult, error) {
	results := make([]phaseResult, 0, len(phases))
	for _, ph := range phases {
		start := time.Now()
		if _, err := client.Exec(ctx, ph.SQL); err != nil {
			return results, fmt.Errorf("phase %s: %w", ph.Name, err)
		}
		ms := float64(time.Since(start).Microseconds()) / 1000
		results = append(results, phaseResult{Name: ph.Name, Group: ph.Group, MS: ms})
		if logf != nil {
			logf("microbench: phase %s took %.1f ms", ph.Name, ms)
		}
	}
	return results, nil
}

// microbenchDatapoints turns the timings into the tier's datapoints and applies
// the intra-run gate: every phase becomes a baseline-gated ms datapoint, and the
// insert-after-truncate ratio becomes its own datapoint asserted against
// params.MaxPhaseRatio (a ratio of two phases of the SAME run, so the assertion
// holds with no stored baseline). A missing or zero cold phase leaves the ratio
// unreported rather than dividing by it. Pure — unit-tested.
func microbenchDatapoints(results []phaseResult, params microbenchParams) (gated, info []core.Datapoint, err error) {
	for _, r := range results {
		gated = append(gated, core.Datapoint{
			Name: microbenchDatapointPrefix + r.Name, Value: r.MS, Unit: "ms", Direction: core.LowerIsBetter,
		})
	}
	ratio, ok := phaseRatio(results, phaseInsertAfterTruncate, phaseInsertCold)
	if !ok {
		return gated, info, nil
	}
	info = append(info, core.Datapoint{
		Name: ratioDatapoint, Value: ratio, Unit: "x", Direction: core.LowerIsBetter,
	})
	if params.MaxPhaseRatio > 0 && ratio > params.MaxPhaseRatio {
		err = fmt.Errorf("perf phase-ratio regression: %s = %.2fx (limit %.2fx) — the same INSERT is %.2fx slower after a TRUNCATE than on the cold table; see phases.txt",
			ratioDatapoint, ratio, params.MaxPhaseRatio, ratio)
	}
	return gated, info, err
}

// phaseRatio is median(numerator group) / denominator-group median, or ok=false
// when either group is absent or the denominator is not positive.
func phaseRatio(results []phaseResult, numerGroup, denomGroup string) (float64, bool) {
	numer := groupValues(results, numerGroup)
	denom := groupValues(results, denomGroup)
	if len(numer) == 0 || len(denom) == 0 {
		return 0, false
	}
	d := median(denom)
	if d <= 0 {
		return 0, false
	}
	return median(numer) / d, true
}

// groupValues collects the timings of one phase group, in order.
func groupValues(results []phaseResult, group string) []float64 {
	var vals []float64
	for _, r := range results {
		if r.Group == group {
			vals = append(vals, r.MS)
		}
	}
	return vals
}

// attachPhases lands the phase table on the result as phases.txt — the evidence
// a fired gate points at.
func attachPhases(t *core.T, results []phaseResult) {
	if len(results) == 0 {
		return
	}
	t.AttachBytes("phases.txt", "log", []byte(formatPhases(results)))
}

// formatPhases renders the phase timings as a fixed-width table. Pure.
func formatPhases(results []phaseResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-28s %12s\n", "phase", "time (ms)")
	for _, r := range results {
		fmt.Fprintf(&b, "%-28s %12.1f\n", r.Name, r.MS)
	}
	if ratio, ok := phaseRatio(results, phaseInsertAfterTruncate, phaseInsertCold); ok {
		fmt.Fprintf(&b, "\nmedian(%s) / %s = %.2fx\n", phaseInsertAfterTruncate, phaseInsertCold, ratio)
	}
	return b.String()
}

// phaseChart renders the phase timings as one named bar per phase. Pure.
func phaseChart(results []phaseResult) core.Chart {
	var series []core.ChartSeries
	for i, r := range results {
		series = append(series, core.ChartSeries{
			Name:   r.Name,
			Points: []core.ChartPoint{{X: float64(i + 1), Y: r.MS}},
		})
	}
	return core.Chart{
		Title: "Phase duration", Kind: core.ChartBar,
		XLabel: "phase", YLabel: "time (ms)", Series: series,
	}
}

// microbenchVersion is the version key the baseline is stored under: the
// env-detected product version (this test measures amp, not the backing), else
// whichever YB version the run resolves.
func microbenchVersion(t *core.T) string {
	if t.Env != nil {
		if v := t.Env.Versions().Product; v != "" {
			return v
		}
	}
	return chartVersion(t.Options)
}

// loadFieldsMicrobench is the perf/microbench load: the row count per bulk
// statement and the number of truncate cycles.
func loadFieldsMicrobench(params microbenchParams) LoadFields {
	return LoadFields{Rows: params.Rows, Cycles: params.TruncateCycles}
}
