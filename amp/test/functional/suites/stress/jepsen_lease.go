// The lease-race nemesis: a second compute takes a branch's write lease from a
// stalled first one while jepsen's clients reach both, so a fence that fails
// shows up in jepsen's own history as two computes acknowledging on the same
// keys.
//
// The controller allows one read_write endpoint per branch
// (endpoints_one_writer_per_branch), so the second compute is a second POD of
// the same endpoint: its Deployment scaled to 2 while the first pod is frozen.
// That is what a node partition produces when the replacement pod starts
// before the old one has died, and the case the 2026-09 jepsen run on amp did
// not cover ("two writers racing for the lease"): the class of meko#1569 /
// meko#1571, a stalled old owner landing writes after another took the branch.
package stress

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/chaos"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

type leaseRaceParams struct {
	Enabled bool
	// Action stalls the current writer; pod_freeze stops every process in the
	// pod, so no daemon can renew the lease or notice it lapsed.
	Action string
	// Stall must exceed the write lease (stress.fence_stall.lease_ttl, 15s by
	// default) plus skew grace, or the second pod cannot take it.
	Stall time.Duration
	// Hold is how long both pods stay behind the Service after the first thaws:
	// the window in which a broken fence would let both acknowledge writes.
	Hold time.Duration
	// Settle bounds the second pod becoming Ready, and the scale-down.
	Settle time.Duration
}

func leaseRaceParamsFromConfig(c *core.Config) leaseRaceParams {
	return leaseRaceParams{
		Enabled: c.GetBool("stress.jepsen.lease_race.enabled", false),
		Action:  c.GetString("stress.jepsen.lease_race.action", chaos.ActionPodFreeze),
		Stall:   c.GetDuration("stress.jepsen.lease_race.stall", 45*time.Second),
		Hold:    c.GetDuration("stress.jepsen.lease_race.hold", 60*time.Second),
		Settle:  c.GetDuration("stress.jepsen.lease_race.settle_timeout", 10*time.Minute),
	}
}

// leaseRun is what one fault window's lease races did, for the step record.
type leaseRun struct {
	mu        sync.Mutex
	Cycles    int
	Takeovers int
	Refused   []string
	Errors    []string
	// Thaw lists frozen pods that were never thawed: the endpoint is then
	// left wedged, and nothing after it is attributable.
	Thaw   []string
	Probes []probeResult
}

// Findings are the probes that caught the old pod serving a stale snapshot
// or acknowledging a write after the new pod took the lease, by tenant.
func (lr *leaseRun) Findings() map[int][]string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	out := map[int][]string{}
	for _, p := range lr.Probes {
		if p.Stale() || p.OldWriteAccepted {
			out[p.Tenant] = append(out[p.Tenant], p.String())
		}
	}
	return out
}

// Failure is why a window's lease race proves nothing about the fence, or nil.
// A second compute that never took over is amp's answer; anything else that
// left no evidence is the harness.
func (lr *leaseRun) Failure(window time.Duration) error {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	switch {
	case len(lr.Thaw) > 0:
		return core.AsEnvFailure(fmt.Errorf("the lease race left a pod frozen: %s", strings.Join(lr.Thaw, "; ")))
	case lr.Takeovers == 0 && len(lr.Refused) > 0:
		return fmt.Errorf("the lease race completed no takeover in %s, so this run proves nothing about the fence: %s %s",
			window, strings.Join(lr.Refused, "; "), strings.Join(lr.Errors, "; "))
	case lr.Takeovers == 0:
		return core.AsEnvFailure(fmt.Errorf("the lease race never reached a takeover in %s: %s",
			window, strings.Join(lr.Errors, "; ")))
	case len(lr.Probes) == 0:
		return core.AsEnvFailure(fmt.Errorf("no lease-race cycle produced a probe result in %s, so there is no evidence about the fence: %s",
			window, strings.Join(lr.Errors, "; ")))
	}
	return nil
}

func (lr *leaseRun) note(field *[]string, format string, args ...any) {
	lr.mu.Lock()
	*field = append(*field, fmt.Sprintf(format, args...))
	lr.mu.Unlock()
}

