package features

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	DefaultRedpathWedgeDeadline = 20 * time.Minute
	DefaultRedpathRetryWait     = 60 * time.Second
	DefaultRedpathAPIDeadline   = 30 * time.Second
	DefaultRedpathOrphanTimeout = 45 * time.Minute
	DefaultRedpathQuotaHeadroom = 0
	DefaultRedpathFaultHold     = 90 * time.Second
	DefaultRedpathTaskStart     = 5 * time.Minute
	DefaultRedpathHelperImage   = "nicolaka/netshoot:v0.13"
)

const redpathCreateDeadline = 55 * time.Minute

const orphanListingGrace = 2 * time.Minute

type redpathConfig struct {
	Inject           bool
	WedgeDeadline    time.Duration
	RetryWait        time.Duration
	APIDeadline      time.Duration
	OrphanTimeout    time.Duration
	QuotaHeadroom    int
	FaultHold        time.Duration
	TaskStartTimeout time.Duration
	HelperImage      string
}

func redpathFromConfig(c *core.Config) redpathConfig {
	return redpathConfig{
		Inject:        c.GetBool("functional.redpath.inject", true),
		WedgeDeadline: c.GetDuration("functional.redpath.wedge_deadline", DefaultRedpathWedgeDeadline),
		RetryWait:     c.GetDuration("functional.redpath.retry_wait", DefaultRedpathRetryWait),
		APIDeadline:   c.GetDuration("functional.redpath.api_deadline", DefaultRedpathAPIDeadline),
		OrphanTimeout: c.GetDuration("functional.redpath.orphan_timeout", DefaultRedpathOrphanTimeout),
		QuotaHeadroom: c.GetInt("functional.redpath.quota_headroom", DefaultRedpathQuotaHeadroom),

		FaultHold:        c.GetDuration("functional.redpath.fault_hold", DefaultRedpathFaultHold),
		TaskStartTimeout: c.GetDuration("functional.redpath.task_start_timeout", DefaultRedpathTaskStart),
		HelperImage:      c.GetString("functional.redpath.helper_image", DefaultRedpathHelperImage),
	}
}

type redpathRun struct {
	a         *ampFeatureRun
	cfg       redpathConfig
	cl        *controller.Client
	bystander controller.Endpoint
	target    ampCluster
}

func (a *ampFeatureRun) newRedpath(ctx context.Context) (*redpathRun, error) {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return nil, err
	}
	r := &redpathRun{a: a, cfg: redpathFromConfig(a.f.t.Options), cl: cl}
	if r.bystander, err = createEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID); err != nil {
		return nil, fmt.Errorf("create the bystander writer the fault must not disturb: %w", err)
	}
	target, err := createClusterOnPool(ctx, cl, a.proj.poolID, "rp-"+a.f.suffix(), a.f.cfg.PgVersion, a.f.register)
	if err != nil {
		return nil, fmt.Errorf("create the cluster the fault is aimed at: %w", err)
	}
	r.target = target
	return r, nil
}

func (r *redpathRun) logf(format string, args ...any) { r.a.f.logf(format, args...) }

func (r *redpathRun) taskFailedWithin(ctx context.Context, task controller.Task, loc string, bound time.Duration, what string) (controller.Task, error) {
	ref := controller.TaskRef(task, loc)
	if ref == "" {
		return controller.Task{}, fmt.Errorf("%s was never planned, so there is no task to fail", what)
	}
	start := time.Now()
	wctx, cancel := context.WithTimeout(ctx, bound)
	done, err := r.cl.WaitTask(wctx, ref)
	cancel()
	elapsed := time.Since(start)
	r.a.f.t.Metric("task_settle_seconds", elapsed.Seconds(), "s")
	r.logf("%s task %s settled %q after %s (%v)", what, ref, done.Status, elapsed.Round(time.Second), err)

	switch done.Status {
	case controller.TaskFailed:
		return done, nil
	case controller.TaskSucceeded:
		return done, fmt.Errorf("%s task %s SUCCEEDED under the injected fault; the control plane reported a result it cannot have produced", what, ref)
	}
	if ctx.Err() != nil {
		return done, ctx.Err()
	}
	if done.Status == "" {
		return done, core.AsEnvFailure(fmt.Errorf("%s task %s never reported a status within %s, so its verdict was never observed: %w", what, ref, bound, err))
	}
	return done, fmt.Errorf("%s task %s was still %q after %s: a task that can never succeed must fail, not sit non-terminal (a wedged task blocks every later roll, meko#1007)",
		what, ref, done.Status, bound)
}

