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
// (the pgEndpointEnv capability — sharedk8s and
// universe envs all serve it), NOT a tserver: no client pod and no benchmark
// driver, each phase is one statement timed from the test process. The
// datapoints and the gates are the same plumbing every other perf test uses.
// Registered by init.
package perf

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
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
// so the test runs on any env able to hand out an endpoint — sharedk8s
// single-container, sharedk8s cluster-per-test, or a universe env — and skips
// cleanly on one that cannot (e.g. --fake-env).
type pgEndpointEnv interface {
	PGEndpoint() pg.ConnInfo
}

func init() {
	core.Register(core.Test{
		Name:       testNameMicrobench,
		Suite:      "perf",
		Category:   core.Perf,
		Env:        core.EnvSpec{Sharing: core.Exclusive, Shape: core.SingleContainer, ClusterSlots: armClusterSlots},
		Tags:       []string{"perf"},
		OwningTeam: core.TeamCompute,
		Description: "Time labelled DDL/bulk-DML phases (create, cold 200k insert, truncate + re-insert cycles, count, PK lookup, update, delete, drop) " +
			"through an amp compute endpoint, and gate median(insert_after_truncate)/insert_cold against perf.microbench.max_phase_ratio — an intra-run ratio, so it asserts with no stored baseline.",
		Params: []string{"perf.microbench", "perf.baseline", "perf.baseline_path", "perf.update_baseline", "perf.latency_rise_pct", "perf.ready_timeout"},
		Run:    runMicrobench,
	})
}

// runMicrobench is the live body: connect to the endpoint the env serves → time
// each phase (each one a child step of run_phases) → emit the phase datapoints
// (baseline-gated) plus the intra-run ratio → assert the ratio.
func runMicrobench(t *core.T) error {
	cfg := perfFromConfig(t.Options)
	params := microbenchParamsFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)

	env, ok := t.Env.(pgEndpointEnv)
	if !ok {
		t.Skip("perf/microbench measures the amp compute endpoint: needs an env that serves one (sharedk8s / universe)")
		return nil
	}

	if tag := resolveLiveArmTag(t, logf); tag != "" {
		return runMicrobenchProvisioned(t, cfg, params, tag, logf)
	}

	id := identityFromConfig(t.Options, "", frameworkCommit(), loadFieldsMicrobench(params))
	t.SetPerfIdentity(id.core())

	client, err := core.ConnectPG(t, env, cfg.ReadyTimeout)
	if err != nil {
		return err
	}
	defer client.Close()

	// This test measures the ENDPOINT, so the pod worth sampling is the endpoint's
	// — there is no self-provisioned YB backing here. Best-effort: an env that
	// cannot serve it samples nothing.
	if sampler := startEndpointMetricsSampler(t.Context, t, logf); sampler != nil {
		defer emitMetricSeries(t, sampler)
	}

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
		stepper := func(name string, fn func(set func(string, any)) error) error {
			return s.Step(name, func(h *core.StepHandle) error { return fn(h.Set) })
		}
		r, err := runPhases(t.Context, client, phases, logf, stepper)
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
		gateErr := emitAndGateAuto(t, cfg, microbenchVersion(t), testNameMicrobench, gated, info, id, logf)
		return errors.Join(ratioErr, gateErr)
	})
}

// runMicrobenchProvisioned is the live-baseline body: two self-provisioned
// stacks — the current images and the tag release — time the same phases
// concurrently, and the gate is the delta between them. The env's endpoint is
// deliberately not measured here: the arms must be twins, and the env's stack
// has no baseline twin.
func runMicrobenchProvisioned(t *core.T, cfg perfConfig, params microbenchParams, tag string, logf func(string, ...any)) error {
	load := loadFieldsMicrobench(params)
	load.Harness = provisionedHarness + cfg.isolationSuffix()
	id := identityFromConfig(t.Options, "", frameworkCommit(), load).forLiveArm(t.Options)
	t.SetPerfIdentity(id.core())

	cur, base, armErr, err := provisionLiveArms(t, cfg, tag, logf)
	if cur != nil {
		defer cur.teardown(logf)
	}
	if base != nil {
		defer base.teardown(logf)
	}
	if err != nil {
		return err
	}

	var curClient, baseClient *pg.Client
	if err := t.Step("create_endpoints", func(*core.StepHandle) error {
		var wg sync.WaitGroup
		curErr, baseErr := error(nil), armErr
		wg.Add(1)
		go func() { defer wg.Done(); curClient, curErr = cur.endpointClient(t.Context) }()
		if base != nil {
			wg.Add(1)
			go func() { defer wg.Done(); baseClient, baseErr = base.endpointClient(t.Context) }()
		}
		wg.Wait()
		if baseErr != nil {
			logf("perf: WARN live baseline arm failed (%v) — gating on history instead", baseErr)
			baseClient = nil
		}
		return curErr
	}); err != nil {
		return err
	}
	defer curClient.Close()
	if baseClient != nil {
		defer baseClient.Close()
	}

	table := "mb_" + t.ResourceSuffix()
	phases := microbenchPhases(table, params)

	var baseResults []phaseResult
	var baseErr error
	var wg sync.WaitGroup
	var pair *phaseBarrier
	if baseClient != nil {
		pair = newPhaseBarrier()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer pair.abandon(0)
			blogf := func(f string, a ...any) { logf("[baseline] "+f, a...) }
			baseResults, baseErr = runPhasesPaired(t.Context, baseClient, phases, blogf, nil, pair)
		}()
	}
	var results []phaseResult
	stepErr := t.Step("run_phases", func(s *core.StepHandle) error {
		stepper := func(name string, fn func(set func(string, any)) error) error {
			return s.Step(name, func(h *core.StepHandle) error { return fn(h.Set) })
		}
		r, err := runPhasesPaired(t.Context, curClient, phases, logf, stepper, pair)
		results = r
		s.Set("phases", len(results))
		s.Set("rows", params.Rows)
		s.Set("baseline", tag)
		return err
	})
	pair.abandon(0)
	wg.Wait()
	attachPhases(t, results)
	if stepErr != nil {
		return stepErr
	}
	t.Chart(phaseChart(results))
	if baseErr != nil {
		logf("perf: WARN live baseline arm failed (%v) — gating on history instead", baseErr)
		baseResults = nil
	}

	return t.Step("gate_phase_ratio", func(s *core.StepHandle) error {
		gated, info, ratioErr := microbenchDatapoints(results, params)
		if r, ok := phaseRatio(results, phaseInsertAfterTruncate, phaseInsertCold); ok {
			s.Set("insert_after_truncate_ratio", fmt.Sprintf("%.2fx", r))
		}
		var lbErr error
		baselineUp := len(baseResults) > 0
		if baselineUp {
			baseGated, _, _ := microbenchDatapoints(baseResults, params)
			extra, gerr := liveGate(t, s, testNameMicrobench, id, gated, baseGated,
				microbenchBestValues(results), microbenchBestValues(baseResults),
				phasePairs(results, baseResults, microbenchDatapointPrefix), tag, liveGateOpts{}, logf)
			lbErr = gerr
			info = append(info, extra...)
		}
		gateErr := emitAndGateHistory(t, cfg, microbenchVersion(t), testNameMicrobench, gated, info, id, !baselineUp, logf)
		return errors.Join(ratioErr, lbErr, gateErr)
	})
}

