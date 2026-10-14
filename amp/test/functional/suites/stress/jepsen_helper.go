// Harness for the jepsen suite: the runner pods, the port aliases that let
// jepsen dial an amp endpoint at all, the pre-nemesis DDL gate, and the two
// fault loops.
package stress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/apps/clientpod"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/jepsen"
	"github.com/yugabyte/amp/test/functional/framework/chaos"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

type jepsenParams struct {
	Workloads []string
	// Tenants is how many endpoints jepsen runs against at once, one runner pod
	// and one independent history each. One endpoint is one compute, and one
	// compute saturates long before the backing does, so load on the backing
	// scales by adding endpoints, not threads.
	Tenants    int
	UseNemesis bool
	// EndpointActions and BackingActions are separate because a chaos action
	// carries ONE process pattern for the whole loop: chaos.process_pattern is
	// "yb-tserver" and chaos.stall.pattern is "endpoint-supervisor", so a single
	// runner over both pod sets would aim half its actions at a process that is
	// not there. Two runners, each with the actions its targets actually have.
	EndpointActions []string
	BackingActions  []string
	// RunnersPerGroup is how many fault loops run over each pod group at once.
	// The group's pods are split between them, so two loops never hold faults
	// on the same pod and one recovery cannot undo another loop's fault.
	RunnersPerGroup int
	// Hold and gap override the global chaos.* bounds for this suite only.
	HoldMin, HoldMax time.Duration
	GapMin, GapMax   time.Duration

	DDLGateTimeout time.Duration
	DDLGatePoll    time.Duration
	Settle         time.Duration
	FaultWindow    time.Duration
	// BackingSettle bounds the backing serving again after a fault window.
	BackingSettle time.Duration

	LeaseRace leaseRaceParams
}

func jepsenParamsFromConfig(c *core.Config) jepsenParams {
	return jepsenParams{
		Workloads:  c.GetStringSlice("stress.jepsen.workloads", nil),
		Tenants:    c.GetInt("stress.jepsen.tenants", 4),
		UseNemesis: c.GetBool("stress.jepsen.use_nemesis", true),
		EndpointActions: c.GetStringSlice("stress.jepsen.endpoint_actions",
			[]string{chaos.ActionPodDelete, chaos.ActionProcessStall, chaos.ActionPodFreeze}),
		BackingActions: c.GetStringSlice("stress.jepsen.backing_actions",
			[]string{chaos.ActionPodDelete, chaos.ActionProcessKill, chaos.ActionProcessRestart,
				chaos.ActionNetworkPartition, chaos.ActionNetworkSlowdown, chaos.ActionPacketLoss}),
		RunnersPerGroup: c.GetInt("stress.jepsen.chaos.runners_per_group", 2),
		HoldMin:         c.GetDuration("stress.jepsen.chaos.hold_min", 10*time.Second),
		HoldMax:         c.GetDuration("stress.jepsen.chaos.hold_max", 30*time.Second),
		GapMin:          c.GetDuration("stress.jepsen.chaos.gap_min", 5*time.Second),
		GapMax:          c.GetDuration("stress.jepsen.chaos.gap_max", 15*time.Second),
		DDLGateTimeout:  c.GetDuration("stress.jepsen.ddl_gate.timeout", 10*time.Minute),
		DDLGatePoll:     c.GetDuration("stress.jepsen.ddl_gate.poll_interval", 2*time.Second),
		Settle:          c.GetDuration("stress.jepsen.ddl_gate.settle", 45*time.Second),
		FaultWindow:     c.GetDuration("stress.jepsen.fault_window", 0),
		BackingSettle:   c.GetDuration("stress.jepsen.backing_settle_timeout", 10*time.Minute),
		LeaseRace:       leaseRaceParamsFromConfig(c),
	}
}