func taskErrorNames(task controller.Task, what string, want []string) error {
	msg := ""
	if task.Error != nil {
		msg = *task.Error
	}
	low := strings.ToLower(msg)
	for _, w := range want {
		if strings.Contains(low, strings.ToLower(w)) {
			return nil
		}
	}
	return fmt.Errorf("%s task %s failed with %q, which names none of %v: the task error is the only thing an operator reads, and a bare deadline does not say what went wrong",
		what, task.ID, msg, want)
}

func (r *redpathRun) noOrphans(ctx context.Context, branchID string, within time.Duration) error {
	var last map[string]string
	start := time.Now()
	// A failed listing is retried once one has succeeded: returning it would
	// report whatever the previous poll saw as orphans long before the sweeper's
	// budget is spent. One that never succeeds is not going to.
	retry := func(err error) (bool, error) {
		if last == nil && time.Since(start) > orphanListingGrace {
			return false, core.AsEnvFailure(fmt.Errorf("no listing of branch %s's compute succeeded within %s: %w", branchID, orphanListingGrace, err))
		}
		return false, nil
	}
	err := pollUntil(ctx, within, 15*time.Second, func() (bool, error) {
		live, err := r.a.f.branchComputePods(ctx, branchID)
		if err != nil {
			return retry(fmt.Errorf("list compute for branch %s: %w", branchID, err))
		}
		rows, err := r.endpointRowPods(ctx, branchID)
		if err != nil {
			return retry(err)
		}
		for k, v := range rows {
			live[k] = v
		}
		last = live
		return len(live) == 0, nil
	})
	if err == nil {
		return nil
	}
	if len(last) == 0 {
		return core.AsEnvFailure(fmt.Errorf("checking for orphaned compute on branch %s: %w", branchID, err))
	}
	var pods []string
	for instance, pod := range last {
		pods = append(pods, instance+" ("+pod+")")
	}
	return fmt.Errorf("%d compute pod(s) for branch %s are still there %s after the task failed: %s; a create that failed must leave nothing behind, or the pod and the reservation it holds are nobody's to reclaim (meko#486)",
		len(pods), branchID, time.Since(start).Round(time.Second), strings.Join(pods, ", "))
}

// A rolled-back release takes the ConfigMap branchComputePods matches on, so the endpoint rows the API still carries are the other half of "nothing left running".
func (r *redpathRun) endpointRowPods(ctx context.Context, branchID string) (map[string]string, error) {
	kube := r.a.f.ampKube()
	if kube == nil {
		return nil, fmt.Errorf("no kubectl for the amp namespace, so orphaned compute cannot be looked for")
	}
	eps, err := r.cl.ListAllEndpoints(ctx, r.target.poolID, r.target.projID, branchID)
	if err != nil {
		return nil, fmt.Errorf("list the endpoints the API still has on branch %s: %w", branchID, err)
	}
	out := map[string]string{}
	for _, ep := range eps {
		instance := ampstack.EndpointWorkloadName(ep.ID)
		pods, err := kube.GetSelector(ctx, "pod", "app.kubernetes.io/instance="+instance,
			`{range .items[*]}{.metadata.name}={.status.phase}{"\n"}{end}`)
		if err != nil {
			return nil, fmt.Errorf("list pods for endpoint %s: %w", ep.ID, err)
		}
		for _, line := range strings.Split(pods, "\n") {
			pod, phase, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok || pod == "" {
				continue
			}
			out[instance] = pod + " " + phase
		}
	}
	return out, nil
}

func (r *redpathRun) stillServing(ctx context.Context) error {
	if err := r.cl.Healthz(ctx); err != nil {
		return fmt.Errorf("the controller is not healthy after the fault: %w", err)
	}
	if _, err := r.cl.ListClusters(ctx, r.a.proj.poolID, 100, 0); err != nil {
		return fmt.Errorf("the API cannot list clusters after the fault: %w", err)
	}
	c, err := r.a.f.endpointSQL(ctx, r.bystander)
	if err != nil {
		return fmt.Errorf("the bystander endpoint %s, on another cluster entirely, no longer accepts connections after the fault: %w", r.bystander.ID, err)
	}
	defer c.Close()
	if _, err := c.QueryInt(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("the bystander endpoint %s no longer answers SQL after the fault: %w", r.bystander.ID, err)
	}
	return nil
}

