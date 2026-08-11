// Multitenancy performance — the number amp exists to be good at.
//
// Being multi-tenant is the product's whole proposition, so "does it still
// perform when tenants share a deployment?" needs an answer with history rather
// than an impression. The arrangement measured here is the one amp is sold as:
// several tenants, each its own cluster and endpoint, each driven by a REAL
// benchmark client of its own. By default they are three different KINDS of
// tenant: a TPC-C shop (benchbase), a bank moving money between accounts and a
// transactional writer (both yb-stress-sample-apps, the workloads the stress
// team already runs). YCSB, TPC-H, CH-benCHmark, pgbench, sysbench and
// SqlDataLoadWithDDL are selectable too. Every driver runs in its own client pod
// inside the cluster, because a shared pod would make client CPU the thing
// under test, and each connects to its own tenant's endpoint Service.
//
// Each workload is measured twice: SOLO, with the deployment to itself, and
// MIXED, with every tenant's driver running at once. What that yields:
//
//   - interference, per workload — its solo throughput over its mixed
//     throughput. Comparing a workload only against ITSELF is what makes the
//     number meaningful when tenants run different benchmarks whose absolute
//     figures share no unit. This is the gate.
//   - the solo and mixed figures themselves, per workload, plus the aggregate,
//     recorded so the portal charts them per version.
//   - control-plane operation latency — cluster create, endpoint create
//     (median and worst) and time-to-first-successful-SQL. "Endpoint creation
//     is taking too long" is a recurring report with no data behind it; this
//     makes it a tracked number.
//   - fork detail — a branch off an empty parent versus off a seeded one, so
//     copy-on-write behaviour is visible instead of assumed.
//
// Only the intra-run interference ratio gates: solo and mixed are measured
// minutes apart on the same hardware by the same driver, so the ratio means
// something with no stored baseline, while an absolute throughput does not.
package perf

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const testNameMultitenant = "perf/multitenant"

// ampMultitenantEnv is the control-plane surface this test needs: somewhere to
// create tenants and a way to reach each one's endpoint. Declared structurally
// so any environment exposing a standing control plane satisfies it.
type ampMultitenantEnv interface {
	AmpController(ctx context.Context) (*controller.Client, error)
	AmpPoolID() string
	ConnectEndpoint(ctx context.Context, ep controller.Endpoint) (*pg.Client, error)
}

// multitenantParams are the knobs, all under perf.multitenant.
type multitenantParams struct {
	// Tenants is the widest point of the scale curve.
	Tenants int
	// Workloads is the driver mix, one per tenant (cycled if there are more
	// tenants than entries).
	Workloads []string
	// MaxInterference gates the mixed phase: a workload's solo throughput over
	// its throughput while every tenant runs. 2.0 means "may halve".
	MaxInterference float64
	// SeedRows is what each tenant writes before being measured, so reads have
	// something to find and the pageserver has something to serve.
	SeedRows int
	// ForkSeedRows is the "large" parent for the fork-versus-size measurement.
	ForkSeedRows int
	// EndpointCPU/EndpointMemory size each tenant's compute. They are set
	// explicitly and small: tenants must FIT, and an unsized endpoint takes the
	// product's default reservation, which is generous enough that a handful of
	// them exhaust a shared test cluster and the deploys then time out.
	EndpointCPU    string
	EndpointMemory string
	// MeasurementDuration is how long ONE iteration of a workload runs. A minute
	// is too short to characterise a tenant: TPC-C spends the first windows
	// ramping connections and warming caches, so a 60s window is mostly warm-up.
	// 3m clears that ramp and, across Iterations, kept the coefficient of
	// variation inside a few percent — while cutting ~30 minutes off a run that
	// measures each of five workloads alone before running them together.
	MeasurementDuration time.Duration
	// Iterations is how many times each condition is measured. Reported values
	// are the median across iterations, so one unlucky run cannot set the
	// number a version comparison is made against.
	Iterations int
}

