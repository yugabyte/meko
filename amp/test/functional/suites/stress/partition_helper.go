// Network partitions on k8s without touching the target's image. iptables and
// tc inside a pod need NET_ADMIN and a binary, and neither the endpoint image
// nor the YB pods have them (the chaos engine's network_partition action is
// unusable there, see configs/default.yaml stress.combined.chaos_actions).
// NetworkPolicy is no route either: amp-dev runs kube-proxy with no enforcing
// CNI, so a policy there is accepted and ignored (run 0d091c20).
//
// What every k8s node does have is the target pod's network namespace. A
// privileged, hostPID helper pod pinned to the same node finds one of the target
// pod's processes through /proc and runs iptables inside that namespace with
// nsenter, so the rules live in the pod's own stack, wherever the CNI's happen
// to be. Every partition is then probed from inside the target before a test
// relies on it: a rule that dropped nothing fails the step, never the product.
package stress

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// iptablesComment tags every rule the partitions add, so healing deletes
// exactly those (iptables -S | grep) and nothing the pod carried before.
const iptablesComment = "ampqa-chaos"

const (
	partitionProbeTimeout  = 5 * time.Second
	partitionProbeInterval = 2 * time.Second
	// partitionEnforceTimeout bounds how long a freshly inserted rule may take
	// to bite; a rule that has not dropped a packet by then never will.
	partitionEnforceTimeout = 60 * time.Second
	helperReadyTimeout      = 5 * time.Minute
)

// tserverRPCPort and ysqlPort are what the probes aim at: a peer tserver's RPC
// port from an isolated tserver, the YSQL port an endpoint's storage daemons
// use (AMP_DEPLOY_YB_HOST_PORTS) from a cut endpoint.
const (
	tserverRPCPort = 9100
	ysqlPort       = 5433
)

func helperImage(c *core.Config) string {
	return c.GetString("stress.partition.helper_image", "nicolaka/netshoot:v0.13")
}

// netnsHelper is the privileged pod through which one target pod's network
// namespace is reached, and the target's uid the pid is resolved from.
type netnsHelper struct {
	kube *k8s.Kubectl
	name string
	uid  string
	node string
	pod  string
}

// attachNetns stands a helper up on the target's node; any pid of the pod does.
func attachNetns(t *core.T, kube *k8s.Kubectl, pod, suffix string) (*netnsHelper, error) {
	node, err := kube.Get(t.Context, "pod", pod, "{.spec.nodeName}")
	if err != nil {
		return nil, fmt.Errorf("read the node of pod %s: %w", pod, err)
	}
	uid, err := kube.Get(t.Context, "pod", pod, "{.metadata.uid}")
	if err != nil {
		return nil, fmt.Errorf("read the uid of pod %s: %w", pod, err)
	}
	if node == "" || uid == "" {
		return nil, fmt.Errorf("pod %s has no node or uid yet (node %q, uid %q)", pod, node, uid)
	}
	h := &netnsHelper{kube: kube, name: "ampqa-netns-" + suffix, uid: uid, node: node, pod: pod}
	if err := kube.Apply(t.Context, h.manifest(node, helperImage(t.Options))); err != nil {
		return nil, fmt.Errorf("create the netns helper pod %s on node %s: %w", h.name, node, err)
	}
	if err := kube.WaitFor(t.Context, "pod/"+h.name, "condition=Ready", helperReadyTimeout); err != nil {
		h.Close(t.Context)
		return nil, fmt.Errorf("netns helper pod %s never became Ready on node %s: %w (%s)", h.name, node, err, kube.PodTrouble(t.Context, h.name))
	}
	if _, err := h.podPID(t.Context); err != nil {
		h.Close(t.Context)
		return nil, err
	}
	return h, nil
}

func (h *netnsHelper) exec() *k8s.Kubectl { return h.kube.ForPod(h.name, "") }

// errNoPodProcess: no process left, so the namespace and its rules are gone.
var errNoPodProcess = errors.New("no process on the node belongs to the target pod")

