// Escalation on ONE tenant: every stage adds another driver against the same
// endpoint until an app stops committing, the aggregate stops growing, or the
// storage layer stops advancing. Hitting the ceiling is a PASSING outcome; only
// a stage-1 failure fails the test. perf/scale_depth_tpcc grows the same
// endpoint along the other axis, dataset size.
package perf

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// A MIX: identical clients only ever find one wall.
var appLadderMix = []string{mixSqlTransactions, mixBankTransfer}

// scaleLadders: the registered escalations, one driver per stage.
var scaleLadders = map[string][]string{
	"scale_tpcc":    {mixTPCC},
	"scale_ycsb":    {mixYCSB},
	"scale_pgbench": {mixPgbench},
	"scale_apps":    appLadderMix,
}

func init() {
	for name, mix := range scaleLadders {
		name, mix := name, mix
		core.Register(core.Test{
			Name:        "perf/" + name,
			Suite:       "perf",
			Category:    core.Perf,
			Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
			Tags:        []string{"perf"},
			OwningTeam:  core.TeamControlPlane,
			Description: "Add one more concurrent driver against the SAME endpoint every stage (" + strings.Join(mix, ", ") + "), drive them all for a full window, and stop when a driver stops committing, the aggregate stops growing, or the endpoint's storage layer stops advancing. Reports how much concurrent load one tenant carried.",
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
		MaxDrivers:    c.GetInt("perf.scale.max_drivers", 10),
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

	base := endpointConn(kube.Namespace(), tenants[0].Endpoint.ID, tenants[0].Endpoint.Address)
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
	stopReason := fmt.Sprintf("carried all %d concurrent drivers", params.MaxDrivers)

	for n := 1; n <= params.MaxDrivers; n++ {
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
					return fmt.Errorf("driver %d (%s): %w", i, drivers[i].name, errs[i])
				}
				agg += samples[i].TPS
				if worst < 0 || samples[i].TPS < worst {
					worst = samples[i].TPS
				}
			}
			s.Set("drivers", len(drivers))
			s.Set("aggregate_tps", fmt.Sprintf("%.1f", agg))
			s.Set("slowest_driver_tps", fmt.Sprintf("%.1f", worst))
			// A fan-in wall and a storage wall look alike from the client side.
			return assertStorageAdvances(t, s, kube, env, tenants[0], n, params.ReadTimeout, logf)
		})
		if err != nil {
			// A stage that ERRORED measured nothing, so it cannot be called a
			// ceiling: the first version reported one, passed, and hid a dead
			// port-forward behind "ceiling = 1 driver". Ceilings come from the
			// measurements below; an error is a failure to explain.
			return fmt.Errorf("%d drivers (ceiling so far %d, %s): %w", n, reached, ladderCurve(aggAt), err)
		}
		reached = n
		aggAt = append(aggAt, core.ChartPoint{X: float64(n), Y: agg})
		if n == 1 {
			baseline = agg
			if baseline <= 0 {
				return fmt.Errorf("a single driver produced no throughput; nothing to scale against")
			}
			continue
		}
		if floor := baseline * params.FloorPct / 100; worst < floor {
			stopReason = fmt.Sprintf("%d drivers: the slowest fell to %.1f tps, below %.0f%% of one alone (%.1f)",
				n, worst, params.FloorPct, baseline)
			break
		}
		if prev := aggAt[len(aggAt)-2].Y; agg < prev {
			stopReason = fmt.Sprintf("%d drivers: aggregate fell to %.1f tps from %.1f while adding one",
				n, agg, prev)
			break
		}
	}

	return t.Step("report_ceiling", func(s *core.StepHandle) error {
		if reached == 0 {
			return fmt.Errorf("no stage completed")
		}
		s.Set("stop_reason", stopReason)
		s.Set("max_drivers", reached)
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
		gateErr := emitAndGate(t, cfg, scaleVersion(t), "perf/"+ladder, gated, info, id, logf)
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
