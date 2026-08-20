// Chaos aimed at amp itself.
//
// stress/combined_load injects faults into the backing YugabyteDB while
// workloads run straight against that database — a valuable test, but amp is
// not in its request path, so nothing there says what happens when AMP's own
// components fall over. This one kills the pods amp deploys — the endpoint
// computes serving tenants — while those tenants write through them, and then
// demands recovery: every endpoint serves again, the control plane can still
// create a tenant, and every row committed before the chaos is still exactly
// what its writer committed.
//
// Recovery is judged after the fault loop stops, not during it: a compute pod
// that is being deleted is expected to refuse connections. What is NOT
// acceptable is data that does not come back, or a deployment that never
// settles.
package stress

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// endpointPodPrefix is what the controller names an endpoint's compute
// deployment, and therefore how its pods are recognised among the control
// plane's own.
const endpointPodPrefix = "yb-ep-"

func init() {
	core.Register(core.Test{
		Name:        "stress/amp_nemesis",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"stress", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Kill amp's own endpoint compute pods while tenants write through them, then assert recovery: every endpoint serves again, the pre-chaos data verifies exactly, and the control plane can still create a tenant.",
		Params:      []string{"stress.amp_nemesis", "chaos"},
		Timeout:     90 * time.Minute,
		Run:         runAmpNemesis,
	})
}

type ampNemesisParams struct {
	Tenants  int
	Duration time.Duration
	// SettleTimeout bounds how long recovery may take once the faults stop.
	SettleTimeout time.Duration
	PayloadBytes  int
}

func ampNemesisParamsFromConfig(c *core.Config) ampNemesisParams {
	return ampNemesisParams{
		Tenants:       c.GetInt("stress.amp_nemesis.tenants", 3),
		Duration:      c.GetDuration("stress.amp_nemesis.duration", 3*time.Minute),
		SettleTimeout: c.GetDuration("stress.amp_nemesis.settle_timeout", 10*time.Minute),
		PayloadBytes:  c.GetInt("stress.amp_nemesis.payload_bytes", 256),
	}
}

func runAmpNemesis(t *core.T) error {
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, ok := t.Env.(ampKubeEnv)
	if !ok || kubeEnv.AmpKube() == nil {
		t.Skip("requires a kube context for the control-plane namespace: chaos has nothing to aim at without it")
		return nil
	}
	params := ampNemesisParamsFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)
	kube := kubeEnv.AmpKube()

	fleet, err := newTenantFleet(t, env, logf)
	if err != nil {
		return err
	}
	defer fleet.Close()

	if err := t.Step("create_tenants", func(s *core.StepHandle) error {
		if err := fleet.Create(t, params.Tenants); err != nil {
			return err
		}
		s.Set("tenants", len(fleet.Tenants))
		return nil
	}); err != nil {
		return err
	}

	// Written before any fault, so the post-chaos verification has an oracle that
	// predates the chaos entirely.
	if err := t.Step("write_baseline_data", func(s *core.StepHandle) error {
		runWriters(t.Context, env, fleet.Tenants, 20*time.Second, params.PayloadBytes)
		rows := totalWritten(fleet.Tenants)
		s.Set("baseline_rows", rows)
		if rows == 0 {
			return fmt.Errorf("no tenant committed a row before the chaos started")
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("chaos_under_load", func(s *core.StepHandle) error {
		pods, err := endpointPods(t, kube, fleet)
		if err != nil {
			return err
		}
		if len(pods) == 0 {
			return fmt.Errorf("the control-plane namespace has no pod for any of this fleet's %d endpoints to aim at",
				len(fleet.Tenants))
		}
		return chaosOnPods(t, s, env, fleet, kube, pods, "amp-nemesis",
			params.Duration, params.PayloadBytes, logf)
	}); err != nil {
		return err
	}

	if err := t.Step("endpoints_serve_again", func(s *core.StepHandle) error {
		if err := assertEndpointsServe(t, env, fleet.Tenants, params.SettleTimeout); err != nil {
			return err
		}
		s.Set("endpoints_recovered", len(fleet.Tenants))
		return nil
	}); err != nil {
		return err
	}

	// The verification the doc always promised and the code never did: every
	// row acknowledged before or during the chaos read back over a fresh
	// connection, count and content both. Without it, silent data loss under
	// compute-pod chaos passed this test.
	if err := t.Step("verify_data", func(s *core.StepHandle) error {
		return verifyTenantData(t, s, env, fleet.Tenants, params.PayloadBytes)
	}); err != nil {
		return err
	}

	if err := t.Step("amp_pods_healthy", func(s *core.StepHandle) error {
		// After the faults stop, everything amp runs must be back up. This is the
		// recovery assertion: a controller or compute still crash-looping is amp
		// having failed to come back, whatever the data says.
		summary, err := assertAmpHealthy(t, kube, fleet.Tenants)
		if err != nil {
			return err
		}
		s.Set("pods", summary)
		return nil
	}); err != nil {
		return err
	}

	return t.Step("control_plane_recovered", func(s *core.StepHandle) error {
		before := len(fleet.Tenants)
		if err := fleet.Create(t, 1); err != nil {
			return fmt.Errorf("the control plane cannot create a tenant after the chaos: %w", err)
		}
		if len(fleet.Tenants) <= before {
			return fmt.Errorf("creating a tenant after the chaos reported success but added none")
		}
		return nil
	})
}

// endpointPods lists THIS fleet's endpoint computes. The controller's own pod is
// left alone (killing it tests the deployment, not the data path), and so is
// every other fleet's: under a shared control plane a prefix-only match shoots
// concurrently running tests.
func endpointPods(t *core.T, kube *k8s.Kubectl, fleet *tenantFleet) ([]string, error) {
	pods, err := kube.ListPods(t.Context)
	if err != nil {
		return nil, err
	}
	return fleetEndpointPods(pods, fleet.Tenants), nil
}

// fleetEndpointPods keeps the pods backing these tenants' endpoints, named
// yb-ep-<endpoint-id>-<replicaset-hash>-<suffix>.
func fleetEndpointPods(pods []string, tenants []tenant) []string {
	var out []string
	for _, p := range pods {
		if isFleetEndpointPod(p, tenants) {
			out = append(out, p)
		}
	}
	return out
}

func isFleetEndpointPod(pod string, tenants []tenant) bool {
	for i := range tenants {
		if id := tenants[i].Endpoint.ID; id != "" && strings.HasPrefix(pod, endpointPodPrefix+id+"-") {
			return true
		}
	}
	return false
}

// foreignEndpointPod reports whether pod is another fleet's endpoint compute.
func foreignEndpointPod(pod string, tenants []tenant) bool {
	return strings.HasPrefix(pod, endpointPodPrefix) && !isFleetEndpointPod(pod, tenants)
}
