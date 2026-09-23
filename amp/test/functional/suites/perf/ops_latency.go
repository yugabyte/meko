package perf

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	testNameOpsLatency = "perf/ops_latency"
	opsPrefix          = "ops."
	opsTable           = "ops_seed"
	opsHarnessRev      = "1"
)

const (
	DefaultOpsRepeats      = 5
	DefaultOpsSeedRows     = 1000000
	DefaultOpsBatchRows    = 200000
	DefaultOpsPayloadBytes = 64
	DefaultOpsEndpointCPU  = "2"
	DefaultOpsEndpointMem  = "4Gi"
	DefaultOpsResizeCPU    = "1"
	DefaultOpsResizeMem    = "2Gi"
)

type opsParams struct {
	Repeats      int
	SeedRows     int
	BatchRows    int
	PayloadBytes int
	EndpointCPU  string
	EndpointMem  string
	ResizeCPU    string
	ResizeMem    string
}

func opsParamsFromConfig(c *core.Config) opsParams {
	return opsParams{
		Repeats:      max(c.GetInt("perf.ops_latency.repeats", DefaultOpsRepeats), 1),
		SeedRows:     c.GetInt("perf.ops_latency.seed_rows", DefaultOpsSeedRows),
		BatchRows:    c.GetInt("perf.ops_latency.batch_rows", DefaultOpsBatchRows),
		PayloadBytes: c.GetInt("perf.ops_latency.payload_bytes", DefaultOpsPayloadBytes),
		EndpointCPU:  c.GetString("perf.ops_latency.endpoint_cpu", DefaultOpsEndpointCPU),
		EndpointMem:  c.GetString("perf.ops_latency.endpoint_memory", DefaultOpsEndpointMem),
		ResizeCPU:    c.GetString("perf.ops_latency.resize_cpu", DefaultOpsResizeCPU),
		ResizeMem:    c.GetString("perf.ops_latency.resize_memory", DefaultOpsResizeMem),
	}
}

func init() {
	core.Register(core.Test{
		Name:       testNameOpsLatency,
		Suite:      "perf",
		Category:   core.Perf,
		Env:        core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:       []string{"perf"},
		OwningTeam: core.TeamControlPlane,
		Description: "Time every control-plane operation on fresh objects, perf.ops_latency.repeats times each: cluster create and delete, branch create off a near-empty parent and fork off a seeded one, branch delete, " +
			"endpoint create cold and warm, ready-to-first-SQL, resize, settings update, endpoint delete, and cancelling a just-planned endpoint create (request to terminal). Each operation gates the median of its repeats as an ops.<op> ms datapoint, with its coefficient of " +
			"variation and the controller's own task window emitted beside it. The seeded fork's first SQL is its own ops.endpoint_first_sql_seeded series rather than mixed into the empty branch's, and both carry the " +
			"harness cost of standing up the port-forward they connect through, so they move with the harness revision as well as with the product.",
		Params:  []string{"perf.ops_latency", "perf.gate"},
		Timeout: 4 * time.Hour,
		Run:     runOpsLatency,
	})
}

func runOpsLatency(t *core.T) error {
	env, ok := t.Env.(ampMultitenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	params := opsParamsFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)
	cl, err := env.AmpController(t.Context)
	if err != nil {
		return err
	}
	var kube *k8s.Kubectl
	if ke, ok := t.Env.(interface{ AmpKube() *k8s.Kubectl }); ok {
		kube = ke.AmpKube()
	}

	id := identityFromConfig(t.Options, "", frameworkCommit(), loadFieldsOps(params))
	t.SetPerfIdentity(id.core())

	warm := endpointImageNodes(t, kube, logf)
	var phases []phaseResult
	var nodes []string
	for i := 1; i <= params.Repeats; i++ {
		r := &opsRun{
			t: t, cl: cl, env: env, kube: kube, poolID: env.AmpPoolID(),
			params: params, iter: i, logf: logf, warmNodes: warm,
		}
		err := r.run()
		phases = append(phases, r.phases...)
		nodes = append(nodes, r.nodes...)
		if err != nil {
			attachPhases(t, phases)
			return fmt.Errorf("repeat %d/%d: %w%s", i, params.Repeats, err, schedulingHint(t, logf))
		}
	}
	attachPhases(t, phases)
	t.Chart(phaseChart(medianPhases(phases)))

	return t.Step("gate_ops_latency", func(s *core.StepHandle) error {
		s.Set("repeats", params.Repeats)
		if len(nodes) > 0 {
			s.Set("endpoint_nodes", strings.Join(nodes, " "))
		}
		gated, info := opsDatapoints(phases)
		s.Set("datapoints", len(gated))
		return emitAndGateAuto(t, perfFromConfig(t.Options), scaleVersion(t), testNameOpsLatency, gated, info, id, logf)
	})
}

