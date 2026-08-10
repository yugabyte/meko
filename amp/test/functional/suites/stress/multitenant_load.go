// Real multitenancy under sustained parallel load, at volume.
//
// stress/combined_load stresses the backing YugabyteDB directly, with amp not in
// the request path; stress/load_sustain drives one endpoint. Neither answers the
// question this one asks: with many tenants writing hard at the same time,
// through their own endpoints, does every tenant still get exactly its own data
// back, does anyone starve, and is the control plane still able to work?
//
// Every write is accounted for by the writer that made it (count + checksum),
// and the verification reads through a FRESH connection per tenant, so what is
// judged is what shared storage holds rather than what a writing session
// remembers.
package stress

import (
	"fmt"
	"log"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/multitenant_load",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"stress", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "N tenants (cluster + endpoint each) writing in parallel on one control plane for a config-tunable duration: every tenant's rows and checksum must match what its own writer committed, no tenant may see another's tables, nobody may be starved, and the control plane must still serve afterwards.",
		Params:      []string{"stress.multitenant"},
		Timeout:     90 * time.Minute,
		Run:         runMultitenantLoad,
	})
}

// multitenantLoadParams are the knobs, all under stress.multitenant.
type multitenantLoadParams struct {
	Tenants      int
	Duration     time.Duration
	PayloadBytes int
	// MinFairness fails the run when the slowest tenant's committed-row count
	// falls below this share of the fastest one's — the starvation gate.
	MinFairness float64
	// SettleTimeout bounds how long an endpoint may take to answer again once
	// the load stops.
	SettleTimeout time.Duration
}

func multitenantLoadParamsFromConfig(c *core.Config) multitenantLoadParams {
	return multitenantLoadParams{
		Tenants:       c.GetInt("stress.multitenant.tenants", 4),
		Duration:      c.GetDuration("stress.multitenant.duration", 3*time.Minute),
		PayloadBytes:  c.GetInt("stress.multitenant.payload_bytes", 512),
		MinFairness:   c.GetFloat("stress.multitenant.min_fairness", 0.2),
		SettleTimeout: c.GetDuration("stress.multitenant.settle_timeout", 5*time.Minute),
	}
}

func runMultitenantLoad(t *core.T) error {
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s); one endpoint cannot host tenants")
		return nil
	}
	params := multitenantLoadParamsFromConfig(t.Options)
	if params.Tenants < 2 {
		t.Skipf("stress.multitenant.tenants = %d: multitenancy needs at least 2", params.Tenants)
		return nil
	}
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

	if err := t.Step("parallel_load", func(s *core.StepHandle) error {
		start := time.Now()
		runWriters(t.Context, env, fleet.Tenants, params.Duration, params.PayloadBytes)
		elapsed := time.Since(start)
		rows := totalWritten(fleet.Tenants)
		s.Set("rows_written", rows)
		s.Set("payload_bytes", params.PayloadBytes)
		s.Set("elapsed", elapsed.Round(time.Second).String())
		// Volume is the point of this test, so it is reported rather than left to
		// be inferred from the row count.
		t.Metric("multitenant.rows_written", float64(rows), "rows")
		t.Metric("multitenant.data_written_mb", float64(rows*int64(params.PayloadBytes))/(1024*1024), "MB")
		if elapsed > 0 {
			t.Metric("multitenant.aggregate_write_rate", float64(rows)/elapsed.Seconds(), "rows/s")
		}
		if rows == 0 {
			return fmt.Errorf("no tenant committed a single row in %s", params.Duration)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("endpoints_still_serve", func(s *core.StepHandle) error {
		if err := assertEndpointsServe(t, env, fleet.Tenants, params.SettleTimeout); err != nil {
			return err
		}
		s.Set("endpoints", len(fleet.Tenants))
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("amp_pods_healthy", func(s *core.StepHandle) error {
		kubeEnv, ok := t.Env.(ampKubeEnv)
		if !ok {
			s.Set("checked", "no kube context")
			return nil
		}
		summary, err := assertAmpHealthy(t, kubeEnv.AmpKube())
		if err != nil {
			return err
		}
		s.Set("pods", summary)
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("assert_no_starvation", func(s *core.StepHandle) error {
		// A tenant that wrote nothing at all is a client that lost its endpoint,
		// not a tenant its neighbours crowded out. Saying so precisely keeps the
		// fairness gate from accusing the product of starvation it did not cause.
		if why := silentTenant(fleet.Tenants); why != "" {
			return fmt.Errorf("%s", why)
		}
		f := fairnessOf(fleet.Tenants)
		s.Set("fairness", fmt.Sprintf("%.2f", f))
		for i := range fleet.Tenants {
			s.Set(fmt.Sprintf("tenant_%d_rows", i), fleet.Tenants[i].Written)
			if fleet.Tenants[i].Reconnects > 0 || fleet.Tenants[i].WriteErrors > 0 {
				s.Set(fmt.Sprintf("tenant_%d_client", i),
					fmt.Sprintf("%d error(s), %d reconnect(s)", fleet.Tenants[i].WriteErrors, fleet.Tenants[i].Reconnects))
			}
		}
		t.Metric("multitenant.fairness", f, "x")
		if f < params.MinFairness {
			return fmt.Errorf("the slowest tenant committed %.0f%% of what the fastest did, under the %.0f%% required by stress.multitenant.min_fairness — one tenant is being starved by its neighbours",
				f*100, params.MinFairness*100)
		}
		return nil
	}); err != nil {
		return err
	}

	// A control plane that survived the load still has to be able to do its job.
	return t.Step("control_plane_still_works", func(s *core.StepHandle) error {
		cl, err := env.AmpController(t.Context)
		if err != nil {
			return fmt.Errorf("the controller is unreachable after the load: %w", err)
		}
		list, err := cl.ListClusters(t.Context, env.AmpPoolID(), 500, 0)
		if err != nil {
			return fmt.Errorf("the controller cannot list clusters after the load: %w", err)
		}
		s.Set("clusters_visible", len(list.Clusters))
		// Creating one more tenant is the real proof it still works.
		before := len(fleet.Tenants)
		if err := fleet.Create(t, 1); err != nil {
			return fmt.Errorf("a new tenant cannot be created after the load: %w", err)
		}
		if len(fleet.Tenants) <= before {
			return fmt.Errorf("creating a tenant after the load reported success but added none")
		}
		return nil
	})
}
