// The meko#777 / meko#941 WAL-loss class: only a safekeeper restart overlapping
// a failing backing runs the startup end-of-WAL scan against failing reads,
// where both bugs lived.
package stress

import (
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/wal_tail_nemesis",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack, FaultsSharedInfra: true},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Commit a fresh WAL tail, then SIGKILL the safekeeper repeatedly while a backing YugabyteDB pod is down, so every safekeeper startup runs its end-of-WAL scan against failing backing reads (the meko#777/meko#941 WAL-loss class); after recovery every acknowledged commit must verify exactly and writes must flow again.",
		Params:      []string{"stress.wal_tail_nemesis", "stress.consistency"},
		Timeout:     90 * time.Minute,
		Run:         runWalTailNemesis,
	})
}

type walTailParams struct {
	Tenants        int
	Rounds         int
	BurstDuration  time.Duration
	KillsPerRound  int
	KillGap        time.Duration
	SettleTimeout  time.Duration
	PayloadBytes   int
	ResumeDuration time.Duration
}

func walTailParamsFromConfig(c *core.Config) walTailParams {
	return walTailParams{
		Tenants:        c.GetInt("stress.wal_tail_nemesis.tenants", 2),
		Rounds:         c.GetInt("stress.wal_tail_nemesis.rounds", 2),
		BurstDuration:  c.GetDuration("stress.wal_tail_nemesis.burst_duration", 20*time.Second),
		KillsPerRound:  c.GetInt("stress.wal_tail_nemesis.kills_per_round", 3),
		KillGap:        c.GetDuration("stress.wal_tail_nemesis.kill_gap", 10*time.Second),
		SettleTimeout:  c.GetDuration("stress.wal_tail_nemesis.settle_timeout", 10*time.Minute),
		PayloadBytes:   c.GetInt("stress.wal_tail_nemesis.payload_bytes", 256),
		ResumeDuration: c.GetDuration("stress.wal_tail_nemesis.resume_duration", 30*time.Second),
	}
}