func multitenantParamsFromConfig(c *core.Config) multitenantParams {
	return multitenantParams{
		// Three tenants per workload in the default mix. One each proved the shape
		// but not the point: with five near-idle tenants the shared storage sat at
		// ~1% CPU and every interference ratio came back 1.0x — the deployment was
		// never contended, so the measurement said nothing about multitenancy.
		// Solo now costs one baseline per WORKLOAD, so the population can grow
		// without the serial phase growing with it.
		Tenants:         c.GetInt("perf.multitenant.tenants", 3*len(defaultWorkloadMix)),
		Workloads:       c.GetStringSlice("perf.multitenant.workloads", defaultWorkloadMix),
		MaxInterference: c.GetFloat("perf.multitenant.max_interference", 4),
		SeedRows:        c.GetInt("perf.multitenant.seed_rows", 2000),
		ForkSeedRows:    c.GetInt("perf.multitenant.fork_seed_rows", 20000),
		// 4 cores and 8Gi, not the half-core the stress tier gives its light
		// writers. A benchmark tenant loads a dataset and then serves a driver
		// flat out. At 500m the compute sat pegged at its own limit: TPC-C's load
		// crawled for half an hour, and the compute could not answer its 1-second
		// liveness probe, so kubelet killed it and the driver saw its connection
		// close mid-benchmark. At 2Gi it was the MEMORY limit: TPC-C's load
		// crossed it (OOMKilled, exit 137, caught live at 1790Mi minutes before),
		// postgres restarted, and prepare took 40 minutes instead of 5–8 while
		// the loader re-drove the data.
		//
		// 2 cores / 4Gi is what a default tenant gets, and it is bounded by the
		// CLUSTER, not by what the benchmark would enjoy: the endpoint requests
		// 40Gi of ephemeral storage against ~43.8Gi allocatable, so it is one
		// endpoint per node whatever the CPU, and the autoprovisioned shape is
		// e2-standard-4 with 3920m allocatable — a request of 4 cores cannot fit
		// one at all and waits for a bigger node per tenant. Asking 15 tenants for
		// 4 cores each got 0/26 nodes and burned the full 35-minute helm budget.
		// Raise these per run when a shape is worth the nodes it costs.
		EndpointCPU:         c.GetString("perf.multitenant.endpoint_cpu", "2"),
		EndpointMemory:      c.GetString("perf.multitenant.endpoint_memory", "4Gi"),
		MeasurementDuration: c.GetDuration("perf.multitenant.measurement_duration", 3*time.Minute),
		Iterations:          c.GetInt("perf.multitenant.iterations", 3),
	}
}

func init() {
	core.Register(core.Test{
		Name:        testNameMultitenant,
		Suite:       "perf",
		Category:    core.Perf,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"perf", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Multitenancy performance on one control plane: each tenant is its own cluster + endpoint driven by its own real benchmark client (TPC-C, SqlBankTransfer, SqlTransactions by default), measured alone and then with every tenant running. Reports per-workload throughput and latency in both conditions, the interference ratio between them (the gate), control-plane op latencies and fork time against an empty versus a seeded parent.",
		Params:      []string{"perf.multitenant"},
		// Setup (tenant creation, client pods, TPC-C's load), then Iterations solo
		// runs per workload SERIALLY — solo means solo — and Iterations mixed runs
		// concurrently. At the defaults (5 workloads × 3 × 5m, plus 3 × 5m mixed)
		// that is ~90 minutes of measurement alone, and the measured phases must
		// never be the part that runs out of budget.
		Timeout: 4 * time.Hour,
		Run:     runMultitenant,
	})
}

// tenant is one tenant: its own cluster, its own endpoint, its own connection.
type tenant struct {
	Index     int
	ClusterID string
	BranchID  string
	Endpoint  controller.Endpoint
	Conn      *pg.Client
	Table     string
}

