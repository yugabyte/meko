// The pre-flight gate every fault window opens with, and the attestation it
// closes with. Without evidence that the stack was healthy when the window
// opened, a slow image pull or a neighbouring test's pod churn inside it reads
// as amp stalling. Both verdicts are environmental by default, since a pod
// nobody targeted going down says nothing about the guarantee under test. A
// test whose subject IS the cascade (backing_disk_full) opts out.
package stress

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

type settleParams struct {
	Duration        time.Duration
	MinTPS          float64
	PodReadyTimeout time.Duration
	RestartIsFail   bool
}

func settleParamsFromConfig(c *core.Config, restartIsFailDefault bool) settleParams {
	return settleParams{
		Duration:        c.GetDuration("stress.settle.duration", 20*time.Second),
		MinTPS:          c.GetFloat("stress.settle.min_tps", 1.0),
		PodReadyTimeout: c.GetDuration("stress.settle.pod_ready_timeout", 5*time.Minute),
		RestartIsFail:   c.GetBool("stress.settle.non_target_restart_is_failure", restartIsFailDefault),
	}
}

type settleGate struct {
	kubes   []*k8s.Kubectl
	pods    map[string]k8s.PodState
	tenants []tenant
	p       settleParams
}

// journals may be nil, but a caller that holds storage to an exact set must
// pass them, or the gate's own probe rows read back as ids no writer issued.
// Every failure here is environmental: no fault has been injected yet.
func awaitSteadyState(t *core.T, s *core.StepHandle, env ampTenantEnv, tenants []tenant,
	journals []*AckJournal, payloadBytes int, dur time.Duration) (*settleGate, error) {
	g := &settleGate{kubes: settleKubes(t), tenants: tenants, p: settleParamsFromConfig(t.Options, false)}
	if dur > 0 {
		g.p.Duration = dur
	}
	if len(g.kubes) == 0 {
		return nil, core.EnvFail("no kube context: the fault window cannot attest the stack was healthy when it opened")
	}

	pods, err := g.awaitPodsReady(t, tenants)
	if err != nil {
		return nil, core.AsEnvFailure(err)
	}
	g.pods = pods

	if err := g.awaitControllerReady(t, env); err != nil {
		return nil, core.AsEnvFailure(err)
	}

	acked, err := g.awaitWriteRate(t, env, tenants, journals, payloadBytes)
	if err != nil {
		return nil, core.AsEnvFailure(err)
	}

	if s != nil {
		s.Set("settle_pods", len(pods))
		s.Set("settle_acked", acked)
		s.Set("settle_duration", g.p.Duration.String())
	}
	return g, nil
}

// targets are pod names or workload name prefixes.
func (g *settleGate) attestNonTarget(t *core.T, s *core.StepHandle, targets ...string) error {
	if g == nil {
		return nil
	}
	now, err := g.podSnapshot(t, g.tenants)
	if err != nil {
		return core.AsEnvFailure(fmt.Errorf("read pod states to close the fault window: %w", err))
	}
	var damage []string
	watched := 0
	for _, name := range sortedKeys(g.pods) {
		if isSettleTarget(name, targets) {
			continue
		}
		watched++
		was := g.pods[name]
		is, ok := now[name]
		switch {
		case !ok:
			damage = append(damage, fmt.Sprintf("%s is gone", name))
		case was.UID != "" && is.UID != "" && was.UID != is.UID:
			damage = append(damage, fmt.Sprintf("%s was replaced (a new pod took its name)", name))
		case is.Restarts > was.Restarts:
			damage = append(damage, fmt.Sprintf("%s restarted %d time(s) (%d before the window, %d after)",
				name, is.Restarts-was.Restarts, was.Restarts, is.Restarts))
		}
	}
	if len(damage) == 0 {
		if s != nil {
			s.Set("non_target_pods", watched)
		}
		return nil
	}
	err = fmt.Errorf("%d pod(s) this test never aimed at went down during the fault window: %s",
		len(damage), strings.Join(damage, "; "))
	if g.p.RestartIsFail {
		return err
	}
	return core.AsEnvFailure(err)
}

func settleKubes(t *core.T) []*k8s.Kubectl {
	var out []*k8s.Kubectl
	add := func(k *k8s.Kubectl) {
		if k == nil {
			return
		}
		for _, have := range out {
			if have.Namespace() == k.Namespace() {
				return
			}
		}
		out = append(out, k)
	}
	if e, ok := t.Env.(ampKubeEnv); ok {
		add(e.AmpKube())
	}
	if e, ok := t.Env.(backingKubeEnv); ok {
		add(e.BackingKube())
	}
	return out
}

