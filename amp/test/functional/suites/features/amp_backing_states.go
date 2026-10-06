// Every lane AMP uses to reach YugabyteDB, probed once before and once after a
// single YBA operation on the universe under it. Each state gets its own
// universe (DedicatedStack), so the operation reaches no other test. The YB
// states stress already runs daily (pod delete, partition, gflag change,
// stepdowns, tablet split, tserver add/drain, disk full) are not repeated here.
package features

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/yba"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybadmin"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/universe"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	backingStateProbeRows = 1000
	// An earlier build of the portal's default release. The previous train
	// cannot carry AMP: the thin-client RPC service it needs first shipped in
	// 2026.1.2.
	DefaultBackingStatesUpgradeFrom = "2026.1.2.0-b120"
	DefaultBackingStatesRestartWait = 60 * time.Second
	DefaultBackingStatesMoveTimeout = 30 * time.Minute
	backingStatesTServerContainer   = "yb-tserver"
	backingStatesTServerConf        = "/tmp/yugabyte/tserver/conf/server.conf"
)

var backingStatesTServerSelectors = []string{"app=yb-tserver", "app.kubernetes.io/name=yb-tserver"}

type backingState struct {
	name        string
	description string
	backing     func(c *core.Config) map[string]string
	run         func(s *backingStatesRun, ctx context.Context) error
}

var backingStates = []backingState{
	{
		name:        "software_upgrade",
		description: "a rolling YugabyteDB software upgrade with finalize, from an earlier build to the run's version, so the thin client meets a new tserver version and a mix of versions mid-roll",
		backing: func(c *core.Config) map[string]string {
			return map[string]string{"apps.yba.universe.yb_version": c.GetString("functional.backing_states.upgrade_from", DefaultBackingStatesUpgradeFrom)}
		},
		run: (*backingStatesRun).softwareUpgrade,
	},
	{
		name:        "data_move",
		description: "a data move that drains one tserver of every tablet and refills it, on a four-node RF3 universe, so both lanes lose and regain the replicas they were talking to",
		backing: func(*core.Config) map[string]string {
			return map[string]string{"apps.yba.k8s.num_nodes": "int:4"}
		},
		run: (*backingStatesRun).dataMove,
	},
	{
		name:        "rolling_restart",
		description: "a YBA rolling restart of every master and tserver, which breaks every thin RPC and YSQL connection AMP holds once",
		run:         (*backingStatesRun).rollingRestart,
	},
}

func init() {
	for _, st := range backingStates {
		st := st
		core.Register(core.Test{
			Name:     "features/amp_backing_states_" + st.name,
			Suite:    "features",
			Category: core.Functional,
			Env: core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack, FaultsSharedInfra: true,
				BackingConfig: st.backing},
			Tags:       []string{"regression"},
			OwningTeam: core.TeamCompute,
			Description: "Probe every AMP lane to YugabyteDB before and after " + st.description + ". " +
				"The probe commits rows (safekeeper to YB), cold-reads them after a compute restart (pageserver to YB), forks at the latest LSN and reads the fork through a new endpoint (controller YSQL, supervisor lease and registry, pageserver lineage reads), and drops the fork (controller jobs).",
			Params:  []string{"functional", "functional.backing_states", "env.ampstack", "apps.yba"},
			Timeout: 3 * time.Hour,
			Run: func(t *core.T) error {
				return runAmpBackingState(t, st)
			},
		})
	}
}

type ybaUniverseEnv interface {
	YBAUniverse() (universe.YBAMaintainer, string)
}

type kubeUniverseEnv interface {
	KubeUniverse() (kubeContext, kubeconfig, namespace string)
}

type backingStatesRun struct {
	a          *ampFeatureRun
	client     universe.YBAMaintainer
	universeID string
	kube       *k8s.Kubectl
	ep         controller.Endpoint
	round      int
}

func runAmpBackingState(t *core.T, st backingState) error {
	benv, ok := t.Env.(ybaUniverseEnv)
	if !ok {
		t.Skip("this env exposes no YBA universe behind amp, so there is no YBA operation to run")
		return nil
	}
	client, universeID := benv.YBAUniverse()
	if client == nil {
		t.Skip("amp's backing here is not a YBA universe")
		return nil
	}
	s := &backingStatesRun{client: client, universeID: universeID}
	if kenv, ok := t.Env.(kubeUniverseEnv); ok {
		kctx, kubeconfig, ns := kenv.KubeUniverse()
		if ns != "" {
			s.kube = k8s.New(k8s.Config{Context: kctx, Namespace: ns}).WithRunner(k8s.KubeconfigRunner(kubeconfig))
		}
	}
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		s.a = a
		return []phase{
			{"probe_before", s.probe},
			{st.name, func(ctx context.Context) error {
				if err := st.run(s, ctx); err != nil {
					return err
				}
				u, err := s.client.GetUniverse(ctx, s.universeID)
				if err != nil {
					return fmt.Errorf("read universe %s after %s: %w", s.universeID, st.name, err)
				}
				if !u.IsReady() {
					return core.AsEnvFailure(fmt.Errorf("universe %s is not ready after %s", s.universeID, st.name))
				}
				return nil
			}},
			{"probe_after", s.probe},
		}
	})
}