func runMultitenant(t *core.T) error {
	env, ok := t.Env.(ampMultitenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s); a single endpoint cannot host tenants")
		return nil
	}
	params := multitenantParamsFromConfig(t.Options)
	if params.Tenants < 2 {
		t.Skipf("perf.multitenant.tenants = %d: multitenancy needs at least 2", params.Tenants)
		return nil
	}
	cfg := perfFromConfig(t.Options)
	bc := benchConfigFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)
	cl, err := env.AmpController(t.Context)
	if err != nil {
		return err
	}
	poolID := env.AmpPoolID()

	var tenants []tenant
	defer func() {
		// Teardown on its own deadline: the test's context may already be done.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		for i := len(tenants) - 1; i >= 0; i-- {
			if tenants[i].Conn != nil {
				tenants[i].Conn.Close()
			}
			if _, err := cl.DeleteClusterAndWait(ctx, poolID, tenants[i].ClusterID); err != nil {
				logf("multitenant: could not delete tenant %d's cluster %s: %v", i, tenants[i].ClusterID, err)
			}
		}
	}()

	if err := t.Step("create_tenants", func(s *core.StepHandle) error {
		var clusterMS, endpointMS, firstSQLMS []float64
		// Concurrent, and not for speed alone: an endpoint pod requests 40Gi of
		// ephemeral storage against ~43.8Gi allocatable per node, so exactly one
		// lands per node and every tenant costs a fresh autoscaled node (~15 min
		// on amp-dev). Serially that is Tenants × 15 min and the first endpoint's
		// own helm --wait budget is what expires. Created together, the nodes are
		// provisioned in one wave.
		made := make([]tenant, params.Tenants)
		times := make([]tenantTimings, params.Tenants)
		errs := make([]error, params.Tenants)
		var wg sync.WaitGroup
		for i := 0; i < params.Tenants; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				made[i], times[i], errs[i] = createTenant(t, cl, env, poolID, i, params)
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				return fmt.Errorf("tenant %d: %w%s", i, err, schedulingHint(t, logf))
			}
			tenants = append(tenants, made[i])
			clusterMS = append(clusterMS, times[i].clusterMS)
			endpointMS = append(endpointMS, times[i].endpointMS)
			firstSQLMS = append(firstSQLMS, times[i].firstSQLMS)
		}
		s.Set("tenants", len(tenants))
		t.Datapoint(core.Datapoint{Name: "multitenant.cluster_create_ms", Value: median(clusterMS), Unit: "ms", Direction: core.LowerIsBetter})
		t.Datapoint(core.Datapoint{Name: "multitenant.endpoint_create_ms", Value: median(endpointMS), Unit: "ms", Direction: core.LowerIsBetter})
		t.Datapoint(core.Datapoint{Name: "multitenant.first_sql_ms", Value: median(firstSQLMS), Unit: "ms", Direction: core.LowerIsBetter})
		// The widest create is what a user waiting on a fresh tenant actually
		// experiences when the deployment is already busy.
		t.Datapoint(core.Datapoint{Name: "multitenant.endpoint_create_max_ms", Value: maxOf(endpointMS), Unit: "ms", Direction: core.LowerIsBetter})
		t.Chart(opLatencyChart(median(clusterMS), median(endpointMS), maxOf(endpointMS), median(firstSQLMS)))
		return nil
	}); err != nil {
		return err
	}

	var clients []tenantClient
	td := &teardownStack{}
	defer func() {
		tctx, cancel := context.WithTimeout(context.Background(), perfTeardownBudget)
		defer cancel()
		_ = td.run(tctx, logf)
	}()

	if err := t.Step("start_tenant_clients", func(s *core.StepHandle) error {
		kubeEnv, ok := t.Env.(interface{ AmpKube() *k8s.Kubectl })
		if !ok || kubeEnv.AmpKube() == nil {
			return fmt.Errorf("real benchmark clients need a kube context for the control-plane namespace")
		}
		var err error
		clients, err = startTenantClients(t.Context, t, cfg, kubeEnv.AmpKube(), attachSink{t: t, logf: logf},
			td, tenants, params.Workloads)
		if err != nil {
			return err
		}
		for _, c := range clients {
			s.Set(fmt.Sprintf("tenant_%d_workload", c.TenantIndex), c.WorkloadName)
		}
		return nil
	}); err != nil {
		return err
	}

	// A tenant whose driver cannot be prepared or measured is DROPPED, not fatal.
	// Every number this test produces arrives in its last phases, so failing the
	// whole run over one bad driver throws away the measurements the others were
	// about to give — which is how this test kept reporting nothing but its four
	// control-plane timings.
	var dropped []string
	if err := t.Step("prepare_workloads", func(s *core.StepHandle) error {
		// Schema + initial load, once per tenant. It is not timed, and TPC-C's
		// load alone runs for minutes, so the tenants prepare concurrently — done
		// serially it consumed the budget the measured phases needed.
		prepErrs := make([]error, len(clients))
		prepMS := make([]float64, len(clients))
		var wg sync.WaitGroup
		for i := range clients {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				start := time.Now()
				prepErrs[i] = clients[i].Workload.Prepare(t.Context, clients[i].Exec)
				prepMS[i] = msSince(start)
			}(i)
		}
		wg.Wait()
		// How long a tenant takes to become usable is a product number in its own
		// right — loading TPC-C through an amp endpoint took half an hour when the
		// compute was throttled. Recorded per workload so it is visible and
		// tracked, instead of hiding inside a setup phase nobody charts.
		for i, c := range clients {
			// A no-op Prepare is not a 0 ms load: the sample-app drivers do their
			// own DDL and seeding on the first operation instead, so charting a 0
			// for them would read as "instant" next to TPC-C's five minutes.
			if prepErrs[i] != nil || prepMS[i] <= 0 {
				continue
			}
			s.Set(fmt.Sprintf("%s_prepare_s", c.WorkloadName), fmt.Sprintf("%.0f", prepMS[i]/1000))
			t.Datapoint(core.Datapoint{
				Name: fmt.Sprintf("multitenant.%s.prepare_ms", c.WorkloadName), Value: prepMS[i],
				Unit: "ms", Direction: core.LowerIsBetter})
		}
		var kept []tenantClient
		for i, c := range clients {
			if err := prepErrs[i]; err != nil {
				logf("multitenant: dropping tenant %d (%s): prepare failed: %v", c.TenantIndex, c.WorkloadName, err)
				dropped = append(dropped, fmt.Sprintf("%s (prepare: %v)", c.WorkloadName, err))
				continue
			}
			kept = append(kept, c)
		}
		clients = kept
		s.Set("prepared", len(clients))
		if len(dropped) > 0 {
			s.Set("dropped", strings.Join(dropped, "; "))
		}
		if len(clients) == 0 {
			return fmt.Errorf("no tenant workload could be prepared: %s", strings.Join(dropped, "; "))
		}
		return nil
	}); err != nil {
		return err
	}

	solo := map[int]Sample{}
	if err := t.Step("measure_solo", func(s *core.StepHandle) error {
		// One tenant at a time, so each workload's number is what it gets with the
		// deployment to itself. This is the reference every mixed number is read
		// against, measured in the same run on the same hardware.
		// One solo baseline per DISTINCT workload, not per tenant: solo is serial by
		// definition, so measuring every tenant alone makes the phase grow with the
		// tenant count (20 tenants x 3 iterations x 3m = 3 hours) while producing
		// N copies of the same number — two tpcc tenants alone on the deployment
		// measure the same thing. Every tenant of that workload is then read
		// against its workload's baseline in the mixed phase, which is where the
		// population size actually matters.
		var kept []tenantClient
		soloDone := map[string]Sample{}
		for _, c := range clients {
			if base, ok := soloDone[c.WorkloadName]; ok {
				kept = append(kept, c)
				solo[c.TenantIndex] = base
				continue
			}
			sample, cv, err := measureRepeated(t, logf, c, "solo", params, bc.SampleInterval)
			if err != nil {
				logf("multitenant: dropping tenant %d (%s): solo run failed: %v", c.TenantIndex, c.WorkloadName, err)
				dropped = append(dropped, fmt.Sprintf("%s (solo: %v)", c.WorkloadName, err))
				continue
			}
			kept = append(kept, c)
			solo[c.TenantIndex] = sample
			soloDone[c.WorkloadName] = sample
			s.Set(fmt.Sprintf("%s_solo_tps", c.WorkloadName), fmt.Sprintf("%.1f", sample.TPS))
			t.Datapoint(core.Datapoint{
				Name: fmt.Sprintf("multitenant.%s.solo_tps", c.WorkloadName), Value: sample.TPS,
				Unit: "tps", Direction: core.HigherIsBetter})
			t.Datapoint(core.Datapoint{
				Name: fmt.Sprintf("multitenant.%s.solo_latency_avg_ms", c.WorkloadName), Value: sample.LatencyAvgMS,
				Unit: "ms", Direction: core.LowerIsBetter})
			// How far the iterations spread. A wide spread means the medians above
			// are not a stable basis for comparing versions, and says so in the
			// report instead of leaving it to be guessed.
			t.Datapoint(core.Datapoint{
				Name: fmt.Sprintf("multitenant.%s.solo_tps_cv", c.WorkloadName), Value: cv,
				Unit: "%", Direction: core.LowerIsBetter})
		}
		clients = kept
		if len(clients) == 0 {
			return fmt.Errorf("no tenant workload produced a solo measurement: %s", strings.Join(dropped, "; "))
		}
		return nil
	}); err != nil {
		return err
	}

	mixed := map[int]Sample{}
	mixedCV := map[int]float64{}
	if err := t.Step("measure_mixed", func(s *core.StepHandle) error {
		// Every tenant's driver at once — the arrangement the product is sold as.
		var wg sync.WaitGroup
		var mu sync.Mutex
		errs := make([]error, len(clients))
		for i := range clients {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				c := clients[idx]
				sample, cv, err := measureRepeated(t, logf, c, "mixed", params, bc.SampleInterval)
				mu.Lock()
				mixedCV[c.TenantIndex] = cv
				defer mu.Unlock()
				if err != nil {
					errs[idx] = fmt.Errorf("tenant %d (%s): mixed run: %w", c.TenantIndex, c.WorkloadName, err)
					return
				}
				mixed[c.TenantIndex] = sample
			}(i)
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			logf("multitenant: some tenants failed their mixed run: %v", err)
		}
		var measured []tenantClient
		for _, c := range clients {
			if _, ok := mixed[c.TenantIndex]; ok {
				measured = append(measured, c)
			}
		}
		clients = measured
		if len(clients) == 0 {
			return fmt.Errorf("no tenant workload produced a mixed measurement: %w", errors.Join(errs...))
		}
		var total float64
		for _, c := range clients {
			sample := mixed[c.TenantIndex]
			total += sample.TPS
			s.Set(fmt.Sprintf("%s_mixed_tps", c.WorkloadName), fmt.Sprintf("%.1f", sample.TPS))
			t.Datapoint(core.Datapoint{
				Name: fmt.Sprintf("multitenant.%s.mixed_tps", c.WorkloadName), Value: sample.TPS,
				Unit: "tps", Direction: core.HigherIsBetter})
			t.Datapoint(core.Datapoint{
				Name: fmt.Sprintf("multitenant.%s.mixed_latency_avg_ms", c.WorkloadName), Value: sample.LatencyAvgMS,
				Unit: "ms", Direction: core.LowerIsBetter})
			t.Datapoint(core.Datapoint{
				Name: fmt.Sprintf("multitenant.%s.mixed_tps_cv", c.WorkloadName), Value: mixedCV[c.TenantIndex],
				Unit: "%", Direction: core.LowerIsBetter})
		}
		t.Datapoint(core.Datapoint{Name: "multitenant.aggregate_tps", Value: total, Unit: "tps", Direction: core.HigherIsBetter})
		t.Chart(throughputChart(clients, solo, mixed))
		for _, chart := range timelineCharts(clients, solo, mixed) {
			t.Chart(chart)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("gate_interference", func(s *core.StepHandle) error {
		if len(dropped) > 0 {
			s.Set("dropped", strings.Join(dropped, "; "))
		}
		// Interference is solo over mixed, per workload: how much of its own
		// throughput a tenant loses to its neighbours. Comparing a workload only
		// against ITSELF is what makes this meaningful when the tenants run
		// different benchmarks whose absolute numbers share no unit.
		var worst float64
		var worstName string
		for _, c := range clients {
			soloTPS, mixedTPS := solo[c.TenantIndex].TPS, mixed[c.TenantIndex].TPS
			if soloTPS <= 0 || mixedTPS <= 0 {
				return fmt.Errorf("tenant %d (%s) produced no throughput (solo %.2f, mixed %.2f), so interference cannot be computed",
					c.TenantIndex, c.WorkloadName, soloTPS, mixedTPS)
			}
			ratio := soloTPS / mixedTPS
			s.Set(fmt.Sprintf("%s_interference", c.WorkloadName), fmt.Sprintf("%.2fx", ratio))
			t.Datapoint(core.Datapoint{
				Name: fmt.Sprintf("multitenant.%s.interference", c.WorkloadName), Value: ratio,
				Unit: "x", Direction: core.LowerIsBetter})
			if ratio > worst {
				worst, worstName = ratio, c.WorkloadName
			}
		}
		t.Datapoint(core.Datapoint{Name: "multitenant.worst_interference", Value: worst, Unit: "x", Direction: core.LowerIsBetter})
		t.Chart(interferenceChart(clients, solo, mixed))
		// One surviving tenant is not a multitenancy measurement: its "mixed" run
		// had no neighbours, so the interference number compares it against
		// itself and lands at 1.0 no matter how bad isolation is. Passing on that
		// would be a green run that measured nothing.
		if len(clients) < 2 {
			return fmt.Errorf("only %d tenant workload produced measurements (%s), so interference compares it against itself and means nothing — dropped: %s",
				len(clients), clients[0].WorkloadName, strings.Join(dropped, "; "))
		}
		s.Set("worst", fmt.Sprintf("%s at %.2fx", worstName, worst))
		if worst > params.MaxInterference {
			return fmt.Errorf("%s lost %.2fx of its throughput to its neighbours, over the %.2fx allowed by perf.multitenant.max_interference",
				worstName, worst, params.MaxInterference)
		}
		return nil
	}); err != nil {
		return err
	}

	return t.Step("fork_latency", func(s *core.StepHandle) error {
		// A minimal parent: a fresh tenant's default branch carrying its schema and
		// one row, against which the seeded parent below is 20k.
		//
		// Not literally empty. A branch nobody has written to has no determinable
		// fork point even with an endpoint deployed — the controller rejects the
		// fork with `cannot determine a fork point for the parent branch (deploy an
		// endpoint on it first, or pass fork_point_lsn)`, which cost a 107-minute
		// run its last step after every measurement had passed. stress/branch_churn
		// already worked this out for itself: "the parent gets one writer + a seed
		// row before any lane forks from it".
		minimal, _, err := createTenant(t, cl, env, poolID, len(tenants), params)
		if err != nil {
			return err
		}
		tenants = append(tenants, minimal)
		if err := seedTenant(t, &tenants[len(tenants)-1], forkMinimalRows); err != nil {
			return fmt.Errorf("seed the minimal fork parent: %w", err)
		}

		emptyMS, err := timeFork(t, cl, poolID, minimal.ClusterID, minimal.BranchID, "fork-empty")
		if err != nil {
			return err
		}
		if err := seedTenant(t, &tenants[len(tenants)-1], params.ForkSeedRows); err != nil {
			return err
		}
		seededMS, err := timeFork(t, cl, poolID, minimal.ClusterID, minimal.BranchID, "fork-seeded")
		if err != nil {
			return err
		}
		s.Set("fork_empty_ms", fmt.Sprintf("%.0f", emptyMS))
		s.Set("fork_seeded_ms", fmt.Sprintf("%.0f", seededMS))
		s.Set("seed_rows", params.ForkSeedRows)
		t.Datapoint(core.Datapoint{Name: "multitenant.fork_empty_ms", Value: emptyMS, Unit: "ms", Direction: core.LowerIsBetter})
		t.Datapoint(core.Datapoint{Name: "multitenant.fork_seeded_ms", Value: seededMS, Unit: "ms", Direction: core.LowerIsBetter})
		// Copy-on-write means forking should not scale with the parent's size;
		// recorded rather than gated, because one data size proves nothing on its
		// own — the history is what will show it drifting.
		t.Datapoint(core.Datapoint{Name: "multitenant.fork_size_ratio", Value: seededMS / emptyMS, Unit: "x", Direction: core.LowerIsBetter})
		t.Chart(forkChart(emptyMS, seededMS, params.ForkSeedRows))
		return nil
	})
}

// forkMinimalRows is the smallest parent a fork can be timed against: enough
// that the branch HAS a fork point, few enough that its size is not what the
// comparison against ForkSeedRows measures.
const forkMinimalRows = 1

// recordDriverRun keeps each driver's own output and says how much of the
// requested window its series actually covers. pgbench's line stopped at 25s of
// a 60s run with no way to tell whether the driver quit early or its progress
// output was lost, because nothing kept the raw text.
func recordDriverRun(t *core.T, logf func(string, ...any), workload, phase string, s Sample, want time.Duration) {
	if s.Raw != "" {
		t.AttachBytes(fmt.Sprintf("driver-%s-%s.log", workload, phase), "text/plain", []byte(s.Raw))
	}
	if n := len(s.Series); n > 0 {
		logf("multitenant: %s %s series covers %.0fs of the %.0fs window in %d points",
			workload, phase, s.Series[n-1].ElapsedSec, want.Seconds(), n)
	} else {
		logf("multitenant: %s %s produced no time series", workload, phase)
	}
}

// measureRepeated runs one tenant's driver params.Iterations times and returns
// the median iteration's numbers, plus the coefficient of variation of
// throughput across iterations as a percentage.
//
// The returned Sample keeps the SERIES of the iteration whose throughput is the
// median, not an average of all of them: iterations do not share a time base, so
// averaging their points smears real dips into a flat line that never happened.
func measureRepeated(t *core.T, logf func(string, ...any), c tenantClient, phase string,
	params multitenantParams, sampleInterval time.Duration) (Sample, float64, error) {
	iters := params.Iterations
	if iters < 1 {
		iters = 1
	}
	var samples []Sample
	var lastErr error
	for i := 0; i < iters; i++ {
		s, err := c.Workload.RunOnce(t.Context, c.Exec,
			RunParams{Duration: params.MeasurementDuration, SampleInterval: sampleInterval})
		recordDriverRun(t, logf, c.WorkloadName, fmt.Sprintf("%s-%d", phase, i+1), s, params.MeasurementDuration)
		if err != nil {
			lastErr = err
			logf("multitenant: %s %s iteration %d/%d failed: %v", c.WorkloadName, phase, i+1, iters, err)
			continue
		}
		samples = append(samples, s)
	}
	if len(samples) == 0 {
		return Sample{}, 0, lastErr
	}
	tps := make([]float64, len(samples))
	lat := make([]float64, len(samples))
	for i, s := range samples {
		tps[i], lat[i] = s.TPS, s.LatencyAvgMS
	}
	medTPS := median(tps)
	rep := samples[0]
	for _, s := range samples {
		if math.Abs(s.TPS-medTPS) < math.Abs(rep.TPS-medTPS) {
			rep = s
		}
	}
	out := Sample{TPS: medTPS, LatencyAvgMS: median(lat), Series: rep.Series, Raw: rep.Raw}
	logf("multitenant: %s %s median of %d iterations: %.1f tps, %.2f ms (cv %.1f%%)",
		c.WorkloadName, phase, len(samples), out.TPS, out.LatencyAvgMS, cvPct(tps))
	return out, cvPct(tps), nil
}

// cvPct is the coefficient of variation as a percentage: 0 means every
// iteration agreed.
func cvPct(vs []float64) float64 {
	if len(vs) < 2 {
		return 0
	}
	var sum float64
	for _, v := range vs {
		sum += v
	}
	mean := sum / float64(len(vs))
	if mean == 0 {
		return 0
	}
	var sq float64
	for _, v := range vs {
		sq += (v - mean) * (v - mean)
	}
	return math.Sqrt(sq/float64(len(vs)-1)) / mean * 100
}

// schedulingHint appends why the cluster could not place a pod, when that is
// why a create failed. An endpoint requests 40Gi of ephemeral storage against
// ~43.8Gi allocatable per node, so exactly one lands per node and every tenant
// needs a fresh autoscaled one — and node auto-provisioning is capped by a
// cluster-wide CPU quota. When that cap is hit the pod simply stays Pending and
// the only thing the controller reports is "helm --wait: context deadline
// exceeded", which reads like a product hang rather than a full cluster.
func schedulingHint(t *core.T, logf func(string, ...any)) string {
	kubeEnv, ok := t.Env.(interface{ AmpKube() *k8s.Kubectl })
	if !ok || kubeEnv.AmpKube() == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := kubeEnv.AmpKube().Events(ctx)
	if err != nil {
		return ""
	}
	var hits []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "FailedScheduling") || strings.Contains(line, "NotTriggerScaleUp") {
			hits = append(hits, strings.TrimSpace(line))
		}
	}
	if len(hits) == 0 {
		return ""
	}
	t.AttachBytes("scheduling-events.log", "text/plain", []byte(strings.Join(hits, "\n")))
	logf("multitenant: %d scheduling events; the cluster could not place a pod", len(hits))
	return " — the cluster could not schedule it: " + hits[len(hits)-1]
}

