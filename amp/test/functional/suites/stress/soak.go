package stress

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"path"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/clientpod"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/metrics"
	"github.com/yugabyte/amp/test/functional/framework/apps/pageserver"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/sampleapp"
	"github.com/yugabyte/amp/test/functional/framework/artifact"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/soak",
		Suite:       "stress",
		Category:    core.LongRunning,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		OwningTeam:  core.TeamCompute,
		Description: "Soak amp for hours with a steady OLTP tenant, a bank tenant and a tenant forking and dropping a branch every 10 minutes, sampling the backing, endpoints and pageservers throughout, then assert six trends over the sampled series: pageserver RSS slope, amp_storage.wals growth, GC drain backlog, delta chain length, steady-tenant p99 latency drift and controller task table growth. Ends with acked-id verification per tenant and a fresh fork that reads the steady tenant's checksum back.",
		Params:      []string{"stress.soak"},
		Timeout:     14 * time.Hour,
		Run:         runSoak,
	})
}

type soakRun struct {
	t      *core.T
	cfg    soakConfig
	env    ampTenantEnv
	kube   *k8s.Kubectl
	cl     *controller.Client
	poolID string
	logf   func(string, ...any)
	sink   attachSink

	fleet     *tenantFleet
	pod       *clientpod.K8s
	loads     *workloadSet
	collector *soakCollector
	samplers  []*metrics.Sampler
	latency   *soakLatency
	storage   *pg.Client
	series    []soakSeries
	start     time.Time

	churnCycles atomic.Int64
	churnErr    error
}

func runSoak(t *core.T) error {
	cfg := soakFromConfig(t.Options)
	if reason, ok := soakGate(cfg); !ok {
		t.Skip(reason)
		return nil
	}
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane: the tenants write THROUGH amp")
		return nil
	}
	kenv, ok := t.Env.(ampKubeEnv)
	if !ok || kenv.AmpKube() == nil {
		t.Skip("requires a kube context for amp's namespace: the sampler reads the endpoints' pageservers")
		return nil
	}
	cl, err := env.AmpController(t.Context)
	if err != nil {
		return core.AsEnvFailure(fmt.Errorf("amp controller client: %w", err))
	}
	logf := t.Masker().Logf(log.Printf)
	r := &soakRun{
		t: t, cfg: cfg, env: env, kube: kenv.AmpKube(), cl: cl, poolID: env.AmpPoolID(),
		logf: logf, sink: attachSink{t: t, logf: logf},
	}

	fleet, err := newTenantFleet(t, env, logf)
	if err != nil {
		return err
	}
	r.fleet = fleet
	defer fleet.Close()
	defer r.shutdown()

	phases := []struct {
		name string
		fn   func(*core.StepHandle, context.Context) error
	}{
		{"create_tenants", r.createTenants},
		{"start_workloads", r.startWorkloads},
		{"start_sampling", r.startSampling},
		{"soak", r.soak},
		{"churn_tenant", r.assertChurn},
		{"verify_acked_ids", r.verifyAcked},
		{"fork_readback", r.forkReadback},
		{"trends", r.assertTrends},
	}
	for _, p := range phases {
		if err := t.Step(p.name, func(s *core.StepHandle) error { return p.fn(s, t.Context) }); err != nil {
			return err
		}
	}
	return nil
}

func (r *soakRun) shutdown() {
	if r.loads != nil {
		r.loads.CollectAll(r.sink)
	}
	if r.pod != nil {
		ctx, cancel := context.WithTimeout(context.Background(), reportKillTimeout)
		if err := r.pod.Stop(ctx, r.sink); err != nil {
			r.logf("soak: stop client pod (best-effort): %v", err)
		}
		cancel()
	}
	for _, s := range r.samplers {
		soakEmitSeries(r.t, s)
	}
	if r.collector != nil {
		r.collector.Stop()
	}
	if r.storage != nil {
		r.storage.Close()
	}
}