// probe drives each lane once and names the lane that broke.
func (s *backingStatesRun) probe(ctx context.Context) error {
	s.round++
	cl, err := s.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	proj := s.a.proj
	if s.ep.ID == "" {
		if s.ep, err = createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID); err != nil {
			return err
		}
	}
	table := fmt.Sprintf("amp_backing_probe_%d", s.round)
	c, err := s.a.f.endpointSQL(ctx, s.ep)
	if err != nil {
		return fmt.Errorf("round %d: connect the writer: %w", s.round, err)
	}
	for _, stmt := range []string{
		"CREATE TABLE " + table + " (id int PRIMARY KEY, v text NOT NULL)",
		fmt.Sprintf("INSERT INTO %s SELECT g, md5(g::text) FROM generate_series(1, %d) g", table, backingStateProbeRows),
	} {
		if _, err := c.Exec(ctx, stmt); err != nil {
			c.Close()
			return fmt.Errorf("round %d, safekeeper to YB: commit %d rows on the endpoint that was running through the operation: %w", s.round, backingStateProbeRows, err)
		}
	}
	sum := "SELECT md5(string_agg(id || v, ',' ORDER BY id)) FROM " + table
	want, err := c.QueryString(ctx, sum)
	c.Close()
	if err != nil {
		return err
	}

	cold, ok, err := s.a.restartEndpointCompute(ctx, s.ep)
	if err != nil {
		return fmt.Errorf("round %d, pageserver to YB: restart the compute for a cold read: %w", s.round, err)
	}
	if ok {
		got, err := cold.QueryString(ctx, sum)
		cold.Close()
		if err != nil {
			return fmt.Errorf("round %d, pageserver to YB: cold read after a compute restart: %w", s.round, err)
		}
		if got != want {
			return fmt.Errorf("round %d, pageserver to YB: %s reads back a different checksum cold than it committed", s.round, table)
		}
	}

	w, err := s.a.f.endpointSQL(ctx, s.ep)
	if err != nil {
		return fmt.Errorf("round %d: reconnect the writer: %w", s.round, err)
	}
	lsn, err := currentLSN(ctx, w)
	w.Close()
	if err != nil {
		return err
	}
	name := fmt.Sprintf("probe-%d-%s", s.round, s.a.f.suffix())
	fork, err := forkChildAt(ctx, cl, proj, proj.branchID, lsn, name, s.a.f.endpointSQL)
	if err != nil {
		return fmt.Errorf("round %d, controller YSQL and supervisor lease: fork at %s and start an endpoint on it: %w", s.round, lsn, err)
	}
	got, err := fork.QueryString(ctx, sum)
	fork.Close()
	if err != nil {
		return fmt.Errorf("round %d, pageserver lineage reads: read %s on the fork: %w", s.round, table, err)
	}
	if got != want {
		return fmt.Errorf("round %d, pageserver lineage reads: the fork at %s reads %s with a different checksum than its parent committed", s.round, lsn, table)
	}

	b, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, name)
	if err != nil {
		return err
	}
	eps, err := cl.ListAllEndpoints(ctx, proj.poolID, proj.projID, b.BranchID)
	if err != nil {
		return fmt.Errorf("round %d, controller to YB: list the fork's endpoints: %w", s.round, err)
	}
	for _, e := range eps {
		if _, err := cl.DeleteEndpointAndWait(ctx, proj.poolID, proj.projID, b.BranchID, e.ID); err != nil {
			return fmt.Errorf("round %d: delete the fork's endpoint: %w", s.round, err)
		}
	}
	if _, err := cl.DeleteBranchAndWait(ctx, proj.poolID, proj.projID, b.BranchID); err != nil {
		return fmt.Errorf("round %d, controller jobs to YB: drop the fork: %w", s.round, err)
	}
	return nil
}

func (s *backingStatesRun) restartWait() time.Duration {
	return s.a.f.t.Options.GetDuration("functional.backing_states.restart_wait", DefaultBackingStatesRestartWait)
}

func (s *backingStatesRun) rollingRestart(ctx context.Context) error {
	if err := s.client.RestartUniverseAndWait(ctx, s.universeID, yba.UpgradeRolling, s.restartWait()); err != nil {
		return fmt.Errorf("rolling restart of universe %s: %w", s.universeID, err)
	}
	return nil
}

