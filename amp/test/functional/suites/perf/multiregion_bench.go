// perf/multiregion_sysbench and perf/multiregion_pgbench are perf/sysbench and
// perf/pgbench on a multi-region backing: a YBA universe over several GCE
// regions with leaders preferred beside the control plane (apps/multiregion),
// one YBA for the test and a universe per arm. Same harness, same gates, their
// own portal history.
package perf

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/multiregion"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/sysbench"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybdeploy"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/reap"
	"github.com/yugabyte/amp/test/functional/framework/env/registry"
	"github.com/yugabyte/amp/test/functional/framework/env/universe"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

const (
	testNameMultiRegionSysbench = "perf/multiregion_sysbench"
	testNameMultiRegionPgbench  = "perf/multiregion_pgbench"
	// DefaultMultiRegionProvisions is one stack: every arm is three VMs in three
	// regions, so the concurrent-provision repeat of the co-located workloads
	// is opt-in here.
	DefaultMultiRegionProvisions = 1
	multiRegionYBATeardownBudget = 30 * time.Minute
)

func init() {
	core.Register(core.Test{
		Name:        testNameMultiRegionSysbench,
		Suite:       "perf",
		Category:    core.Perf,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe},
		Tags:        []string{"perf"},
		OwningTeam:  core.TeamCompute,
		Description: "Run sysbench through an amp compute endpoint backed by a YBA universe spread over several GCE regions (leaders preferred beside the control plane), and gate tps + latency against this topology's own history.",
		Params:      []string{"perf", "multiregion", "apps.yba.gcp", "env.ampstack"},
		Preflight:   multiregion.Preflight,
		Timeout:     4 * time.Hour,
		Run:         runMultiRegionSysbench,
	})
	core.Register(core.Test{
		Name:        testNameMultiRegionPgbench,
		Suite:       "perf",
		Category:    core.Perf,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe},
		Tags:        []string{"perf"},
		OwningTeam:  core.TeamCompute,
		Description: "Run YB's pgbench (ysql_bench) through an amp compute endpoint backed by a YBA universe spread over several GCE regions (leaders preferred beside the control plane), and gate tps + latency against this topology's own history.",
		Params:      []string{"perf", "multiregion", "apps.yba.gcp", "env.ampstack"},
		Preflight:   multiregion.Preflight,
		Timeout:     4 * time.Hour,
		Run:         runMultiRegionPgbench,
	})
}

func runMultiRegionSysbench(t *core.T) error {
	params := sysbenchParamsFromConfig(t.Options)
	return runMultiRegionBench(t, multiRegionWorkload{
		testName:     testNameMultiRegionSysbench,
		runStep:      "run_sysbench",
		podPrefix:    "ampqa-mr-sysbench",
		attachPrefix: "sysbench",
		noTPSMsg:     "sysbench produced no tps — parse of sysbench output failed (see attached sysbench-iter-*.out)",
		deliver: func(ctx context.Context, _ *core.T, _ perfConfig, podExec exec.RemoteExecutor) error {
			return sysbench.EnsureInstalled(ctx, podExec)
		},
		mkWorkload: func(conn pg.ConnInfo) Workload { return sysbenchWorkloadFor(params, conn) },
		specs:      sysbenchMetricSpecs,
		datapoints: sysbenchDatapoints,
		load:       func(bc BenchConfig, cfg perfConfig) LoadFields { return loadFieldsSysbench(bc, cfg, params) },
	})
}

func runMultiRegionPgbench(t *core.T) error {
	params := pgbenchParamsFromConfig(t.Options)
	return runMultiRegionBench(t, multiRegionWorkload{
		testName:     testNameMultiRegionPgbench,
		runStep:      "run_pgbench",
		podPrefix:    "ampqa-mr-pgbench",
		attachPrefix: "pgbench",
		noTPSMsg:     "pgbench produced no tps — parse of pgbench output failed (see attached pgbench-iter-*.out)",
		deliver:      deliverPgbenchArtifact,
		mkWorkload:   func(conn pg.ConnInfo) Workload { return pgbenchWorkloadFor(params, conn) },
		specs:        pgbenchMetricSpecs,
		datapoints:   pgbenchDatapoints,
		load:         func(bc BenchConfig, cfg perfConfig) LoadFields { return loadFieldsPgbench(bc, cfg, params) },
	})
}

type multiRegionWorkload struct {
	testName, runStep, podPrefix, attachPrefix, noTPSMsg string
	deliver                                              func(ctx context.Context, t *core.T, cfg perfConfig, podExec exec.RemoteExecutor) error
	mkWorkload                                           workloadFactory
	specs                                                func(BenchResult) []metricSpec
	datapoints                                           func(BenchResult) (gated, info []core.Datapoint)
	load                                                 func(bc BenchConfig, cfg perfConfig) LoadFields
}

