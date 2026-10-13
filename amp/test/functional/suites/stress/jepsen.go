// The yugabyte/jepsen consistency suite, driven against an amp endpoint while
// framework/chaos faults the stack.
//
// Every other test in this tier states its own invariant and checks it: an ack
// journal, a bank total, a checksum. This one brings an EXTERNAL checker — elle,
// and jepsen's bank/long-fork/single-key-acid models — which is worth having
// precisely because it was not written against amp and cannot be tuned to
// agree with it.
//
// What a green result here does and does not mean. amp allows a SINGLE WRITER
// per branch (stress/fence_stall is the test of that), so jepsen runs against
// one node, not a five-node cluster. The checkers still hold — they check the
// storage layer under the compute, and the recovery path across a compute
// restart, which is exactly what the fault loop exercises — but the multi-node
// linearizability story the stress framework's own jepsen suite tells does NOT
// transfer, and a reader must not take it from here.
//
// To load the backing, the suite runs one jepsen instance per tenant
// (stress.jepsen.tenants), all on the same workload at once and under one
// shared fault window, and checks each history on its own.
//
// jepsen's OWN nemesis is off (--nemesis none). Faults come from
// framework/chaos, so they are the actions amp understands, they are driven by
// this run's seed, and --seed replays them.
package stress

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/apps/jepsen"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/jepsen",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack, FaultsSharedInfra: true},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Run the yugabyte/jepsen consistency suite against several amp endpoints at once while chaos faults the endpoint computes and the backing YugabyteDB, and hold every workload's own checker — elle append, bank, long-fork, single-key-acid, counter, set — to a valid verdict. Faults start only once the workload's DDL has completed and the control plane holds no task in flight, so a fault never lands on a schema that is still being built.",
		Params:      []string{"stress.jepsen", "apps.jepsen", "chaos"},
		Timeout:     8 * time.Hour,
		Run:         runJepsen,
	})
}

