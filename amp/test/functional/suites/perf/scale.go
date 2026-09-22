// Escalation on ONE tenant: every stage adds another driver against the same
// endpoint until it stops doing useful work — throughput at or below min_tps,
// aggregate collapsed under collapse_pct of its peak, a driver that stops
// committing or falls over, or storage that stops advancing. A falling
// aggregate is recorded and climbed past. Hitting the ceiling is a PASSING
// outcome; only a stage-1 failure fails the test. perf/scale_depth_tpcc grows
// the same endpoint along the other axis, dataset size.
package perf

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// wallError marks a stage failure that IS the ceiling: the load stopped being
// served. Anything else — a client pod that would not start, artifacts that
// would not deliver, a dead exec transport — is the harness failing to ask the
// question, and must fail the test instead of being recorded as a product
// limit. The first version of this ladder had no such split and reported a dead
// port-forward at stage 2 as "ceiling = 1 driver" in a green run.
type wallError struct{ err error }

func (w wallError) Error() string { return w.err.Error() }
func (w wallError) Unwrap() error { return w.err }

// A MIX: identical clients only ever find one wall.
var appLadderMix = []string{mixSqlTransactions, mixBankTransfer}

// scaleLadders: the registered escalations, one driver per stage.
var scaleLadders = map[string][]string{
	"scale_tpcc":    {mixTPCC},
	"scale_ycsb":    {mixYCSB},
	"scale_pgbench": {mixPgbench},
	"scale_apps":    appLadderMix,
}

// scaleTier keeps the ladders off the perf schedule: a wall costs hours and a
// cluster, and reaching one is a finding, not a regression. Run with --tag scale.
const scaleTier = "scale"

func init() {
	for name, mix := range scaleLadders {
		name, mix := name, mix
		core.Register(core.Test{
			Name:        "perf/" + name,
			Suite:       "perf",
			Category:    core.Perf,
			Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
			Tags:        []string{scaleTier},
			OwningTeam:  core.TeamControlPlane,
			Description: "Add one more concurrent driver against the SAME endpoint every stage (" + strings.Join(mix, ", ") + "), drive them all for a full window, and keep climbing past diminishing returns: the ladder ends only when throughput falls to min_tps, the aggregate collapses under collapse_pct of its peak, a driver stops committing or falls over, or the storage layer stops advancing. Reports how much concurrent load one tenant carried, and which wall ended it.",
			Params:      []string{"perf.scale"},
			Timeout:     4 * time.Hour,
			Run: func(t *core.T) error {
				return runScaleLadder(t, name, mix)
			},
		})
	}
}

type scaleParams struct {
	MaxDrivers    int
	StageDuration time.Duration
	// Budget caps the whole ladder in wall-clock: an endpoint that never breaks
	// must still hand the run back.
	Budget time.Duration
	// MinTPS is "no useful work": at or below it the endpoint has stopped
	// serving, which is a hard stop.
	MinTPS float64
	// CollapsePct is the share of the PEAK aggregate below which throughput
	// counts as collapsed rather than merely degraded.
	CollapsePct float64
	// FloorPct: stop when the slowest driver falls below this share of one alone.
	FloorPct float64
	// ReadTimeout bounds the storage probe; a wedged WAL path never answers, so
	// the deadline is the detector.
	ReadTimeout time.Duration
	EndpointCPU string
	EndpointMem string
}

// loadFieldsScale is what must match for two ladder runs to be comparable. The
// ladder name rides in Script so the four ladders never share a signature.
func loadFieldsScale(ladder string, p scaleParams) LoadFields {
	return LoadFields{
		Script:        ladder,
		Clients:       p.MaxDrivers,
		MeasurementMS: p.StageDuration.Milliseconds(),
	}
}

// scaleVersion keys the baseline: this test measures amp, not the backing.
func scaleVersion(t *core.T) string {
	if t.Env != nil {
		if v := t.Env.Versions().Product; v != "" {
			return v
		}
	}
	return chartVersion(t.Options)
}

func scaleParamsFromConfig(c *core.Config) scaleParams {
	return scaleParams{
		MaxDrivers:    c.GetInt("perf.scale.max_drivers", 64),
		Budget:        c.GetDuration("perf.scale.budget", 3*time.Hour),
		MinTPS:        c.GetFloat("perf.scale.min_tps", 1.0),
		CollapsePct:   c.GetFloat("perf.scale.collapse_pct", 25),
		StageDuration: c.GetDuration("perf.scale.stage_duration", 6*time.Minute),
		FloorPct:      c.GetFloat("perf.scale.floor_pct", 25),
		ReadTimeout:   c.GetDuration("perf.scale.read_timeout", 2*time.Minute),
		EndpointCPU:   c.GetString("perf.scale.endpoint_cpu", "4"),
		EndpointMem:   c.GetString("perf.scale.endpoint_memory", "8Gi"),
	}
}