func (r *soakRun) createTenants(s *core.StepHandle, ctx context.Context) error {
	if err := r.fleet.Create(r.t, r.cfg.Tenants); err != nil {
		return err
	}
	s.Set("tenants", len(r.fleet.Tenants))
	return nil
}

func (r *soakRun) startWorkloads(s *core.StepHandle, ctx context.Context) error {
	addr, ok := r.env.(endpointAddrEnv)
	if !ok {
		return fmt.Errorf("this env cannot name an endpoint's in-cluster address, so a pod cannot dial it")
	}
	cp := clientpod.NewK8s(r.kube)
	spec := clientpod.Spec{
		Name:    "ampqa-soak-" + r.t.ResourceSuffix(),
		Image:   r.cfg.ClientImage,
		Command: []string{"sh", "-c", "sleep infinity"},
	}
	if r.cfg.ClientPullSecret != "" {
		spec.ImagePullSecrets = []string{r.cfg.ClientPullSecret}
	}
	if err := cp.Start(ctx, spec); err != nil {
		return fmt.Errorf("start client pod: %w", err)
	}
	r.pod = cp
	podExec := cp.Exec()
	jarPath, err := r.deployJar(ctx, podExec)
	if err != nil {
		return err
	}

	wlCfg := maintenanceConfig{
		WorkloadThreadsRead:  r.cfg.ThreadsRead,
		WorkloadThreadsWrite: r.cfg.ThreadsWrite,
		// The jar stops on its own RunTime, so it must outlast the soak.
		WorkloadRunTime: r.cfg.Duration + time.Hour,
		SettleTimeout:   r.cfg.SettleTimeout,
	}
	r.loads = &workloadSet{logf: r.logf}
	for i, spec := range []struct {
		wl    sampleapp.Workload
		label string
		extra map[string]string
	}{
		{sampleapp.SqlTransactions, "sqltransactions", nil},
		{sampleapp.SqlBankTransfer, "sqlbanktransfer", bankOnAmpProps(r.cfg.BankAccounts)},
	} {
		ten := &r.fleet.Tenants[i]
		host, port := addr.EndpointInClusterAddr(ten.Endpoint)
		if err := awaitTenantAuth(ctx, r.kube, ten.Endpoint.ID, host, port, r.cfg.Creds, r.logf); err != nil {
			return err
		}
		r.loads.workloads = append(r.loads.workloads, sampleAppOnEndpoint(podExec, jarPath, spec.wl, i,
			host, port, r.cfg.Creds, wlCfg,
			fmt.Sprintf("%s-%s-t%d", spec.label, r.t.ResourceSuffix(), i),
			r.workloadUUID(spec.label, i), spec.extra, r.logf))
	}
	return r.loads.Start(ctx)
}

func (r *soakRun) deployJar(ctx context.Context, podExec exec.RemoteExecutor) (string, error) {
	specs, err := artifact.Resolve(r.t.Options)
	if err != nil {
		return "", fmt.Errorf("resolve artifacts: %w", err)
	}
	sa, ok := specs["sampleapp"]
	if !ok {
		return "", fmt.Errorf("artifacts.sampleapp is not declared")
	}
	secrets, err := core.NewSecrets(r.t.Options)
	if err != nil {
		return "", fmt.Errorf("secrets: %w", err)
	}
	jarSecret, err := secrets.Value(r.cfg.JarSecret)
	if err != nil {
		return "", fmt.Errorf("resolve sample-app jar secret %q: %w", r.cfg.JarSecret, err)
	}
	dep := &artifact.Deployer{}
	if err := dep.Deploy(ctx, []exec.RemoteExecutor{podExec}, sa,
		artifact.DeployOptions{RemoteDir: r.cfg.RemoteDir, Secret: jarSecret}); err != nil {
		return "", fmt.Errorf("deploy the sample-app jar: %w", err)
	}
	return path.Join(r.cfg.RemoteDir, sa.GlobPath), nil
}

