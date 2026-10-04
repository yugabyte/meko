// The data lifecycle a customer actually runs, timed on a loaded tenant: fork a
// branch off millions of rows, bring an endpoint up on the fork and read it
// cold, write into the fork, pause and resume it (once perf.lifecycle.pause_resume
// is on), delete half the parent's rows, drop the table, delete the fork and
// finally the whole cluster. The whole lifecycle runs perf.lifecycle.iterations
// times CONCURRENTLY, each on a fresh tenant. Every phase is timed as a step; the ten in
// trackedPhases also become a `lifecycle.<phase>` datapoint carrying the MEDIAN
// across iterations, gated against a concurrent previous-release arm when one
// resolves (portal history otherwise), and median(fork_loaded)/median(fork_small)
// is gated inside the run.
package perf

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	testNameLifecycle = "perf/lifecycle"
	lifecycleTable    = "lc_big"
	lifecycleSeed     = "lc_seed"
	lifecyclePrefix   = "lifecycle."
	// "2" = iterations run concurrently, so every phase includes co-tenant
	// contention; not comparable with sequential-iteration history.
	lifecycleHarnessRev = "2"

	DefaultLifecycleRows          = 3000000
	DefaultLifecycleBatchRows     = 200000
	DefaultLifecycleForkWriteRows = 200000
	DefaultLifecyclePayloadBytes  = 64
	DefaultLifecycleMaxForkRatio  = 5.0
	DefaultLifecyclePauseResume   = false
	DefaultLifecycleEndpointCPU   = "2"
	DefaultLifecycleEndpointMem   = "4Gi"
	DefaultLifecycleIterations    = 5
	// Five concurrent 3M-row tenants filled the 10Gi default on every tserver
	// ("Node ... has insufficient disk space"), which then surfaced as killed
	// sessions, failed lease acquires and stalled forks.
	DefaultLifecycleBackingStorage = "30Gi"
)

type lifecycleParams struct {
	Iterations    int
	Rows          int
	BatchRows     int
	ForkWriteRows int
	PayloadBytes  int
	MaxForkRatio  float64
	EndpointCPU   string
	EndpointMem   string
	PauseResume   bool
}

func lifecycleParamsFromConfig(c *core.Config) lifecycleParams {
	return lifecycleParams{
		Iterations:    c.GetInt("perf.lifecycle.iterations", DefaultLifecycleIterations),
		Rows:          c.GetInt("perf.lifecycle.rows", DefaultLifecycleRows),
		BatchRows:     c.GetInt("perf.lifecycle.batch_rows", DefaultLifecycleBatchRows),
		ForkWriteRows: c.GetInt("perf.lifecycle.fork_write_rows", DefaultLifecycleForkWriteRows),
		PayloadBytes:  c.GetInt("perf.lifecycle.payload_bytes", DefaultLifecyclePayloadBytes),
		MaxForkRatio:  c.GetFloat("perf.lifecycle.max_fork_ratio", DefaultLifecycleMaxForkRatio),
		EndpointCPU:   c.GetString("perf.lifecycle.endpoint_cpu", DefaultLifecycleEndpointCPU),
		EndpointMem:   c.GetString("perf.lifecycle.endpoint_memory", DefaultLifecycleEndpointMem),
		PauseResume:   c.GetBool("perf.lifecycle.pause_resume", DefaultLifecyclePauseResume),
	}
}

func init() {
	core.Register(core.Test{
		Name:       testNameLifecycle,
		Suite:      "perf",
		Category:   core.Perf,
		Env:        core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack, ClusterSlots: lifecycleClusterSlots},
		Tags:       []string{"perf"},
		OwningTeam: core.TeamControlPlane,
		Description: "Time the data lifecycle on a loaded tenant: create the tenant, fork a near-empty branch, load millions of rows plus an index, fork the loaded branch, " +
			"bring an endpoint up on the fork and count it cold and warm, write into the fork, pause and resume the fork (perf.lifecycle.pause_resume), delete half the parent's rows, drop the table, " +
			"delete the fork's endpoint and branch, and delete the whole cluster. The lifecycle runs perf.lifecycle.iterations times concurrently, each on a fresh tenant; the fork, load, " +
			"read, big-delete and drop phases carry the median as a gated lifecycle.<phase> ms datapoint, and median(fork_loaded) / median(fork_small) is gated in-run against perf.lifecycle.max_fork_ratio.",
		Params:  []string{"perf.lifecycle", "perf.gate", "perf.baseline", "perf.baseline_path", "perf.update_baseline", "perf.latency_rise_pct", "perf.ready_timeout"},
		Timeout: 2 * time.Hour,
		Run:     runLifecycle,
	})
}

