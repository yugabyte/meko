// Package netshape adds network latency between pods on k8s without touching
// their images. Neither the endpoint image nor the YB pods carry tc or
// NET_ADMIN, so a privileged, hostPID helper pod pinned to the target's node
// finds one of the target's processes through /proc and runs tc inside that
// network namespace with nsenter. Delay is per destination: a prio qdisc with
// one netem band per distinct one-way delay and a u32 filter per destination
// IP, so traffic to anything unlisted is untouched. Applied on both ends of a
// pair, two one-way delays add up to the pair's round trip.
package netshape

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	helperReadyTimeout = 5 * time.Minute
	closeBudget        = 2 * time.Minute
	// netemLimit is the netem queue depth in packets. The default 1000 drops
	// packets once delay x packet rate exceeds it, which turns a latency
	// experiment into a loss experiment at 50 ms and a few thousand pps.
	netemLimit = 100000
)

// Helper reaches one target pod's network namespace.
type Helper struct {
	kube *k8s.Kubectl
	name string
	uid  string
	node string
	pod  string
}

// Attach stands a helper up on the target's node. Any pid of the pod does.
func Attach(ctx context.Context, kube *k8s.Kubectl, pod, suffix, image string) (*Helper, error) {
	node, err := kube.Get(ctx, "pod", pod, "{.spec.nodeName}")
	if err != nil {
		return nil, fmt.Errorf("read the node of pod %s: %w", pod, err)
	}
	uid, err := kube.Get(ctx, "pod", pod, "{.metadata.uid}")
	if err != nil {
		return nil, fmt.Errorf("read the uid of pod %s: %w", pod, err)
	}
	if node == "" || uid == "" {
		return nil, fmt.Errorf("pod %s has no node or uid yet (node %q, uid %q)", pod, node, uid)
	}
	h := &Helper{kube: kube, name: "ampqa-netshape-" + suffix, uid: uid, node: node, pod: pod}
	if err := kube.Apply(ctx, h.manifest(image)); err != nil {
		return nil, fmt.Errorf("create the netshape helper pod %s on node %s: %w", h.name, node, err)
	}
	if err := kube.WaitFor(ctx, "pod/"+h.name, "condition=Ready", helperReadyTimeout); err != nil {
		h.Close(ctx)
		return nil, fmt.Errorf("netshape helper pod %s never became Ready on node %s: %w (%s)", h.name, node, err, kube.PodTrouble(ctx, h.name))
	}
	if _, err := h.podPID(ctx); err != nil {
		h.Close(ctx)
		return nil, err
	}
	return h, nil
}

// Pod is the target pod's name.
func (h *Helper) Pod() string { return h.pod }

// Adopt binds to a helper pod that already stands on the target's node, for a
// caller that created it itself and for tests that script the executor. Attach
// is the lifecycle entry point and the one live callers want.
func Adopt(kube *k8s.Kubectl, name, uid, node, pod string) *Helper {
	return &Helper{kube: kube, name: name, uid: uid, node: node, pod: pod}
}