// runLeaseRace runs takeover cycles on every tenant until d has passed.
func runLeaseRace(t *core.T, rigs []*jepsenRig, env ampTenantEnv, fleet *tenantFleet, p leaseRaceParams,
	d time.Duration, sink attachSink, logf func(string, ...any)) *leaseRun {

	run := &leaseRun{}
	deadline := time.Now().Add(d)
	var wg sync.WaitGroup
	for i := range rigs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Add(p.Stall+p.Hold).Before(deadline) && t.Context.Err() == nil {
				run.mu.Lock()
				run.Cycles++
				cycle := run.Cycles
				run.mu.Unlock()
				if err := leaseCycle(t, rigs[i], env, fleet, i, cycle, p, run, sink, logf); err != nil {
					run.note(&run.Errors, "tenant %d: %v", i, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	return run
}

// endpointPodNames lists the endpoint's pods, by name.
func endpointPodNames(ctx context.Context, rig *jepsenRig, ep controller.Endpoint) ([]string, error) {
	pods, err := rig.kube.ListPods(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, pn := range pods {
		if strings.HasPrefix(pn, endpointPodPrefix+ep.ID+"-") {
			out = append(out, pn)
		}
	}
	return out, nil
}

// endpointScalable is the workload behind the endpoint's own Service, found by
// that Service's selector: its name is the helm release's, which the test does
// not choose. It is a Deployment today and a bare ReplicaSet once the endpoint
// moves off the rollout layer, the same fallback
// ampstack.WaitEndpointWorkloadReady makes. A Deployment's own ReplicaSets
// match the selector too, so the Deployment is looked for first.
func endpointScalable(ctx context.Context, rig *jepsenRig, ep controller.Endpoint) (kind, name string, err error) {
	selector, err := rig.kube.Get(ctx, "svc", "yb-ep-"+ep.ID, "{.spec.selector}")
	if err != nil {
		return "", "", err
	}
	sel, err := parseJSONStringMap(selector)
	if err != nil || len(sel) == 0 {
		return "", "", fmt.Errorf("endpoint %s Service has no usable selector (%q): %v", ep.ID, selector, err)
	}
	var terms []string
	for _, k := range sortedMapKeys(sel) {
		terms = append(terms, k+"="+sel[k])
	}
	selStr := strings.Join(terms, ",")
	for _, kind := range []string{"deployment", "replicaset"} {
		names, err := rig.kube.GetSelector(ctx, kind, selStr, "{.items[*].metadata.name}")
		if err != nil {
			return "", "", err
		}
		switch f := strings.Fields(names); len(f) {
		case 0:
			continue
		case 1:
			return kind, f[0], nil
		default:
			return "", "", fmt.Errorf("endpoint %s: want exactly one %s for selector %s, found %q", ep.ID, kind, selStr, names)
		}
	}
	return "", "", fmt.Errorf("endpoint %s: no Deployment or ReplicaSet for selector %s", ep.ID, selStr)
}

// leaseCycle: freeze the endpoint's pod past its lease, start a second pod of
// the same endpoint, thaw the first while the second holds the lease, hold,
// then scale back to one pod.
func leaseCycle(t *core.T, rig *jepsenRig, env ampTenantEnv, fleet *tenantFleet, i, cycle int,
	p leaseRaceParams, run *leaseRun, sink attachSink, logf func(string, ...any)) error {

	ten := &fleet.Tenants[i]
	ep := ten.Endpoint
	pods, err := endpointPodNames(t.Context, rig, ep)
	if err != nil {
		return err
	}
	if len(pods) != 1 {
		return fmt.Errorf("endpoint %s has %d pods before the cycle, want 1: %v", ep.ID, len(pods), pods)
	}
	first := pods[0]
	kind, workload, err := endpointScalable(t.Context, rig, ep)
	if err != nil {
		return err
	}

	act, err := chaos.DefaultRegistry().Build(p.Action, chaos.FromConfig(t.Options))
	if err != nil {
		return err
	}
	target := chaos.NewPodTarget(first, rig.kube, first, "")
	if err := act.Inject(t.Context, target); err != nil {
		_ = act.Recover(context.WithoutCancel(t.Context), target)
		return fmt.Errorf("stall pod %s (%s): %w", first, p.Action, err)
	}
	stalledAt := time.Now()
	thawed := false
	thaw := func() {
		if !thawed {
			thawed = true
			if err := act.Recover(context.WithoutCancel(t.Context), target); err != nil {
				run.note(&run.Thaw, "tenant %d: thaw pod %s: %v", i, first, err)
			}
		}
	}
	defer thaw()
	scaledUp := false
	defer func() {
		if scaledUp {
			if err := scaleWorkload(context.WithoutCancel(t.Context), rig.kube, kind, workload, 1); err != nil {
				run.note(&run.Errors, "tenant %d: scale %s/%s back to 1: %v", i, kind, workload, err)
			}
		}
	}()

	select {
	case <-t.Context.Done():
		return t.Context.Err()
	case <-time.After(p.Stall):
	}
	if err := scaleWorkload(t.Context, rig.kube, kind, workload, 2); err != nil {
		return fmt.Errorf("scale %s/%s to 2: %w", kind, workload, err)
	}
	scaledUp = true

	// The second pod is Ready once its postgres listens, which needs the lease.
	second, err := waitSecondReady(t, rig, ep, first, p.Settle)
	if err != nil {
		run.note(&run.Refused, "tenant %d: second compute of %s never became Ready %s after %s was frozen: %v",
			i, ep.ID, time.Since(stalledAt).Round(time.Second), first, err)
		return nil
	}
	thaw()
	logf("jepsen lease race tenant %d: pod %s serves endpoint %s while frozen-then-thawed %s is back; both behind its Service for %s",
		i, second, ep.ID, first, p.Hold)
	run.mu.Lock()
	run.Takeovers++
	run.mu.Unlock()

	holdUntil := time.Now().Add(p.Hold)
	wait := p.Hold / 2
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	if pr, err := probeComputes(t, rig.kube, i, cycle, first, second, wait); err != nil {
		run.note(&run.Errors, "tenant %d: probe: %v", i, err)
	} else {
		logf("jepsen lease race tenant %d: %s", i, pr)
		run.mu.Lock()
		run.Probes = append(run.Probes, pr)
		run.mu.Unlock()
	}
	select {
	case <-t.Context.Done():
		return t.Context.Err()
	case <-time.After(time.Until(holdUntil)):
	}
	// Both pods' logs, before the scale-down removes one of them.
	for _, pn := range []string{first, second} {
		lctx, lcancel := context.WithTimeout(context.WithoutCancel(t.Context), time.Minute)
		if out, err := rig.kube.LogsAll(lctx, "pod/"+pn, 200000); err == nil {
			_ = sink.Add(fmt.Sprintf("endpoint/cycle%d/%s.log", cycle, pn), "log", []byte(out))
		}
		lcancel()
	}
	if err := scaleWorkload(t.Context, rig.kube, kind, workload, 1); err != nil {
		return fmt.Errorf("scale %s/%s back to 1: %w", kind, workload, err)
	}
	scaledUp = false
	return waitOnePod(t, rig, ep, p.Settle)
}

func waitSecondReady(t *core.T, rig *jepsenRig, ep controller.Endpoint, first string, d time.Duration) (string, error) {
	deadline := time.Now().Add(d)
	for {
		pods, err := endpointPodNames(t.Context, rig, ep)
		if err == nil {
			for _, pn := range pods {
				if pn == first {
					continue
				}
				ready, _ := rig.kube.Get(t.Context, "pod", pn, `{.status.conditions[?(@.type=="Ready")].status}`)
				if strings.TrimSpace(ready) == "True" {
					return pn, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no second Ready pod within %s (pods %v)", d, pods)
		}
		select {
		case <-t.Context.Done():
			return "", t.Context.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// waitOnePod waits for the scale-down to leave exactly one Ready pod.
func waitOnePod(t *core.T, rig *jepsenRig, ep controller.Endpoint, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		pods, err := endpointPodNames(t.Context, rig, ep)
		if err == nil && len(pods) == 1 {
			ready, _ := rig.kube.Get(t.Context, "pod", pods[0], `{.status.conditions[?(@.type=="Ready")].status}`)
			if strings.TrimSpace(ready) == "True" {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("endpoint %s did not settle on one Ready pod within %s (pods %v)", ep.ID, d, pods)
		}
		select {
		case <-t.Context.Done():
			return t.Context.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// scaleWorkload sets a Deployment's or ReplicaSet's replica count with a merge
// patch.
func scaleWorkload(ctx context.Context, kube *k8s.Kubectl, kind, name string, replicas int) error {
	return kube.Patch(ctx, kind, name, "merge", fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
}