func (r *redpathRun) retrySucceeds(ctx context.Context, what string, fn func(context.Context) error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(r.cfg.RetryWait):
	}
	if err := fn(ctx); err != nil {
		return fmt.Errorf("%s still fails %s after the fault was removed, so the fault left the control plane unable to serve a request it served before: %w", what, r.cfg.RetryWait, err)
	}
	return nil
}

// A failed create can leave its endpoint row behind until the sweeper takes it
// (meko#486), and a branch admits one writer, so a plain re-create hits a conflict.
func (r *redpathRun) writerServes(ctx context.Context, req controller.CreateEndpointRequest) error {
	if ep, ok := lastEndpoint(ctx, r.cl, r.target.poolID, r.target.projID, r.target.branchID); ok {
		err := r.serves(ctx, ep)
		if err == nil {
			return nil
		}
		r.logf("the endpoint %s left on branch %s does not serve (%v); deleting it before the retry", ep.ID, r.target.branchID, err)
		if _, err := r.cl.DeleteEndpointAndWait(ctx, r.target.poolID, r.target.projID, r.target.branchID, ep.ID); err != nil {
			return fmt.Errorf("delete the endpoint the failed create left on branch %s: %w", r.target.branchID, err)
		}
	}
	ep, err := createEndpointReq(ctx, r.cl, r.target.poolID, r.target.projID, r.target.branchID, req)
	if err != nil {
		return err
	}
	return r.serves(ctx, ep)
}

func (r *redpathRun) serves(ctx context.Context, ep controller.Endpoint) error {
	c, err := r.a.f.endpointSQL(ctx, ep)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.QueryInt(ctx, "SELECT 1")
	return err
}

func (r *redpathRun) joinTask(ctx context.Context, task controller.Task, loc, kind string) string {
	ref := controller.TaskRef(task, loc)
	if ref == "" {
		return "not-planned"
	}
	jctx, cancel := context.WithTimeout(ctx, redpathJoinBudget)
	defer cancel()
	done, err := r.cl.WaitTask(jctx, ref)
	if done.Status != "" {
		return done.Status
	}
	r.logf("could not read %s task %s to completion: %v", kind, ref, err)
	if ctx.Err() == nil {
		return "still-running"
	}
	return "unknown"
}

const redpathJoinBudget = 20 * time.Minute

// Deployments and not pods: a pod cap also denies the controller its own replacement pod, and a Deployment cap is refused at admission instead of pending for helm's 35 minutes.
type resourceQuota struct {
	kube *k8s.Kubectl
	name string
}

func applyDeploymentQuota(ctx context.Context, kube *k8s.Kubectl, suffix string, headroom int) (*resourceQuota, error) {
	deploys, err := kube.GetSelector(ctx, "deployment", "", "{.items[*].metadata.name}")
	if err != nil {
		return nil, fmt.Errorf("count the Deployments already in %q: %w", kube.Namespace(), err)
	}
	q := &resourceQuota{kube: kube, name: "ampqa-redpath-" + suffix}
	manifest := fmt.Sprintf(`apiVersion: v1
kind: ResourceQuota
metadata:
  name: %s
  namespace: %s
spec:
  hard:
    count/deployments.apps: "%d"
`, q.name, kube.Namespace(), len(strings.Fields(deploys))+headroom)
	if err := kube.Apply(ctx, manifest); err != nil {
		return nil, fmt.Errorf("apply the Deployment quota in %q: %w", kube.Namespace(), err)
	}
	return q, nil
}

func (q *resourceQuota) remove(ctx context.Context) error {
	if q == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	return q.kube.Delete(ctx, "resourcequota", q.name)
}

// iptables and nsenter are in neither image and amp-dev's CNI does not enforce NetworkPolicy, so the rule goes in from a privileged hostPID helper on the controller's node.
type backingCut struct {
	kube   *k8s.Kubectl
	helper string
	uid    string
	node   string
	pod    string
	healed bool
}

const redpathCutComment = "ampqa-redpath"

const redpathIptables = `IPT=iptables-legacy; $IPT -L -n >/dev/null 2>&1 || IPT=iptables; $IPT`

const redpathNoPID = "ampqa-no-pid"

var errRedpathNoPodProcess = errors.New("no process on the node belongs to the controller pod")