// phaseResult is one timed phase: its datapoint name, the group it aggregates
// into, and how long the statement took.
type phaseResult struct {
	Name  string
	Group string
	MS    float64
	// Iter is the iteration that produced it (0 when the test runs once); the
	// live arm pairs the arms' timings on (Name, Iter).
	Iter int
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

// phaseStepper wraps one phase's timed execution in a reported step named after
// the phase, so the portal's timeline draws a span per phase; set records
// properties on it. A nil stepper runs the phases unwrapped (unit tests).
type phaseStepper func(name string, fn func(set func(string, any)) error) error

// runPhases executes each phase in order and records its wall time. It returns
// the phases completed so far alongside any error, so a partial run still
// reports its evidence.
func runPhases(ctx context.Context, client pgExecer, phases []phase, logf func(string, ...any), stepper phaseStepper) ([]phaseResult, error) {
	return runPhasesPaired(ctx, client, phases, logf, stepper, nil)
}

// runPhasesPaired rendezvouses with the partner arm before each phase, so both
// arms measure it under the same cluster weather.
func runPhasesPaired(ctx context.Context, client pgExecer, phases []phase, logf func(string, ...any), stepper phaseStepper, pair *phaseBarrier) ([]phaseResult, error) {
	results := make([]phaseResult, 0, len(phases))
	for _, ph := range phases {
		pair.sync(0, ph.Name)
		var ms float64
		// The timing stays inside the step body: the step's span is the measured
		// window, not a second measurement of it.
		run := func(set func(string, any)) error {
			start := time.Now()
			_, err := client.Exec(ctx, ph.SQL)
			ms = float64(time.Since(start).Microseconds()) / 1000
			set("group", ph.Group)
			set("ms", fmt.Sprintf("%.1f", ms))
			return err
		}
		var err error
		if stepper != nil {
			err = stepper(ph.Name, run)
		} else {
			err = run(func(string, any) {})
		}
		if err != nil {
			return results, fmt.Errorf("phase %s: %w", ph.Name, err)
		}
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
// Gated: one datapoint per phase group. A repeated group (the truncate cycles)
// gates its MEDIAN and keeps the per-cycle timings as info, so the history still
// records them. One 200k-row INSERT swings 20-40% between cycles of the same run,
// so gating each cycle against its twin on the other arm gated that jitter.
func microbenchDatapoints(results []phaseResult, params microbenchParams) (gated, info []core.Datapoint, err error) {
	seen := map[string]bool{}
	for _, r := range results {
		vals := groupValues(results, r.Group)
		if len(vals) == 1 {
			gated = append(gated, phaseDatapoint(microbenchDatapointPrefix+r.Name, r.MS))
			continue
		}
		info = append(info, phaseDatapoint(microbenchDatapointPrefix+r.Name, r.MS))
		if !seen[r.Group] {
			seen[r.Group] = true
			gated = append(gated, phaseDatapoint(microbenchDatapointPrefix+r.Group, median(vals)))
		}
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

func phaseDatapoint(name string, ms float64) core.Datapoint {
	return core.Datapoint{Name: name, Value: ms, Unit: "ms", Direction: core.LowerIsBetter}
}

// microbenchBestValues is each repeated group's fastest cycle, the second leg
// of the live-arm verdict: a real regression slows the best draw too.
func microbenchBestValues(results []phaseResult) map[string]float64 {
	out := map[string]float64{}
	for _, r := range results {
		if vals := groupValues(results, r.Group); len(vals) > 1 {
			out[microbenchDatapointPrefix+r.Group] = aggregate(vals).Min
		}
	}
	return out
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
