package stress

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/chaos"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// Shared machinery of the nemesis tests.

// backingKubeEnv: a kube handle scoped to the backing's own namespace.
type backingKubeEnv interface {
	BackingKube() *k8s.Kubectl
}

// One selector per backing shape (ybk8s, YBA universe, built-in yugabyted).
// Masters are left alone: this tier aims at the WAL/data path.
var backingPodSelectors = []string{
	"app=yb-tserver",
	"app.kubernetes.io/name=yb-tserver",
	"app=yugabyted",
}

// First selector that matches wins: a backing never has two shapes at once.
func backingPods(t *core.T, kube *k8s.Kubectl) ([]string, error) {
	for _, sel := range backingPodSelectors {
		names, err := kube.GetSelector(t.Context, "pod", sel, "{.items[*].metadata.name}")
		if err != nil {
			return nil, fmt.Errorf("list backing pods %s in %q: %w", sel, kube.Namespace(), err)
		}
		if pods := strings.Fields(names); len(pods) > 0 {
			return pods, nil
		}
	}
	return nil, nil
}

// chaosOnPods faults pods while tenants write, and fails when nothing was
// injected: a chaos step that never fired proves nothing. pod_delete only —
// the strongest recoverable fault, and the only one meaningful everywhere.
func chaosOnPods(t *core.T, s *core.StepHandle, env ampTenantEnv, fleet *tenantFleet,
	kube *k8s.Kubectl, pods []string, seedName string, d time.Duration, payloadBytes int,
	logf func(string, ...any)) error {
	s.Set("targets", strings.Join(pods, ","))

	targets := make([]*chaos.Target, 0, len(pods))
	for _, pod := range pods {
		targets = append(targets, chaos.NewPodTarget(pod, kube, pod, ""))
	}

	var mu sync.Mutex
	injects := 0
	cfg := chaos.FromConfig(t.Options)
	cfg.Actions = []string{chaos.ActionPodDelete}
	cfg.Observe = func(e chaos.Event) {
		if e.Phase == "inject" {
			mu.Lock()
			injects++
			mu.Unlock()
		}
		if e.Err != nil {
			logf("chaos %s %s on %s: %v", e.Phase, e.Action, e.Target, e.Err)
		}
	}
	runner, err := chaos.NewRunner(t.Seed.Fork(seedName), chaos.DefaultRegistry(), cfg, targets)
	if err != nil {
		return err
	}
	if err := runner.Start(); err != nil {
		return err
	}
	runWriters(t.Context, env, fleet.Tenants, d, payloadBytes)
	runner.Stop()

	mu.Lock()
	defer mu.Unlock()
	s.Set("injects", injects)
	if injects == 0 {
		return fmt.Errorf("the chaos loop injected no fault in %s, so this run proves nothing", d)
	}
	return nil
}

// verifyTenantData reads each tenant's table back over a FRESH connection:
// at least Written rows (an unacknowledged write may still have committed, so
// more is legal, fewer is loss), and every row v = id*7 with the right pad.
func verifyTenantData(t *core.T, s *core.StepHandle, env ampTenantEnv, tenants []tenant, payloadBytes int) error {
	var total int64
	for i := range tenants {
		ten := &tenants[i]
		conn, err := env.ConnectEndpoint(t.Context, ten.Endpoint)
		if err != nil {
			return fmt.Errorf("tenant %d: fresh connection for verification: %w", i, err)
		}
		rows, err := conn.QueryInt(t.Context, fmt.Sprintf("SELECT count(*) FROM %s", ten.Table))
		if err != nil {
			conn.Close()
			return fmt.Errorf("tenant %d: count %s: %w", i, ten.Table, err)
		}
		bad, err := conn.QueryInt(t.Context, fmt.Sprintf(
			"SELECT count(*) FROM %s WHERE v <> id * 7 OR length(pad) <> %d", ten.Table, payloadBytes))
		conn.Close()
		if err != nil {
			return fmt.Errorf("tenant %d: checksum scan of %s: %w", i, ten.Table, err)
		}
		if rows < ten.Written {
			return fmt.Errorf("tenant %d lost data: %d acknowledged writes, %d rows stored", i, ten.Written, rows)
		}
		if bad != 0 {
			return fmt.Errorf("tenant %d has %d corrupted row(s): v <> id*7 or wrong pad length", i, bad)
		}
		total += rows
	}
	s.Set("rows_verified", total)
	return nil
}