// Resolved per call: a liveness restart retires the pid, not the namespace.
func (h *netnsHelper) podPID(ctx context.Context) (string, error) {
	res, err := h.exec().Run(ctx, findPodPIDScript(h.uid))
	if err != nil {
		return "", fmt.Errorf("scan /proc on node %s for pod %s: %w", h.node, h.pod, err)
	}
	out := strings.TrimSpace(res.Stdout)
	// A non-zero exit is kubectl's, not the script's: OSRun reports a dead
	// helper pod the same way, and that must never read as "rules are gone".
	if res.ExitCode != 0 {
		return "", fmt.Errorf("scan /proc on node %s for pod %s exited %d: %s", h.node, h.pod, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if out == noPodPIDSentinel {
		return "", fmt.Errorf("%w: pod %s (uid %s) on node %s, so the helper cannot enter its network namespace",
			errNoPodProcess, h.pod, h.uid, h.node)
	}
	if out == "" {
		return "", fmt.Errorf("scan /proc on node %s for pod %s printed nothing (stderr %q)", h.node, h.pod, strings.TrimSpace(res.Stderr))
	}
	return out, nil
}

// iptables runs one iptables invocation inside the target's network namespace,
// erroring on a non-zero exit. iptables-legacy is preferred where the kernel
// serves it (COS does, for kube-proxy) and iptables-nft is the fallback.
func (h *netnsHelper) iptables(ctx context.Context, args string) error {
	pid, err := h.podPID(ctx)
	if err != nil {
		return err
	}
	res, err := h.exec().Run(ctx, fmt.Sprintf("nsenter -t %s -n -- sh -c '%s %s'", pid, iptablesPick, args))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("iptables %s in the pod's netns exited %d: %s", args, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// iptablesPick is the shell prologue choosing the iptables flavour and running
// the caller's arguments through it.
const iptablesPick = `IPT=iptables-legacy; $IPT -L -n >/dev/null 2>&1 || IPT=iptables; $IPT`

// clearRules deletes every rule the partitions tagged, whichever chain holds it.
func (h *netnsHelper) clearRules(ctx context.Context) error {
	script := fmt.Sprintf(`IPT=iptables-legacy; $IPT -L -n >/dev/null 2>&1 || IPT=iptables; `+
		`$IPT -S | grep -- "--comment %s" | sed "s/^-A/-D/" | while read -r r; do $IPT $r || exit 1; done`, iptablesComment)
	pid, err := h.podPID(ctx)
	if errors.Is(err, errNoPodProcess) {
		return nil
	}
	if err != nil {
		return err
	}
	res, err := h.exec().Run(ctx, fmt.Sprintf("nsenter -t %s -n -- sh -c '%s'", pid, script))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("deleting the %s rules in the pod's netns exited %d: %s", iptablesComment, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// Close deletes the helper pod. Rooted in a context that survives the run's
// cancellation, like every teardown.
func (h *netnsHelper) Close(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	_ = h.kube.DeleteNow(ctx, h.name)
}

func (h *netnsHelper) manifest(node, image string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    app: ampqa-netns-helper
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
`, h.name, h.kube.Namespace(), node, image)
}

// noPodPIDSentinel is how a scan that RAN and matched nothing reports itself,
// which an exec that never ran cannot forge.
const noPodPIDSentinel = "ampqa-no-pid"

// findPodPIDScript prints the first host pid whose cgroup path carries the pod
// uid, in either spelling the cgroup drivers use (dashes, or underscores under
// systemd slices), and the sentinel when none does.
func findPodPIDScript(uid string) string {
	return fmt.Sprintf(`uid=%s; u2=$(echo "$uid" | tr - _); for p in /proc/[0-9]*; do `+
		`grep -qs -e "$uid" -e "$u2" "$p/cgroup" && { echo "${p#/proc/}"; exit 0; }; done; echo %s`, uid, noPodPIDSentinel)
}

// podPartition is one set of rules applied in one pod's network namespace.
type podPartition struct {
	helper *netnsHelper
	pod    string
	healed bool
}

// isolatePod drops every packet in and out of pod except loopback: the
// whole-node partition of a foundation node. The pod stays up and, to the
// kubelet, exec-able.
func isolatePod(t *core.T, kube *k8s.Kubectl, pod, suffix string) (*podPartition, error) {
	h, err := attachNetns(t, kube, pod, suffix)
	if err != nil {
		return nil, err
	}
	p := &podPartition{helper: h, pod: pod}
	for _, rule := range []string{
		"-I INPUT ! -i lo -j DROP -m comment --comment " + iptablesComment,
		"-I OUTPUT ! -o lo -j DROP -m comment --comment " + iptablesComment,
	} {
		if err := h.iptables(t.Context, rule); err != nil {
			_ = p.Heal(t.Context)
			return nil, fmt.Errorf("isolate pod %s: %w", pod, err)
		}
	}
	return p, nil
}

// cutPodFromBacking drops pod's egress to every backing address: the YB pods
// and the ClusterIPs in front of them (the endpoint dials the tserver Service,
// so the destination its packets carry is the Service's IP, not a pod's). The
// compute stays reachable for its clients and everything else it talks to.
func cutPodFromBacking(t *core.T, kube *k8s.Kubectl, pod, suffix string, bkube *k8s.Kubectl) (*podPartition, error) {
	addrs, err := backingAddresses(t, bkube)
	if err != nil {
		return nil, err
	}
	h, err := attachNetns(t, kube, pod, suffix)
	if err != nil {
		return nil, err
	}
	p := &podPartition{helper: h, pod: pod}
	for _, ip := range addrs {
		if err := h.iptables(t.Context, fmt.Sprintf("-I OUTPUT -d %s -j DROP -m comment --comment %s", ip, iptablesComment)); err != nil {
			_ = p.Heal(t.Context)
			return nil, fmt.Errorf("cut pod %s off %s: %w", pod, ip, err)
		}
	}
	return p, nil
}

// Heal deletes the tagged rules and the helper pod. Idempotent; the rules are
// removed even when the run is being cancelled, since a partition left applied
// outlives the test on a shared cluster.
func (p *podPartition) Heal(ctx context.Context) error {
	if p == nil || p.healed {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	err := p.helper.clearRules(ctx)
	if err == nil {
		p.healed = true
		p.helper.Close(ctx)
	}
	return err
}

// backingAddresses lists every IP the backing namespace answers on: its pods'
// and its Services' ClusterIPs (headless ones have none).
func backingAddresses(t *core.T, bkube *k8s.Kubectl) ([]string, error) {
	pods, err := bkube.GetSelector(t.Context, "pod", "", "{.items[*].status.podIP}")
	if err != nil {
		return nil, fmt.Errorf("list pod IPs in %q: %w", bkube.Namespace(), err)
	}
	svcs, err := bkube.GetSelector(t.Context, "service", "", "{.items[*].spec.clusterIP}")
	if err != nil {
		return nil, fmt.Errorf("list Service ClusterIPs in %q: %w", bkube.Namespace(), err)
	}
	var out []string
	for _, ip := range strings.Fields(pods + " " + svcs) {
		if ip != "" && ip != "None" {
			out = append(out, ip)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("namespace %q has no pod or Service IP to cut the endpoint off from", bkube.Namespace())
	}
	return out, nil
}

// tcpOpen reports whether a TCP connect from inside the pod behind ex reaches
// ip:port. bash's /dev/tcp is in both images; nothing else is assumed.
func tcpOpen(ctx context.Context, ex *k8s.Kubectl, ip string, port int) (bool, error) {
	res, err := ex.Run(ctx, fmt.Sprintf(
		"timeout %d bash -c 'exec 3<>/dev/tcp/%s/%d' >/dev/null 2>&1; echo $?",
		int(partitionProbeTimeout.Seconds()), ip, port))
	if err != nil {
		return false, fmt.Errorf("probe %s:%d from the pod: %w", ip, port, err)
	}
	code, perr := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if perr != nil {
		return false, fmt.Errorf("probe %s:%d from the pod: unreadable exit %q (stderr %q)", ip, port, res.Stdout, strings.TrimSpace(res.Stderr))
	}
	return code == 0, nil
}

// awaitLink polls the pod's view of ip:port until it is open (want=true) or
// closed (want=false), and reports how long that took. A partition that never
// bites, or never lifts, is an error here, not a passing step.
func awaitLink(t *core.T, ex *k8s.Kubectl, pod, ip string, port int, want bool, timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	state := "closed"
	if want {
		state = "open"
	}
	err := t.WaitFor(fmt.Sprintf("%s:%d to be %s from pod %s", ip, port, state, pod),
		timeout, partitionProbeInterval, func() (bool, error) {
			open, err := tcpOpen(t.Context, ex, ip, port)
			if err != nil {
				return false, err
			}
			return open == want, nil
		})
	return time.Since(start), err
}

// backingPodIPs maps every backing serving pod to its IP.
func backingPodIPs(t *core.T, kube *k8s.Kubectl) (map[string]string, error) {
	pods, err := backingPods(t, kube)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(pods))
	for _, pod := range pods {
		ip, err := kube.Get(t.Context, "pod", pod, "{.status.podIP}")
		if err != nil {
			return nil, fmt.Errorf("read the IP of backing pod %s: %w", pod, err)
		}
		if ip == "" {
			return nil, fmt.Errorf("backing pod %s has no IP yet", pod)
		}
		out[pod] = ip
	}
	return out, nil
}

// partitionWriteStats is what the gap-tracking writers saw over one window.
// boundedWriters stops a tenant's writer at its first hung INSERT, which says a
// stall happened but not how long it lasted; a partition's interesting number
// is exactly that.
type partitionWriteStats struct {
	acked, errored, hung int64
	firstErr             atomic.Value
	// LongestGap is the longest span with no acknowledgement from any one
	// tenant, measured from the window's start; Resumed says whether an
	// acknowledgement ended it or the window did.
	LongestGap time.Duration
	Resumed    bool
	// LastAckOffset is how far into the window the last acknowledgement came,
	// -1 when there was none.
	LastAckOffset time.Duration
}

func (st *partitionWriteStats) note(gap time.Duration, resumed bool, lastAck time.Duration, mu *sync.Mutex) {
	mu.Lock()
	defer mu.Unlock()
	if gap > st.LongestGap {
		st.LongestGap, st.Resumed = gap, resumed
	}
	if lastAck > st.LastAckOffset {
		st.LastAckOffset = lastAck
	}
}

// partitionWriters runs one sequential writer per tenant for d, each INSERT
// under its own stall deadline, journals every id's outcome, and keeps writing
// through hangs and errors on a fresh connection so the window measures how
// long the write path was out, not just that it went out.
func partitionWriters(ctx context.Context, env ampTenantEnv, tenants []tenant, journals []*AckJournal,
	d, stall time.Duration, payloadBytes int) *partitionWriteStats {
	st := &partitionWriteStats{LastAckOffset: -1}
	var mu sync.Mutex
	start := time.Now()
	loadCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var wg sync.WaitGroup
	for i := range tenants {
		wg.Add(1)
		go func(ten *tenant, j *AckJournal) {
			defer wg.Done()
			lastAck := start
			for loadCtx.Err() == nil {
				if ten.Conn == nil {
					conn, err := env.ConnectEndpoint(loadCtx, ten.Endpoint)
					if err != nil {
						st.firstErr.CompareAndSwap(nil, "connect: "+err.Error())
						sleepCtx(loadCtx, time.Second)
						continue
					}
					ten.Conn = conn
					atomic.AddInt64(&ten.Reconnects, 1)
				}
				id := ten.nextRowID()
				value := id * 7
				atomic.AddInt64(&ten.Attempted, 1)
				stmtCtx, stmtCancel := context.WithTimeout(context.WithoutCancel(loadCtx), stall)
				_, err := ten.Conn.Exec(stmtCtx, fmt.Sprintf(
					"INSERT INTO %s (id, v, pad) VALUES ($1, $2, repeat('t', $3::int)) ON CONFLICT (id) DO NOTHING", ten.Table),
					id, value, payloadBytes)
				stmtCancel()
				switch {
				case err == nil:
					now := time.Now()
					atomic.AddInt64(&ten.Written, 1)
					atomic.AddInt64(&st.acked, 1)
					j.RecordAcked(id, value)
					st.note(now.Sub(lastAck), true, now.Sub(start), &mu)
					lastAck = now
				case errors.Is(stmtCtx.Err(), context.DeadlineExceeded):
					atomic.AddInt64(&st.hung, 1)
					j.RecordUnknown(id, value)
					ten.Conn.Close()
					ten.Conn = nil
				default:
					atomic.AddInt64(&ten.WriteErrors, 1)
					atomic.AddInt64(&st.errored, 1)
					st.firstErr.CompareAndSwap(nil, err.Error())
					j.RecordUnknown(id, value)
					ten.Conn.Close()
					ten.Conn = nil
					sleepCtx(loadCtx, time.Second)
				}
			}
			st.note(time.Since(lastAck), false, -1, &mu)
		}(&tenants[i], journals[i])
	}
	wg.Wait()
	return st
}

type podSlowdown struct {
	helper *netnsHelper
	pod    string
	dev    string
	healed bool
}

// Egress to port on every peer only, so the rest of the pod's traffic keeps the
// default prio band: a tserver that is up and answering, just late.
func slowPodToPeers(t *core.T, kube *k8s.Kubectl, pod, suffix string, port int, delay time.Duration) (*podSlowdown, error) {
	if delay <= 0 {
		return nil, fmt.Errorf("slow pod %s: the delay must be positive, got %s", pod, delay)
	}
	h, err := attachNetns(t, kube, pod, suffix)
	if err != nil {
		return nil, err
	}
	dev, err := h.defaultDev(t.Context)
	if err != nil {
		h.Close(t.Context)
		return nil, err
	}
	p := &podSlowdown{helper: h, pod: pod, dev: dev}
	for _, cmd := range []string{
		fmt.Sprintf("tc qdisc add dev %s root handle 1: prio", dev),
		fmt.Sprintf("tc qdisc add dev %s parent 1:3 handle 30: netem delay %dms", dev, delay.Milliseconds()),
		fmt.Sprintf("tc filter add dev %s protocol ip parent 1:0 prio 3 u32 match ip dport %d 0xffff flowid 1:3", dev, port),
	} {
		if err := h.inNetns(t.Context, cmd); err != nil {
			_ = p.Heal(t.Context)
			return nil, fmt.Errorf("slow pod %s toward port %d: %w", pod, port, err)
		}
	}
	// A qdisc the kernel accepted but did not install would slow nothing, and
	// the operation would then pass on a fault that never happened.
	out, err := h.outputInNetns(t.Context, "tc qdisc show dev "+dev)
	if err != nil {
		_ = p.Heal(t.Context)
		return nil, err
	}
	if !strings.Contains(out, "netem") {
		_ = p.Heal(t.Context)
		return nil, fmt.Errorf("no netem qdisc on %s in pod %s after adding one: %s", dev, pod, strings.TrimSpace(out))
	}
	return p, nil
}

// Heal removes the qdisc and the helper pod. Idempotent. The helper goes even
// when the qdisc delete fails: nothing retries Heal, so keeping it would only
// strand a privileged pod on the cluster.
func (p *podSlowdown) Heal(ctx context.Context) error {
	if p == nil || p.healed {
		return nil
	}
	p.healed = true
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	err := p.helper.inNetns(ctx, "tc qdisc del dev "+p.dev+" root")
	if errors.Is(err, errNoPodProcess) {
		err = nil
	}
	p.helper.Close(ctx)
	return err
}

func (h *netnsHelper) defaultDev(ctx context.Context) (string, error) {
	out, err := h.outputInNetns(ctx, "ip -o -4 route show default")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", fmt.Errorf("pod %s has no default route to slow (%q)", h.pod, strings.TrimSpace(out))
}

func (h *netnsHelper) inNetns(ctx context.Context, cmd string) error {
	_, err := h.outputInNetns(ctx, cmd)
	return err
}

func (h *netnsHelper) outputInNetns(ctx context.Context, cmd string) (string, error) {
	pid, err := h.podPID(ctx)
	if err != nil {
		return "", err
	}
	res, err := h.exec().Run(ctx, fmt.Sprintf("nsenter -t %s -n -- sh -c '%s'", pid, cmd))
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return res.Stdout, fmt.Errorf("%q in pod %s's netns exited %d: %s", cmd, h.pod, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}