func endpointImageNodes(t *core.T, kube *k8s.Kubectl, logf func(string, ...any)) map[string]bool {
	out := map[string]bool{}
	image := ampstack.FromConfig(t.Options).EndpointImage
	if kube == nil || image == "" {
		return out
	}
	listing, err := kube.GetSelector(t.Context, "node", "",
		`{range .items[*]}{.metadata.name}{"\t"}{range .status.images[*]}{.names[*]}{" "}{end}{"\n"}{end}`)
	if err != nil {
		logf("ops_latency: node image inventory unavailable (%v), the cold/warm split falls back to node reuse", err)
		return out
	}
	for _, line := range strings.Split(listing, "\n") {
		node, images, ok := strings.Cut(line, "\t")
		if ok && node != "" && strings.Contains(images, image) {
			out[node] = true
		}
	}
	return out
}

type opsRun struct {
	t         *core.T
	cl        *controller.Client
	env       ampMultitenantEnv
	kube      *k8s.Kubectl
	poolID    string
	params    opsParams
	iter      int
	logf      func(string, ...any)
	warmNodes map[string]bool

	clusterID  string
	branchID   string
	emptyFork  string
	seededFork string
	baseEP     controller.Endpoint
	forkEP     controller.Endpoint
	conn       *pg.Client
	deleted    bool

	phases []phaseResult
	nodes  []string
}

func (r *opsRun) run() error {
	defer r.cleanup()
	for _, fn := range []func() error{
		r.createCluster,
		r.createBaseEndpoint,
		r.branchOffNearEmpty,
		r.seed,
		r.forkSeeded,
		r.createForkEndpoint,
		r.resizeForkEndpoint,
		r.updateForkSettings,
		r.deleteForkEndpoint,
		r.cancelEndpointCreate,
		r.deleteCluster,
	} {
		if err := fn(); err != nil {
			return err
		}
	}
	return nil
}