func (g *settleGate) awaitPodsReady(t *core.T, tenants []tenant) (map[string]k8s.PodState, error) {
	deadline := time.Now().Add(g.p.PodReadyTimeout)
	for {
		pods, err := g.podSnapshot(t, tenants)
		if err != nil {
			return nil, err
		}
		var unready []string
		for _, name := range sortedKeys(pods) {
			if st := pods[name]; st.Phase != "Running" || !st.Ready {
				unready = append(unready, fmt.Sprintf("%s (%s, ready=%v)", name, st.Phase, st.Ready))
			}
		}
		if len(pods) == 0 {
			return nil, fmt.Errorf("no pod in %d namespace(s): the attestation would have nothing to compare and the gate would pass on an empty stack", len(g.kubes))
		}
		if len(unready) == 0 {
			return pods, nil
		}
		if t.Context.Err() != nil {
			return nil, t.Context.Err()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the stack was not healthy when the fault window opened: %d pod(s) still not running and ready after %s: %s",
				len(unready), g.p.PodReadyTimeout, strings.Join(unready, ", "))
		}
		time.Sleep(5 * time.Second)
	}
}

// Succeeded pods (a finished job), another fleet's endpoints and the operator's
// otel collectors are left out: their churn is not this test's.
func (g *settleGate) podSnapshot(t *core.T, tenants []tenant) (map[string]k8s.PodState, error) {
	out := make(map[string]k8s.PodState)
	for _, kube := range g.kubes {
		states, err := kube.PodStates(t.Context)
		if err != nil {
			return nil, fmt.Errorf("read pod states in %q: %w", kube.Namespace(), err)
		}
		for _, st := range states {
			if st.Phase == "Succeeded" || telemetryCollectorPod(st.Name) || foreignEndpointPod(st.Name, tenants) {
				continue
			}
			out[kube.Namespace()+"/"+st.Name] = st
		}
	}
	return out, nil
}

func (g *settleGate) awaitControllerReady(t *core.T, env ampTenantEnv) error {
	deadline := time.Now().Add(g.p.PodReadyTimeout)
	for {
		cl, err := env.AmpController(t.Context)
		if err == nil {
			if err = cl.Readyz(t.Context); err == nil {
				return nil
			}
		}
		if t.Context.Err() != nil {
			return t.Context.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the controller does not answer /readyz within %s, so the fault window would open on a control plane that was already down: %w",
				g.p.PodReadyTimeout, err)
		}
		time.Sleep(5 * time.Second)
	}
}

// Ready pods and a green /readyz still leave the write path untested: the
// endpoint answers SQL long before its pageserver is serving.
func (g *settleGate) awaitWriteRate(t *core.T, env ampTenantEnv, tenants []tenant,
	journals []*AckJournal, payloadBytes int) (int64, error) {
	if len(tenants) == 0 {
		return 0, fmt.Errorf("the fleet has no tenant, so no writer can be held to %.2f row(s)/s and the gate would pass without touching the write path", g.p.MinTPS)
	}
	acked := make([]int64, len(tenants))
	var total int64
	var wg sync.WaitGroup
	start := time.Now()
	deadline := start.Add(g.p.Duration)
	for i := range tenants {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ten := &tenants[i]
			var j *AckJournal
			if journals != nil {
				j = journals[ten.Index]
			}
			for time.Now().Before(deadline) && t.Context.Err() == nil {
				if err := probeWrite(t.Context, env, ten, j, payloadBytes); err == nil {
					atomic.AddInt64(&acked[i], 1)
					atomic.AddInt64(&total, 1)
				}
			}
		}(i)
	}
	wg.Wait()
	if t.Context.Err() != nil {
		return total, t.Context.Err()
	}

	elapsed := time.Since(start).Seconds()
	for i := range tenants {
		if tps := float64(acked[i]) / elapsed; tps < g.p.MinTPS {
			return total, fmt.Errorf("tenant %d (%s) committed %.2f row(s)/s over the %s settle window, under the %.2f/s the fault window needs to mean anything: the write path was already unhealthy (%d write error(s), %d reconnect(s))",
				tenants[i].Index, tenants[i].Name, tps, g.p.Duration, g.p.MinTPS,
				atomic.LoadInt64(&tenants[i].WriteErrors), atomic.LoadInt64(&tenants[i].Reconnects))
		}
	}
	return total, nil
}

func isSettleTarget(name string, targets []string) bool {
	_, pod, _ := strings.Cut(name, "/")
	for _, tg := range targets {
		if tg == "" {
			continue
		}
		if pod == tg || strings.HasPrefix(pod, tg) {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]k8s.PodState) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// key is the test's own switch: the shared stress.settle one is always set by
// the embedded defaults, so it cannot carry a per-test default of true.
func (g *settleGate) asProductFailure(c *core.Config, key string) *settleGate {
	if g != nil {
		g.p.RestartIsFail = c.GetBool(key, true)
	}
	return g
}

// Prefixes, not names: a deleted compute pod comes back under a new random
// suffix.
func fleetEndpointTargets(tenants []tenant) []string {
	out := make([]string, 0, len(tenants))
	for i := range tenants {
		if id := tenants[i].Endpoint.ID; id != "" {
			out = append(out, endpointPodPrefix+id+"-")
		}
	}
	return out
}