// RunOutput is Run returning trimmed stdout, erroring on a non-zero exit.
func (h *Helper) RunOutput(ctx context.Context, script string) (string, error) {
	res, err := h.Run(ctx, script)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%q in the netns of pod %s exited %d: %s", script, h.pod, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (h *Helper) exec() *k8s.Kubectl { return h.kube.ForPod(h.name, "") }

// ErrNoPodProcess means the pod has no process left on the node, so its
// network namespace — and anything applied inside it — is gone. A caller
// undoing something treats it as already undone.
var ErrNoPodProcess = errors.New("no process on the node belongs to the target pod")

const noPodPIDSentinel = "ampqa-no-pid"

// findPodPIDScript prints the lowest host pid whose cgroup path carries the pod
// uid in either spelling the cgroup drivers use, and the sentinel when none
// does. Lowest by number, not by glob order: the oldest process is a container
// init, while the first of /proc/[0-9]* can be a backend that exits before
// nsenter opens its namespace (run 249b7524).
func findPodPIDScript(uid string) string {
	return fmt.Sprintf(`uid=%s; u2=$(echo "$uid" | tr - _); for p in $(ls /proc | grep -E '^[0-9]+$' | sort -n); do `+
		`grep -qs -e "$uid" -e "$u2" "/proc/$p/cgroup" && { echo "$p"; exit 0; }; done; echo %s`, uid, noPodPIDSentinel)
}

// Resolved per call: a container restart retires the pid, not the namespace.
func (h *Helper) podPID(ctx context.Context) (string, error) {
	res, err := h.exec().Run(ctx, findPodPIDScript(h.uid))
	if err != nil {
		return "", fmt.Errorf("scan /proc on node %s for pod %s: %w", h.node, h.pod, err)
	}
	out := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 {
		return "", fmt.Errorf("scan /proc on node %s for pod %s exited %d: %s", h.node, h.pod, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if out == noPodPIDSentinel {
		return "", fmt.Errorf("%w: pod %s (uid %s) on node %s", ErrNoPodProcess, h.pod, h.uid, h.node)
	}
	if out == "" {
		return "", fmt.Errorf("scan /proc on node %s for pod %s printed nothing (stderr %q)", h.node, h.pod, strings.TrimSpace(res.Stderr))
	}
	return out, nil
}

// stalePID is nsenter's complaint when the resolved process exited between
// the scan and the enter; the namespace is still there behind another pid.
const stalePID = "/ns/net: No such file or directory"

// Run executes script inside the target's network namespace, re-resolving the
// pid once if the one it picked is gone by the time nsenter opens it.
func (h *Helper) Run(ctx context.Context, script string) (exec.Result, error) {
	var res exec.Result
	for attempt := 0; attempt < 2; attempt++ {
		pid, err := h.podPID(ctx)
		if err != nil {
			return exec.Result{}, err
		}
		res, err = h.exec().Run(ctx, fmt.Sprintf("nsenter -t %s -n -- sh -c %s", pid, shellQuote(script)))
		if err != nil || res.ExitCode == 0 || !strings.Contains(res.Stderr, stalePID) {
			return res, err
		}
	}
	return res, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Delay is one destination's added one-way delay.
type Delay struct {
	Dst    string
	OneWay time.Duration
}

// Script renders the tc commands applying delays on device: the root prio
// qdisc, one netem band per distinct delay, one u32 filter per destination.
// Unlisted traffic falls through the last band unshaped. Empty delays render
// nothing to apply.
func Script(device string, delays []Delay) string {
	groups := map[time.Duration][]string{}
	for _, d := range delays {
		if d.OneWay <= 0 {
			continue
		}
		groups[d.OneWay] = append(groups[d.OneWay], d.Dst)
	}
	if len(groups) == 0 {
		return ""
	}
	keys := make([]time.Duration, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	bands := len(keys) + 1
	if bands < 2 {
		bands = 2
	}
	priomap := strings.TrimSpace(strings.Repeat(strconv.Itoa(bands-1)+" ", 16))
	var b strings.Builder
	fmt.Fprintf(&b, "set -e; tc qdisc del dev %s root 2>/dev/null || true; ", device)
	fmt.Fprintf(&b, "tc qdisc add dev %s root handle 1: prio bands %d priomap %s; ", device, bands, priomap)
	for i, k := range keys {
		band := i + 1
		fmt.Fprintf(&b, "tc qdisc add dev %s parent 1:%d handle %d: netem limit %d delay %dus; ",
			device, band, band*10, netemLimit, k.Microseconds())
		dsts := groups[k]
		sort.Strings(dsts)
		for _, dst := range dsts {
			fmt.Fprintf(&b, "tc filter add dev %s parent 1: protocol ip prio 1 u32 match ip dst %s/32 flowid 1:%d; ", device, dst, band)
		}
	}
	return strings.TrimSpace(b.String())
}

// Apply installs delays on device inside the target's network namespace,
// replacing whatever shaping was there. A member co-located with every other
// one has nothing to add and is CLEARED rather than left alone: returning
// early there would carry the previous topology's qdisc into this one.
func (h *Helper) Apply(ctx context.Context, device string, delays []Delay) error {
	script := Script(device, delays)
	if script == "" {
		return h.clear(ctx, device)
	}
	res, err := h.Run(ctx, script)
	if err != nil {
		return fmt.Errorf("shape %s: %w", h.pod, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("shape %s: tc exited %d: %s", h.pod, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// Clear removes every qdisc this package added on device, restoring the kernel
// default. Rooted in a context that survives the run's cancellation: shaping
// left applied outlives the test on a shared cluster.
func (h *Helper) Clear(ctx context.Context, device string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeBudget)
	defer cancel()
	return h.clear(ctx, device)
}

// clear is Clear on the caller's own context. A pod whose processes are all
// gone has nothing left to clear.
func (h *Helper) clear(ctx context.Context, device string) error {
	res, err := h.Run(ctx, fmt.Sprintf("tc qdisc del dev %s root 2>&1 || true", device))
	if errors.Is(err, ErrNoPodProcess) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("clear shaping on %s: %w", h.pod, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("clear shaping on %s: exited %d: %s", h.pod, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// pingAttempts is how many times a ping whose kubectl exec never ran (an
// empty stdout, the API server unreachable for a moment) is retried; a ping
// that ran and lost every packet is not, that is an answer.
const pingAttempts = 3

// A var so the unit test does not wait it out.
var pingRetryGap = 5 * time.Second

// Ping measures the average round trip from the target pod to ip over count
// echoes, sent 200 ms apart.
func (h *Helper) Ping(ctx context.Context, ip string, count int) (time.Duration, error) {
	if count < 1 {
		count = 1
	}
	var last error
	for attempt := 1; attempt <= pingAttempts; attempt++ {
		res, err := h.Run(ctx, fmt.Sprintf("ping -n -q -c %d -i 0.2 -W 3 %s", count, ip))
		if err != nil {
			return 0, fmt.Errorf("ping %s from %s: %w", ip, h.pod, err)
		}
		rtt, perr := ParsePingRTT(res.Stdout)
		if perr == nil {
			return rtt, nil
		}
		last = fmt.Errorf("ping %s from %s (exit %d): %w: %s", ip, h.pod, res.ExitCode, perr, strings.TrimSpace(res.Stdout+res.Stderr))
		if strings.TrimSpace(res.Stdout) != "" {
			return 0, last
		}
		if attempt < pingAttempts {
			select {
			case <-time.After(pingRetryGap):
			case <-ctx.Done():
				return 0, errors.Join(ctx.Err(), last)
			}
		}
	}
	return 0, last
}

// pingSummary matches both iputils ("rtt min/avg/max/mdev = a/b/c/d ms") and
// busybox ("round-trip min/avg/max = a/b/c ms").
var pingSummary = regexp.MustCompile(`min/avg/max[^=]*= *([0-9.]+)/([0-9.]+)/`)

// ParsePingRTT reads the average round trip out of ping's summary line.
func ParsePingRTT(out string) (time.Duration, error) {
	m := pingSummary.FindStringSubmatch(out)
	if m == nil {
		return 0, errors.New("no rtt summary in ping output")
	}
	avg, err := strconv.ParseFloat(m[2], 64)
	if err != nil {
		return 0, fmt.Errorf("unreadable avg rtt %q", m[2])
	}
	return time.Duration(avg * float64(time.Millisecond)), nil
}

// Close deletes the helper pod. Rooted in a context that survives the run's
// cancellation, like every teardown.
func (h *Helper) Close(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeBudget)
	defer cancel()
	_ = h.kube.DeleteNow(ctx, h.name)
}

func (h *Helper) manifest(image string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    app: ampqa-netshape-helper
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