// The jar demands an RFC-4122-shaped --uuid; seed-forked so a replay repeats it.
func (r *soakRun) workloadUUID(label string, i int) string {
	rnd := r.t.Seed.Fork(fmt.Sprintf("uuid-%s-%d", label, i)).Rand()
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		rnd.Uint32(), uint16(rnd.Uint32()), uint16(rnd.Uint32()), uint16(rnd.Uint32()), rnd.Uint64()&0xffffffffffff)
}

func (r *soakRun) startSampling(s *core.StepHandle, ctx context.Context) error {
	r.start = time.Now()
	r.collector = newSoakCollector(r.cfg.SampleInterval, r.cfg.MaxSamples, soakSeriesUnits(), r.logf)
	r.collector.Add("pageserver", soakPageserverProbe(r.kube, r.fleet.Tenants))
	r.collector.Add("controller_tasks", soakTasksProbe(r.cl))

	if be, ok := r.env.(soakStorageEnv); ok {
		ci, err := be.YBStorage(ctx)
		if err != nil {
			return core.AsEnvFailure(fmt.Errorf("route to the backing amp storage database: %w", err))
		}
		ci.Database = ampStorageDB
		conn, err := pg.Connect(ctx, ci)
		if err != nil {
			return core.AsEnvFailure(fmt.Errorf("connect the backing amp storage database: %w", err))
		}
		r.storage = conn
		r.collector.Add("wals", soakWalsProbe(conn))
	} else {
		return fmt.Errorf("this env exposes no route to its backing amp storage database, so amp_storage.wals growth cannot be measured")
	}
	r.collector.Start()

	mcfg := metrics.FromConfig(r.t.Options)
	mcfg.SampleInterval = r.cfg.SampleInterval
	if mcfg.Enabled {
		for i := range r.fleet.Tenants {
			id := r.fleet.Tenants[i].Endpoint.ID
			if id == "" {
				continue
			}
			s := metrics.NewEndpoint(r.kube, mcfg, "app.kubernetes.io/instance="+ampstack.EndpointWorkloadName(id)).WithLogf(r.logf)
			if err := s.Start(ctx); err != nil {
				r.logf("soak: endpoint metrics sampler for tenant %d not started (best-effort): %v", i, err)
				continue
			}
			r.samplers = append(r.samplers, s)
		}
	}
	return nil
}

type soakStorageEnv interface {
	YBStorage(ctx context.Context) (pg.ConnInfo, error)
}

func (r *soakRun) soak(s *core.StepHandle, ctx context.Context) error {
	loadCtx, cancel := context.WithTimeout(ctx, r.cfg.Duration)
	defer cancel()

	r.latency = newSoakLatency(r.cfg.LatencyBucket, soakLatencyBucketCap, time.Now())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r.probeLatency(loadCtx) }()
	go func() { defer wg.Done(); r.churn(loadCtx) }()

	<-startTenantWriters(loadCtx, r.env, r.fleet.Tenants, r.cfg.PayloadBytes)
	wg.Wait()

	rows := totalWritten(r.fleet.Tenants)
	if rows == 0 {
		return fmt.Errorf("no tenant committed a single row in %s", r.cfg.Duration)
	}
	r.t.Metric("soak.rows_written", float64(rows), "rows")
	r.t.Metric("soak.churn_cycles", float64(r.churnCycles.Load()), "count")
	s.Set("duration", r.cfg.Duration.String())
	s.Set("rows_written", rows)
	s.Set("churn_cycles", r.churnCycles.Load())
	if err := r.loads.Check(ctx, "the soak"); err != nil {
		return err
	}
	if s := r.collector.Stop(); len(s) > 0 {
		r.series = s
		r.t.AttachBytes("soak-samples.jsonl", "log", soakJSONL(s))
	}
	return nil
}

const soakLatencyBucketCap = 5000