type lifecycleRun struct {
	t        *core.T
	iter     int
	pair     *phaseBarrier
	pairIter int
	// baseline is the arm's release tag; "" = the run's own images.
	baseline string
	cl       *controller.Client
	env      ampMultitenantEnv
	poolID   string
	params   lifecycleParams
	logf     func(string, ...any)
	parent   tenant
	fork     tenant
	results  []phaseResult
	tableMB  float64
	deleted  bool
	// placed is where each endpoint's compute pod first ran, by endpoint ID.
	placed map[string]podPlacement
}

func runLifecycle(t *core.T) error {
	env, ok := t.Env.(ampMultitenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, _ := t.Env.(interface{ AmpKube() *k8s.Kubectl })
	var kube *k8s.Kubectl
	if kubeEnv != nil {
		kube = kubeEnv.AmpKube()
	}
	params := lifecycleParamsFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)

	if tag := resolveLiveArmTag(t, logf); tag != "" {
		return runLifecycleProvisioned(t, params, tag, logf)
	}

	cl, err := env.AmpController(t.Context)
	if err != nil {
		return err
	}
	id := identityFromConfig(t.Options, "", frameworkCommit(), loadFieldsLifecycle(params))
	t.SetPerfIdentity(id.core())

	all, sizes, err := lifecycleIterations(t, cl, env, kube, params, "", nil, logf)
	if err != nil {
		return err
	}
	return gateLifecycle(t, params, id, all, sizes, nil, "", nil, logf)
}

// runLifecycleProvisioned is the live-baseline body: two self-provisioned
// control planes — the current images and the tag release — run the same
// lifecycle concurrently, and the gate is the delta between their phase
// medians. The env's control plane is deliberately not measured here: the arms
// must be twins, and the env's stack has no baseline twin.
func runLifecycleProvisioned(t *core.T, params lifecycleParams, tag string, logf func(string, ...any)) error {
	cfg := perfFromConfig(t.Options)
	cfg.BackingStorageSize = t.Options.GetString("perf.lifecycle.backing_storage_size", DefaultLifecycleBackingStorage)
	load := loadFieldsLifecycle(params)
	load.Harness += "+" + provisionedHarness + cfg.isolationSuffix()
	id := identityFromConfig(t.Options, "", frameworkCommit(), load).forLiveArm(t.Options)
	t.SetPerfIdentity(id.core())

	cur, base, baseErr, err := provisionLiveArms(t, testNameLifecycle, cfg, tag, logf)
	if cur != nil {
		defer cur.teardown(logf)
	}
	if base != nil {
		defer base.teardown(logf)
	}
	if err != nil {
		return err
	}

	var baseAll []phaseResult
	var wg sync.WaitGroup
	var pair *phaseBarrier
	if base != nil {
		pair = newPhaseBarrier()
		wg.Add(1)
		go func() {
			defer wg.Done()
			baseAll, _, baseErr = lifecycleIterations(t, base.cl, base, base.kube, params, tag, pair, logf)
		}()
	}
	all, sizes, err := lifecycleIterations(t, cur.cl, cur, cur.kube, params, "", pair, logf)
	wg.Wait()
	if err != nil {
		cur.collectDiag(t, logf)
		return err
	}
	if baseErr != nil {
		base.collectDiag(t, logf)
		logf("perf: WARN live baseline arm failed (%v) — gating on history instead", baseErr)
		baseAll = nil
	}
	return gateLifecycle(t, params, id, all, sizes, baseAll, tag, baseErr, logf)
}

// lifecycleIterations runs the iteration goroutines against one control plane
// and collapses their results. baseline marks the arm's steps and suppresses
// its attachments ("" = the run's own arm).
func lifecycleIterations(t *core.T, cl *controller.Client, env ampMultitenantEnv, kube *k8s.Kubectl,
	params lifecycleParams, baseline string, pair *phaseBarrier, logf func(string, ...any)) ([]phaseResult, []float64, error) {
	iterations := max(params.Iterations, 1)
	runs := make([]*lifecycleRun, iterations)
	errs := make([]error, iterations)
	var wg sync.WaitGroup
	for i := 1; i <= iterations; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer pair.abandon(i)
			runs[i-1], errs[i-1] = runLifecycleIteration(t, cl, env, kube, params, i, iterations, baseline, pair, logf)
		}(i)
	}
	wg.Wait()
	var all []phaseResult
	var sizes []float64
	for _, r := range runs {
		all = append(all, r.results...)
		if r.tableMB > 0 {
			sizes = append(sizes, r.tableMB)
		}
	}
	if baseline == "" {
		attachPhases(t, all)
	}
	var iterErrs []error
	allEnv := true
	for _, err := range errs {
		if err != nil && !core.IsEnvFailure(err) {
			allEnv = false
		}
	}
	for i, err := range errs {
		if err == nil {
			continue
		}
		// errors.As finds an EnvError anywhere in a join, so one env iteration
		// would hide a product failure in another unless its type is dropped.
		if !allEnv && core.IsEnvFailure(err) {
			err = errors.New(err.Error())
		}
		iterErrs = append(iterErrs, fmt.Errorf("iteration %d/%d: %w", i+1, iterations, err))
	}
	return all, sizes, errors.Join(iterErrs...)
}