// runMultiRegionBench is the shared body: gate, one YBA for the test, then the
// bench harness with a universe per arm in place of the co-located store.
func runMultiRegionBench(t *core.T, w multiRegionWorkload) error {
	cfg := perfFromConfig(t.Options)
	if reason, ok := perfGate(t.Options, cfg); !ok {
		t.Skip(reason)
		return nil
	}
	if reason, ok := gateNoDoubleProvision(t.Env); !ok {
		t.Skip(reason)
		return nil
	}
	shape := multiregion.FromConfig(t.Options)
	if err := shape.Validate(); err != nil {
		return err
	}
	cfg.RF = shape.ReplicationFactor
	cfg.Provisions = maxInt(1, t.Options.GetInt("multiregion.perf.provisions", DefaultMultiRegionProvisions))
	logf := t.Masker().Logf(log.Printf)

	// One derived config for the whole test: the backing is provisioned from it
	// AND the amp control plane is configured from it. Deriving only for the
	// provisioner leaves the controller on the shipped 5m readiness wait, which
	// is the failure the dedicated-stack path already had to fix — the startup
	// migrations are cross-region DDL here.
	opts := t.Options.Derive(shape.Overlay())
	prov, err := ybdeploy.NewProvisioner(opts, ybdeploy.ModeYBAProvisioned, logf)
	if err != nil {
		return core.EnvFail("multi-region backing: %w", err)
	}
	reg := openPerfReapRegistry(logf)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), multiRegionYBATeardownBudget)
		defer cancel()
		if err := prov.Close(ctx); err != nil {
			logf("perf multiregion: teardown YBA (best-effort): %v", err)
		}
	}()

	backing := func(ctx context.Context, r *benchProvision) error {
		store, td, plan, err := prov.Provision(ctx, "mr-"+r.sfx)
		if td != nil {
			r.td.push("teardown multi-region universe", td)
		}
		// Recorded before the error check and dropped only after a clean
		// teardown: this is the first perf test to create cloud VMs, so a run
		// killed mid-flight would otherwise leave three GCE nodes per arm with
		// nothing for `ampqa env ls` / `env reap` to act on.
		if id := recordPerfReapPlan(reg, plan, r.sfx, logf); id != "" {
			r.td.push("drop the crash-recovery reap record", func(context.Context) error {
				_, rerr := reg.Remove(id)
				return rerr
			})
		}
		if err != nil {
			return core.EnvFail("provision multi-region universe: %w", err)
		}
		if err := checkProvisionedShape(ctx, store, shape, r.logf); err != nil {
			return err
		}
		b := ybbacking.ExternalBacking(store)
		r.ampBacking = &b
		return nil
	}

	bc := benchConfigFromConfig(t.Options)
	load := w.load(bc, cfg)
	return runBenchProvisioned(t, benchWorkloadSpec{
		ampOptions:   opts,
		testName:     w.testName,
		runStep:      w.runStep,
		podPrefix:    w.podPrefix,
		attachPrefix: w.attachPrefix,
		noTPSMsg:     w.noTPSMsg,
		deliver:      w.deliver,
		backing:      backing,
		mkWorkload:   w.mkWorkload,
		specs:        w.specs,
		datapoints:   w.datapoints,
	}, cfg, bc, identityFromConfig(t.Options, "", frameworkCommit(), load))
}

// checkProvisionedShape refuses to benchmark a universe that is not the
// declared topology: a number measured on the wrong shape would enter this
// test's history as if it were comparable.
func checkProvisionedShape(ctx context.Context, store ybbacking.BackingStore, shape multiregion.Config, logf func(string, ...any)) error {
	env := ybdeploy.UniverseEnvOf(store)
	acc, ok := env.(interface {
		YBAUniverse() (universe.YBAMaintainer, string)
	})
	if !ok {
		return fmt.Errorf("the provisioned backing (%T) exposes no YBA universe", env)
	}
	yb, id := acc.YBAUniverse()
	if yb == nil {
		return fmt.Errorf("the provisioned backing is not a YBA universe")
	}
	u, err := yb.GetUniverse(ctx, id)
	if err != nil {
		return core.EnvFail("read universe %s: %w", id, err)
	}
	logf("perf multiregion: universe %s topology %s (leaders preferred in %v)", u.Name, u.DescribeTopology(), u.AffinitizedRegions())
	return shape.Check(u)
}

// openPerfReapRegistry opens the standing registry for the crash-recovery
// record. A resolution failure disables recording rather than failing the run.
func openPerfReapRegistry(logf func(string, ...any)) *registry.Registry {
	path, err := registry.DefaultPath()
	if err != nil {
		logf("perf multiregion: reap registry unavailable, crash-recovery record disabled: %v", err)
		return nil
	}
	return registry.New(path)
}

// recordPerfReapPlan stores one arm's reap plan and returns its entry id, or ""
// when there is nothing to record. Best-effort: a write failure is logged.
func recordPerfReapPlan(reg *registry.Registry, plan *reap.Plan, suffix string, logf func(string, ...any)) string {
	if reg == nil || plan == nil || plan.Empty() {
		return ""
	}
	planJSON, err := plan.JSON()
	if err != nil || planJSON == "" {
		return ""
	}
	entry, err := reg.Add(registry.Entry{
		Name:     "perf-multiregion-" + suffix,
		Shape:    "functional-run",
		Provider: "functional",
		Data:     map[string]string{"reap_plan": planJSON},
	})
	if err != nil {
		logf("perf multiregion: record crash-recovery reap plan (best-effort): %v", err)
		return ""
	}
	return entry.ID
}
