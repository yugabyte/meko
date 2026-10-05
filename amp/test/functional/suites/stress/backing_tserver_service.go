// The operator foundation hands amp one address for the whole universe: a
// ClusterIP Service over every tserver with sessionAffinity None. The thin
// client's sessions live on the tserver that issued them, so a connection the
// Service balances onto another tserver answers `unknown session` (meko#1493).
// A customer lost an RF3 database to it when an endpoint redeploy cold-started
// every compute. The rest of the suite addresses tservers by pod name, where
// this cannot happen, so this test is the only one in the customer's shape.
package stress

import (
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func init() {
	core.Register(core.Test{
		Name:     "stress/backing_tserver_service",
		Suite:    "stress",
		Category: core.Stress,
		Env: core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack,
			BackingConfig: func(*core.Config) map[string]string {
				return map[string]string{
					"env.ampstack.backing_address.mode":             ampstack.BackingAddressService,
					"env.ampstack.backing_address.session_affinity": "None",
				}
			}},
		Tags:       []string{"stress"},
		OwningTeam: core.TeamCompute,
		Description: "Address a multi-tserver backing through one ClusterIP Service with sessionAffinity None, as the operator foundation does. " +
			"A branch's writer is restarted cycle after cycle, reading and writing on the cold compute: every acknowledged row must read back, " +
			"and no endpoint log may carry an `unknown session` from the thin client, even one a retry hid (meko#1493).",
		Params:  []string{"stress.backing_tserver_service", "env.ampstack.backing_address"},
		Timeout: 75 * time.Minute,
		Run:     runBackingTserverService,
	})
}

func tserverPods(t *core.T, kube *k8s.Kubectl) ([]string, error) {
	for _, sel := range tserverSelectors {
		names, err := kube.GetSelector(t.Context, "pod", sel, "{.items[*].metadata.name}")
		if err != nil {
			return nil, fmt.Errorf("list tserver pods %s in %q: %w", sel, kube.Namespace(), err)
		}
		if pods := strings.Fields(names); len(pods) > 0 {
			return pods, nil
		}
	}
	return nil, nil
}

func runBackingTserverService(t *core.T) error {
	c := t.Options
	cycles := c.GetInt("stress.backing_tserver_service.cycles", 6)
	rows := c.GetInt("stress.backing_tserver_service.rows", 5000)
	rowsPerCycle := c.GetInt("stress.backing_tserver_service.rows_per_cycle", 500)
	lb, ok, err := newLiveBranchRun(t, c.GetDuration("stress.backing_tserver_service.ready_timeout", 10*time.Minute))
	if !ok {
		t.Skip("requires a standing amp control plane with a kube handle for the endpoint pods (sharedk8s)")
		return nil
	}
	if err != nil {
		return err
	}
	bk, ok := t.Env.(backingKubeEnv)
	if !ok || bk.BackingKube() == nil {
		t.Skip("requires a backing in this cluster: the tserver Service is created beside it")
		return nil
	}
	pods, err := tserverPods(t, bk.BackingKube())
	if err != nil {
		return err
	}
	if len(pods) == 0 {
		return core.EnvFail("the backing in %q runs no tserver pod: this env did not give the test the YugabyteDB-in-k8s backing a tserver Service needs", bk.BackingKube().Namespace())
	}
	if len(pods) < 2 {
		t.Skipf("the backing runs %d tserver(s): a Service over one tserver cannot route a session to the wrong one; run with an RF3 backing", len(pods))
		return nil
	}
	defer lb.cleanup()
	return lb.steps(
		liveStep{"live_branch", func(s *core.StepHandle) error {
			s.Set("tservers", len(pods))
			if err := lb.createLiveBranch(s, "ts-svc", rows); err != nil {
				return err
			}
			for _, le := range lb.eps {
				hp, err := lb.hostPorts(le)
				if err != nil {
					return err
				}
				s.Set(le.role+"_yb_host_ports", hp)
				if !strings.Contains(hp, "ampqa-tserver-conn-") || strings.Contains(hp, ",") {
					return core.EnvFail("this env did not apply backing_address=service: the %s endpoint got YB_HOST_PORTS=%q", le.role, hp)
				}
			}
			return nil
		}},
		liveStep{"restart_cycles", func(s *core.StepHandle) error { return lb.restartCycles(s, cycles, rowsPerCycle, nil) }},
		liveStep{"no_unknown_session", lb.noUnknownSession},
	)
}