// tenantTimings are the control-plane latencies creating one tenant costs.
type tenantTimings struct {
	clusterMS  float64
	endpointMS float64
	firstSQLMS float64
}

func createTenant(t *core.T, cl *controller.Client, env ampMultitenantEnv, poolID string, i int, params multitenantParams) (tenant, tenantTimings, error) {
	var tm tenantTimings
	name := fmt.Sprintf("mt-%s-%d", t.ResourceSuffix(), i)

	start := time.Now()
	if _, err := cl.CreateClusterAndWait(t.Context, poolID, controller.CreateClusterRequest{Name: name}); err != nil {
		return tenant{}, tm, fmt.Errorf("create cluster: %w", err)
	}
	tm.clusterMS = msSince(start)

	cluster, err := findCluster(t.Context, cl, poolID, name)
	if err != nil {
		return tenant{}, tm, err
	}

	start = time.Now()
	if _, err := cl.CreateEndpointAndWait(t.Context, poolID, cluster.ID, cluster.DefaultBranch.BranchID,
		controller.CreateEndpointRequest{
			Role:      "read_write",
			Resources: &controller.EndpointResources{CPU: params.EndpointCPU, Memory: params.EndpointMemory},
		}); err != nil {
		return tenant{}, tm, fmt.Errorf("create endpoint: %w", err)
	}
	tm.endpointMS = msSince(start)

	eps, err := cl.ListAllEndpoints(t.Context, poolID, cluster.ID, cluster.DefaultBranch.BranchID)
	if err != nil {
		return tenant{}, tm, err
	}
	if len(eps) == 0 {
		return tenant{}, tm, fmt.Errorf("the branch lists no endpoint after a successful create")
	}
	ep := eps[len(eps)-1]

	start = time.Now()
	conn, err := env.ConnectEndpoint(t.Context, ep)
	if err != nil {
		return tenant{}, tm, fmt.Errorf("connect endpoint: %w", err)
	}
	if _, err := conn.QueryInt(t.Context, "SELECT 1"); err != nil {
		conn.Close()
		return tenant{}, tm, fmt.Errorf("first statement: %w", err)
	}
	tm.firstSQLMS = msSince(start)

	return tenant{
		Index: i, ClusterID: cluster.ID, BranchID: cluster.DefaultBranch.BranchID,
		Endpoint: ep, Conn: conn, Table: fmt.Sprintf("mt_load_%d", i),
	}, tm, nil
}