// selectWorkloads resolves the configured filters against the catalog. Empty →
// every YSQL workload. A YCQL entry is dropped with its reason recorded: amp's
// compute speaks Postgres, so those workloads have nothing here to run against.
func selectWorkloads(filters []string) (sel []jepsen.Workload, dropped []string) {
	if len(filters) == 0 {
		return jepsen.YSQL(), nil
	}
	seen := map[jepsen.Workload]bool{}
	for _, f := range filters {
		matches := jepsen.Filter(strings.TrimSpace(f))
		if len(matches) == 0 {
			dropped = append(dropped, f+" (matches no workload)")
			continue
		}
		for _, w := range matches {
			if strings.HasPrefix(string(w), "ycql/") {
				dropped = append(dropped, string(w)+" (YCQL: amp compute speaks Postgres)")
				continue
			}
			if !seen[w] {
				seen[w] = true
				sel = append(sel, w)
			}
		}
	}
	return sel, dropped
}

// jepsenRig is everything one tenant's jepsen run needs, built once and reused
// across workloads: the runner pod and the alias Service. There is one rig per
// tenant: jepsen names its log and store by workload, so two runs in one pod
// would write over each other.
type jepsenRig struct {
	kube    *k8s.Kubectl
	pod     *clientpod.K8s
	podName string
	podExec *k8s.Kubectl
	// aliases are the jepsen "nodes": one alias Service per tenant, over the
	// endpoint's own selector. Under the lease race it reaches both pods.
	aliases []string
	// backingKube is the backing YugabyteDB's own namespace, when the env has
	// one. Nil means the fault loop aims at the endpoint computes only.
	backingKube *k8s.Kubectl
	logf        func(string, ...any)
}

// jepsenAliasPort is the port jepsen dials. It is NOT configurable, because
// jepsen does not make it configurable: yugabyte/jepsen hardcodes the YSQL port
// at 5433 (src/yugabyte/ysql/client.clj). An amp endpoint's Service publishes
// the compute's own port instead, so the alias below is a second Service over
// the SAME selector, republishing that port as 5433. This is the whole reason
// the alias exists — without it jepsen cannot reach an amp endpoint at all.
const jepsenAliasPort = 5433

// newJepsenRig launches the runner pod for tenant index. The rig comes back
// even when the launch fails: a failed wait still leaves a pod that the rig's
// Close has to delete.
func newJepsenRig(t *core.T, kube *k8s.Kubectl, cfg jepsen.Config, index int, logf func(string, ...any)) (*jepsenRig, error) {
	name := fmt.Sprintf("ampqa-jepsen-%s-%d", t.ResourceSuffix(), index)
	cp := clientpod.NewK8s(kube)
	r := &jepsenRig{kube: kube, pod: cp, podName: name, podExec: kube.ForPod(name, ""), logf: logf}
	if be, ok := t.Env.(backingKubeEnv); ok {
		r.backingKube = be.BackingKube()
	}
	if err := cp.Start(t.Context, clientpod.Spec{
		Name:  name,
		Image: cfg.Image,
		// The pod is a shell the framework execs into; jepsen itself is launched
		// per workload, so the container must simply stay up.
		Command: []string{"sh", "-c", "sleep infinity"},
	}); err != nil {
		return r, core.AsEnvFailure(fmt.Errorf("start jepsen runner pod %s: %w", name, err))
	}
	return r, nil
}

// jepsenAliasName names tenant's alias Service, jepsen's one node.
func jepsenAliasName(suffix string, tenant int) string {
	return fmt.Sprintf("ampqa-jepsen-ep-%s-%d", suffix, tenant)
}