func runWalTailNemesis(t *core.T) error {
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	ampEnv, ok := t.Env.(ampKubeEnv)
	if !ok || ampEnv.AmpKube() == nil {
		t.Skip("requires a kube handle for the control-plane namespace: safekeeper pids are only reachable by pod exec")
		return nil
	}
	backingEnv, ok := t.Env.(backingKubeEnv)
	if !ok || backingEnv.BackingKube() == nil {
		t.Skip("requires a kube handle for the backing's namespace: this fault pairs a safekeeper restart with a degraded backing")
		return nil
	}
	params := walTailParamsFromConfig(t.Options)
	if params.Rounds < 1 || params.KillsPerRound < 1 {
		return fmt.Errorf("stress.wal_tail_nemesis.rounds and kills_per_round must be at least 1, got %d and %d: no fault would be injected", params.Rounds, params.KillsPerRound)
	}
	logf := t.Masker().Logf(log.Printf)
	kube := ampEnv.AmpKube()

	fleet, err := newTenantFleet(t, env, logf)
	if err != nil {
		return err
	}
	defer fleet.Close()

	var journals []*AckJournal
	if err := t.Step("create_tenants", func(s *core.StepHandle) error {
		if err := fleet.Create(t, params.Tenants); err != nil {
			return err
		}
		journals = make([]*AckJournal, len(fleet.Tenants))
		for i := range journals {
			journals[i] = NewAckJournal()
		}
		s.Set("tenants", len(fleet.Tenants))
		return nil
	}); err != nil {
		return err
	}

	rnd := t.Seed.Fork("wal-tail").Rand()
	for r := 1; r <= params.Rounds; r++ {
		// The bug erases WAL the pageserver has not consumed yet.
		if err := t.Step(fmt.Sprintf("fresh_wal_tail_%d", r), func(s *core.StepHandle) error {
			before := totalAcked(journals)
			runJournaledWriters(t.Context, env, fleet.Tenants, journals, t.Seed.Fork(fmt.Sprintf("burst-%d", r)),
				params.BurstDuration, commitDurabilityParams{PayloadBytes: params.PayloadBytes}, nil)
			delta := totalAcked(journals) - before
			s.Set("acked", delta)
			if delta == 0 {
				why := walTailWriteFailure(t, kube, s, fmt.Sprintf("fresh_wal_tail_%d", r), fleet.Tenants, journals, logf)
				if r == 1 {
					return fmt.Errorf("no tenant was acknowledged for a single write in %s, so there is nothing at the WAL tail for the fault to threaten: %s",
						params.BurstDuration, why)
				}
				// Not a precondition from round 2 on: the round before it recovered and read the acked set back.
				return fmt.Errorf("no tenant was acknowledged for a single write in %s after round %d recovered and every endpoint answered SQL: the endpoints serve reads but no COMMIT completes (cf. meko#727): %s",
					params.BurstDuration, r-1, why)
			}
			return nil
		}); err != nil {
			return err
		}

		if err := t.Step(fmt.Sprintf("degrade_backing_%d", r), func(s *core.StepHandle) error {
			bkube := backingEnv.BackingKube()
			pods, err := backingPods(t, bkube)
			if err != nil {
				return err
			}
			if len(pods) == 0 {
				return fmt.Errorf("no backing pod matched any known selector in namespace %q", bkube.Namespace())
			}
			pod := pods[rnd.IntN(len(pods))]
			if err := bkube.DeleteNow(t.Context, pod); err != nil {
				return fmt.Errorf("force-delete backing pod %s: %w", pod, err)
			}
			s.Set("backing_pod", pod)
			return nil
		}); err != nil {
			return err
		}

		if err := t.Step(fmt.Sprintf("restart_safekeepers_%d", r), func(s *core.StepHandle) error {
			landed := 0
			var failures []string
			for k := 0; k < params.KillsPerRound; k++ {
				for i := range fleet.Tenants {
					// A supervisor past its restart budget legitimately lets the pod be replaced.
					pod, err := endpointPodFor(t, kube, fleet.Tenants[i].Endpoint)
					if err != nil {
						failures = append(failures, err.Error())
						continue
					}
					if _, err := killDaemonOnce(t, kube.ForPod(pod, ""), "safekeeper"); err != nil {
						failures = append(failures, err.Error())
						logf("wal_tail_nemesis: %v", err)
						continue
					}
					landed++
				}
				if k+1 < params.KillsPerRound {
					if err := sleepCtx(t.Context, params.KillGap); err != nil {
						return err
					}
				}
			}
			s.Set("kills_landed", landed)
			s.Set("kills_planned", params.KillsPerRound*len(fleet.Tenants))
			if landed == 0 {
				return fmt.Errorf("not one safekeeper kill landed while the backing was degraded, so this round injected no overlap and proves nothing: %s",
					strings.Join(failures, "; "))
			}
			return nil
		}); err != nil {
			return err
		}

		if err := t.Step(fmt.Sprintf("backing_recovered_%d", r), func(s *core.StepHandle) error {
			waited, err := awaitBackingReady(t, backingEnv.BackingKube(), params.SettleTimeout)
			if err != nil {
				return err
			}
			s.Set("ready_after", waited.Round(time.Second).String())
			return nil
		}); err != nil {
			return err
		}

		if err := t.Step(fmt.Sprintf("endpoints_serve_%d", r), func(s *core.StepHandle) error {
			if err := assertEndpointsServe(t, env, fleet.Tenants, params.SettleTimeout); err != nil {
				return err
			}
			s.Set("endpoints_recovered", len(fleet.Tenants))
			return nil
		}); err != nil {
			return err
		}

		// The oracle: an acked commit the end-of-WAL scan erased is lost data.
		if err := t.Step(fmt.Sprintf("verify_acked_set_%d", r), func(s *core.StepHandle) error {
			return verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes)
		}); err != nil {
			return err
		}
	}

	if err := t.Step("writes_flow_again", func(s *core.StepHandle) error {
		before := totalAcked(journals)
		runJournaledWriters(t.Context, env, fleet.Tenants, journals, t.Seed.Fork("resume"),
			params.ResumeDuration, commitDurabilityParams{PayloadBytes: params.PayloadBytes}, nil)
		delta := totalAcked(journals) - before
		s.Set("acked_after_recovery", delta)
		if delta == 0 {
			return fmt.Errorf("no tenant was acknowledged for a single write in %s after the backing recovered and every endpoint answered SQL: the endpoints serve reads but no COMMIT completes (cf. meko#727): %s",
				params.ResumeDuration, walTailWriteFailure(t, kube, s, "writes_flow_again", fleet.Tenants, journals, logf))
		}
		return verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes)
	}); err != nil {
		return err
	}

	return t.Step("physical_consistency", func(s *core.StepHandle) error {
		err := verifyPhysicalConsistency(t, s, env, fleet.Tenants)
		if err != nil {
			// Teardown deletes these pods, so the evidence goes with them.
			for i := range fleet.Tenants {
				ten := &fleet.Tenants[i]
				attachEndpointDiagnostics(t, kube, ten, "physical_consistency-"+ten.Name, logf)
			}
		}
		return err
	})
}

// walTailWriteFailure collects why a burst acked nothing while the evidence still exists: teardown deletes the tenants' endpoint pods before the run collects anything.
func walTailWriteFailure(t *core.T, kube *k8s.Kubectl, s *core.StepHandle, step string,
	tenants []tenant, journals []*AckJournal, logf func(string, ...any)) string {
	var per []string
	for i := range tenants {
		ten := &tenants[i]
		attempted := atomic.LoadInt64(&ten.Attempted)
		errs := atomic.LoadInt64(&ten.WriteErrors)
		reconnects := atomic.LoadInt64(&ten.Reconnects)
		s.Set(fmt.Sprintf("tenant_%d_attempted", i), attempted)
		s.Set(fmt.Sprintf("tenant_%d_write_errors", i), errs)
		s.Set(fmt.Sprintf("tenant_%d_reconnects", i), reconnects)
		last := journals[i].LastErr()
		if last == "" {
			last = "no failure recorded"
		}
		per = append(per, fmt.Sprintf("%s attempted %d, %d errors, %d reconnects, last: %s",
			ten.Name, attempted, errs, reconnects, last))
		attachEndpointDiagnostics(t, kube, ten, fmt.Sprintf("%s-%s", step, ten.Name), logf)
	}
	return strings.Join(per, "; ")
}