// Its own connection: sharing the writer's would measure queueing behind it.
func (r *soakRun) probeLatency(ctx context.Context) {
	ten := &r.fleet.Tenants[0]
	rnd := r.t.Seed.Fork("soak-latency").Rand()
	var conn *pg.Client
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	for ctx.Err() == nil {
		if conn == nil {
			c, err := r.env.ConnectEndpoint(ctx, ten.Endpoint)
			if err != nil {
				r.latency.recordErr()
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
				continue
			}
			conn = c
		}
		written := atomic.LoadInt64(&ten.Written)
		id := int64(ten.Index) * tenantIDSpan
		if written > 0 {
			id += int64(rnd.Uint64N(uint64(written)))
		}
		began := time.Now()
		_, err := conn.QueryInt(ctx, fmt.Sprintf("SELECT coalesce(max(v), 0) FROM %s WHERE id = $1", ten.Table), id)
		if err == nil {
			r.latency.record(began, time.Since(began))
		} else if ctx.Err() == nil {
			r.latency.recordErr()
			conn.Close()
			conn = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.cfg.LatencyProbe):
		}
	}
}

func (r *soakRun) churn(ctx context.Context) {
	ten := &r.fleet.Tenants[len(r.fleet.Tenants)-1]
	cluster, err := findCluster(ctx, r.cl, r.poolID, ten.Name)
	if err != nil {
		r.churnErr = fmt.Errorf("resolve the churn tenant's cluster: %w", err)
		return
	}
	run := &churnRun{}
	for cycle := 0; ctx.Err() == nil; cycle++ {
		name := fmt.Sprintf("soak-churn-%d-%s", cycle, r.t.ResourceSuffix())
		began := time.Now()
		if err := churnCycle(ctx, run, r.env, r.cl, r.poolID, cluster, name, churnCancelNone); err != nil {
			if ctx.Err() != nil {
				return
			}
			r.churnErr = fmt.Errorf("churn cycle %d (branch %s): %w", cycle, name, err)
			return
		}
		r.churnCycles.Add(1)
		r.logf("soak: churn cycle %d done in %s", cycle, time.Since(began).Round(time.Second))
		if wait := r.cfg.ChurnInterval - time.Since(began); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}
}

func (r *soakRun) assertChurn(s *core.StepHandle, ctx context.Context) error {
	if r.churnErr != nil {
		return r.churnErr
	}
	if r.churnCycles.Load() == 0 {
		return fmt.Errorf("the churn tenant completed no fork-and-drop cycle in %s, so nothing churned the control plane for the whole soak", r.cfg.Duration)
	}
	return nil
}

func (r *soakRun) verifyAcked(s *core.StepHandle, ctx context.Context) error {
	if err := assertEndpointsServe(r.t, r.env, r.fleet.Tenants, r.cfg.SettleTimeout); err != nil {
		return err
	}
	return verifyTenantData(r.t, s, r.env, r.fleet.Tenants, r.cfg.PayloadBytes)
}

func (r *soakRun) forkReadback(s *core.StepHandle, ctx context.Context) error {
	ten := &r.fleet.Tenants[0]
	// The fork point is the parent's DURABLE LSN, so without the flush the fork is a row behind.
	if err := r.flushParent(ctx, ten.Endpoint); err != nil {
		return err
	}
	want, err := r.checksum(ctx, ten.Endpoint, ten.Table)
	if err != nil {
		return fmt.Errorf("read the steady tenant's checksum from its own endpoint: %w", err)
	}
	name := "soak-fork-" + r.t.ResourceSuffix()
	if _, err := r.cl.CreateBranchAndWait(ctx, r.poolID, ten.ClusterID, controller.CreateBranchRequest{
		Name: name, ParentBranchID: ten.BranchID}); err != nil {
		return fmt.Errorf("fork the steady tenant's branch: %w", err)
	}
	br, err := findBranch(ctx, r.cl, r.poolID, ten.ClusterID, name)
	if err != nil {
		return err
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), churnTeardownBudget)
		defer cancel()
		if _, err := r.cl.DeleteBranchAndWait(dctx, r.poolID, ten.ClusterID, br.BranchID); err != nil {
			r.logf("soak: delete the read-back fork (best-effort): %v", err)
		}
	}()
	if _, err := r.cl.CreateEndpointAndWait(ctx, r.poolID, ten.ClusterID, br.BranchID,
		controller.CreateEndpointRequest{Role: tenantRole, Resources: r.fleet.endpointResources()}); err != nil {
		return fmt.Errorf("create an endpoint on the read-back fork: %w", err)
	}
	eps, err := r.cl.ListAllEndpoints(ctx, r.poolID, ten.ClusterID, br.BranchID)
	if err != nil || len(eps) == 0 {
		return fmt.Errorf("the read-back fork lists no endpoint after a successful create: %v", err)
	}
	ep := eps[len(eps)-1]
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), churnTeardownBudget)
		defer cancel()
		if _, err := r.cl.DeleteEndpointAndWait(dctx, r.poolID, ten.ClusterID, br.BranchID, ep.ID); err != nil {
			r.logf("soak: delete the read-back fork's endpoint (best-effort): %v", err)
		}
	}()
	got, err := r.checksum(ctx, ep, ten.Table)
	if err != nil {
		return fmt.Errorf("read the checksum back through the fork's endpoint: %w", err)
	}
	if got != want {
		return fmt.Errorf("a fresh fork of the steady tenant's branch serves %s, but the parent holds %s — the fork point was taken after the writers stopped, so the two must agree", got, want)
	}
	r.t.Metric("soak.fork_readback_rows", float64(1), "count")
	return nil
}