func runJepsen(t *core.T) error {
	if !t.Options.GetBool("stress.jepsen.enabled", false) {
		t.Skip("stress.jepsen.enabled is off: the suite pulls a jepsen checkout at pod start and a full YSQL pass is hours long")
		return nil
	}
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, ok := t.Env.(ampKubeEnv)
	if !ok || kubeEnv.AmpKube() == nil {
		t.Skip("requires a kube context for the control-plane namespace: the runner pod and the faults have nowhere to go without it")
		return nil
	}

	params := jepsenParamsFromConfig(t.Options)
	cfg := jepsen.FromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)
	kube := kubeEnv.AmpKube()
	sink := attachSink{t: t, logf: logf}

	workloads, dropped := selectWorkloads(params.Workloads)
	if len(workloads) == 0 {
		t.Skip("stress.jepsen.workloads selected no runnable workload")
		return nil
	}

	// One tenant per jepsen instance: each needs its own database, and amp gives
	// one writer per branch. All of them run every workload at once, so the
	// backing carries every tenant's load while the faults land.
	if params.Tenants < 1 {
		params.Tenants = 1
	}
	fleet, err := newTenantFleet(t, env, logf)
	if err != nil {
		return err
	}
	defer fleet.Close()
	// Registered after fleet.Close, so it runs first, while the pods exist.
	defer attachTenantEndpointLogs(kube, fleet, sink, logf)

	rigs := make([]*jepsenRig, params.Tenants)
	defer func() {
		for _, r := range rigs {
			if r != nil {
				r.Close(sink)
			}
		}
	}()

	if err := t.Step("provision", func(s *core.StepHandle) error {
		// Standing up the tenants is setup, not the subject: no fault has been
		// injected yet, so nothing that goes wrong here says anything about how
		// amp behaves under one. It matters because this suite can run against a
		// SHARED control plane (env.stack=shared), where the previous run's
		// faults may still be draining out of the backing — a create that meets
		// that reads as a product failure otherwise, and the report lies.
		if err := fleet.Create(t, params.Tenants); err != nil {
			return core.AsEnvFailure(fmt.Errorf("stand up the %d tenant(s) jepsen runs against: %w", params.Tenants, err))
		}
		for i := range rigs {
			r, err := newJepsenRig(t, kube, cfg, i, logf)
			rigs[i] = r
			if err != nil {
				return err
			}
		}
		if err := eachRig(rigs, func(i int, r *jepsenRig) error {
			return r.PointAlias(t.Context, jepsenAliasName(t.ResourceSuffix(), i), fleet.Tenants[i].Endpoint)
		}); err != nil {
			return core.AsEnvFailure(err)
		}
		s.Set("tenants", params.Tenants)
		s.Set("workloads", len(workloads))
		if len(dropped) > 0 {
			s.Set("workloads_dropped", strings.Join(dropped, "; "))
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("prepare_runner", func(s *core.StepHandle) error {
		s.Set("repo", cfg.RepoURL+"@"+cfg.RepoRef)
		var setupErr, patchErr, dbErr error
		var mu sync.Mutex
		record := func(dst *error, err error) {
			mu.Lock()
			*dst = errors.Join(*dst, err)
			mu.Unlock()
		}
		_ = eachRig(rigs, func(i int, r *jepsenRig) error {
			setupApp := r.AppFor(workloads[0], cfg, "setup")
			if err := setupApp.Setup(t.Context, cfg.SetupSpec()); err != nil {
				record(&setupErr, fmt.Errorf("tenant %d: %w", i, err))
				return nil
			}
			// Patched before the first run, and NOT an env failure: an anchor
			// that no longer matches means the upstream branch moved under a
			// patch this run's config says it needs, so the run would not be
			// testing what it claims. That is a finding about this suite, not
			// about the cluster.
			applied, err := setupApp.ApplyPatches(t.Context, cfg.RepoDir, cfg.Patches)
			if err != nil {
				record(&patchErr, fmt.Errorf("tenant %d: %w", i, err))
				return nil
			}
			// Every rig applies the same patches to the same ref; one report is enough.
			if i == 0 {
				for _, p := range applied {
					if p.Skipped {
						s.Set("patch:"+p.File, "skipped — "+p.SkipReason)
						continue
					}
					s.Set("patch:"+p.File, fmt.Sprintf("%d replacement(s) — %s", p.Replacements, p.Reason))
				}
				s.Set("patches_applied", len(applied))
			}
			if err := jepsen.SetupDatabase(t.Context, fleet.Tenants[i].Conn, 2*time.Minute); err != nil {
				record(&dbErr, fmt.Errorf("tenant %d: create the jepsen role and database on the endpoint: %w", i, err))
			}
			return nil
		})
		if patchErr != nil {
			return patchErr
		}
		if err := errors.Join(setupErr, dbErr); err != nil {
			return core.AsEnvFailure(err)
		}
		return nil
	}); err != nil {
		return err
	}

	var invalid []string
	for _, w := range workloads {
		w := w
		for iter := 0; iter < cfg.Repeats; iter++ {
			iter := iter
			name := string(w)
			if cfg.Repeats > 1 {
				name = fmt.Sprintf("%s#%d", w, iter)
			}
			if err := t.Step(name, func(s *core.StepHandle) error {
				bad, err := runOneJepsenWorkload(t, s, rigs, env, fleet, cfg, params, w, iter, sink, logf)
				for _, b := range bad {
					invalid = append(invalid, name+"@"+b)
				}
				return err
			}); err != nil {
				return err
			}
		}
	}

	return t.Step("verdict", func(s *core.StepHandle) error {
		s.Set("histories_checked", len(workloads)*cfg.Repeats*params.Tenants)
		s.Set("histories_invalid", len(invalid))
		if len(invalid) > 0 {
			return fmt.Errorf("%d history(ies) are not clean, by jepsen's verdict or the lease probe: %s",
				len(invalid), strings.Join(invalid, ", "))
		}
		return nil
	})
}

// runOneJepsenWorkload runs one workload on every tenant at once, under one
// shared fault window. It returns the tenants whose history is not clean, each
// labelled with why ("tenant1 (jepsen invalid)", "tenant2 (lease probe)"), and
// the loop CONTINUES past them, so one violation does not hide the rest of the
// suite. An error is a harness or environment problem.
func runOneJepsenWorkload(t *core.T, s *core.StepHandle, rigs []*jepsenRig, env ampTenantEnv,
	fleet *tenantFleet, cfg jepsen.Config, params jepsenParams, w jepsen.Workload, iter int,
	sink attachSink, logf func(string, ...any)) ([]string, error) {

	sinks := make([]apps.ArtifactSink, len(rigs))
	for i := range rigs {
		sinks[i] = tenantSink(sink, i, iter, cfg.Repeats)
	}

	if err := eachRig(rigs, func(i int, _ *jepsenRig) error {
		return jepsen.ResetDatabase(t.Context, fleet.Tenants[i].Conn, 10*time.Minute)
	}); err != nil {
		return nil, core.AsEnvFailure(fmt.Errorf("reset the jepsen database before %s: %w", w, err))
	}

	jobs := make([]*jepsen.App, len(rigs))
	for i, r := range rigs {
		jobs[i] = r.AppFor(w, cfg, fmt.Sprintf("%s-%d-t%d", w.FileName(), iter, i))
	}
	concurrency, limit := cfg.Shape(w)
	s.Set("concurrency_per_tenant", concurrency)
	s.Set("time_limit", limit.String())

	// The store is collected before the process is killed, whatever happened —
	// losing the evidence for a verdict is not acceptable (apps.StopWithCollect).
	collected := make([]bool, len(jobs))
	defer func() {
		for i, app := range jobs {
			if !collected[i] {
				if err := app.ReportKill(context.Background(), sinks[i]); err != nil {
					logf("jepsen %s tenant %d: report-kill: %v", w, i, err)
				}
			}
			if err := app.CleanStore(context.Background()); err != nil {
				logf("jepsen %s tenant %d: clean store: %v", w, i, err)
			}
		}
	}()

	for _, app := range jobs {
		if err := app.Start(t.Context); err != nil {
			return nil, err
		}
	}

	if err := awaitDDLGate(t, s, jobs, fleet, env, params); err != nil {
		return nil, err
	}

	var lease *leaseRun
	if params.UseNemesis {
		window := params.FaultWindow
		if window <= 0 {
			window = limit - params.Settle
		}
		if window < 30*time.Second {
			window = 30 * time.Second
		}
		faultParams := params
		var leaseWG sync.WaitGroup
		if params.LeaseRace.Enabled {
			// The lease race owns the endpoints: endpoint chaos would delete or
			// freeze the very writers it is handing the branch between.
			faultParams.EndpointActions = nil
			leaseWG.Add(1)
			go func() {
				defer leaseWG.Done()
				lease = runLeaseRace(t, rigs, env, fleet, params.LeaseRace, window, sink, logf)
			}()
		}
		// Under the lease race with nothing else to fault, the race is the whole
		// nemesis and there is no chaos loop to run.
		var run faultRun
		var err error
		if !params.LeaseRace.Enabled || hasFaultTargets(faultParams, rigs[0].backingKube) {
			run, err = runJepsenFaults(t, rigs[0].kube, rigs[0].backingKube, fleet, faultParams, window, "jepsen/"+w.FileName(), logf)
		}
		leaseWG.Wait()
		if lease != nil {
			s.Set("lease_cycles", lease.Cycles)
			s.Set("lease_takeovers", lease.Takeovers)
			if len(lease.Refused) > 0 {
				s.Set("lease_refused", strings.Join(lease.Refused, "; "))
			}
			if len(lease.Errors) > 0 {
				s.Set("lease_errors", strings.Join(lease.Errors, "; "))
			}
			var probes []string
			for _, pr := range lease.Probes {
				probes = append(probes, pr.String())
			}
			s.Set("lease_probes", strings.Join(probes, "; "))
			if len(lease.Thaw) > 0 {
				s.Set("lease_thaw_errors", strings.Join(lease.Thaw, "; "))
			}
			if ferr := lease.Failure(window); ferr != nil {
				return nil, errors.Join(ferr, err)
			}
		}
		s.Set("fault_injects", run.Injects)
		s.Set("fault_targets", strings.Join(run.Targets, ","))
		if len(run.InjectErrors) > 0 {
			s.Set("fault_inject_errors", strings.Join(run.InjectErrors, "; "))
		}
		if len(run.Recovers) > 0 {
			s.Set("fault_recover_errors", strings.Join(run.Recovers, "; "))
		}
		if run.BackingSettled > 0 {
			s.Set("backing_settled_after", run.BackingSettled.Round(time.Second).String())
		}
		if err != nil {
			return nil, err
		}
	}

	verdicts := make([]jepsen.Verdict, len(jobs))
	if err := eachRig(rigs, func(i int, _ *jepsenRig) error {
		if err := jobs[i].WaitAnalysis(t.Context); err != nil {
			return err
		}
		v, err := jobs[i].Verdict(t.Context)
		verdicts[i] = v
		return err
	}); err != nil {
		return nil, err
	}

	// The probe names the pod a fencing failure happened on; it is a finding
	// for that tenant even where jepsen's own history happened not to catch it.
	var probeFindings map[int][]string
	if lease != nil {
		probeFindings = lease.Findings()
		var all []string
		for i := range jobs {
			all = append(all, probeFindings[i]...)
		}
		if len(all) > 0 {
			s.Set("lease_findings", strings.Join(all, "; "))
		}
	}

	var invalid []string
	for i, app := range jobs {
		verdict := verdicts[i]
		s.Set(fmt.Sprintf("verdict_tenant%d", i), string(verdict))
		valid := verdict == jepsen.VerdictValid
		probeBad := len(probeFindings[i]) > 0

		// An UNKNOWN verdict accuses the history of nothing, but some checker
		// did not finish, so the workload proves less than a pass would.
		// Reporting it as a pass is how a suite quietly stops testing anything;
		// it counts as a finding and keeps its full store as the evidence.
		if verdict == jepsen.VerdictUnknown {
			logf("jepsen %s tenant %d: analysis incomplete (%q) — the run proves less than a pass", w, i, jepsen.MarkerUnknown)
		}

		// A passing append run's store is hundreds of MiB of history nobody
		// reads; anything else IS the finding.
		if valid && !probeBad {
			_ = app.CollectMainLogOnly(t.Context, sinks[i])
		} else {
			_ = app.Collect(t.Context, sinks[i])
		}
		switch {
		case !valid:
			invalid = append(invalid, fmt.Sprintf("tenant%d (jepsen %s)", i, verdict))
		case probeBad:
			invalid = append(invalid, fmt.Sprintf("tenant%d (lease probe)", i))
		}
		_ = app.Stop(t.Context)
		collected[i] = true
	}
	return invalid, nil
}