func (r *opsRun) cleanup() {
	if r.conn != nil {
		r.conn.Close()
		r.conn = nil
	}
	if r.clusterID == "" || r.deleted {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if _, err := r.cl.DeleteClusterAndWait(ctx, r.poolID, r.clusterID); err != nil {
		r.logf("ops_latency: could not delete cluster %s: %v", r.clusterID, err)
	}
}

func (r *opsRun) record(group string, ms float64) {
	r.phases = append(r.phases, phaseResult{Name: group, Group: group, MS: ms})
	r.logf("ops_latency: [i%d] %s took %.1f ms", r.iter, group, ms)
}

func (r *opsRun) async(group string, call func() (controller.Task, string, error)) error {
	return r.step(group, func(s *core.StepHandle) error {
		start := time.Now()
		task, loc, err := call()
		r.record(group+"_api", msSince(start))
		if err != nil {
			return err
		}
		done, err := r.cl.WaitTask(r.t.Context, controller.TaskRef(task, loc))
		r.record(group, msSince(start))
		if err != nil {
			return err
		}
		if w := done.UpdatedAt.Sub(done.CreatedAt); w > 0 {
			r.record(group+"_task", float64(w.Microseconds())/1000)
		}
		s.Set("task", done.ID)
		return nil
	})
}

func (r *opsRun) sync(group string, call func() error) error {
	return r.step(group, func(*core.StepHandle) error {
		start := time.Now()
		err := call()
		if err != nil {
			return err
		}
		r.record(group, msSince(start))
		return nil
	})
}

func (r *opsRun) step(name string, fn func(*core.StepHandle) error) error {
	return r.t.Step(name, func(s *core.StepHandle) error {
		s.Set("repeat", r.iter)
		if err := fn(s); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	})
}

func (r *opsRun) createCluster() error {
	name := fmt.Sprintf("ops-%s-%d", r.t.ResourceSuffix(), r.iter)
	if err := r.async("cluster_create", func() (controller.Task, string, error) {
		return r.cl.CreateCluster(r.t.Context, r.poolID, controller.CreateClusterRequest{Name: name})
	}); err != nil {
		return err
	}
	cluster, err := findCluster(r.t.Context, r.cl, r.poolID, name)
	if err != nil {
		return err
	}
	r.clusterID, r.branchID = cluster.ID, cluster.DefaultBranch.BranchID
	return nil
}

func (r *opsRun) createEndpoint(group, branchID string) (controller.Endpoint, error) {
	if err := r.async(group, func() (controller.Task, string, error) {
		return r.cl.CreateEndpoint(r.t.Context, r.poolID, r.clusterID, branchID, controller.CreateEndpointRequest{
			Role:      "read_write",
			Resources: &controller.EndpointResources{CPU: r.params.EndpointCPU, Memory: r.params.EndpointMem},
		})
	}); err != nil {
		return controller.Endpoint{}, err
	}
	eps, err := r.cl.ListAllEndpoints(r.t.Context, r.poolID, r.clusterID, branchID)
	if err != nil {
		return controller.Endpoint{}, err
	}
	if len(eps) == 0 {
		return controller.Endpoint{}, fmt.Errorf("the branch lists no endpoint after a successful create")
	}
	return eps[len(eps)-1], nil
}

func (r *opsRun) classifyCreate(group, endpointID string) error {
	if r.kube == nil {
		return nil
	}
	node := r.endpointNode(endpointID)
	if node == "" {
		return fmt.Errorf("endpoint %s: no node resolved for the create, so it can be filed neither cold nor warm", endpointID)
	}
	r.nodes = append(r.nodes, node)
	suffix := "_cold"
	if r.warmNodes[node] {
		suffix = "_warm"
	}
	r.warmNodes[node] = true
	for i := range r.phases {
		if r.phases[i].Group == group {
			r.phases[i].Name, r.phases[i].Group = group+suffix, group+suffix
		}
	}
	return nil
}

func (r *opsRun) endpointNode(endpointID string) string {
	if r.kube == nil {
		return ""
	}
	out, err := r.kube.GetSelector(r.t.Context, "pod",
		"app.kubernetes.io/instance="+ampstack.EndpointWorkloadName(endpointID), "{.items[0].spec.nodeName}")
	if err != nil {
		r.logf("ops_latency: node of endpoint %s unknown (%v)", endpointID, err)
		return ""
	}
	return strings.TrimSpace(out)
}

func (r *opsRun) firstSQL(group string, ep controller.Endpoint) (*pg.Client, error) {
	var conn *pg.Client
	err := r.step(group, func(*core.StepHandle) error {
		start := time.Now()
		c, err := r.env.ConnectEndpoint(r.t.Context, ep)
		if err != nil {
			return err
		}
		if _, err := c.QueryInt(r.t.Context, "SELECT 1"); err != nil {
			c.Close()
			return err
		}
		r.record(group, msSince(start))
		conn = c
		return nil
	})
	return conn, err
}

func (r *opsRun) createBaseEndpoint() error {
	ep, err := r.createEndpoint("endpoint_create", r.branchID)
	if err != nil {
		return err
	}
	r.baseEP = ep
	conn, err := r.firstSQL("endpoint_first_sql", ep)
	if err != nil {
		return err
	}
	r.conn = conn
	return r.classifyCreate("endpoint_create", ep.ID)
}

func (r *opsRun) branchOffNearEmpty() error {
	ctx := r.t.Context
	if _, err := r.conn.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s (id bigint PRIMARY KEY, v bigint NOT NULL, pad text)", opsTable)); err != nil {
		return err
	}
	if _, err := r.conn.Exec(ctx, "INSERT INTO "+opsTable+" VALUES (0, 0, '') ON CONFLICT (id) DO NOTHING"); err != nil {
		return err
	}
	name := "ops-empty-" + r.t.ResourceSuffix()
	if err := r.async("branch_create_empty", func() (controller.Task, string, error) {
		return r.cl.CreateBranch(ctx, r.poolID, r.clusterID, controller.CreateBranchRequest{
			Name: name, ParentBranchID: r.branchID})
	}); err != nil {
		return err
	}
	br, err := findBranchByName(ctx, r.cl, r.poolID, r.clusterID, name)
	if err != nil {
		return err
	}
	r.emptyFork = br.BranchID
	return r.async("branch_delete", func() (controller.Task, string, error) {
		return r.cl.DeleteBranch(ctx, r.poolID, r.clusterID, r.emptyFork)
	})
}