// Iterations run concurrently, each on its own tenant (universe-side the env is
// shared; the cluster, branches and endpoints are the iteration's own), so the
// spread is tenant-to-tenant and the wall clock stays one lifecycle.
func runLifecycleIteration(t *core.T, cl *controller.Client, env ampMultitenantEnv, kube *k8s.Kubectl,
	params lifecycleParams, iter, iterations int, baseline string, pair *phaseBarrier, logf func(string, ...any)) (*lifecycleRun, error) {
	r := &lifecycleRun{
		t: t, cl: cl, env: env, poolID: env.AmpPoolID(),
		params: params, logf: logf, baseline: baseline,
		pair: pair, pairIter: iter,
	}
	if iterations > 1 {
		r.iter = iter
	}
	td := &teardownStack{}

	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		r.closeConns()
		if r.parent.ClusterID != "" && !r.deleted {
			if _, err := cl.DeleteClusterAndWait(ctx, r.poolID, r.parent.ClusterID); err != nil {
				r.logf("lifecycle: could not delete cluster %s: %v", r.parent.ClusterID, err)
			}
		}
		_ = td.run(ctx, r.logf)
	}()

	steps := []struct {
		name string
		fn   func() error
	}{
		{"create_tenant", func() error { return r.standUpTenant(kube, td) }},
		{"fork_small", r.forkSmall},
		{"load", r.load},
		{"fork_loaded", r.forkLoaded},
		{"fork_endpoint", r.forkEndpoint},
		{"fork_reads", r.forkReads},
		{"fork_write", r.forkWrite},
		{"fork_pause_resume", r.forkPauseResume},
		{"parent_delete_half", r.parentDeleteHalf},
		{"parent_drop_table", r.parentDropTable},
		{"delete_fork", r.deleteFork},
		{"delete_cluster", r.deleteCluster},
	}
	for _, s := range steps {
		err := s.fn()
		// The fork endpoint, once it exists, is the one the failing phases run on.
		target := r.parent
		if r.fork.Endpoint.ID != "" {
			target = r.fork
		}
		if err == nil {
			r.notePlacement(kube, target.Endpoint.ID)
			continue
		}
		if kube != nil && target.Endpoint.ID != "" {
			if diag := captureEndpointDiag(t, kube, target, s.name, r.logf); diag != "" {
				r.logf("lifecycle: endpoint at %s: %s", s.name, diag)
			}
			if gone := r.nodeRemoved(kube, target.Endpoint.ID); gone != "" {
				return r, core.EnvFail("%s: %w (%s)", s.name, err, gone)
			}
		}
		return r, fmt.Errorf("%s: %w", s.name, err)
	}
	return r, nil
}

type podPlacement struct{ uid, node string }

func endpointPlacement(ctx context.Context, kube *k8s.Kubectl, endpointID string) (podPlacement, error) {
	out, err := kube.GetSelector(ctx, "pod", "app.kubernetes.io/instance="+ampstack.EndpointWorkloadName(endpointID),
		"{.items[0].metadata.uid} {.items[0].spec.nodeName}")
	if err != nil {
		return podPlacement{}, err
	}
	f := strings.Fields(out)
	if len(f) < 2 {
		return podPlacement{}, fmt.Errorf("endpoint %s has no scheduled pod (%q)", endpointID, out)
	}
	return podPlacement{uid: f[0], node: f[1]}, nil
}

func (r *lifecycleRun) notePlacement(kube *k8s.Kubectl, endpointID string) {
	if kube == nil || endpointID == "" {
		return
	}
	if _, ok := r.placed[endpointID]; ok {
		return
	}
	if p, err := endpointPlacement(r.t.Context, kube, endpointID); err == nil {
		if r.placed == nil {
			r.placed = map[string]podPlacement{}
		}
		r.placed[endpointID] = p
	}
}

