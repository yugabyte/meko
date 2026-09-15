package chaos

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// Built-in action names. A scenario selects actions by these (Config.Actions,
// registry lookup).
const (
	ActionPodDelete        = "pod_delete"
	ActionProcessKill      = "process_kill"
	ActionProcessRestart   = "process_restart"
	ActionNodeRestart      = "node_restart"
	ActionNetworkPartition = "network_partition"
	ActionNetworkSlowdown  = "network_slowdown"
	ActionPacketLoss       = "packet_loss"
	ActionNodeStop         = "node_stop"
	ActionProcessStall     = "process_stall"
	ActionPodFreeze        = "pod_freeze"
)

// iptablesComment tags the rules NetworkPartition adds so Recover removes
// exactly the chaos engine's rules and nothing else.
const iptablesComment = "ampqa-chaos"

// --- pod_delete: delete a YB pod; k8s reschedules it (StatefulSet). Analog of
// the stress framework's K8SRestartNemesis (src/apps/nemesis/k8s.py). ---

type podDelete struct{ waitTimeout time.Duration }

func (podDelete) Name() string { return ActionPodDelete }

func (a podDelete) Inject(ctx context.Context, t *Target) error {
	if t.kind != KindPod {
		return fmt.Errorf("%s requires a pod target, got %s", a.Name(), t.kind)
	}
	return t.kube.Delete(ctx, "pod", t.pod)
}

// `kubectl wait` returns NotFound on a pod not yet recreated, faking recovery.
func (a podDelete) Recover(ctx context.Context, t *Target) error {
	if t.kind != KindPod {
		return nil
	}
	if err := a.awaitPodExists(ctx, t); err != nil {
		return err
	}
	return t.kube.WaitFor(ctx, "pod/"+t.pod, "condition=Ready", a.waitTimeout)
}

const podExistsPollInterval = 2 * time.Second

func (a podDelete) awaitPodExists(ctx context.Context, t *Target) error {
	deadline := time.Now().Add(a.waitTimeout)
	for {
		_, err := t.kube.Get(ctx, "pod", t.pod, "{.metadata.name}")
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pod %s was never recreated within %s of being deleted: %w", t.pod, a.waitTimeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(podExistsPollInterval):
		}
	}
}

// --- process_kill: SIGKILL the YB process; recover ensures it is back. Analog
// of ProcessKill (src/apps/nemesis/system.py / yugabyte.py). ---

type processKill struct {
	pattern     string
	unit        string
	waitTimeout time.Duration
}

func (processKill) Name() string { return ActionProcessKill }

func (a processKill) Inject(ctx context.Context, t *Target) error {
	// pkill exits 1 when nothing matched, which is not an injection failure.
	return t.runTolerant(ctx, "pkill -KILL -f "+shellQuote(a.pattern))
}

func (a processKill) Recover(ctx context.Context, t *Target) error {
	// On a host the process is supervised by systemd; restart the unit. On a
	// pod the container runtime restarts the entrypoint process on its own.
	if t.kind == KindHost {
		_ = t.runTolerant(ctx, "systemctl start "+shellQuote(a.unit))
	}
	return waitProcessUp(ctx, t, a.pattern, a.waitTimeout)
}

// --- process_restart: restart the YB process in place; recover waits for it to
// come back. Analog of NodeRestartNemesis / process restart. ---

type processRestart struct {
	pattern     string
	unit        string
	waitTimeout time.Duration
}

func (processRestart) Name() string { return ActionProcessRestart }

func (a processRestart) Inject(ctx context.Context, t *Target) error {
	if t.kind == KindHost {
		return t.run(ctx, "systemctl restart "+shellQuote(a.unit))
	}
	return t.runTolerant(ctx, "pkill -TERM -f "+shellQuote(a.pattern))
}

func (a processRestart) Recover(ctx context.Context, t *Target) error {
	return waitProcessUp(ctx, t, a.pattern, a.waitTimeout)
}

// --- node_restart: restart the whole node — pod delete (reschedule) or restart
// every YB unit on a host — and wait for it to rejoin. ---

type nodeRestart struct {
	units       []string
	pattern     string
	waitTimeout time.Duration
}