func (r *opsRun) seed() error {
	p := r.params
	return r.step("seed", func(s *core.StepHandle) error {
		s.Set("rows", p.SeedRows)
		return opsInsert(r.t.Context, r.conn, 1, int64(p.SeedRows), p.BatchRows, p.PayloadBytes)
	})
}

func (r *opsRun) forkSeeded() error {
	ctx := r.t.Context
	name := "ops-seeded-" + r.t.ResourceSuffix()
	if err := r.async("branch_fork_seeded", func() (controller.Task, string, error) {
		return r.cl.CreateBranch(ctx, r.poolID, r.clusterID, controller.CreateBranchRequest{
			Name: name, ParentBranchID: r.branchID})
	}); err != nil {
		return err
	}
	br, err := findBranchByName(ctx, r.cl, r.poolID, r.clusterID, name)
	if err != nil {
		return err
	}
	r.seededFork = br.BranchID
	return nil
}

func (r *opsRun) createForkEndpoint() error {
	ep, err := r.createEndpoint("endpoint_create", r.seededFork)
	if err != nil {
		return err
	}
	r.forkEP = ep
	conn, err := r.firstSQL("endpoint_first_sql_seeded", ep)
	if err != nil {
		return err
	}
	conn.Close()
	return r.classifyCreate("endpoint_create", ep.ID)
}

func (r *opsRun) resizeForkEndpoint() error {
	return r.async("endpoint_resize", func() (controller.Task, string, error) {
		return r.cl.ResizeEndpoint(r.t.Context, r.poolID, r.clusterID, r.seededFork, r.forkEP.ID,
			controller.ResizeEndpointRequest{Resources: controller.EndpointResources{
				CPU: r.params.ResizeCPU, Memory: r.params.ResizeMem}})
	})
}

func (r *opsRun) updateForkSettings() error {
	settings := map[string]string{"work_mem": "32MB"}
	return r.sync("endpoint_settings", func() error {
		_, err := r.cl.UpdateEndpointSettings(r.t.Context, r.poolID, r.clusterID, r.seededFork, r.forkEP.ID,
			controller.UpdateSettingsRequest{PgSettings: &settings})
		return err
	})
}

func (r *opsRun) deleteForkEndpoint() error {
	return r.async("endpoint_delete", func() (controller.Task, string, error) {
		return r.cl.DeleteEndpoint(r.t.Context, r.poolID, r.clusterID, r.seededFork, r.forkEP.ID)
	})
}