// The amp-dev autoscaler drains nodes under running tests, and the endpoint
// chart has no safe-to-evict knob: a compute whose node left the cluster was
// killed by the environment. Returns "" unless that is proven.
func (r *lifecycleRun) nodeRemoved(kube *k8s.Kubectl, endpointID string) string {
	was, ok := r.placed[endpointID]
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if now, err := endpointPlacement(ctx, kube, endpointID); err == nil && now.uid == was.uid {
		return ""
	}
	_, err := kube.Get(ctx, "node", was.node, "{.metadata.name}")
	if err == nil || !strings.Contains(err.Error(), "NotFound") {
		return ""
	}
	return fmt.Sprintf("endpoint %s's compute pod %s ran on node %s, which has since been removed from the cluster", endpointID, was.uid, was.node)
}

// Steps keep their bare name across iterations; the iteration is a step
// parameter, so the report view is not spammed with i<N>_-prefixed rows.
func (r *lifecycleRun) phase(name string, fn func() error) error {
	r.pair.sync(r.pairIter, name)
	return r.t.Step(name, func(s *core.StepHandle) error {
		r.setIteration(s)
		start := time.Now()
		err := fn()
		ms := msSince(start)
		s.Set("ms", fmt.Sprintf("%.1f", ms))
		if err != nil {
			return err
		}
		r.record(name, ms)
		return nil
	})
}

func (r *lifecycleRun) setIteration(s *core.StepHandle) {
	if r.iter > 0 {
		s.Set("iteration", r.iter)
	}
	if r.baseline != "" {
		s.Set("baseline", r.baseline)
	}
}

func (r *lifecycleRun) record(name string, ms float64) {
	r.results = append(r.results, phaseResult{Name: name, Group: name, MS: ms})
	arm := ""
	if r.baseline != "" {
		arm = "[baseline] "
	}
	if r.iter > 0 {
		r.logf("lifecycle: %s[i%d] %s took %.1f ms", arm, r.iter, name, ms)
	} else {
		r.logf("lifecycle: %s%s took %.1f ms", arm, name, ms)
	}
}

func (r *lifecycleRun) closeConns() {
	if r.fork.Conn != nil {
		r.fork.Conn.Close()
		r.fork.Conn = nil
	}
	if r.parent.Conn != nil {
		r.parent.Conn.Close()
		r.parent.Conn = nil
	}
}

func (r *lifecycleRun) tenantParams() multitenantParams {
	return multitenantParams{EndpointCPU: r.params.EndpointCPU, EndpointMemory: r.params.EndpointMem}
}

func (r *lifecycleRun) standUpTenant(kube *k8s.Kubectl, td *teardownStack) error {
	r.pair.sync(r.pairIter, "create_tenant")
	return r.t.Step("create_tenant", func(s *core.StepHandle) error {
		r.setIteration(s)
		ten, tm, err := createTenant(r.t, r.cl, r.env, r.poolID, max(r.iter-1, 0), r.tenantParams())
		if err != nil {
			return fmt.Errorf("%w%s", err, schedulingHint(r.t, r.logf))
		}
		r.parent = ten
		r.record("create_cluster", tm.clusterMS)
		r.record("create_endpoint", tm.endpointMS)
		r.record("first_sql", tm.firstSQLMS)
		s.Set("cluster", ten.ClusterID)
		s.Set("endpoint", ten.Endpoint.ID)
		s.Set("endpoint_size", r.params.EndpointCPU+" / "+r.params.EndpointMem)
		if kube != nil && r.iter <= 1 {
			// The env's own sampler watches only ITS standing endpoint, not this
			// tenant's; sample the first iteration's tenant (one sampler, so the
			// "[endpoint]" keys stay single) and mark collection so the env drain
			// does not emit a second set. The baseline arm's twin tenant is
			// sampled too and reported nowhere, so the pair carries the same load.
			ep := ampEndpoint{Kube: kube, EndpointID: ten.Endpoint.ID}
			if r.baseline != "" {
				loadOnlySamplers(r.t.Context, r.t, nil, &ep, td, r.logf)
			} else if startEndpointSampler(r.t.Context, r.t, ep, td, r.logf) != nil {
				r.t.MarkServerMetricsCollected()
			}
		}
		return nil
	})
}

