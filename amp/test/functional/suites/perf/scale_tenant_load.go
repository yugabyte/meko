// Escalation on TENANT COUNT: every stage creates one more tenant — its own
// cluster, branch and endpoint — puts a real driver on it, and drives all of
// them at once. perf/scale_tpcc asks how much one endpoint carries; this asks
// what an extra loaded tenant costs everyone already there. Hitting a ceiling
// is a PASSING outcome; only a stage-1 failure fails the test.
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

var tenantLoadLadders = map[string]string{
	"scale_tenant_load_tpcc":  mixTPCC,
	"scale_tenant_load_ycsb":  mixYCSB,
	"scale_tenant_load_sqltx": mixSqlTransactions,
}

func init() {
	for name, workload := range tenantLoadLadders {
		name, workload := name, workload
		core.Register(core.Test{
			Name:        "perf/" + name,
			Suite:       "perf",
			Category:    core.Perf,
			Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
			Tags:        []string{scaleTier},
			OwningTeam:  core.TeamControlPlane,
			Description: "Add one more LOADED tenant (own cluster, own endpoint, own " + workload + " driver) every stage and drive them all for a full window. Reports how tenant throughput and fairness change as the deployment fills up, and where it breaks.",
			Params:      []string{"perf.scale_tenant_load"},
			Timeout:     5 * time.Hour,
			Run: func(t *core.T) error {
				return runTenantLoadLadder(t, name, workload)
			},
		})
	}
}

type tenantLoadParams struct {
	MaxTenants    int
	StageDuration time.Duration
	Budget        time.Duration
	MinTPS        float64
	// FloorPct: a tenant below this share of the solo baseline is starved.
	// Recorded, not a stop — tenants are independent, so starvation here is
	// the isolation finding itself.
	FloorPct    float64
	ReadTimeout time.Duration
	EndpointCPU string
	EndpointMem string
}

func tenantLoadParamsFromConfig(c *core.Config) tenantLoadParams {
	return tenantLoadParams{
		MaxTenants:    c.GetInt("perf.scale_tenant_load.max_tenants", 10),
		StageDuration: c.GetDuration("perf.scale_tenant_load.stage_duration", 6*time.Minute),
		Budget:        c.GetDuration("perf.scale_tenant_load.budget", 4*time.Hour),
		MinTPS:        c.GetFloat("perf.scale_tenant_load.min_tps", 1.0),
		FloorPct:      c.GetFloat("perf.scale_tenant_load.floor_pct", 50),
		ReadTimeout:   c.GetDuration("perf.scale_tenant_load.read_timeout", 2*time.Minute),
		EndpointCPU:   c.GetString("perf.scale_tenant_load.endpoint_cpu", "2"),
		EndpointMem:   c.GetString("perf.scale_tenant_load.endpoint_memory", "4Gi"),
	}
}

func loadFieldsTenantLoad(ladder string, p tenantLoadParams) LoadFields {
	return LoadFields{
		Script:        ladder + "-" + p.EndpointCPU + "-" + p.EndpointMem,
		Clients:       p.MaxTenants,
		MeasurementMS: p.StageDuration.Milliseconds(),
	}
}

// tenantDriver is one loaded tenant: its own endpoint and its own client pod.
type tenantDriver struct {
	ten tenant
	w   Workload
	ex  exec.RemoteExecutor
}

