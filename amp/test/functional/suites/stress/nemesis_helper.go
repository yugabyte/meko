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

// Shared machinery of the nemesis tests: aim a chaos loop at a set of pods
// while every tenant writes, and afterwards hold storage to exactly what the
// writers were acknowledged for.

// backingKubeEnv is the capability a backing-directed nemesis needs: a kube
// handle scoped to wherever the backing YugabyteDB's pods actually run.
type backingKubeEnv interface {
	BackingKube() *k8s.Kubectl
}

// backingPodSelectors matches the backing's serving pods across every backing
// this framework deploys: the ybk8s chart labels its tservers app=yb-tserver,
// a YBA-provisioned universe labels them app.kubernetes.io/name=yb-tserver,
// and the built-in single-pod yugabyted is app=yugabyted. Masters are left
// alone on purpose — this tier aims at the WAL/data path the endpoints write
// through, and a master quorum loss is its own future test.
var backingPodSelectors = []string{
	"app=yb-tserver",
	"app.kubernetes.io/name=yb-tserver",
	"app=yugabyted",
}

// backingPods lists the backing's serving pods, trying each known selector in
// the backing's own namespace. The first selector that matches wins: one
// backing never carries two of these shapes at once.
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

// chaosOnPods runs the fault loop against pods while every tenant writes for
// d, and fails when no fault was actually injected — a chaos step that never
// fired proves nothing and must say so rather than pass. pod_delete only: it
// is the strongest recoverable fault (the pod's owner must fully replace it),
// and the only one meaningful against every target this suite aims at.
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

// verifyTenantData reads every tenant's table back over a FRESH connection and
// holds storage to what the writer was acknowledged for: at least Written
// distinct rows (an error the client counted as unacknowledged may still have
// committed, so more is legal — fewer is loss), every row's v equal to id*7
// and its pad exactly payloadBytes long (v is a pure function of id precisely
// so corruption is checkable without knowing which ids landed).
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