// A cancel that loses the race records no sample: it timed a create, not a cancel.
func (r *opsRun) cancelEndpointCreate() error {
	ctx := r.t.Context
	return r.step("task_cancel", func(s *core.StepHandle) error {
		task, loc, err := r.cl.CreateEndpoint(ctx, r.poolID, r.clusterID, r.seededFork, controller.CreateEndpointRequest{
			Role:      "read_write",
			Resources: &controller.EndpointResources{CPU: r.params.EndpointCPU, Memory: r.params.EndpointMem},
		})
		if err != nil {
			return err
		}
		ref := controller.TaskRef(task, loc)
		start := time.Now()
		_, cerr := r.cl.CancelTask(ctx, ref)
		done, werr := r.cl.WaitTask(ctx, ref)
		ms := msSince(start)
		s.Set("task", ref)
		switch {
		case done.Status == controller.TaskFailed && done.CancelRequestedAt != nil:
			r.record("task_cancel", ms)
		case done.Status == controller.TaskSucceeded:
			r.logf("ops_latency: [i%d] the cancel of create %s lost the race (%v)", r.iter, ref, cerr)
		default:
			return fmt.Errorf("cancelled create %s ended %q: cancel %v, wait %v", ref, done.Status, cerr, werr)
		}
		eps, err := r.cl.ListAllEndpoints(ctx, r.poolID, r.clusterID, r.seededFork)
		if err != nil {
			return err
		}
		for _, ep := range eps {
			if _, err := r.cl.DeleteEndpointAndWait(ctx, r.poolID, r.clusterID, r.seededFork, ep.ID); err != nil {
				return fmt.Errorf("delete endpoint %s the cancel left: %w", ep.ID, err)
			}
		}
		return nil
	})
}

func (r *opsRun) deleteCluster() error {
	if r.conn != nil {
		r.conn.Close()
		r.conn = nil
	}
	if err := r.async("cluster_delete", func() (controller.Task, string, error) {
		return r.cl.DeleteCluster(r.t.Context, r.poolID, r.clusterID)
	}); err != nil {
		return err
	}
	r.deleted = true
	return nil
}

func opsInsert(ctx context.Context, conn *pg.Client, from, to int64, batch, payload int) error {
	if batch < 1 {
		batch = 1
	}
	for lo := from; lo <= to; lo += int64(batch) {
		hi := min(lo+int64(batch)-1, to)
		if _, err := conn.Exec(ctx, fmt.Sprintf(
			"INSERT INTO %s (id, v, pad) SELECT g, g * 7, repeat('x', %d) FROM generate_series($1::bigint, $2::bigint) g ON CONFLICT (id) DO NOTHING",
			opsTable, payload), lo, hi); err != nil {
			return fmt.Errorf("rows %d..%d: %w", lo, hi, err)
		}
	}
	return nil
}

func opsDatapoints(results []phaseResult) (gated, info []core.Datapoint) {
	for _, g := range phaseGroups(results) {
		vals := groupValues(results, g)
		if len(vals) == 0 {
			continue
		}
		st := aggregate(vals)
		dp := core.Datapoint{Name: opsPrefix + g, Value: st.Median, Unit: "ms",
			Direction: core.LowerIsBetter, SpreadPct: spreadPct(st)}
		if strings.HasSuffix(g, "_task") || g == "seed" {
			info = append(info, dp)
			continue
		}
		gated = append(gated, dp)
		info = append(info, core.Datapoint{Name: opsPrefix + g + ".cv", Value: st.CV,
			Unit: "x", Direction: core.LowerIsBetter})
	}
	return gated, info
}

func loadFieldsOps(p opsParams) LoadFields {
	return LoadFields{
		Script:     fmt.Sprintf("ops_latency@%s/%s>%s/%s/b%d", p.EndpointCPU, p.EndpointMem, p.ResizeCPU, p.ResizeMem, p.BatchRows),
		Iterations: p.Repeats,
		Rows:       p.SeedRows,
		Scale:      p.PayloadBytes,
		Harness:    opsHarnessRev,
	}
}
