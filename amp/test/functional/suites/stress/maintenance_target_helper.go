// The backing stress/backing_maintenance puts through maintenance, driven via YBA's own upgrade API (the way an operator does it).
package stress

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/yba"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybadmin"
	"github.com/yugabyte/amp/test/functional/framework/env/universe"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

type maintenanceTarget interface {
	Describe() string
	RollingRestart(ctx context.Context) error
	SetTServerGflag(ctx context.Context, key, value string) error
	UpgradeSoftware(ctx context.Context, version string) error
	Health(ctx context.Context) error
	// Version is read back after an upgrade rather than assumed.
	Version(ctx context.Context) (string, error)
	UpgradeTarget(ctx context.Context) (string, error)
}

// dataMover is optional: draining needs somewhere in-cluster to run yb-admin, which a VM universe lacks.
type dataMover interface {
	SupportsDataMove() bool
	NewAdmin(ctx context.Context, logf func(string, ...any)) (*ybadmin.Client, error)
	DataMove(ctx context.Context, admin *ybadmin.Client, rf int, pick func(n int) int, moveTimeout time.Duration, logf func(string, ...any)) error
}

type ybaTarget struct {
	client     universe.YBAMaintainer
	universeID string
	// restartSleep is YBA's pause after each node comes back before the next.
	restartSleep time.Duration
	// arch selects the release artifact to import; k8s means a helm chart, not an el8 tarball.
	arch string
	k8s  bool
	// kube lets the drain run yb-admin in a tserver pod; nil for a VM universe.
	kube *k8s.Kubectl
	logf func(string, ...any)
}

func (t *ybaTarget) Describe() string { return "YBA universe " + t.universeID }

func (t *ybaTarget) RollingRestart(ctx context.Context) error {
	return t.client.RestartUniverseAndWait(ctx, t.universeID, yba.UpgradeRolling, t.restartSleep)
}

// Rolling deliberately: Non-Restart would skip the restart under test.
func (t *ybaTarget) SetTServerGflag(ctx context.Context, key, value string) error {
	return t.client.UpgradeGflagsAndWait(ctx, t.universeID, nil,
		map[string]string{key: value}, yba.UpgradeRolling, t.restartSleep)
}

// A YBA whose catalog lacks the release fails the upgrade with a less specific error; normal on a freshly-deployed YBA.
func (t *ybaTarget) UpgradeSoftware(ctx context.Context, version string) error {
	if err := t.client.EnsureReleaseAvailable(ctx, version, t.arch, t.k8s); err != nil {
		return err
	}
	return t.client.UpgradeSoftwareAndWait(ctx, t.universeID, version, yba.UpgradeRolling, t.restartSleep)
}

// YBA will not roll a universe past its own version, which is the ceiling.
func (t *ybaTarget) UpgradeTarget(ctx context.Context) (string, error) {
	return t.client.AppVersion(ctx)
}

func (t *ybaTarget) Health(ctx context.Context) error {
	u, err := t.client.GetUniverse(ctx, t.universeID)
	if err != nil {
		return fmt.Errorf("read universe %s: %w", t.universeID, err)
	}
	if !u.IsReady() {
		return fmt.Errorf("universe %s is not ready after the operation (update in progress or last update failed)", t.universeID)
	}
	return nil
}

func (t *ybaTarget) Version(ctx context.Context) (string, error) {
	u, err := t.client.GetUniverse(ctx, t.universeID)
	if err != nil {
		return "", err
	}
	return u.Version(), nil
}