func runTenantLoadLadder(t *core.T, ladder, workload string) error {
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
	params := tenantLoadParamsFromConfig(t.Options)
	cfg := perfFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)
	kube := kubeEnv.AmpKube()
	cl, err := env.AmpController(t.Context)
	if err != nil {
		return err
	}
	poolID := env.AmpPoolID()

	var drivers []tenantDriver
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		for i := range drivers {
			if drivers[i].ten.Conn != nil {
				drivers[i].ten.Conn.Close()
			}
			if _, err := cl.DeleteClusterAndWait(ctx, poolID, drivers[i].ten.ClusterID); err != nil {
				logf("%s: could not delete cluster %s: %v", ladder, drivers[i].ten.ClusterID, err)
			}
		}
	}()

	td := &teardownStack{}
	defer func() {
		tctx, cancel := context.WithTimeout(context.Background(), perfTeardownBudget)
		defer cancel()
		_ = td.run(tctx, logf)
	}()

	addTenant := func(i int) (float64, error) {
		start := time.Now()
		ten, _, err := createTenant(t, cl, env, poolID, i, multitenantParams{
			SeedRows:       1,
			EndpointCPU:    params.EndpointCPU,
			EndpointMemory: params.EndpointMem,
		})
		if err != nil {
			return 0, fmt.Errorf("tenant %d: %w%s", i, err, schedulingHint(t, logf))
		}
		createMS := msSince(start)
		drivers = append(drivers, tenantDriver{ten: ten})
		if i == 0 {
			if err := ensureProbeTable(t.Context, ten.Conn); err != nil {
				return 0, fmt.Errorf("tenant 0: create the probe table: %w", err)
			}
		}

		conn := endpointConn(kube.Namespace(), ten.Endpoint.ID, ten.Endpoint.Address)
		podName := fmt.Sprintf("ampqa-tl-%s-%d", t.ResourceSuffix(), i)
		ex, err := startPerfClientPod(t.Context, kube, attachSink{t: t, logf: logf}, td,
			podName, cfg.ClientImage, cfg.ClientPullSecret)
		if err != nil {
			return 0, fmt.Errorf("tenant %d client pod: %w", i, err)
		}
		jarPath, err := deliverDriversOnce(t.Context, t, cfg, []string{workload}, []exec.RemoteExecutor{ex})
		if err != nil {
			return 0, fmt.Errorf("tenant %d artifacts: %w", i, err)
		}
		w, err := mixWorkloadFor(workload, cfg, t.Options, conn, mixDeps{
			jarPath: jarPath,
			marker:  fmt.Sprintf("tl-%s-%d", t.ResourceSuffix(), i),
			logf:    logf,
		})
		if err != nil {
			return 0, fmt.Errorf("tenant %d: %w", i, err)
		}
		if err := w.Prepare(t.Context, ex); err != nil {
			return 0, fmt.Errorf("tenant %d prepare: %w", i, err)
		}
		drivers[i].w, drivers[i].ex = w, ex
		return createMS, nil
	}

	var aggAt, tenant0At []core.ChartPoint
	var reached int
	baseline := 0.0
	peakAgg, peakAt := 0.0, 0
	firstStarvedAt, brokeAt := 0, 0
	stopReason := fmt.Sprintf("carried all %d loaded tenants without breaking", params.MaxTenants)
	deadline := time.Now().Add(params.Budget)

	for n := 1; n <= params.MaxTenants; n++ {
		if n > 1 && time.Now().After(deadline) {
			stopReason = fmt.Sprintf("%d tenants: ran out of the %s ladder budget before anything broke", n-1, params.Budget)
			break
		}
		stage := fmt.Sprintf("stage_%02d_tenants%d", n, n)
		var agg, worst, first float64
		err := t.Step(stage, func(s *core.StepHandle) error {
			createMS, err := addTenant(n - 1)
			if err != nil {
				return err
			}
			s.Set("tenant_create_ms", fmt.Sprintf("%.0f", createMS))

			samples := make([]Sample, len(drivers))
			errs := make([]error, len(drivers))
			var wg sync.WaitGroup
			for i := range drivers {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					samples[i], errs[i] = drivers[i].w.RunOnce(t.Context, drivers[i].ex,
						RunParams{Duration: params.StageDuration, SampleInterval: time.Second})
					recordDriverRun(t, logf, fmt.Sprintf("tenant-%d", i), stage, samples[i], params.StageDuration)
				}(i)
			}
			wg.Wait()

			worst = -1
			for i := range drivers {
				if errs[i] != nil {
					if isTransportErr(errs[i]) {
						return fmt.Errorf("tenant %d: lost its exec transport: %w", i, errs[i])
					}
					diag := captureEndpointDiag(t, kube, drivers[i].ten, fmt.Sprintf("%s-tenants%d", ladder, n), logf)
					if diag != "" {
						s.Set("endpoint_state", strings.ReplaceAll(diag, "\n", " | "))
					}
					return wallError{fmt.Errorf("tenant %d stopped serving [endpoint: %s]: %w",
						i, strings.ReplaceAll(diag, "\n", " | "), errs[i])}
				}
				agg += samples[i].TPS
				if worst < 0 || samples[i].TPS < worst {
					worst = samples[i].TPS
				}
			}
			first = samples[0].TPS
			s.Set("tenants", len(drivers))
			s.Set("aggregate_tps", fmt.Sprintf("%.1f", agg))
			s.Set("slowest_tenant_tps", fmt.Sprintf("%.1f", worst))
			s.Set("tenant0_tps", fmt.Sprintf("%.1f", first))
			// A zero with output is a starved tenant, the wall this test hunts.
			// A zero with NO output is the driver failing to measure: recording
			// it as a wall once reported "broke at 2 tenants" on an idle
			// deployment whose second driver never produced a single line.
			if worst <= params.MinTPS {
				for i := range samples {
					if samples[i].TPS > params.MinTPS {
						continue
					}
					diag := captureEndpointDiag(t, kube, drivers[i].ten, fmt.Sprintf("%s-tenants%d-zero", ladder, n), logf)
					if diag != "" {
						s.Set("endpoint_state", strings.ReplaceAll(diag, "\n", " | "))
					}
					if strings.TrimSpace(samples[i].Raw) == "" {
						return fmt.Errorf("tenant %d reported 0 tps and its driver produced no output at all; that is a measurement that did not happen, not a wall [endpoint: %s]",
							i, strings.ReplaceAll(diag, "\n", " | "))
					}
					return wallError{fmt.Errorf("tenant %d stopped committing (0.0 tps) while its driver kept reporting [endpoint: %s]",
						i, strings.ReplaceAll(diag, "\n", " | "))}
				}
			}
			if err := assertStorageAdvances(t, s, kube, env, drivers[0].ten, n, params.ReadTimeout, logf); err != nil {
				return wallError{err}
			}
			return nil
		})
		if err != nil {
			var wall wallError
			if !errors.As(err, &wall) {
				return fmt.Errorf("%d tenants (ceiling so far %d, %s): %w", n, reached, ladderCurve(aggAt), err)
			}
			if n == 1 {
				return fmt.Errorf("a single loaded tenant could not be measured: %w", err)
			}
			stopReason = fmt.Sprintf("%d tenants: broke — %v", n, err)
			brokeAt = n
			break
		}
		reached = n
		aggAt = append(aggAt, core.ChartPoint{X: float64(n), Y: agg})
		tenant0At = append(tenant0At, core.ChartPoint{X: float64(n), Y: first})
		if agg > peakAgg {
			peakAgg, peakAt = agg, n
		}
		if n == 1 {
			baseline = agg
			if baseline <= 0 {
				return fmt.Errorf("a single loaded tenant produced no throughput; nothing to scale against")
			}
			continue
		}
		if floor := baseline * params.FloorPct / 100; worst < floor && firstStarvedAt == 0 {
			firstStarvedAt = n
			logf("%s: %d tenants: the slowest fell to %.1f tps, below %.0f%% of one alone (%.1f) — continuing",
				ladder, n, worst, params.FloorPct, baseline)
		}
	}

	return t.Step("report_ceiling", func(s *core.StepHandle) error {
		if reached == 0 {
			return fmt.Errorf("no stage completed")
		}
		s.Set("stop_reason", stopReason)
		s.Set("max_tenants", reached)
		s.Set("peak_aggregate_tps", fmt.Sprintf("%.1f", peakAgg))
		s.Set("peak_at_tenants", peakAt)
		s.Set("broke_at_tenants", brokeAt)
		s.Set("first_starved_at", firstStarvedAt)
		s.Set("endpoint_size", params.EndpointCPU+" / "+params.EndpointMem)

		gated := []core.Datapoint{
			{Name: "scale." + ladder + ".max_tenants",
				Value: float64(reached), Unit: "tenants", Direction: core.HigherIsBetter},
			{Name: "scale." + ladder + ".aggregate_tps_at_ceiling",
				Value: aggAt[len(aggAt)-1].Y, Unit: "tps", Direction: core.HigherIsBetter},
		}
		info := []core.Datapoint{
			{Name: "scale." + ladder + ".single_tenant_tps",
				Value: aggAt[0].Y, Unit: "tps", Direction: core.HigherIsBetter},
		}
		if reached > 1 {
			// 1.0 = each new loaded tenant costs the others nothing.
			gated = append(gated, core.Datapoint{Name: "scale." + ladder + ".efficiency",
				Value: (aggAt[len(aggAt)-1].Y / aggAt[0].Y) / float64(reached), Unit: "x", Direction: core.HigherIsBetter})
			info = append(info, core.Datapoint{Name: "scale." + ladder + ".tenant0_retention",
				Value: tenant0At[len(tenant0At)-1].Y / baseline, Unit: "x", Direction: core.HigherIsBetter})
		}
		id := identityFromConfig(t.Options, "", frameworkCommit(), loadFieldsTenantLoad(ladder, params))
		t.SetPerfIdentity(id.core())
		gateErr := emitAndGate(t, cfg, scaleVersion(t), "perf/"+ladder, gated, info, id, logf)
		t.Chart(core.Chart{
			Title: ladder + ": throughput vs loaded tenants (" + params.EndpointCPU + "/" + params.EndpointMem + ")",
			Kind:  core.ChartLine, XLabel: "tenants", YLabel: "tps",
			Series: []core.ChartSeries{
				{Name: "aggregate", Points: aggAt},
				{Name: "tenant 0", Points: tenant0At},
			},
		})
		for i, p := range aggAt {
			logf("%s: %d tenant(s): aggregate %.1f tps, tenant0 %.1f tps", ladder, i+1, p.Y, tenant0At[i].Y)
		}
		return gateErr
	})
}