func findCluster(ctx context.Context, cl *controller.Client, poolID, name string) (controller.Cluster, error) {
	list, err := cl.ListClusters(ctx, poolID, 200, 0)
	if err != nil {
		return controller.Cluster{}, err
	}
	for i := len(list.Clusters) - 1; i >= 0; i-- {
		if list.Clusters[i].Name == name {
			return list.Clusters[i], nil
		}
	}
	return controller.Cluster{}, fmt.Errorf("cluster %q is absent after a successful create", name)
}

func seedTenant(t *core.T, ten *tenant, rows int) error {
	if _, err := ten.Conn.Exec(t.Context, fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s (id bigint PRIMARY KEY, v bigint NOT NULL, pad text)", ten.Table)); err != nil {
		return err
	}
	// One statement per batch keeps the seed from being its own benchmark.
	const batch = 500
	for start := 0; start < rows; start += batch {
		end := min(start+batch, rows)
		// The parameters are cast explicitly: a bare $1 arrives as "unknown", and
		// generate_series has no unknown-argument form to resolve to.
		if _, err := ten.Conn.Exec(t.Context, fmt.Sprintf(
			`INSERT INTO %s (id, v, pad)
			 SELECT g, g * 7, repeat('x', 64) FROM generate_series($1::bigint, $2::bigint) g
			 ON CONFLICT (id) DO NOTHING`, ten.Table), start, end-1); err != nil {
			return err
		}
	}
	return nil
}

func msSince(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}

func maxOf(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	out := vals[0]
	for _, v := range vals {
		out = max(out, v)
	}
	return out
}

// timeFork creates a branch off parentBranchID and reports how long the
// controller took to finish the task.
func timeFork(t *core.T, cl *controller.Client, poolID, clusterID, parentBranchID, name string) (float64, error) {
	start := time.Now()
	if _, err := cl.CreateBranchAndWait(t.Context, poolID, clusterID, controller.CreateBranchRequest{
		Name: name + "-" + t.ResourceSuffix(), ParentBranchID: parentBranchID,
	}); err != nil {
		return 0, fmt.Errorf("fork %s: %w", name, err)
	}
	return msSince(start), nil
}