// ladderDriver is one client: own pod, own database, shared endpoint.
type ladderDriver struct {
	name string
	db   string
	w    Workload
	ex   exec.RemoteExecutor
}

// ladderCurve renders what the ladder measured before it stopped, so a failure
// still reports the curve it had.
func ladderCurve(points []core.ChartPoint) string {
	if len(points) == 0 {
		return "no stage completed"
	}
	parts := make([]string, 0, len(points))
	for _, p := range points {
		parts = append(parts, fmt.Sprintf("%.0f:%.1ftps", p.X, p.Y))
	}
	return strings.Join(parts, " ")
}

func runScaleLadder(t *core.T, ladder string, mix []string) error {
	env, ok := t.Env.(ampMultitenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, ok := t.Env.(interface{ AmpKube() *k8s.Kubectl })
	if !ok || kubeEnv.AmpKube() == nil {
		t.Skip("driving real clients needs a kube context for the control-plane namespace")
		return nil
	}
	params := scaleParamsFromConfig(t.Options)
	cfg := perfFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)
	kube := kubeEnv.AmpKube()
	cl, err := env.AmpController(t.Context)
	if err != nil {
		return err
	}
	poolID := env.AmpPoolID()

	var tenants []tenant
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		for i := range tenants {
			if tenants[i].Conn != nil {
				tenants[i].Conn.Close()
			}
			if _, err := cl.DeleteClusterAndWait(ctx, poolID, tenants[i].ClusterID); err != nil {
				logf("%s: could not delete cluster %s: %v", ladder, tenants[i].ClusterID, err)
			}
		}
	}()

	td := &teardownStack{}
	defer func() {
		tctx, cancel := context.WithTimeout(context.Background(), perfTeardownBudget)
		defer cancel()
		_ = td.run(tctx, logf)
	}()

	if err := t.Step("create_endpoint", func(s *core.StepHandle) error {
		ten, _, err := createTenant(t, cl, env, poolID, 0, multitenantParams{
			SeedRows:       1,
			EndpointCPU:    params.EndpointCPU,
			EndpointMemory: params.EndpointMem,
		})
		if err != nil {
			return fmt.Errorf("%w%s", err, schedulingHint(t, logf))
		}
		tenants = append(tenants, ten)
		if err := ensureProbeTable(t.Context, ten.Conn); err != nil {
			return fmt.Errorf("create the probe table: %w", err)
		}
		s.Set("endpoint", ten.Endpoint.ID)
		s.Set("endpoint_size", params.EndpointCPU+" / "+params.EndpointMem)
		s.Set("ladder", strings.Join(mix, ", "))
		return nil
	}); err != nil {
		return err
	}

	base, err := authedEndpointConn(t.Context, kube, tenants[0].Endpoint.ID, tenants[0].Endpoint.Address, logf)
	if err != nil {
		return err
	}
	var drivers []ladderDriver

	// Own database per driver: benchbase always names its tables the same, so
	// copies would load over each other.
	addDriver := func(i int) error {
		name := mix[i%len(mix)]
		db := fmt.Sprintf("scale_%d", i)
		if _, err := tenants[0].Conn.Exec(t.Context, "CREATE DATABASE "+db); err != nil {
			return fmt.Errorf("driver %d: create database %s: %w", i, db, err)
		}
		conn := base
		conn.Database = db

		podName := fmt.Sprintf("ampqa-ld-%s-%d", t.ResourceSuffix(), i)
		ex, err := startPerfClientPod(t.Context, kube, attachSink{t: t, logf: logf}, td,
			podName, cfg.ClientImage, cfg.ClientPullSecret)
		if err != nil {
			return fmt.Errorf("driver %d client pod: %w", i, err)
		}
		jarPath, err := deliverDriversOnce(t.Context, t, cfg, []string{name}, []exec.RemoteExecutor{ex})
		if err != nil {
			return fmt.Errorf("driver %d artifacts: %w", i, err)
		}
		w, err := mixWorkloadFor(name, cfg, t.Options, conn, mixDeps{
			jarPath: jarPath,
			marker:  fmt.Sprintf("ld-%s-%d", t.ResourceSuffix(), i),
			logf:    logf,
		})
		if err != nil {
			return fmt.Errorf("driver %d: %w", i, err)
		}
		if err := w.Prepare(t.Context, ex); err != nil {
			return fmt.Errorf("driver %d prepare: %w", i, err)
		}
		drivers = append(drivers, ladderDriver{name: name, db: db, w: w, ex: ex})
		return nil
	}

	var aggAt []core.ChartPoint
	var reached int
	baseline := 0.0
	// Peak is the efficiency ceiling: the most this endpoint ever carried.
	// Passing it is a finding, NOT a reason to stop — the question this test
	// answers is where the endpoint breaks, and diminishing returns are a long
	// way from broken. First* record where each softer signal was crossed.
	peakAgg, peakAt := 0.0, 0
	firstDropAt, firstStarvedAt, brokeAt := 0, 0, 0
	stopReason := fmt.Sprintf("carried all %d concurrent drivers without breaking", params.MaxDrivers)
	deadline := time.Now().Add(params.Budget)

	for n := 1; n <= params.MaxDrivers; n++ {
		if n > 1 && time.Now().After(deadline) {
			stopReason = fmt.Sprintf("%d drivers: ran out of the %s ladder budget before anything broke", n-1, params.Budget)
			break
		}
		stage := fmt.Sprintf("stage_%02d_drivers%d", n, n)
		var agg, worst float64
		err := t.Step(stage, func(s *core.StepHandle) error {
			if err := addDriver(n - 1); err != nil {
				return err
			}
			samples := make([]Sample, len(drivers))
			errs := make([]error, len(drivers))
			var wg sync.WaitGroup
			for i := range drivers {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					samples[i], errs[i] = drivers[i].w.RunOnce(t.Context, drivers[i].ex,
						RunParams{Duration: params.StageDuration, SampleInterval: time.Second})
					recordDriverRun(t, logf, fmt.Sprintf("%s-%d", drivers[i].name, i), stage, samples[i], params.StageDuration)
				}(i)
			}
			wg.Wait()

			worst = -1
			for i := range drivers {
				if errs[i] != nil {
					// A driver that fell over under load is the wall; a driver
					// we could not even talk to is not.
					if isTransportErr(errs[i]) {
						return fmt.Errorf("driver %d (%s): lost its exec transport: %w", i, drivers[i].name, errs[i])
					}
					diag := captureEndpointDiag(t, kube, tenants[0], fmt.Sprintf("%s-drivers%d", ladder, n), logf)
					if diag != "" {
						s.Set("endpoint_state", strings.ReplaceAll(diag, "\n", " | "))
					}
					return wallError{fmt.Errorf("driver %d (%s) stopped serving [endpoint: %s]: %w",
						i, drivers[i].name, strings.ReplaceAll(diag, "\n", " | "), errs[i])}
				}
				agg += samples[i].TPS
				if worst < 0 || samples[i].TPS < worst {
					worst = samples[i].TPS
				}
			}
			s.Set("drivers", len(drivers))
			s.Set("aggregate_tps", fmt.Sprintf("%.1f", agg))
			s.Set("slowest_driver_tps", fmt.Sprintf("%.1f", worst))
			// Zero with output = a starved driver, the wall. Zero with NO output
			// = the driver failed to measure; treating that as a wall reported
			// phantom ceilings on healthy endpoints twice.
			if worst <= params.MinTPS {
				for i := range samples {
					if samples[i].TPS > params.MinTPS {
						continue
					}
					diag := captureEndpointDiag(t, kube, tenants[0], fmt.Sprintf("%s-drivers%d-zero", ladder, n), logf)
					if diag != "" {
						s.Set("endpoint_state", strings.ReplaceAll(diag, "\n", " | "))
					}
					if strings.TrimSpace(samples[i].Raw) == "" {
						return fmt.Errorf("driver %d (%s) reported 0 tps and produced no output at all; that is a measurement that did not happen, not a wall [endpoint: %s]",
							i, drivers[i].name, strings.ReplaceAll(diag, "\n", " | "))
					}
					return wallError{fmt.Errorf("driver %d (%s) stopped committing (0.0 tps) while still reporting [endpoint: %s]",
						i, drivers[i].name, strings.ReplaceAll(diag, "\n", " | "))}
				}
			}
			// A fan-in wall and a storage wall look alike from the client side.
			if err := assertStorageAdvances(t, s, kube, env, tenants[0], n, params.ReadTimeout, logf); err != nil {
				// Same split as the driver path: a probe that lost its transport
				// (including a reconnect that failed) is the harness, not a wall.
				if isTransportErr(err) {
					return fmt.Errorf("post-stage storage probe lost its transport: %w", err)
				}
				return wallError{err}
			}
			return nil
		})
		if err != nil {
			// Stage 1 failing means the ladder never started: nothing was
			// measured and there is nothing to report, so that is a test
			// failure. Past stage 1, a driver that errors or hangs under load IS
			// the wall this test hunts for — recording it as the breaking point
			// is the whole point of a stress ladder.
			var wall wallError
			if !errors.As(err, &wall) {
				// The harness could not ask the question. Not a ceiling.
				return fmt.Errorf("%d drivers (ceiling so far %d, %s): %w", n, reached, ladderCurve(aggAt), err)
			}
			if n == 1 {
				return fmt.Errorf("a single driver could not be measured: %w", err)
			}
			stopReason = fmt.Sprintf("%d drivers: broke — %v", n, err)
			brokeAt = n
			break
		}
		reached = n
		aggAt = append(aggAt, core.ChartPoint{X: float64(n), Y: agg})
		if agg > peakAgg {
			peakAgg, peakAt = agg, n
		}
		if n == 1 {
			baseline = agg
			if baseline <= 0 {
				return fmt.Errorf("a single driver produced no throughput; nothing to scale against")
			}
			continue
		}
		// Softer signals are RECORDED and passed through. Stopping here is what
		// made this ladder quit at 2 drivers with the endpoint perfectly
		// healthy, which answers a capacity-planning question, not a stress one.
		if floor := baseline * params.FloorPct / 100; worst < floor && firstStarvedAt == 0 {
			firstStarvedAt = n
			logf("%s: %d drivers: the slowest fell to %.1f tps, below %.0f%% of one alone (%.1f) — continuing",
				ladder, n, worst, params.FloorPct, baseline)
		}
		if prev := aggAt[len(aggAt)-2].Y; agg < prev && firstDropAt == 0 {
			firstDropAt = n
			logf("%s: %d drivers: aggregate fell to %.1f tps from %.1f — past the efficiency ceiling, continuing",
				ladder, n, agg, prev)
		}
		// HARD stops: the endpoint has actually stopped serving useful work.
		if agg <= params.MinTPS {
			stopReason = fmt.Sprintf("%d drivers: throughput collapsed to %.1f tps", n, agg)
			brokeAt = n
			break
		}
		if peakAgg > 0 && agg < peakAgg*params.CollapsePct/100 {
			stopReason = fmt.Sprintf("%d drivers: aggregate collapsed to %.1f tps, under %.0f%% of the %.1f peak at %d drivers",
				n, agg, params.CollapsePct, peakAgg, peakAt)
			brokeAt = n
			break
		}
	}

	return t.Step("report_ceiling", func(s *core.StepHandle) error {
		if reached == 0 {
			return fmt.Errorf("no stage completed")
		}
		s.Set("stop_reason", stopReason)
		s.Set("max_drivers", reached)
		s.Set("peak_aggregate_tps", fmt.Sprintf("%.1f", peakAgg))
		s.Set("peak_at_drivers", peakAt)
		s.Set("broke_at_drivers", brokeAt)
		s.Set("first_aggregate_drop_at", firstDropAt)
		s.Set("first_starved_at", firstStarvedAt)
		// Gated, not merely emitted: a ceiling that collapses from 10 drivers to
		// 2 is the regression this test exists to catch, and an ungated number
		// reports it as a green run with a smaller figure in it.
		gated := []core.Datapoint{
			{Name: "scale." + ladder + ".max_drivers",
				Value: float64(reached), Unit: "drivers", Direction: core.HigherIsBetter},
			{Name: "scale." + ladder + ".aggregate_tps_at_ceiling",
				Value: aggAt[len(aggAt)-1].Y, Unit: "tps", Direction: core.HigherIsBetter},
		}
		info := []core.Datapoint{
			{Name: "scale." + ladder + ".single_driver_tps",
				Value: aggAt[0].Y, Unit: "tps", Direction: core.HigherIsBetter},
		}
		// 1.0 = linear scaling, 0 = the extra drivers bought nothing.
		if reached > 1 {
			gated = append(gated, core.Datapoint{Name: "scale." + ladder + ".efficiency",
				Value: (aggAt[len(aggAt)-1].Y / aggAt[0].Y) / float64(reached), Unit: "x", Direction: core.HigherIsBetter})
		}
		id := identityFromConfig(t.Options, "", frameworkCommit(), loadFieldsScale(ladder, params))
		t.SetPerfIdentity(id.core())
		gateErr := emitAndGateAuto(t, cfg, scaleVersion(t), "perf/"+ladder, gated, info, id, logf)
		t.Chart(core.Chart{
			Title: ladder + ": aggregate throughput vs concurrent drivers", Kind: core.ChartLine,
			XLabel: "drivers", YLabel: "tps",
			Series: []core.ChartSeries{{Name: "aggregate", Points: aggAt}},
		})
		for i, p := range aggAt {
			logf("%s: %d driver(s): aggregate %.1f tps", ladder, i+1, p.Y)
		}
		return gateErr
	})
}
