package stress

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ybadmin"
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

	gate, err := awaitSteadyState(t, s, env, fleet.Tenants, nil, payloadBytes, 0)
	if err != nil {
		return err
	}

	targets := make([]*chaos.Target, 0, len(pods))
	for _, pod := range pods {
		targets = append(targets, chaos.NewPodTarget(pod, kube, pod, ""))
	}

	var mu sync.Mutex
	injects := 0
	var recoverErrs []string
	cfg := chaos.FromConfig(t.Options)
	cfg.Actions = []string{chaos.ActionPodDelete}
	cfg.Observe = func(e chaos.Event) {
		mu.Lock()
		if e.Phase == "inject" {
			injects++
		}
		// A recover failure was a log line only; the step still reported success.
		if e.Phase == "recover" && e.Err != nil {
			recoverErrs = append(recoverErrs, fmt.Sprintf("%s on %s: %v", e.Action, e.Target, e.Err))
		}
		mu.Unlock()
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
	s.Set("injects", injects)
	if len(recoverErrs) > 0 {
		s.Set("recover_errors", strings.Join(recoverErrs, "; "))
	}
	fired := injects
	mu.Unlock()

	if fired == 0 {
		return fmt.Errorf("the chaos loop injected no fault in %s, so this run proves nothing", d)
	}
	return gate.attestNonTarget(t, s, pods...)
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

// Chaos only stops INJECTING; Ready must HOLD, as a pod in its grace period reports it.
func awaitBackingReady(t *core.T, kube *k8s.Kubectl, settle time.Duration) (time.Duration, error) {
	start := time.Now()
	deadline := start.Add(settle)
	stable := 0
	for {
		why, err := backingNotReady(t, kube)
		if err != nil {
			why = err.Error()
		}
		if err == nil && why == "" {
			stable++
			if stable >= backingReadySettled {
				return time.Since(start), nil
			}
		} else {
			stable = 0
		}
		if t.Context.Err() != nil {
			return time.Since(start), t.Context.Err()
		}
		if time.Now().After(deadline) {
			if why == "" {
				why = fmt.Sprintf("its pods keep dropping out of Ready before holding it for %d polls", backingReadySettled)
			}
			return time.Since(start), fmt.Errorf(
				"the backing is still not serving %s after the chaos stopped: %s", settle, why)
		}
		time.Sleep(backingReadyPollInterval)
	}
}

const (
	backingReadyPollInterval = 5 * time.Second
	backingReadySettled      = 3
)

// errNoTabletView: yb-admin needs a tserver-shaped pod, which the built-in
// yugabyted is not.
var errNoTabletView = errors.New("no tserver pod to run yb-admin in")

// Pods Ready is not the store serving: a restarted universe can leave tablets
// with no leader (meko#1366), and the first write to one waits out YB's deadline.
//
// The admin client is rebuilt per poll: every serving pod was just deleted, so
// the one this resolves may be replaced mid-wait.
func awaitBackingTabletsLed(t *core.T, kube *k8s.Kubectl, settle time.Duration, logf func(string, ...any)) (time.Duration, error) {
	start := time.Now()
	deadline := start.Add(settle)
	var last []string
	var raw string
	var err error
	for {
		var admin *ybadmin.Client
		admin, err = backingAdmin(t.Context, kube, logf)
		if errors.Is(err, errNoTabletView) {
			return time.Since(start), err
		}
		if err == nil {
			last, raw, err = leaderlessTablets(t.Context, admin, ampStorageDB)
			if err == nil && len(last) == 0 {
				return time.Since(start), nil
			}
		}
		if t.Context.Err() != nil {
			return time.Since(start), t.Context.Err()
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(backingReadyPollInterval)
	}
	if raw != "" {
		t.AttachBytes("backing-leaderless-tablets.txt", "log", []byte(raw))
	}
	if err != nil {
		return time.Since(start), fmt.Errorf("list %s tablets on the backing: %w", ampStorageDB, err)
	}
	return time.Since(start), fmt.Errorf("%d %s tablet(s) still have no leader %s after the backing's pods were Ready, so a write to any of them hangs until YB's client deadline (meko#1366): %s",
		len(last), ampStorageDB, settle, strings.Join(last, ", "))
}

func backingAdmin(ctx context.Context, kube *k8s.Kubectl, logf func(string, ...any)) (*ybadmin.Client, error) {
	pod, err := firstTserverPod(ctx, kube)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoTabletView, err)
	}
	masters, err := masterAddrsFromTServerConf(ctx, kube)
	if err != nil {
		return nil, err
	}
	return ybadmin.New(kube.ForPod(pod, tserverContainer), masters, ybadmin.WithLogf(logf)), nil
}

// Substring, as ybadmin documents: the printed keyspace varies by version, and
// matching none of them is a parse failure to report, not a led universe.
func leaderlessTablets(ctx context.Context, admin *ybadmin.Client, database string) ([]string, string, error) {
	tables, rawTables, err := admin.ListTables(ctx)
	if err != nil {
		return nil, rawTables, err
	}
	var out []string
	var raw strings.Builder
	raw.WriteString(rawTables)
	matched := 0
	for _, tbl := range tables {
		if !strings.Contains(tbl.Keyspace, database) {
			continue
		}
		matched++
		tablets, err := admin.ListTablets(ctx, tbl.ID, 0)
		if err != nil {
			return nil, raw.String(), fmt.Errorf("%s.%s: %w", tbl.Keyspace, tbl.Name, err)
		}
		for _, tab := range tablets {
			if tab.Leader == "" {
				out = append(out, tbl.Keyspace+"."+tbl.Name+"/"+tab.ID)
				fmt.Fprintf(&raw, "\n%s.%s tablet %s: no leader", tbl.Keyspace, tbl.Name, tab.ID)
			}
		}
	}
	if matched == 0 {
		return nil, raw.String(), fmt.Errorf("no %s table among the %d yb-admin lists: the listing could not be read, so nothing was checked for a leader", database, len(tables))
	}
	sort.Strings(out)
	return out, raw.String(), nil
}

func backingNotReady(t *core.T, kube *k8s.Kubectl) (string, error) {
	pods, err := backingPods(t, kube)
	if err != nil {
		return "", err
	}
	if len(pods) == 0 {
		return fmt.Sprintf("no backing pod exists in namespace %q yet", kube.Namespace()), nil
	}
	states, err := kube.PodStates(t.Context)
	if err != nil {
		return "", err
	}
	byName := make(map[string]k8s.PodState, len(states))
	for _, st := range states {
		byName[st.Name] = st
	}
	var bad []string
	for _, pod := range pods {
		st, ok := byName[pod]
		if !ok {
			bad = append(bad, pod+" (absent)")
			continue
		}
		if st.Phase != "Running" || !st.Ready {
			bad = append(bad, fmt.Sprintf("%s (%s, ready=%v)", pod, st.Phase, st.Ready))
		}
	}
	return strings.Join(bad, ", "), nil
}