func (nodeRestart) Name() string { return ActionNodeRestart }

func (a nodeRestart) Inject(ctx context.Context, t *Target) error {
	if t.kind == KindPod {
		return t.kube.Delete(ctx, "pod", t.pod)
	}
	for _, unit := range a.units {
		_ = t.runTolerant(ctx, "systemctl restart "+shellQuote(unit))
	}
	return nil
}

func (a nodeRestart) Recover(ctx context.Context, t *Target) error {
	if t.kind == KindPod {
		return t.kube.WaitFor(ctx, "pod/"+t.pod, "condition=Ready", a.waitTimeout)
	}
	return waitProcessUp(ctx, t, a.pattern, a.waitTimeout)
}

// --- network_partition: drop inbound traffic to the YB RPC/SQL/CQL ports via
// iptables, isolating the node; recover deletes exactly the chaos rules. Analog
// of NetworkPartitionNemesis. ---

type networkPartition struct {
	device string
	ports  []int
}

func (networkPartition) Name() string { return ActionNetworkPartition }

func (a networkPartition) Inject(ctx context.Context, t *Target) error {
	for _, p := range a.ports {
		if err := t.run(ctx, a.rule("-A", p)); err != nil {
			return err
		}
	}
	return nil
}

func (a networkPartition) Recover(ctx context.Context, t *Target) error {
	var firstErr error
	for _, p := range a.ports {
		if err := t.runTolerant(ctx, a.rule("-D", p)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// rule renders an iptables INPUT rule for one port, tagged with the chaos
// comment so Recover can delete precisely this rule (op is "-A" or "-D").
func (a networkPartition) rule(op string, port int) string {
	return "iptables " + op + " INPUT -p tcp --dport " + strconv.Itoa(port) +
		" -j DROP -m comment --comment " + iptablesComment
}

// --- network_slowdown: add egress latency via tc netem; recover tears the
// qdisc down. Analog of NetworkSlowdownNemesis. ---

type networkSlowdown struct {
	device  string
	delayMs int
}

func (networkSlowdown) Name() string { return ActionNetworkSlowdown }

func (a networkSlowdown) Inject(ctx context.Context, t *Target) error {
	return t.run(ctx, "tc qdisc add dev "+a.device+" root netem delay "+strconv.Itoa(a.delayMs)+"ms")
}

func (a networkSlowdown) Recover(ctx context.Context, t *Target) error {
	return t.runTolerant(ctx, "tc qdisc del dev "+a.device+" root")
}

// --- packet_loss: drop a percentage of egress packets via tc netem; recover
// tears the qdisc down. Analog of the packet-loss nemesis. ---

type packetLoss struct {
	device  string
	percent int
}

func (packetLoss) Name() string { return ActionPacketLoss }

func (a packetLoss) Inject(ctx context.Context, t *Target) error {
	return t.run(ctx, "tc qdisc add dev "+a.device+" root netem loss "+strconv.Itoa(a.percent)+"%")
}

func (a packetLoss) Recover(ctx context.Context, t *Target) error {
	return t.runTolerant(ctx, "tc qdisc del dev "+a.device+" root")
}

// --- node_stop: stop the underlying cloud VM, then start it again on recover.
// Requires a CloudVM attached to the target (WithCloud). Analog of
// UnifiedStartStopNemesis (src/apps/nemesis/unified_cloud_nemesis.py). ---

type nodeStop struct{ waitTimeout time.Duration }

func (nodeStop) Name() string { return ActionNodeStop }

func (a nodeStop) Inject(ctx context.Context, t *Target) error {
	if t.cloud == nil {
		return fmt.Errorf("%s requires a target with a CloudVM (WithCloud)", a.Name())
	}
	return t.cloud.Stop(ctx)
}

func (a nodeStop) Recover(ctx context.Context, t *Target) error {
	if t.cloud == nil {
		return nil
	}
	if err := t.cloud.Start(ctx); err != nil {
		return err
	}
	return t.cloud.WaitRunning(ctx, a.waitTimeout)
}

// waitProcessUp polls `pgrep -f pattern` on the target until it reports the
// process running or timeout elapses. pgrep needs no elevation, so it runs
// directly (no sudo prefix).
func waitProcessUp(ctx context.Context, t *Target, pattern string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	cmd := "pgrep -f " + shellQuote(pattern)
	for {
		res, err := t.exec.Run(ctx, cmd)
		if err == nil && res.ExitCode == 0 && strings.TrimSpace(res.Stdout) != "" {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process %q not back within %s", pattern, timeout)
		}
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// --- process_stall: SIGSTOP the matched process, SIGCONT it on recover. It
// keeps every lease, socket and file it held, which is the long-GC-pause shape
// a kill cannot produce. ---

type processStall struct{ pattern string }

func (processStall) Name() string { return ActionProcessStall }

func (a processStall) Inject(ctx context.Context, t *Target) error {
	// pkill exits 1 when nothing matched, which is not an injection failure.
	return t.runTolerant(ctx, "pkill -STOP -f "+shellQuote(selfExcludingPattern(a.pattern)))
}

func (a processStall) Recover(ctx context.Context, t *Target) error {
	return t.runTolerant(ctx, "pkill -CONT -f "+shellQuote(selfExcludingPattern(a.pattern)))
}

// pkill -f also matches the shell running pkill, whose own cmdline carries the
// pattern, and SIGSTOP on that shell hangs the exec forever. Bracketing the
// first character leaves the ERE matching the target and not that literal.
func selfExcludingPattern(p string) string {
	if p == "" {
		return p
	}
	return "[" + p[:1] + "]" + p[1:]
}

// --- pod_freeze: freeze every process in the pod through its cgroup v2
// cgroup.freeze, and thaw it on recover. Nothing inside the pod can do this to
// itself and a frozen pod cannot be exec-ed into at all, so the write goes
// through a privileged hostPID helper pod pinned to the same node, as
// suites/stress/partition_helper.go does. The namespace it enters is the
// host's, not the target's: a cgroup path resolves nowhere else. ---

type podFreeze struct {
	image       string
	waitTimeout time.Duration
	seq         atomic.Uint64

	mu      sync.Mutex
	helpers map[string]*freezeHelper
}

func newPodFreeze(image string, waitTimeout time.Duration) *podFreeze {
	return &podFreeze{image: image, waitTimeout: waitTimeout, helpers: map[string]*freezeHelper{}}
}

func (*podFreeze) Name() string { return ActionPodFreeze }

func (a *podFreeze) Inject(ctx context.Context, t *Target) error {
	if t.kind != KindPod {
		return fmt.Errorf("%s requires a pod target, got %s", a.Name(), t.kind)
	}
	h, err := a.helperFor(ctx, t)
	if err != nil {
		return err
	}
	return h.setFreeze(ctx, freezeOn)
}

func (a *podFreeze) Recover(ctx context.Context, t *Target) error {
	a.mu.Lock()
	h := a.helpers[t.name]
	a.mu.Unlock()
	// No helper means Inject never got as far as freezing anything.
	if h == nil {
		return nil
	}
	// The helper is the only way back to a frozen pod, so it goes only once the
	// thaw took. A failed lift stays retryable.
	if err := h.setFreeze(ctx, freezeOff); err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.helpers, t.name)
	a.mu.Unlock()
	h.close(ctx)
	return nil
}

const (
	freezeOn  = "1"
	freezeOff = "0"
	// How a scan that RAN and matched nothing reports itself, which an exec
	// that never ran cannot forge.
	freezeNoPIDSentinel = "ampqa-no-pid"
)

func (a *podFreeze) helperFor(ctx context.Context, t *Target) (*freezeHelper, error) {
	a.mu.Lock()
	h := a.helpers[t.name]
	a.mu.Unlock()
	if h != nil {
		return h, nil
	}
	node, err := t.kube.Get(ctx, "pod", t.pod, "{.spec.nodeName}")
	if err != nil {
		return nil, fmt.Errorf("read the node of pod %s: %w", t.pod, err)
	}
	uid, err := t.kube.Get(ctx, "pod", t.pod, "{.metadata.uid}")
	if err != nil {
		return nil, fmt.Errorf("read the uid of pod %s: %w", t.pod, err)
	}
	if node == "" || uid == "" {
		return nil, fmt.Errorf("pod %s has no node or uid yet (node %q, uid %q)", t.pod, node, uid)
	}
	h = &freezeHelper{
		kube: t.kube,
		// A name per injection: close() only requests deletion, and an Apply
		// against a still-terminating pod is a rejected spec update.
		name: fmt.Sprintf("ampqa-freeze-%s-%d", helperSuffix(uid), a.seq.Add(1)),
		uid:  uid,
		node: node,
		pod:  t.pod,
	}
	if err := t.kube.Apply(ctx, h.manifest(a.image)); err != nil {
		return nil, fmt.Errorf("create the freeze helper pod %s on node %s: %w", h.name, node, err)
	}
	if err := t.kube.WaitFor(ctx, "pod/"+h.name, "condition=Ready", a.waitTimeout); err != nil {
		h.close(ctx)
		return nil, fmt.Errorf("freeze helper pod %s never became Ready on node %s: %w (%s)", h.name, node, err, t.kube.PodTrouble(ctx, h.name))
	}
	a.mu.Lock()
	a.helpers[t.name] = h
	a.mu.Unlock()
	return h, nil
}

func helperSuffix(uid string) string {
	s := strings.ToLower(strings.ReplaceAll(uid, "-", ""))
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

type freezeHelper struct {
	kube *k8s.Kubectl
	name string
	uid  string
	node string
	pod  string
}

// The read-back confirms the write, not the effective state: cgroup.freeze
// echoes what was asked for, and the freeze is proven by the exec probe in
// suites/stress/fence_stall.go.
func (h *freezeHelper) setFreeze(ctx context.Context, value string) error {
	res, err := h.kube.ForPod(h.name, "").Run(ctx, freezeScript(h.uid, value))
	if err != nil {
		return fmt.Errorf("write cgroup.freeze=%s for pod %s through helper %s: %w", value, h.pod, h.name, err)
	}
	out := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 {
		return fmt.Errorf("write cgroup.freeze=%s for pod %s exited %d: %s", value, h.pod, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	// Thawing a pod whose processes are already gone is a no-op, not a failure.
	// Freezing one that has none never happened.
	if out == freezeNoPIDSentinel {
		if value == freezeOff {
			return nil
		}
		return fmt.Errorf("no process on node %s belongs to pod %s (uid %s), so there is no cgroup to freeze", h.node, h.pod, h.uid)
	}
	if out != value {
		return fmt.Errorf("cgroup.freeze for pod %s reads %q after writing %q: the kernel did not take the write", h.pod, out, value)
	}
	return nil
}

func (h *freezeHelper) close(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	_ = h.kube.DeleteNow(ctx, h.name)
}

// The write happens inside the HOST mount and cgroup namespaces (nsenter -t 1
// -m -C): a cgroup path read from any other namespace does not resolve under
// /sys/fs/cgroup. It targets the POD slice, the parent of the container scope,
// so every container freezes. The container scope is reached only where that
// parent carries no cgroup.freeze, which a cgroup v2 node always does.
func freezeScript(uid, value string) string {
	inner := fmt.Sprintf(`u2=$(echo %s | tr - _); p=; `+
		`for d in /proc/[0-9]*; do grep -qs -e %s -e "$u2" "$d/cgroup" && { p=$d; break; }; done; `+
		`[ -n "$p" ] || { echo %s; exit 0; }; `+
		`cg=$(sed -n "s|^0::||p" "$p/cgroup"); `+
		`[ -n "$cg" ] || { echo "pod %s is not on a cgroup v2 node" >&2; exit 1; }; `+
		`f=/sys/fs/cgroup$(dirname "$cg")/cgroup.freeze; `+
		`[ -e "$f" ] || f=/sys/fs/cgroup$cg/cgroup.freeze; `+
		`echo %s > "$f" || exit 1; cat "$f"`,
		uid, uid, freezeNoPIDSentinel, uid, value)
	return "nsenter -t 1 -m -C -- sh -c '" + inner + "'"
}

func (h *freezeHelper) manifest(image string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    app: ampqa-freeze-helper
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
`, h.name, h.kube.Namespace(), h.node, image)
}