func (r *soakRun) flushParent(ctx context.Context, ep controller.Endpoint) error {
	conn, err := r.env.ConnectEndpoint(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect the steady tenant's endpoint to flush it: %w", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(ctx, "CHECKPOINT"); err != nil {
		return fmt.Errorf("checkpoint the steady tenant before forking: %w", err)
	}
	lsn, err := conn.QueryString(ctx, "SELECT pg_current_wal_flush_lsn()::text")
	if err != nil {
		return fmt.Errorf("read the steady tenant's flush LSN: %w", err)
	}
	// The branch id the ENDPOINT reports is the only one the pageserver agrees with.
	branchID, err := conn.QueryString(ctx, "SELECT current_setting('yb_amp.branch_id')")
	if err != nil {
		return fmt.Errorf("read the steady tenant's branch id: %w", err)
	}
	pod, err := r.endpointPod(ctx, ep.ID)
	if err != nil {
		return err
	}
	if err := pageserver.New(soakPodExec(r.kube, pod), pageserver.DefaultHTTPPort).FlushBranch(ctx, branchID, lsn); err != nil {
		return fmt.Errorf("flush the steady tenant's branch to storage before forking: %w", err)
	}
	return nil
}

func (r *soakRun) endpointPod(ctx context.Context, endpointID string) (string, error) {
	sel := "app.kubernetes.io/instance=" + ampstack.EndpointWorkloadName(endpointID)
	names, err := r.kube.GetSelector(ctx, "pod", sel, "{.items[*].metadata.name}")
	if err != nil {
		return "", fmt.Errorf("resolve the pod behind endpoint %s: %w", endpointID, err)
	}
	fields := strings.Fields(names)
	if len(fields) == 0 {
		return "", fmt.Errorf("no pod matches %s", sel)
	}
	return fields[0], nil
}

func (r *soakRun) checksum(ctx context.Context, ep controller.Endpoint, table string) (string, error) {
	conn, err := r.env.ConnectEndpoint(ctx, ep)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	rows, err := conn.QueryInt(ctx, "SELECT count(*) FROM "+table)
	if err != nil {
		return "", err
	}
	sum, err := conn.QueryInt(ctx, "SELECT coalesce(sum(v), 0) FROM "+table)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d row(s), checksum %d", rows, sum), nil
}