func (s *backingStatesRun) softwareUpgrade(ctx context.Context) error {
	u, err := s.client.GetUniverse(ctx, s.universeID)
	if err != nil {
		return err
	}
	from := u.Version()
	to := s.a.f.t.Options.GetString("versions.yb", "")
	if to == "" {
		if to, err = s.client.AppVersion(ctx); err != nil {
			return err
		}
	}
	cmp, err := core.Compare(from, to)
	if err != nil {
		return fmt.Errorf("compare the universe's version %q with the target %q: %w", from, to, err)
	}
	if cmp >= 0 {
		return core.AsEnvFailure(fmt.Errorf("the universe was created on %s, not behind the upgrade target %s: set functional.backing_states.upgrade_from to an earlier build that still carries the thin-client RPC (2026.1.2 or later)", from, to))
	}
	s.a.f.logf("software upgrade %s -> %s", from, to)
	if err := s.client.EnsureReleaseAvailable(ctx, to, "x86_64", s.kube != nil); err != nil {
		return err
	}
	if err := s.client.UpgradeSoftwareAndWait(ctx, s.universeID, to, yba.UpgradeRolling, s.restartWait()); err != nil {
		return fmt.Errorf("upgrade universe %s from %s to %s: %w", s.universeID, from, to, err)
	}
	if u, err = s.client.GetUniverse(ctx, s.universeID); err != nil {
		return err
	}
	if got := u.Version(); !strings.Contains(got, to) {
		return fmt.Errorf("the universe reports version %q after the upgrade to %s", got, to)
	}
	return nil
}

func (s *backingStatesRun) dataMove(ctx context.Context) error {
	if s.kube == nil {
		return core.AsEnvFailure(fmt.Errorf("the universe is not in this kube cluster, so there is nowhere to run yb-admin for the data move"))
	}
	pod, err := s.tserverPod(ctx)
	if err != nil {
		return err
	}
	res, err := s.kube.ForPod(pod, backingStatesTServerContainer).Run(ctx, "cat "+backingStatesTServerConf)
	if err != nil {
		return fmt.Errorf("read %s on %s: %w", backingStatesTServerConf, pod, err)
	}
	masters := ""
	for _, line := range strings.Split(res.Stdout, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "--tserver_master_addrs="); ok {
			masters = v
		}
	}
	if masters == "" {
		return fmt.Errorf("no --tserver_master_addrs in %s on %s", backingStatesTServerConf, pod)
	}
	admin := ybadmin.New(s.kube.ForPod(pod, backingStatesTServerContainer), masters, ybadmin.WithLogf(s.a.f.logf))
	servers, err := admin.AliveTabletServers(ctx)
	if err != nil {
		return fmt.Errorf("list tservers to drain: %w", err)
	}
	if len(servers) < 4 {
		return core.AsEnvFailure(fmt.Errorf("the universe has %d live tservers, and draining one at RF3 needs four", len(servers)))
	}
	victim := servers[int(s.a.f.t.Seed.Fork("data-move").Rand().Uint32()%uint32(len(servers)))].HostPort
	timeout := s.a.f.t.Options.GetDuration("functional.backing_states.move_timeout", DefaultBackingStatesMoveTimeout)
	s.a.f.logf("data move: draining tserver %s", victim)
	if err := admin.Blacklist(ctx, ybadmin.BlacklistFull, "ADD", victim); err != nil {
		return fmt.Errorf("blacklist %s: %w", victim, err)
	}
	restored := false
	defer func() {
		if !restored {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
			_ = admin.Blacklist(rctx, ybadmin.BlacklistFull, "REMOVE", victim)
			cancel()
		}
	}()
	if _, err := admin.WaitLoadMove(ctx, admin.LoadMoveCompletion, timeout, ybadmin.DefaultPoll); err != nil {
		return fmt.Errorf("drain %s: %w", victim, err)
	}
	if err := admin.Blacklist(ctx, ybadmin.BlacklistFull, "REMOVE", victim); err != nil {
		return fmt.Errorf("un-blacklist %s: %w", victim, err)
	}
	restored = true
	if err := admin.WaitLoadBalancerIdle(ctx, timeout, ybadmin.DefaultPoll); err != nil {
		return fmt.Errorf("the universe never rebalanced after %s was restored: %w", victim, err)
	}
	return nil
}

func (s *backingStatesRun) tserverPod(ctx context.Context) (string, error) {
	for _, sel := range backingStatesTServerSelectors {
		out, err := s.kube.GetSelector(ctx, "pod", sel, "{.items[0].metadata.name}")
		if err == nil && strings.TrimSpace(out) != "" {
			return strings.TrimSpace(out), nil
		}
	}
	return "", fmt.Errorf("no tserver pod in namespace %s", s.kube.Namespace())
}