// PointAlias publishes the endpoint's compute at port 5433 under alias name,
// reusing the product's OWN Service selector rather than guessing labels, so
// the alias keeps reaching the endpoint across pod replacements. It creates
// the alias, or retargets it when it exists.
func (r *jepsenRig) PointAlias(ctx context.Context, name string, ep controller.Endpoint) error {
	src := "yb-ep-" + ep.ID
	selector, err := r.kube.Get(ctx, "svc", src, "{.spec.selector}")
	if err != nil {
		return fmt.Errorf("read the selector of the endpoint's own Service %s: %w", src, err)
	}
	targetPort, err := r.kube.Get(ctx, "svc", src, "{.spec.ports[0].targetPort}")
	if err != nil {
		return fmt.Errorf("read the target port of the endpoint's own Service %s: %w", src, err)
	}
	if strings.TrimSpace(targetPort) == "" {
		return fmt.Errorf("the endpoint's own Service %s has no target port", src)
	}
	sel, err := parseJSONStringMap(selector)
	if err != nil {
		return fmt.Errorf("parse the selector of %s (%q): %w", src, selector, err)
	}
	if len(sel) == 0 {
		return fmt.Errorf("the endpoint's own Service %s has no selector, so an alias over it would select every pod in the namespace", src)
	}
	if err := r.kube.Apply(ctx, jepsenAliasManifest(name, sel, strings.TrimSpace(targetPort))); err != nil {
		return fmt.Errorf("apply the jepsen endpoint alias Service %s: %w", name, err)
	}
	for _, a := range r.aliases {
		if a == name {
			return nil
		}
	}
	r.aliases = append(r.aliases, name)
	return nil
}