// A branch nobody wrote to has no fork point, so the parent gets one seed row first.
func (r *lifecycleRun) forkSmall() error {
	ctx := r.t.Context
	if _, err := r.parent.Conn.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+lifecycleSeed+" (id int PRIMARY KEY)"); err != nil {
		return err
	}
	if _, err := r.parent.Conn.Exec(ctx, "INSERT INTO "+lifecycleSeed+" VALUES (1) ON CONFLICT (id) DO NOTHING"); err != nil {
		return err
	}
	name := "lc-small-" + r.t.ResourceSuffix()
	if err := r.phase("fork_small", func() error {
		_, err := r.cl.CreateBranchAndWait(ctx, r.poolID, r.parent.ClusterID, controller.CreateBranchRequest{
			Name: name, ParentBranchID: r.parent.BranchID})
		return err
	}); err != nil {
		return err
	}
	br, err := findBranchByName(ctx, r.cl, r.poolID, r.parent.ClusterID, name)
	if err != nil {
		return err
	}
	return r.phase("delete_branch_small", func() error {
		_, err := r.cl.DeleteBranchAndWait(ctx, r.poolID, r.parent.ClusterID, br.BranchID)
		return err
	})
}

func (r *lifecycleRun) load() error {
	ctx := r.t.Context
	p := r.params
	if _, err := r.parent.Conn.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+lifecycleTable+" (id bigint PRIMARY KEY, v bigint NOT NULL, pad text)"); err != nil {
		return err
	}
	if err := r.phase("load", func() error {
		return lifecycleInsert(ctx, r.parent.Conn, 1, int64(p.Rows), p.BatchRows, p.PayloadBytes)
	}); err != nil {
		return err
	}
	if err := r.phase("create_index", func() error {
		_, err := r.parent.Conn.Exec(ctx, "CREATE INDEX "+lifecycleTable+"_v ON "+lifecycleTable+" (v)")
		return err
	}); err != nil {
		return err
	}
	return r.t.Step("measure_table", func(s *core.StepHandle) error {
		r.setIteration(s)
		bytes, err := r.parent.Conn.QueryInt(ctx, "SELECT pg_total_relation_size('"+lifecycleTable+"')")
		if err != nil {
			return err
		}
		mb := float64(bytes) / (1024 * 1024)
		s.Set("table_mb", fmt.Sprintf("%.1f", mb))
		s.Set("rows", p.Rows)
		r.tableMB = mb
		// The size functions are reimplemented over pageserver storage; a value
		// below the raw payload means that implementation broke, not a small table.
		if floor := float64(p.Rows) * float64(p.PayloadBytes) / (1024 * 1024); mb < floor {
			return fmt.Errorf("pg_total_relation_size reports %.1f MiB for %d rows of %dB payload (floor %.1f MiB)", mb, p.Rows, p.PayloadBytes, floor)
		}
		return nil
	})
}

func (r *lifecycleRun) forkLoaded() error {
	ctx := r.t.Context
	name := "lc-loaded-" + r.t.ResourceSuffix()
	if err := r.phase("fork_loaded", func() error {
		_, err := r.cl.CreateBranchAndWait(ctx, r.poolID, r.parent.ClusterID, controller.CreateBranchRequest{
			Name: name, ParentBranchID: r.parent.BranchID})
		return err
	}); err != nil {
		return err
	}
	br, err := findBranchByName(ctx, r.cl, r.poolID, r.parent.ClusterID, name)
	if err != nil {
		return err
	}
	r.fork = tenant{Index: 1, ClusterID: r.parent.ClusterID, BranchID: br.BranchID, Table: lifecycleTable}
	return nil
}

func (r *lifecycleRun) forkEndpoint() error {
	ctx := r.t.Context
	if err := r.phase("fork_endpoint", func() error {
		_, err := r.cl.CreateEndpointAndWait(ctx, r.poolID, r.fork.ClusterID, r.fork.BranchID, controller.CreateEndpointRequest{
			Role:      "read_write",
			Resources: &controller.EndpointResources{CPU: r.params.EndpointCPU, Memory: r.params.EndpointMem},
		})
		return err
	}); err != nil {
		// Without this the caller's diagnostics capture the parent's healthy endpoint.
		if eps, lerr := r.cl.ListAllEndpoints(ctx, r.poolID, r.fork.ClusterID, r.fork.BranchID); lerr == nil && len(eps) > 0 {
			r.fork.Endpoint = eps[len(eps)-1]
		}
		return err
	}
	eps, err := r.cl.ListAllEndpoints(ctx, r.poolID, r.fork.ClusterID, r.fork.BranchID)
	if err != nil {
		return err
	}
	if len(eps) == 0 {
		return errors.New("the fork lists no endpoint after a successful create")
	}
	r.fork.Endpoint = eps[len(eps)-1]
	return r.phase("fork_first_sql", func() error {
		conn, err := r.env.ConnectEndpoint(ctx, r.fork.Endpoint)
		if err != nil {
			return err
		}
		if _, err := conn.QueryInt(ctx, "SELECT 1"); err != nil {
			conn.Close()
			return err
		}
		r.fork.Conn = conn
		return nil
	})
}

