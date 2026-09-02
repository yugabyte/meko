// Chaos aimed at the YugabyteDB under amp: amp_nemesis kills amp's compute
// pods, nothing killed the database they write THROUGH. Kills the backing's
// serving pods (whichever backing the run deployed) while tenants write, then
// demands full recovery — endpoints serve, writes FLOW again, rows verify.
// meko#727 is why the write probe exists.
package stress

import (
	"fmt"
	"log"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/backing_nemesis",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane, FaultsSharedInfra: true},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Kill the backing YugabyteDB's serving pods (YBA universe / ybk8s tservers, or the built-in yugabyted) while tenants write through amp endpoints, then assert full recovery: endpoints serve, writes flow again, and every acknowledged row verifies exactly.",
		Params:      []string{"stress.backing_nemesis", "chaos"},
		Timeout:     90 * time.Minute,
		Run:         runBackingNemesis,
	})
}

type backingNemesisParams struct {
	Tenants  int
	Duration time.Duration
	// SettleTimeout bounds how long recovery may take once the faults stop.
	SettleTimeout time.Duration
	PayloadBytes  int
	// ResumeDuration: the post-chaos write window (the meko#727 probe).
	ResumeDuration time.Duration
	// ControlPlaneRecoveryMax bounds the controller's error window after the
	// last kill (meko#961).
	ControlPlaneRecoveryMax time.Duration
}

func backingNemesisParamsFromConfig(c *core.Config) backingNemesisParams {
	return backingNemesisParams{
		Tenants:                 c.GetInt("stress.backing_nemesis.tenants", 3),
		Duration:                c.GetDuration("stress.backing_nemesis.duration", 3*time.Minute),
		SettleTimeout:           c.GetDuration("stress.backing_nemesis.settle_timeout", 10*time.Minute),
		PayloadBytes:            c.GetInt("stress.backing_nemesis.payload_bytes", 256),
		ResumeDuration:          c.GetDuration("stress.backing_nemesis.resume_duration", 30*time.Second),
		ControlPlaneRecoveryMax: c.GetDuration("stress.backing_nemesis.control_plane_recovery_max", 2*time.Minute),
	}
}

func runBackingNemesis(t *core.T) error {
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	backingEnv, ok := t.Env.(backingKubeEnv)
	if !ok || backingEnv.BackingKube() == nil {
		t.Skip("requires a kube handle for the backing's namespace: chaos has nothing to aim at without it")
		return nil
	}
	params := backingNemesisParamsFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)

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

	var cpRecovery time.Duration
	cpMeasured := false
	if err := t.Step("chaos_on_backing", func(s *core.StepHandle) error {
		kube := backingEnv.BackingKube()
		pods, err := backingPods(t, kube)
		if err != nil {
			return err
		}
		if len(pods) == 0 {
			return fmt.Errorf("no backing pod matched any known selector in namespace %q", kube.Namespace())
		}
		probe := newControlPlaneProbe(fleet.cl, fleet.poolID)
		probe.Start(t.Context)
		chaosErr := chaosOnPods(t, s, env, fleet, kube, pods, "backing-nemesis",
			params.Duration, params.PayloadBytes, logf)
		if chaosErr != nil {
			probe.Abort()
			return chaosErr
		}
		chaosEnd := time.Now()
		cpRecovery, cpMeasured = probe.Stop(chaosEnd, params.SettleTimeout)
		tally, bad := probe.Summary()
		s.Set("control_plane_probe", tally)
		s.Set("control_plane_recovery", cpRecovery.Round(time.Second).String())
		s.Set("control_plane_recovery_measured", cpMeasured)
		if cpMeasured {
			t.Metric("control_plane_recovery_seconds", cpRecovery.Seconds(), "s")
		}
		if bad > 0 {
			return fmt.Errorf("the controller answered %d non-503 5xx response(s) while the backing was being killed (%s); an unreachable store is a retryable 503, never a 500 (meko#886)", bad, tally)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("backing_recovered", func(s *core.StepHandle) error {
		waited, err := awaitBackingReady(t, backingEnv.BackingKube(), params.SettleTimeout)
		if err != nil {
			return err
		}
		s.Set("ready_after", waited.Round(time.Second).String())
		return nil
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

	// meko#727: reads answer from the pageserver while no COMMIT returns.
	if err := t.Step("writes_flow_again", func(s *core.StepHandle) error {
		waited, err := awaitWritePath(t.Context, env, fleet.Tenants, nil, params.SettleTimeout, params.PayloadBytes)
		if err != nil {
			return fmt.Errorf("the write path never came back (cf. meko#727): %w", err)
		}
		s.Set("write_path_back_after", waited.Round(time.Second).String())
		before := totalWritten(fleet.Tenants)
		runWriters(t.Context, env, fleet.Tenants, params.ResumeDuration, params.PayloadBytes)
		delta := totalWritten(fleet.Tenants) - before
		s.Set("rows_after_chaos", delta)
		if delta == 0 {
			return fmt.Errorf("every tenant committed a probe row, then none committed anything in the following %s. The write path stalled again (cf. meko#727)",
				params.ResumeDuration)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("verify_data", func(s *core.StepHandle) error {
		return verifyTenantData(t, s, env, fleet.Tenants, params.PayloadBytes)
	}); err != nil {
		return err
	}

	return t.Step("control_plane_recovered", func(s *core.StepHandle) error {
		waited, err := awaitControlPlaneReady(t, fleet, params.SettleTimeout)
		if err != nil {
			return err
		}
		s.Set("ready_after", waited.Round(time.Second).String())
		if !cpMeasured {
			return fmt.Errorf("the controller never answered a probe 2xx in the %s after the last backing pod kill, so its recovery could not be measured (meko#961)", params.SettleTimeout)
		}
		if cpRecovery > params.ControlPlaneRecoveryMax {
			return fmt.Errorf("the controller kept answering errors for %s after the last backing pod kill, over the %s allowed; its pool must drop a departed backing pod within seconds, not minutes (meko#961)",
				cpRecovery.Round(time.Second), params.ControlPlaneRecoveryMax)
		}
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