func (r *soakRun) assertTrends(s *core.StepHandle, ctx context.Context) error {
	var problems, skipped []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	// Not a pass and not a failure: a bound the run was too short to measure.
	skip := func(format string, args ...any) { skipped = append(skipped, fmt.Sprintf(format, args...)) }

	r.trendDatapoints()
	for _, ser := range r.series {
		if line, final, ok := maxOverLines(ser, lastValue); ok {
			s.Set(ser.Name, fmt.Sprintf("%.2f %s (%s)", final, ser.Unit, line))
		}
	}

	if ser, ok := findSeries(r.series, soakSeriesRSS); ok {
		// Slope and sample count must come from the SAME line, or a well-sampled pod lets a two-point fit elsewhere be asserted.
		line, slope, fitted, found := "", 0.0, 0, false
		for _, l := range sortedLines(ser) {
			fit := tail(after(ser.Lines[l], r.cfg.RSSWarmup), r.cfg.RSSFitFraction)
			if len(fit) > fitted {
				fitted = len(fit)
			}
			if len(fit) < r.cfg.MinFitSamples {
				continue
			}
			if s := slopePerHour(fit); !found || s > slope {
				line, slope, found = l, s, true
			}
		}
		switch {
		case !found:
			skip("pageserver RSS: %d sample(s) survive the %s stress.soak.rss_warmup, under the %d stress.soak.min_fit_samples a slope needs — a run this short measures the pageserver warming up, not a leak",
				fitted, r.cfg.RSSWarmup, r.cfg.MinFitSamples)
		case slope > r.cfg.RSSSlopeMiBPerHour:
			fail("pageserver RSS on %s grows %.1f MiB/h over the last %.0f%% of the run after the %s warm-up, above the %.1f MiB/h allowed by stress.soak.rss_slope_mib_per_hour",
				line, slope, r.cfg.RSSFitFraction*100, r.cfg.RSSWarmup, r.cfg.RSSSlopeMiBPerHour)
		}
	} else {
		fail("no pageserver RSS was sampled, so the leak assertion did not run")
	}

	if ser, ok := findSeries(r.series, soakSeriesWals); ok {
		pts := ser.Lines["backing"]
		at := r.cfg.WalsBaselineAfter
		base, reached := valueAt(pts, at)
		switch {
		case !reached:
			skip("amp_storage.wals was never sampled %s into the run (stress.soak.wals_baseline_after), so its growth bound was not asserted", at)
		case base > 0 && lastValue(pts) > base*r.cfg.WalsGrowthFactor:
			fail("amp_storage.wals holds %.0f rows, over %.1fx the %.0f it held %s in, the bound stress.soak.wals_growth_factor sets — retention is not being reclaimed",
				lastValue(pts), r.cfg.WalsGrowthFactor, base, at)
		}
	} else {
		fail("amp_storage.wals was never counted, so its growth bound did not run")
	}

	if ser, ok := findSeries(r.series, soakSeriesGCLag); ok {
		limit := r.cfg.GCLagMonotonicLimit
		line, seconds, found := maxOverLines(ser, longestRisingRun)
		rising := time.Duration(seconds * float64(time.Second))
		switch {
		case r.cfg.Duration <= limit:
			skip("the GC drain lag may rise for %s (stress.soak.gc_lag_monotonic_limit), which a %s run cannot exceed, so the backlog bound was not asserted", limit, r.cfg.Duration)
		case found && rising > limit:
			fail("the GC drain lag on %s only rose for %s without once falling, past the %s stress.soak.gc_lag_monotonic_limit allows — the drain is not keeping up with what is being queued",
				line, rising.Round(time.Minute), limit)
		}
	} else {
		fail("no GC drain lag was sampled, so the backlog assertion did not run")
	}

	if ser, ok := findSeries(r.series, soakSeriesChain); ok {
		limit := r.cfg.ChainLimit + r.cfg.ChainLimitSlack
		share, total := belowLimitFraction(ser, limit)
		switch {
		case total < r.cfg.MinFitSamples:
			skip("delta chain length: %d sample(s), under the %d stress.soak.min_fit_samples — the read path was too quiet for pageserver_deltas_per_read_global to move", total, r.cfg.MinFitSamples)
		case share < r.cfg.ChainSampleFraction:
			fail("only %.1f%% of %d delta-chain samples are at or below %.0f deltas (stress.soak.chain_limit plus chain_limit_slack), under the %.0f%% stress.soak.chain_sample_fraction requires — materialisation is falling behind ingest",
				share*100, total, limit, r.cfg.ChainSampleFraction*100)
		}
	} else {
		fail("no delta chain length was sampled, so the chain assertion did not run")
	}

	p99s := r.latency.p99s(soakLatencyMinSamples)
	switch {
	case len(p99s) < 2:
		skip("the steady tenant produced %d usable %s latency bucket(s), so its drift bound was not asserted", len(p99s), r.cfg.LatencyBucket)
	case p99s[0] > 0 && p99s[len(p99s)-1] > p99s[0]*r.cfg.LatencyGrowthFactor:
		fail("the steady tenant's p99 read latency ends at %.1f ms against %.1f ms in its first %s bucket, over the %.1fx stress.soak.latency_growth_factor allows",
			p99s[len(p99s)-1], p99s[0], r.cfg.LatencyBucket, r.cfg.LatencyGrowthFactor)
	}

	if ser, ok := findSeries(r.series, soakSeriesTasks); ok {
		if last := lastValue(ser.Lines["controller"]); last > float64(r.cfg.MaxTasks) {
			fail("the controller task table holds %.0f rows, past the %d stress.soak.max_tasks allows — finished tasks are not being pruned",
				last, r.cfg.MaxTasks)
		}
	} else {
		fail("the controller task table was never counted, so its growth bound did not run")
	}

	if len(skipped) > 0 {
		s.Set("not_asserted", joinProblems(skipped))
		r.logf("soak: not asserted at a %s duration: %s", r.cfg.Duration, joinProblems(skipped))
	}
	if len(problems) > 0 {
		return fmt.Errorf("after %s of soak: %s", r.cfg.Duration, joinProblems(problems))
	}
	return nil
}