// jepsenAliasManifest renders the alias Service: the endpoint's own selector,
// republished on the port jepsen hardcodes.
func jepsenAliasManifest(name string, selector map[string]string, targetPort string) string {
	var sel strings.Builder
	for _, k := range sortedMapKeys(selector) {
		fmt.Fprintf(&sel, "\n    %s: %q", k, selector[k])
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  labels:
    ampqa: "1"
spec:
  type: ClusterIP
  selector:%s
  ports:
    - name: ysql
      port: %d
      targetPort: %s
`, name, sel.String(), jepsenAliasPort, targetPort)
}

// AppFor builds the jepsen App for one workload on this rig.
func (r *jepsenRig) AppFor(w jepsen.Workload, cfg jepsen.Config, seedMarker string) *jepsen.App {
	concurrency, limit := cfg.Shape(w)
	return jepsen.New(r.podExec, jepsen.RunSpec{
		Workload:    w,
		Nodes:       append([]string(nil), r.aliases...),
		Concurrency: concurrency,
		TimeLimit:   limit,
		RepoDir:     cfg.RepoDir,
		NoSSH:       true,
		Marker:      seedMarker,
	},
		jepsen.WithLogf(r.logf),
		jepsen.WithSetupTimeout(cfg.SetupTimeout),
		jepsen.WithAnalysisTimeout(cfg.AnalysisTimeout),
		jepsen.WithPollInterval(cfg.PollInterval),
		jepsen.WithCollectMaxBytes(cfg.CollectMaxBytes),
	)
}

// Close collects the runner pod's own logs and tears the rig down, on an
// independent deadline: the test context is usually already cancelled here.
func (r *jepsenRig) Close(sink apps.ArtifactSink) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, a := range r.aliases {
		if err := r.kube.Delete(ctx, "svc", a); err != nil && r.logf != nil {
			r.logf("jepsen: delete alias service %s: %v", a, err)
		}
	}
	if r.pod != nil {
		if err := r.pod.Stop(ctx, sink); err != nil && r.logf != nil {
			r.logf("jepsen: stop runner pod: %v", err)
		}
	}
}

// awaitDDLGate is the barrier every fault window in this suite opens with.
//
// A fault injected while jepsen is still creating its schema fails the run for
// a table that never got created — and that failure is indistinguishable from a
// real consistency finding, which is what makes it expensive. Three conditions,
// all required, then a settle grace on top:
//
//  1. every jepsen instance logged "Setup successful" (its own DDL and
//     initial data are done);
//  2. pg_stat_activity on every endpoint shows no DDL still running — the log line
//     says jepsen's setup returned, not that the catalog change behind it has
//     drained;
//  3. the control plane holds no non-terminal task — a branch or endpoint task
//     still running is amp mid-change, and faulting that is a different test.
//
// Every failure here is environmental: no fault has been injected yet, so
// nothing observed can be attributed to the product's fault behaviour.
func awaitDDLGate(t *core.T, s *core.StepHandle, jobs []*jepsen.App, fleet *tenantFleet,
	env ampTenantEnv, p jepsenParams) error {

	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i, app := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := app.WaitReady(t.Context); err != nil {
				errs[i] = fmt.Errorf("tenant %d: jepsen setup never completed, so there is nothing steady to fault: %w", i, err)
				return
			}
			if err := fleet.Tenants[i].Conn.AwaitNoDDL(t.Context, p.DDLGateTimeout, p.DDLGatePoll); err != nil {
				errs[i] = fmt.Errorf("tenant %d: the endpoint is still running DDL after jepsen reported setup complete: %w", i, err)
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return core.AsEnvFailure(err)
	}
	s.Set("gate_setup_ok", true)
	s.Set("gate_no_ddl", true)

	if err := awaitNoPendingTasks(t, env, p.DDLGateTimeout); err != nil {
		return core.AsEnvFailure(err)
	}
	s.Set("gate_no_tasks", true)

	// The first seconds after setup are schema caches warming, not steady state.
	select {
	case <-t.Context.Done():
		return t.Context.Err()
	case <-time.After(p.Settle):
	}
	s.Set("gate_settle", p.Settle.String())
	return nil
}

// awaitNoPendingTasks blocks until the control plane reports no pending or
// running task.
func awaitNoPendingTasks(t *core.T, env ampTenantEnv, timeout time.Duration) error {
	cl, err := env.AmpController(t.Context)
	if err != nil {
		return err
	}
	var live []string
	var listErr error
	err = t.WaitFor("every control-plane task to reach a terminal status", timeout, 5*time.Second,
		func() (bool, error) {
			live = live[:0]
			for _, status := range []string{controller.TaskPending, controller.TaskRunning} {
				list, lerr := cl.ListTasks(t.Context, controller.ListTasksOptions{Limit: 500, Status: status})
				if lerr != nil {
					listErr = lerr
					return false, nil
				}
				for _, task := range list.Tasks {
					live = append(live, fmt.Sprintf("%s(%s,%s)", task.ID, task.Kind, task.Status))
				}
			}
			listErr = nil
			return len(live) == 0, nil
		})
	if err != nil && listErr != nil {
		return fmt.Errorf("could not list the control plane's tasks within %s: %w", timeout, listErr)
	}
	if err != nil {
		return fmt.Errorf("the control plane still has %d non-terminal task(s) after %s, so amp is mid-change and a fault now would not be testing the workload: %s",
			len(live), timeout, strings.Join(live, ", "))
	}
	return nil
}

// faultRun is what runJepsenFaults did in one window.
type faultRun struct {
	Injects      int
	InjectErrors []string
	Recovers     []string
	Targets      []string
	// BackingSettled is how long the backing took to serve again after the
	// window; zero when nothing faulted it.
	BackingSettled time.Duration
}

// hasFaultTargets reports whether runJepsenFaults has anything to fault.
func hasFaultTargets(p jepsenParams, backingKube *k8s.Kubectl) bool {
	return len(p.EndpointActions) > 0 || (len(p.BackingActions) > 0 && backingKube != nil)
}

// runJepsenFaults runs fault loops for d, RunnersPerGroup over the tenants'
// endpoints and RunnersPerGroup over the backing's serving pods, then waits
// for the backing to serve again. It reports how many faults actually fired.
// A window that injected nothing is a failure: it proves nothing, and a green
// verdict from it would be read as one.

func runJepsenFaults(t *core.T, kube, backingKube *k8s.Kubectl, fleet *tenantFleet,
	p jepsenParams, d time.Duration, seedName string, logf func(string, ...any)) (faultRun, error) {

	var out faultRun
	var mu sync.Mutex
	observe := func(e chaos.Event) {
		mu.Lock()
		// Only a fault that landed counts: an action the target cannot run (no
		// tc in the image, say) must not make an empty window look faulted.
		if e.Phase == "inject" && e.Err == nil {
			out.Injects++
		}
		if e.Phase == "inject" && e.Err != nil {
			out.InjectErrors = append(out.InjectErrors, fmt.Sprintf("%s on %s: %v", e.Action, e.Target, e.Err))
		}
		if e.Phase == "recover" && e.Err != nil {
			out.Recovers = append(out.Recovers, fmt.Sprintf("%s on %s: %v", e.Action, e.Target, e.Err))
		}
		mu.Unlock()
		if e.Err != nil {
			logf("chaos %s %s on %s: %v", e.Phase, e.Action, e.Target, e.Err)
		}
	}

	var runners []*chaos.Runner
	stopped := false
	stopAll := func() {
		if !stopped {
			stopped = true
			for _, r := range runners {
				r.Stop()
			}
		}
	}
	defer stopAll()
	start := func(pods []string, actions []string, kb *k8s.Kubectl, reg *chaos.Registry, fork string) error {
		if len(pods) == 0 || len(actions) == 0 {
			return nil
		}
		for k, share := range splitPods(pods, p.RunnersPerGroup) {
			targets := make([]*chaos.Target, 0, len(share))
			for _, pod := range share {
				targets = append(targets, chaos.NewPodTarget(pod, kb, pod, ""))
			}
			cfg := chaos.FromConfig(t.Options)
			cfg.Actions = actions
			cfg.Observe = observe
			cfg.HoldMin, cfg.HoldMax = p.HoldMin, p.HoldMax
			cfg.GapMin, cfg.GapMax = p.GapMin, p.GapMax
			runner, err := chaos.NewRunner(t.Seed.Fork(fmt.Sprintf("%s/%s/%d", seedName, fork, k)),
				reg, cfg, targets)
			if err != nil {
				return err
			}
			if err := runner.Start(); err != nil {
				return err
			}
			runners = append(runners, runner)
		}
		out.Targets = append(out.Targets, pods...)
		return nil
	}

	// Endpoint targets are the endpoints, not their pods: a deleted pod comes
	// back under a new name, so endpointRegistry resolves the current pod at
	// each inject.
	var endpoints []string
	for _, ten := range fleet.Tenants {
		endpoints = append(endpoints, endpointPodPrefix+ten.Endpoint.ID+"-")
	}
	if err := start(endpoints, p.EndpointActions, kube, endpointRegistry(netnsRegistry(t, kube), kube), "endpoint"); err != nil {
		return out, err
	}
	backingFaulted := false
	if backingKube != nil && len(p.BackingActions) > 0 {
		bPods, berr := backingPods(t, backingKube)
		if berr != nil {
			logf("jepsen: backing pods not enumerable, faulting the endpoint only: %v", berr)
		} else {
			if err := start(bPods, p.BackingActions, backingKube, netnsRegistry(t, backingKube), "backing"); err != nil {
				return out, err
			}
			backingFaulted = len(bPods) > 0
		}
	}

	if len(runners) == 0 {
		return out, fmt.Errorf("no pod could be targeted, so this fault window would prove nothing")
	}

	select {
	case <-t.Context.Done():
	case <-time.After(d):
	}
	stopAll()

	// The next workload must not start on a backing that is still down or has
	// leaderless tablets (meko#1366): its reset would fail as env damage, or its
	// faults would pile onto an unhealed quorum.
	if backingFaulted {
		settleStart := time.Now()
		if _, err := awaitBackingReady(t, backingKube, p.BackingSettle); err != nil {
			return out, core.AsEnvFailure(fmt.Errorf("the backing did not serve again after the fault window: %w", err))
		}
		if _, err := awaitBackingTabletsLed(t, backingKube, p.BackingSettle, logf); err != nil {
			return out, core.AsEnvFailure(fmt.Errorf("the backing's tablets did not all regain a leader after the fault window: %w", err))
		}
		out.BackingSettled = time.Since(settleStart)
	}

	mu.Lock()
	defer mu.Unlock()
	if out.Injects == 0 {
		return out, fmt.Errorf("the chaos loop injected no fault in %s, so this run proves nothing", d)
	}
	return out, nil
}

// endpointRegistry wraps every action of base so that a target named by an
// endpoint's pod prefix hits whichever pod serves that endpoint at inject
// time.
func endpointRegistry(base *chaos.Registry, kube *k8s.Kubectl) *chaos.Registry {
	r := chaos.NewRegistry()
	for _, name := range base.Names() {
		r.Register(name, func(c chaos.Config) chaos.ChaosAction {
			inner, err := base.Build(name, c)
			if err != nil {
				return &endpointAction{name: name, buildErr: err}
			}
			return &endpointAction{name: name, inner: inner, kube: kube, recoverTimeout: c.RecoverTimeout,
				active: map[string]*chaos.Target{}}
		})
	}
	return r
}

// endpointAction runs inner against the pod serving an endpoint now. The
// target's name is the endpoint's pod prefix (yb-ep-<id>-).
type endpointAction struct {
	name           string
	inner          chaos.ChaosAction
	buildErr       error
	kube           *k8s.Kubectl
	recoverTimeout time.Duration
	mu             sync.Mutex
	active         map[string]*chaos.Target
}

func (a *endpointAction) Name() string { return a.name }

func (a *endpointAction) Inject(ctx context.Context, tg *chaos.Target) error {
	if a.buildErr != nil {
		return a.buildErr
	}
	pod, err := endpointReadyPod(ctx, a.kube, tg.Name())
	if err != nil {
		return err
	}
	pt := chaos.NewPodTarget(pod, a.kube, pod, "")
	if err := a.inner.Inject(ctx, pt); err != nil {
		return err
	}
	a.mu.Lock()
	a.active[tg.Name()] = pt
	a.mu.Unlock()
	return nil
}

// Recover undoes the inject. A deleted pod is not coming back under its name,
// so pod_delete's recovery is the endpoint having a Ready pod again.
func (a *endpointAction) Recover(ctx context.Context, tg *chaos.Target) error {
	a.mu.Lock()
	pt := a.active[tg.Name()]
	delete(a.active, tg.Name())
	a.mu.Unlock()
	if pt == nil {
		return nil
	}
	if a.name != chaos.ActionPodDelete {
		return a.inner.Recover(ctx, pt)
	}
	timeout := a.recoverTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for {
		pod, err := endpointReadyPod(ctx, a.kube, tg.Name())
		if err == nil && pod != pt.Name() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("endpoint %s has no Ready replacement for deleted pod %s within %s", tg.Name(), pt.Name(), timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// endpointReadyPod is the one Ready pod whose name starts with prefix.
func endpointReadyPod(ctx context.Context, kube *k8s.Kubectl, prefix string) (string, error) {
	pods, err := kube.ListPods(ctx)
	if err != nil {
		return "", err
	}
	var ready []string
	for _, pn := range pods {
		if !strings.HasPrefix(pn, prefix) {
			continue
		}
		st, err := kube.Get(ctx, "pod", pn, `{.status.conditions[?(@.type=="Ready")].status}`)
		if err == nil && strings.TrimSpace(st) == "True" {
			ready = append(ready, pn)
		}
	}
	if len(ready) != 1 {
		return "", fmt.Errorf("endpoint %s: want one Ready pod to fault, found %v", prefix, ready)
	}
	return ready[0], nil
}

// splitPods deals pods round-robin into at most n disjoint shares, one per
// fault loop. Fewer pods than loops gives fewer loops: a loop with no target
// would fail to start.
func splitPods(pods []string, n int) [][]string {
	if n < 1 {
		n = 1
	}
	if n > len(pods) {
		n = len(pods)
	}
	shares := make([][]string, n)
	for i, pod := range pods {
		shares[i%n] = append(shares[i%n], pod)
	}
	return shares
}

// resetAttempt bounds one reset on one connection before the tenant reconnects.
const resetAttempt = time.Minute

// resetTenantDatabase resets jepsen's database, reconnecting when an attempt
// fails. A lease takeover replaces the endpoint pod, and the tenant's connection
// runs through a kubectl port-forward that stays bound to the pod it started on,
// so retrying on that connection fails until the deadline (run c1291e18).
func resetTenantDatabase(ctx context.Context, env ampTenantEnv, ten *tenant, timeout time.Duration) error {
	return resetReconnecting(ctx, timeout, resetAttempt,
		func() jepsen.SQLExecutor { return ten.Conn },
		func(ctx context.Context) error {
			conn, err := env.ConnectEndpoint(ctx, ten.Endpoint)
			if err != nil {
				return err
			}
			ten.Conn.Close()
			ten.Conn = conn
			return nil
		})
}

func resetReconnecting(ctx context.Context, timeout, attempt time.Duration,
	conn func() jepsen.SQLExecutor, reconnect func(context.Context) error) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return last
		}
		// ResetDatabase checks its timeout only between statements, so a statement
		// hung on a stranded connection is bounded by this context instead.
		actx, cancel := context.WithTimeout(ctx, min(attempt, left))
		err := jepsen.ResetDatabase(actx, conn(), min(attempt, left))
		cancel()
		if err == nil {
			return nil
		}
		last = err
		if ctx.Err() != nil {
			return err
		}
		rctx, cancel := context.WithDeadline(ctx, deadline)
		if rerr := reconnect(rctx); rerr != nil {
			last = fmt.Errorf("%w; reconnect: %v", err, rerr)
		}
		cancel()
	}
}

// eachRig runs fn on every rig at once and joins their errors.
func eachRig(rigs []*jepsenRig, fn func(i int, r *jepsenRig) error) error {
	errs := make([]error, len(rigs))
	var wg sync.WaitGroup
	for i, r := range rigs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(i, r); err != nil {
				errs[i] = fmt.Errorf("tenant %d: %w", i, err)
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// parseJSONStringMap reads the flat {"k":"v"} object kubectl's jsonpath emits
// for a Service selector.
func parseJSONStringMap(s string) (map[string]string, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "{}" {
		return nil, nil
	}
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, fmt.Errorf("not a JSON object")
	}
	out := map[string]string{}
	for _, pair := range splitTopLevel(s[1 : len(s)-1]) {
		k, v, ok := strings.Cut(pair, ":")
		if !ok {
			return nil, fmt.Errorf("malformed entry %q", pair)
		}
		key := strings.Trim(strings.TrimSpace(k), `"`)
		val := strings.Trim(strings.TrimSpace(v), `"`)
		if key == "" {
			return nil, fmt.Errorf("empty key in %q", pair)
		}
		out[key] = val
	}
	return out, nil
}

// splitTopLevel splits on commas outside quotes. A label value cannot contain a
// comma, but a quoted one would break a naive split and silently drop a
// selector term — which would widen the alias to other pods.
func splitTopLevel(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}

func sortedMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// prefixSink files every artifact under prefix.
type prefixSink struct {
	inner  apps.ArtifactSink
	prefix string
}

func (p prefixSink) Add(name, kind string, content []byte) error {
	return p.inner.Add(p.prefix+name, kind, content)
}

// tenantSink files one tenant's jepsen artifacts under tenant<i>/ (and
// iter<n>/ when a workload repeats). Every tenant collects under the same
// jepsen/<workload>/ names, so without it a later tenant overwrites an earlier
// one's evidence.
func tenantSink(sink apps.ArtifactSink, tenant, iter, repeats int) apps.ArtifactSink {
	prefix := fmt.Sprintf("tenant%d/", tenant)
	if repeats > 1 {
		prefix += fmt.Sprintf("iter%d/", iter)
	}
	return prefixSink{inner: sink, prefix: prefix}
}