func (r *lifecycleRun) forkReads() error {
	ctx := r.t.Context
	want := int64(r.params.Rows)
	if err := r.phase("fork_count_cold", func() error { return expectCount(ctx, r.fork.Conn, want) }); err != nil {
		return err
	}
	if err := r.phase("fork_count_warm", func() error { return expectCount(ctx, r.fork.Conn, want) }); err != nil {
		return err
	}
	if err := r.phase("fork_pk_lookup", func() error {
		_, err := r.fork.Conn.QueryInt(ctx, fmt.Sprintf("SELECT v FROM %s WHERE id = %d", lifecycleTable, r.params.Rows/2))
		return err
	}); err != nil {
		return err
	}
	return r.phase("fork_index_scan", func() error {
		_, err := r.fork.Conn.QueryInt(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE v BETWEEN %d AND %d",
			lifecycleTable, 7*int64(r.params.Rows/2), 7*int64(r.params.Rows/2)+7000))
		return err
	})
}

func (r *lifecycleRun) forkWrite() error {
	ctx := r.t.Context
	p := r.params
	if err := r.phase("fork_insert", func() error {
		return lifecycleInsert(ctx, r.fork.Conn, int64(p.Rows)+1, int64(p.Rows+p.ForkWriteRows), p.BatchRows, p.PayloadBytes)
	}); err != nil {
		return err
	}
	if err := expectCount(ctx, r.parent.Conn, int64(p.Rows)); err != nil {
		return fmt.Errorf("the parent saw the fork's write: %w", err)
	}
	return nil
}

// Off by default: the controller has no pause/resume routes yet (meko#518).
func (r *lifecycleRun) forkPauseResume() error {
	if !r.params.PauseResume {
		return nil
	}
	ctx := r.t.Context
	r.fork.Conn.Close()
	r.fork.Conn = nil
	if err := r.phase("fork_pause", func() error {
		_, err := r.cl.PauseEndpointAndWait(ctx, r.poolID, r.fork.ClusterID, r.fork.BranchID, r.fork.Endpoint.ID)
		return err
	}); err != nil {
		return err
	}
	if err := r.phase("fork_resume", func() error {
		_, err := r.cl.ResumeEndpointAndWait(ctx, r.poolID, r.fork.ClusterID, r.fork.BranchID, r.fork.Endpoint.ID)
		return err
	}); err != nil {
		return err
	}
	ep, err := r.cl.GetEndpoint(ctx, r.poolID, r.fork.ClusterID, r.fork.BranchID, r.fork.Endpoint.ID)
	if err != nil {
		return err
	}
	r.fork.Endpoint = ep
	return r.phase("fork_resume_first_count", func() error {
		conn, err := r.env.ConnectEndpoint(ctx, r.fork.Endpoint)
		if err != nil {
			return err
		}
		r.fork.Conn = conn
		return expectCount(ctx, conn, int64(r.params.Rows+r.params.ForkWriteRows))
	})
}