// runDataMove steps the master leader down, blacklists one tserver to drain its tablets, waits, then un-blacklists so the balancer refills it. Runs under live workload.
func runDataMove(ctx context.Context, admin *ybadmin.Client, rf int, pick func(n int) int, moveTimeout time.Duration, logf func(string, ...any)) error {
	if err := admin.StepDownMasterLeader(ctx); err != nil {
		return fmt.Errorf("step the master leader down: %w", err)
	}
	servers, err := admin.AliveTabletServers(ctx)
	if err != nil {
		return fmt.Errorf("list tservers to drain: %w", err)
	}
	if len(servers) <= rf {
		return fmt.Errorf("only %d live tservers at RF=%d: a drained node's replicas would have nowhere to go", len(servers), rf)
	}
	victim := servers[pick(len(servers))].HostPort
	logf("data move: draining tserver %s", victim)

	if err := admin.Blacklist(ctx, ybadmin.BlacklistFull, "ADD", victim); err != nil {
		return fmt.Errorf("blacklist %s: %w", victim, err)
	}
	// Registered before the wait: a timed-out drain must not strand the node.
	restored := false
	defer func() {
		if restored {
			return
		}
		rctx, cancel := context.WithTimeout(context.Background(), moveTimeout)
		defer cancel()
		if e := admin.Blacklist(rctx, ybadmin.BlacklistFull, "REMOVE", victim); e != nil {
			logf("data move: restoring %s to the cluster (best-effort): %v", victim, e)
		}
	}()

	if _, err := admin.WaitLoadMove(ctx, admin.LoadMoveCompletion, moveTimeout, ybadmin.DefaultPoll); err != nil {
		return fmt.Errorf("drain %s: %w", victim, err)
	}
	if err := admin.Blacklist(ctx, ybadmin.BlacklistFull, "REMOVE", victim); err != nil {
		return fmt.Errorf("un-blacklist %s: %w", victim, err)
	}
	restored = true
	if err := admin.WaitLoadBalancerIdle(ctx, moveTimeout, ybadmin.DefaultPoll); err != nil {
		return fmt.Errorf("the cluster never rebalanced after restoring %s: %w", victim, err)
	}
	return nil
}

func firstTserverPod(ctx context.Context, kube *k8s.Kubectl) (string, error) {
	var errs []string
	for _, sel := range tserverSelectors {
		out, err := kube.GetSelector(ctx, "pod", sel, "{.items[0].metadata.name}")
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", sel, err))
			continue
		}
		if pod := strings.TrimSpace(out); pod != "" {
			return pod, nil
		}
		errs = append(errs, sel+": no pods")
	}
	return "", fmt.Errorf("no tserver pod in namespace %s (tried %s)", kube.Namespace(), strings.Join(errs, "; "))
}

func (t *ybaTarget) DataMove(ctx context.Context, admin *ybadmin.Client, rf int, pick func(n int) int, moveTimeout time.Duration, logf func(string, ...any)) error {
	if t.kube == nil {
		return fmt.Errorf("data_move needs to run yb-admin inside the cluster, and %s is not a kubernetes universe", t.Describe())
	}
	return runDataMove(ctx, admin, rf, pick, moveTimeout, logf)
}

func (t *ybaTarget) NewAdmin(ctx context.Context, logf func(string, ...any)) (*ybadmin.Client, error) {
	if t.kube == nil {
		return nil, fmt.Errorf("%s has nowhere in-cluster to run yb-admin", t.Describe())
	}
	masters, err := masterAddrsFromTServerConf(ctx, t.kube)
	if err != nil {
		return nil, err
	}
	pod, err := firstTserverPod(ctx, t.kube)
	if err != nil {
		return nil, err
	}
	return ybadmin.New(t.kube.ForPod(pod, tserverContainer), masters, ybadmin.WithLogf(logf)), nil
}

func (t *ybaTarget) SupportsDataMove() bool { return t.kube != nil }

// Reads --tserver_master_addrs from the running tserver's own conf, so it holds regardless of chart naming style or YBA's object prefix.
func masterAddrsFromTServerConf(ctx context.Context, kube *k8s.Kubectl) (string, error) {
	pod, err := firstTserverPod(ctx, kube)
	if err != nil {
		return "", err
	}
	res, err := kube.ForPod(pod, tserverContainer).Run(ctx, "cat "+tserverConfPath)
	if err != nil {
		return "", fmt.Errorf("read %s on %s: %w", tserverConfPath, pod, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("read %s on %s exited %d: %s", tserverConfPath, pod, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "--tserver_master_addrs="); ok && v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("no --tserver_master_addrs in %s on %s", tserverConfPath, pod)
}

// Both chart naming styles; hardcoding either finds nothing on the other's cluster.
var tserverSelectors = []string{"app=yb-tserver", "app.kubernetes.io/name=yb-tserver"}
