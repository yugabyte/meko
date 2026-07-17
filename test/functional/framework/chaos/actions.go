package chaos

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
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

func (a podDelete) Recover(ctx context.Context, t *Target) error {
	if t.kind != KindPod {
		return nil
	}
	return t.kube.WaitFor(ctx, "pod/"+t.pod, "condition=Ready", a.waitTimeout)
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
	// pkill exits 1 when nothing matched; that is not an injection failure.
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