func (r *lifecycleRun) parentDeleteHalf() error {
	ctx := r.t.Context
	half := int64(r.params.Rows / 2)
	if err := r.phase("delete_half", func() error {
		n, err := r.parent.Conn.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE id <= %d", lifecycleTable, half))
		if err != nil {
			return err
		}
		if n != half {
			return fmt.Errorf("DELETE reported %d rows, want %d", n, half)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := r.phase("count_after_delete", func() error {
		return expectCount(ctx, r.parent.Conn, int64(r.params.Rows)-half)
	}); err != nil {
		return err
	}
	if r.fork.Conn != nil {
		if err := expectCount(ctx, r.fork.Conn, int64(r.params.Rows+r.params.ForkWriteRows)); err != nil {
			return fmt.Errorf("the fork saw the parent's delete: %w", err)
		}
	}
	return nil
}

func (r *lifecycleRun) parentDropTable() error {
	return r.phase("drop_table", func() error {
		_, err := r.parent.Conn.Exec(r.t.Context, "DROP TABLE "+lifecycleTable)
		return err
	})
}

func (r *lifecycleRun) deleteFork() error {
	ctx := r.t.Context
	if r.fork.Conn != nil {
		r.fork.Conn.Close()
		r.fork.Conn = nil
	}
	if err := r.phase("delete_fork_endpoint", func() error {
		_, err := r.cl.DeleteEndpointAndWait(ctx, r.poolID, r.fork.ClusterID, r.fork.BranchID, r.fork.Endpoint.ID)
		return err
	}); err != nil {
		return err
	}
	return r.phase("delete_fork_branch", func() error {
		_, err := r.cl.DeleteBranchAndWait(ctx, r.poolID, r.fork.ClusterID, r.fork.BranchID)
		return err
	})
}

func (r *lifecycleRun) deleteCluster() error {
	r.closeConns()
	return r.phase("delete_cluster", func() error {
		_, err := r.cl.DeleteClusterAndWait(r.t.Context, r.poolID, r.parent.ClusterID)
		if err == nil {
			r.deleted = true
		}
		return err
	})
}

func gateLifecycle(t *core.T, p lifecycleParams, id Identity, results []phaseResult, sizes []float64,
	baseResults []phaseResult, tag string, baseErr error, logf func(string, ...any)) error {
	return t.Step("gate_lifecycle", func(s *core.StepHandle) error {
		t.Chart(phaseChart(medianPhases(results)))
		gated, info, ratioErr := lifecycleDatapoints(results, sizes, p)
		if ratio, ok := phaseRatio(results, "fork_loaded", "fork_small"); ok {
			s.Set("fork_size_ratio", fmt.Sprintf("%.2fx", ratio))
		}
		s.Set("iterations", max(p.Iterations, 1))
		cfg := perfFromConfig(t.Options)
		var lbErr error
		baselineUp := tag != "" && len(baseResults) > 0
		// A degraded run gates on history instead, so record why the arm is missing:
		// the reason otherwise lives only in the Jenkins console.
		if !baselineUp && baseErr != nil {
			s.Set("live_baseline", "arm failed, gated on history: "+baseErr.Error())
		}
		if baselineUp {
			baseGated, _, _ := lifecycleDatapoints(baseResults, nil, p)
			extra, gerr := liveGate(t, s, testNameLifecycle, id, gated, baseGated,
				lifecycleBestValues(results), lifecycleBestValues(baseResults),
				nil, tag, liveGateOpts{SingleStackPair: true}, logf)
			lbErr = gerr
			info = append(info, extra...)
		}
		gateErr := emitAndGateHistory(t, cfg, scaleVersion(t), testNameLifecycle, gated, info, id, !baselineUp, logf)
		return errors.Join(ratioErr, lbErr, gateErr)
	})
}

// trackedPhases are the phases worth a datapoint: what this test exists to
// answer, minus what perf/multitenant already gates better (cluster/endpoint
// create, first SQL) and the teardown plumbing. Every phase is still timed and
// visible as a step; only these are tracked over time.
var trackedPhases = []string{
	"fork_small", "fork_loaded", "load",
	"fork_count_cold", "fork_count_warm", "fork_insert",
	"delete_half", "count_after_delete", "drop_table", "delete_fork_branch",
}

// lifecycleBestValues is each tracked phase's best iteration, keyed like the
// gated datapoints — the live-arm gate's second opinion against node-lottery medians.
func lifecycleBestValues(results []phaseResult) map[string]float64 {
	out := map[string]float64{}
	for _, g := range trackedPhases {
		if vals := groupValues(results, g); len(vals) > 0 {
			out[lifecyclePrefix+g] = aggregate(vals).Min
		}
	}
	return out
}

// Gates the MEDIAN of each tracked phase, never one sample; the per-iteration
// timings live in phases.txt, so any spread stat can be derived report-side.
func lifecycleDatapoints(results []phaseResult, sizes []float64, p lifecycleParams) (gated, info []core.Datapoint, err error) {
	for _, g := range trackedPhases {
		vals := groupValues(results, g)
		if len(vals) == 0 {
			continue
		}
		st := aggregate(vals)
		gated = append(gated, core.Datapoint{Name: lifecyclePrefix + g, Value: st.Median, Unit: "ms",
			Direction: core.LowerIsBetter, SpreadPct: spreadPct(st)})
		if g == "load" && st.Median > 0 {
			info = append(info, core.Datapoint{Name: lifecyclePrefix + "load_rows_per_s", Value: float64(p.Rows) / (st.Median / 1000), Unit: "rows/s", Direction: core.HigherIsBetter})
		}
	}
	if len(sizes) > 0 {
		info = append(info, core.Datapoint{Name: lifecyclePrefix + "table_mb", Value: median(sizes), Unit: "MiB", Direction: core.LowerIsBetter})
	}
	ratio, ok := phaseRatio(results, "fork_loaded", "fork_small")
	if !ok {
		return gated, info, nil
	}
	info = append(info, core.Datapoint{Name: lifecyclePrefix + "fork_size_ratio", Value: ratio, Unit: "x", Direction: core.LowerIsBetter})
	if p.MaxForkRatio > 0 && ratio > p.MaxForkRatio {
		err = fmt.Errorf("forking the loaded branch (%d rows) took %.2fx the near-empty fork (limit %.2fx, median of %d iterations); see phases.txt",
			p.Rows, ratio, p.MaxForkRatio, len(groupValues(results, "fork_loaded")))
	}
	return gated, info, err
}

func phaseGroups(results []phaseResult) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range results {
		if !seen[r.Group] {
			seen[r.Group] = true
			out = append(out, r.Group)
		}
	}
	return out
}

// medianPhases collapses the iterations into one bar per phase for the chart.
func medianPhases(results []phaseResult) []phaseResult {
	var out []phaseResult
	for _, g := range phaseGroups(results) {
		out = append(out, phaseResult{Name: g, Group: g, MS: median(groupValues(results, g))})
	}
	return out
}

func lifecycleInsert(ctx context.Context, conn *pg.Client, from, to int64, batch, payload int) error {
	if batch < 1 {
		batch = 1
	}
	started, err := conn.QueryString(ctx, lifecyclePostmasterStart)
	if err != nil {
		return fmt.Errorf("read the postmaster start time before the load: %w", err)
	}
	stmt := fmt.Sprintf(
		"INSERT INTO %s (id, v, pad) SELECT g, g * 7, repeat('x', %d) FROM generate_series($1::bigint, $2::bigint) g ON CONFLICT (id) DO NOTHING",
		lifecycleTable, payload)
	for lo := from; lo <= to; lo += int64(batch) {
		hi := min(lo+int64(batch)-1, to)
		for drops := 0; ; drops++ {
			_, err := conn.Exec(ctx, stmt, lo, hi)
			if err == nil {
				break
			}
			if !core.IsConnDrop(err) || drops >= lifecycleMaxConnDrops {
				return fmt.Errorf("rows %d..%d: %w", lo, hi, err)
			}
			// The batch is idempotent, so a severed port-forward is retried, but only
			// if the same postmaster is still up: a compute restart stays a failure.
			if aerr := samePostmaster(ctx, conn, started); aerr != nil {
				return fmt.Errorf("rows %d..%d: %w (%v)", lo, hi, err, aerr)
			}
			log.Printf("perf/lifecycle: rows %d..%d: connection dropped with postmaster %s still up, retrying: %v", lo, hi, started, err)
		}
	}
	return nil
}

const (
	lifecyclePostmasterStart = "SELECT pg_postmaster_start_time()::text"
	lifecycleMaxConnDrops    = 3
)

func samePostmaster(ctx context.Context, conn *pg.Client, started string) error {
	var now string
	var err error
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if now, err = conn.QueryString(ctx, lifecyclePostmasterStart); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		return fmt.Errorf("postgres did not answer within 1m of the drop: %w", err)
	}
	if now != started {
		return fmt.Errorf("the postmaster restarted (started %s, now %s)", started, now)
	}
	return nil
}

func expectCount(ctx context.Context, conn *pg.Client, want int64) error {
	n, err := conn.QueryInt(ctx, "SELECT count(*) FROM "+lifecycleTable)
	if err != nil {
		return err
	}
	if n != want {
		return fmt.Errorf("count(*) = %d, want %d", n, want)
	}
	return nil
}

func findBranchByName(ctx context.Context, cl *controller.Client, poolID, clusterID, name string) (controller.Branch, error) {
	brs, err := cl.ListAllBranches(ctx, poolID, clusterID)
	if err != nil {
		return controller.Branch{}, err
	}
	for i := len(brs) - 1; i >= 0; i-- {
		if brs[i].Name == name {
			return brs[i], nil
		}
	}
	return controller.Branch{}, fmt.Errorf("branch %q is absent after a successful create", name)
}

// Script carries every knob without a numeric LoadFields slot, so runs that
// differ in batch size or the pause phases get their own LoadSignature.
func loadFieldsLifecycle(p lifecycleParams) LoadFields {
	script := fmt.Sprintf("lifecycle@%s/%s/b%d", p.EndpointCPU, p.EndpointMem, p.BatchRows)
	if p.PauseResume {
		script += "+pause"
	}
	return LoadFields{
		Script:     script,
		Rows:       p.Rows,
		Clients:    p.ForkWriteRows,
		Scale:      p.PayloadBytes,
		Iterations: p.Iterations,
		Harness:    lifecycleHarnessRev,
	}
}