const soakLatencyMinSamples = 50

func joinProblems(problems []string) string {
	out := ""
	for i, p := range problems {
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out
}

func (r *soakRun) trendDatapoints() {
	units := soakSeriesUnits()
	for _, ser := range r.series {
		line, final, found := maxOverLines(ser, lastValue)
		if !found {
			continue
		}
		_, slope, _ := maxOverLines(ser, func(pts []soakPoint) float64 {
			return slopePerHour(tail(after(pts, r.cfg.RSSWarmup), r.cfg.RSSFitFraction))
		})
		r.t.Datapoint(core.Datapoint{Name: "soak." + ser.Name + ".final", Value: final, Unit: units[ser.Name], Direction: core.LowerIsBetter})
		r.t.Datapoint(core.Datapoint{Name: "soak." + ser.Name + ".slope_per_hour", Value: slope, Unit: units[ser.Name] + "/h", Direction: core.LowerIsBetter})
		r.logf("soak: %s peaks on %s at %.2f %s, slope %.2f %s/h", ser.Name, line, final, units[ser.Name], slope, units[ser.Name])
	}
	if p99s := r.latency.p99s(soakLatencyMinSamples); len(p99s) > 0 {
		r.t.Datapoint(core.Datapoint{Name: "soak." + soakSeriesLatency + ".final", Value: p99s[len(p99s)-1], Unit: "ms", Direction: core.LowerIsBetter})
		if len(p99s) > 1 && p99s[0] > 0 {
			drift := (p99s[len(p99s)-1]/p99s[0] - 1) * 100 / r.cfg.LatencyBucket.Hours() / float64(len(p99s)-1)
			r.t.Datapoint(core.Datapoint{Name: "soak." + soakSeriesLatency + ".slope_per_hour", Value: drift, Unit: "%/h", Direction: core.LowerIsBetter})
		}
	}
}

func soakEmitSeries(t *core.T, s *metrics.Sampler) {
	for _, ser := range s.Stop() {
		t.MetricSeries(core.Metric{
			Key: ser.Key, Value: ser.Average(), Unit: ser.Unit, RoundVal: ser.Round,
			Raw: ser.Raw, Desc: ser.Desc, Series: ser.Pairs(),
		})
	}
}
