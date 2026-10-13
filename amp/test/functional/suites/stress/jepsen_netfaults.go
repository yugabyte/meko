// The three network actions jepsen's backing loop draws, re-implemented over
// the netns helper (framework/apps/netshape, via partition_helper.go). The chaos registry's own versions run
// iptables and tc by exec'ing into the target, and a YB pod has neither the
// binaries nor NET_ADMIN, so there every inject fails.
package stress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/netshape"
	"github.com/yugabyte/amp/test/functional/framework/chaos"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// healer is an applied network fault; podPartition, podSlowdown and podLoss
// all are.
type healer interface {
	Heal(ctx context.Context) error
}

// netnsSeq numbers helper pods across every loop in the process: two loops
// faulting at once must never ask for the same pod name.
var netnsSeq atomic.Int64

// netnsRegistry is the default registry with network_partition,
// network_slowdown and packet_loss applied through a helper pod instead.
// kube must be the namespace of the targets the runner is given.
func netnsRegistry(t *core.T, kube *k8s.Kubectl) *chaos.Registry {
	r := chaos.DefaultRegistry()
	suffix := func() string { return fmt.Sprintf("%s-j%d", t.ResourceSuffix(), netnsSeq.Add(1)) }
	add := func(name string, apply func(pod, suffix string, c chaos.Config) (healer, error)) {
		r.Register(name, func(c chaos.Config) chaos.ChaosAction {
			return &netnsAction{name: name, active: map[string]healer{},
				apply: func(pod string) (healer, error) { return apply(pod, suffix(), c) }}
		})
	}
	add(chaos.ActionNetworkPartition, func(pod, sfx string, _ chaos.Config) (healer, error) {
		return isolateProven(t, kube, pod, sfx)
	})
	add(chaos.ActionNetworkSlowdown, func(pod, sfx string, c chaos.Config) (healer, error) {
		return slowPodToPeers(t, kube, pod, sfx, tserverRPCPort, time.Duration(c.DelayMs)*time.Millisecond)
	})
	add(chaos.ActionPacketLoss, func(pod, sfx string, c chaos.Config) (healer, error) {
		return losePackets(t, kube, pod, sfx, c.LossPercent)
	})
	return r
}

// isolateProven partitions a backing pod, then probes a peer's RPC port from
// inside it until the link is closed. A rule that drops nothing must fail the
// inject, never count as a landed fault.
func isolateProven(t *core.T, kube *k8s.Kubectl, pod, suffix string) (healer, error) {
	pods, err := backingPods(t, kube)
	if err != nil {
		return nil, fmt.Errorf("partition %s: list its peers: %w", pod, err)
	}
	peer := ""
	for _, p := range pods {
		if p != pod {
			peer = p
			break
		}
	}
	if peer == "" {
		return nil, fmt.Errorf("partition %s: no peer to prove the partition against (pods %v)", pod, pods)
	}
	part, err := isolatePod(t, kube, pod, suffix)
	if err != nil {
		return nil, err
	}
	addr := podIPNow(t.Context, kube, peer, "")
	if _, err := awaitLink(t, kube.ForPod(pod, ""), pod, addr, tserverRPCPort, false, partitionEnforceTimeout); err != nil {
		_ = part.Heal(t.Context)
		return nil, fmt.Errorf("the DROP rules in %s's network namespace never bit, so no partition was injected: %w", pod, err)
	}
	return part, nil
}

// netnsAction adapts a helper-pod fault to chaos.ChaosAction. The target's
// name is its pod name (runJepsenFaults builds them that way).
type netnsAction struct {
	name   string
	apply  func(pod string) (healer, error)
	mu     sync.Mutex
	active map[string]healer
}

func (a *netnsAction) Name() string { return a.name }

func (a *netnsAction) Inject(_ context.Context, tg *chaos.Target) error {
	h, err := a.apply(tg.Name())
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.active[tg.Name()] = h
	a.mu.Unlock()
	return nil
}

// Recover heals what Inject applied. An inject that failed left nothing: each
// helper heals its own partial state before returning the error.
func (a *netnsAction) Recover(ctx context.Context, tg *chaos.Target) error {
	a.mu.Lock()
	h := a.active[tg.Name()]
	delete(a.active, tg.Name())
	a.mu.Unlock()
	if h == nil {
		return nil
	}
	return h.Heal(ctx)
}

// podLoss is a netem loss qdisc on a pod's default device.
type podLoss struct {
	helper *netshape.Helper
	dev    string
	healed bool
}

// losePackets drops percent of everything the pod sends. Egress only, like
// the registry's packet_loss: the peers' replies are unaffected.
func losePackets(t *core.T, kube *k8s.Kubectl, pod, suffix string, percent int) (*podLoss, error) {
	if percent <= 0 || percent > 100 {
		return nil, fmt.Errorf("packet loss on pod %s: percent must be in 1..100, got %d", pod, percent)
	}
	h, err := attachNetns(t, kube, pod, suffix)
	if err != nil {
		return nil, err
	}
	dev, err := defaultDev(t.Context, h)
	if err != nil {
		h.Close(t.Context)
		return nil, err
	}
	p := &podLoss{helper: h, dev: dev}
	if _, err := h.RunOutput(t.Context, fmt.Sprintf("tc qdisc add dev %s root netem loss %d%%", dev, percent)); err != nil {
		_ = p.Heal(t.Context)
		return nil, fmt.Errorf("packet loss on pod %s: %w", pod, err)
	}
	// Same check as slowPodToPeers: an accepted but uninstalled qdisc drops nothing.
	out, err := h.RunOutput(t.Context, "tc qdisc show dev "+dev)
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

// Heal removes the qdisc and the helper pod. Idempotent; the helper goes even
// when the delete fails, for the reason podSlowdown.Heal gives.
func (p *podLoss) Heal(ctx context.Context) error {
	if p == nil || p.healed {
		return nil
	}
	p.healed = true
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	_, err := p.helper.RunOutput(ctx, "tc qdisc del dev "+p.dev+" root")
	if errors.Is(err, netshape.ErrNoPodProcess) {
		err = nil
	}
	p.helper.Close(ctx)
	return err
}