func (r *redpathRun) backingHosts(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		if host, _, err := net.SplitHostPort(h); err == nil {
			h = host
		}
		if h = strings.TrimSpace(h); h != "" && h != "None" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	if be, ok := r.a.f.ampEnv.(backingKubeEnv); ok && be.BackingKube() != nil {
		bkube := be.BackingKube()
		pods, err := bkube.GetSelector(ctx, "pod", "", "{.items[*].status.podIP}")
		if err != nil {
			return nil, fmt.Errorf("list pod IPs in %q: %w", bkube.Namespace(), err)
		}
		svcs, err := bkube.GetSelector(ctx, "service", "", "{.items[*].spec.clusterIP}")
		if err != nil {
			return nil, fmt.Errorf("list Service ClusterIPs in %q: %w", bkube.Namespace(), err)
		}
		for _, ip := range strings.Fields(pods + " " + svcs) {
			add(ip)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("namespace %q has no pod or Service IP to cut the controller off from", bkube.Namespace())
		}
		return out, nil
	}
	if store := r.a.f.store; store != nil {
		add(store.ConnInfo().Host)
		for _, hp := range strings.Split(store.HostPortsCSV(), ",") {
			add(hp)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("this run exposes neither a kube handle on the backing namespace nor a resolved backing address, so the controller cannot be cut off from its backing")
	}
	return out, nil
}

func (r *redpathRun) cutControllerFromBacking(ctx context.Context) (*backingCut, error) {
	hosts, err := r.backingHosts(ctx)
	if err != nil {
		return nil, err
	}
	kube := r.a.f.ampKube()
	if kube == nil {
		return nil, fmt.Errorf("no kubectl for the amp namespace, so the controller pod's network namespace cannot be reached")
	}
	name := ampstack.FromConfig(r.a.f.t.Options).ControllerName
	pods, err := kube.GetSelector(ctx, "pod", "app="+name, "{.items[*].metadata.name}")
	if err != nil {
		return nil, fmt.Errorf("find the controller pod (selector app=%s): %w", name, err)
	}
	fields := strings.Fields(pods)
	if len(fields) == 0 {
		return nil, fmt.Errorf("no pod matches app=%s in %q", name, kube.Namespace())
	}
	c := &backingCut{kube: kube, helper: "ampqa-rp-netns-" + r.a.f.suffix(), pod: fields[0]}
	if c.node, err = kube.Get(ctx, "pod", c.pod, "{.spec.nodeName}"); err != nil {
		return nil, fmt.Errorf("read the node of controller pod %s: %w", c.pod, err)
	}
	if c.uid, err = kube.Get(ctx, "pod", c.pod, "{.metadata.uid}"); err != nil {
		return nil, fmt.Errorf("read the uid of controller pod %s: %w", c.pod, err)
	}
	if c.node == "" || c.uid == "" {
		return nil, fmt.Errorf("controller pod %s has no node or uid yet (node %q, uid %q)", c.pod, c.node, c.uid)
	}
	if err := kube.Apply(ctx, c.manifest(r.cfg.HelperImage)); err != nil {
		return nil, fmt.Errorf("create the netns helper pod %s on node %s: %w", c.helper, c.node, err)
	}
	if err := kube.WaitFor(ctx, "pod/"+c.helper, "condition=Ready", 5*time.Minute); err != nil {
		c.close(ctx)
		return nil, fmt.Errorf("netns helper pod %s never became Ready on node %s: %w (%s)", c.helper, c.node, err, kube.PodTrouble(ctx, c.helper))
	}
	probe, hasProbe := r.backingProbe(ctx)
	if hasProbe && !c.reaches(ctx, probe) {
		c.teardown(ctx)
		return nil, fmt.Errorf("controller pod %s cannot reach the backing at %s before any rule was inserted, so a cut here would prove nothing", c.pod, probe)
	}
	for _, h := range hosts {
		if err := c.iptables(ctx, fmt.Sprintf("-I OUTPUT -d %s -j DROP -m comment --comment %s", h, redpathCutComment)); err != nil {
			healErr := c.teardown(ctx)
			return nil, fmt.Errorf("cut controller pod %s off %s: %w (heal: %v)", c.pod, h, err, healErr)
		}
	}
	if hasProbe && c.reaches(ctx, probe) {
		healErr := c.teardown(ctx)
		return nil, fmt.Errorf("the DROP rules are in the netns of controller pod %s but it still reaches the backing at %s, so the cut dropped nothing (heal: %v)", c.pod, probe, healErr)
	}
	r.logf("controller pod %s cut off from the backing at %s", c.pod, strings.Join(hosts, ", "))
	return c, nil
}

func (r *redpathRun) backingProbe(ctx context.Context) (string, bool) {
	if be, ok := r.a.f.ampEnv.(backingKubeEnv); ok && be.BackingKube() != nil {
		out, err := be.BackingKube().GetSelector(ctx, "service", "",
			`{range .items[*]}{.spec.clusterIP} {.spec.ports[0].port}{"\n"}{end}`)
		if err == nil {
			for _, line := range strings.Split(out, "\n") {
				ip, port, ok := strings.Cut(strings.TrimSpace(line), " ")
				if ok && ip != "" && ip != "None" && port != "" {
					return ip + " " + port, true
				}
			}
		}
	}
	if store := r.a.f.store; store != nil {
		ci := store.ConnInfo()
		if ci.Host != "" && ci.Port != 0 {
			return fmt.Sprintf("%s %d", ci.Host, ci.Port), true
		}
	}
	return "", false
}

func (c *backingCut) reaches(ctx context.Context, hostPort string) bool {
	pid, err := c.podPID(ctx)
	if err != nil {
		return false
	}
	res, err := c.exec().Run(ctx, fmt.Sprintf("nsenter -t %s -n -- nc -z -w 3 %s", pid, hostPort))
	return err == nil && res.ExitCode == 0
}

func (c *backingCut) teardown(ctx context.Context) error {
	err := c.heal(ctx)
	c.close(ctx)
	return err
}

func (c *backingCut) exec() *k8s.Kubectl { return c.kube.ForPod(c.helper, "") }

// The LOWEST pid by number, not the first of /proc/[0-9]*: the oldest process
// is a container init, while glob order can pick a backend that exits before
// nsenter opens its namespace. apps/netshape holds the same scan, and this
// copy is the third of the privileged hostPID pattern in the tree.
func (c *backingCut) podPID(ctx context.Context) (string, error) {
	script := fmt.Sprintf(`uid=%s; u2=$(echo "$uid" | tr - _); for p in $(ls /proc | grep -E '^[0-9]+$' | sort -n); do `+
		`grep -qs -e "$uid" -e "$u2" "/proc/$p/cgroup" && { echo "$p"; exit 0; }; done; echo %s`, c.uid, redpathNoPID)
	res, err := c.exec().Run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("scan /proc on node %s for pod %s: %w", c.node, c.pod, err)
	}
	out := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 {
		return "", fmt.Errorf("scan /proc on node %s for pod %s exited %d: %s", c.node, c.pod, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if out == redpathNoPID {
		return "", fmt.Errorf("%w: pod %s (uid %s) on node %s", errRedpathNoPodProcess, c.pod, c.uid, c.node)
	}
	if out == "" {
		return "", fmt.Errorf("scan /proc on node %s for pod %s printed nothing (stderr %q)", c.node, c.pod, strings.TrimSpace(res.Stderr))
	}
	return out, nil
}

// The pid is resolved per call: a container restart retires it, not the netns.
func (c *backingCut) iptables(ctx context.Context, args string) error {
	pid, err := c.podPID(ctx)
	if err != nil {
		return err
	}
	res, err := c.exec().Run(ctx, fmt.Sprintf("nsenter -t %s -n -- sh -c '%s %s'", pid, redpathIptables, args))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("iptables %s in the controller's netns exited %d: %s", args, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func (c *backingCut) heal(ctx context.Context) error {
	if c == nil || c.healed {
		return nil
	}
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	script := fmt.Sprintf(`%s -S | grep -- "--comment %s" | sed "s/^-A/-D/" | while read -r r; do %s $r || exit 1; done`,
		redpathIptables, redpathCutComment, redpathIptables)
	pid, err := c.podPID(hctx)
	if errors.Is(err, errRedpathNoPodProcess) {
		c.healed = true
		c.close(hctx)
		return nil
	}
	if err != nil {
		return err
	}
	res, err := c.exec().Run(hctx, fmt.Sprintf("nsenter -t %s -n -- sh -c '%s'", pid, script))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("deleting the %s rules in the controller's netns exited %d: %s", redpathCutComment, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	c.healed = true
	c.close(hctx)
	return nil
}

func (c *backingCut) close(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	_ = c.kube.DeleteNow(ctx, c.helper)
}

func (c *backingCut) manifest(image string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    app: ampqa-redpath-netns
spec:
  nodeName: %s
  hostPID: true
  restartPolicy: Never
  terminationGracePeriodSeconds: 0
  tolerations:
    - operator: Exists
  containers:
    - name: helper
      image: %s
      command: ["sleep", "infinity"]
      securityContext:
        privileged: true
`, c.helper, c.kube.Namespace(), c.node, image)
}
