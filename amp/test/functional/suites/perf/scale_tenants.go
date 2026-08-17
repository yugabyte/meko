// Escalation by TENANT COUNT on one control plane, and which layer says no —
// control plane, k8s scheduling, or the shared backing. perf/multitenant fixes
// the population at 15 and measures interference; this grows it until it stops.
package perf

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func init() {
	core.Register(core.Test{
		Name:        "perf/scale_tenants",
		Suite:       "perf",
		Category:    core.Perf,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{scaleTier},
		OwningTeam:  core.TeamControlPlane,
		Description: "Add tenants (cluster + branch + endpoint) to ONE control plane a batch at a time, writing to every tenant after each batch, until a tenant cannot be created or an existing one stops serving. Reports how many tenants a small deployment carried, what create latency did on the way, and which layer said no.",
		Params:      []string{"perf.scale_tenants"},
		Timeout:     4 * time.Hour,
		Run:         runScaleTenants,
	})
}

type scaleTenantsParams struct {
	BatchSize int
	// MaxTenants is the backstop; the ladder is meant to end on the product or
	// on the cluster's capacity, not on the config.
	MaxTenants int
	SeedRows   int
	Budget     time.Duration
	// VerifySample bounds the post-batch check: writing to EVERY tenant every
	// batch is quadratic, so a sample of the oldest, newest and middle tenant
	// is what each batch proves.
	VerifySample int
	EndpointCPU  string
	EndpointMem  string
}

func scaleTenantsParamsFromConfig(c *core.Config) scaleTenantsParams {
	return scaleTenantsParams{
		BatchSize:    c.GetInt("perf.scale_tenants.batch_size", 3),
		MaxTenants:   c.GetInt("perf.scale_tenants.max_tenants", 200),
		SeedRows:     c.GetInt("perf.scale_tenants.seed_rows", 50),
		Budget:       c.GetDuration("perf.scale_tenants.budget", 3*time.Hour),
		VerifySample: c.GetInt("perf.scale_tenants.verify_sample", 3),
		EndpointCPU:  c.GetString("perf.scale_tenants.endpoint_cpu", "2"),
		EndpointMem:  c.GetString("perf.scale_tenants.endpoint_memory", "4Gi"),
	}
}

func runScaleTenants(t *core.T) error {
	env, ok := t.Env.(ampMultitenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, _ := t.Env.(interface{ AmpKube() *k8s.Kubectl })
	var kube *k8s.Kubectl
	if kubeEnv != nil {
		kube = kubeEnv.AmpKube()
	}
	params := scaleTenantsParamsFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)
	cl, err := env.AmpController(t.Context)
	if err != nil {
		return err
	}
	poolID := env.AmpPoolID()

	var tenants []tenant
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
		defer cancel()
		for i := range tenants {
			if tenants[i].Conn != nil {
				tenants[i].Conn.Close()
			}
			if _, err := cl.DeleteClusterAndWait(ctx, poolID, tenants[i].ClusterID); err != nil {
				logf("scale_tenants: could not delete cluster %s: %v", tenants[i].ClusterID, err)
			}
		}
	}()

	var (
		createAt  []core.ChartPoint
		brokeAt   int
		peakCount int
	)
	stopReason := fmt.Sprintf("carried all %d tenants without breaking", params.MaxTenants)
	deadline := time.Now().Add(params.Budget)

	for batch := 1; len(tenants) < params.MaxTenants; batch++ {
		if batch > 1 && time.Now().After(deadline) {
			stopReason = fmt.Sprintf("%d tenants: ran out of the %s ladder budget before anything broke", len(tenants), params.Budget)
			break
		}
		stage := fmt.Sprintf("batch_%02d_tenants%d", batch, len(tenants)+params.BatchSize)
		err := t.Step(stage, func(s *core.StepHandle) error {
			start := time.Now()
			for i := 0; i < params.BatchSize; i++ {
				ten, _, err := createTenant(t, cl, env, poolID, len(tenants), multitenantParams{
					SeedRows:       params.SeedRows,
					EndpointCPU:    params.EndpointCPU,
					EndpointMemory: params.EndpointMem,
				})
				if err != nil {
					s.Set("tenants_total", len(tenants))
					// Capacity and product refusals look identical from here;
					// the scheduling hint is what tells them apart.
					return fmt.Errorf("tenant %d could not be created%s: %w", len(tenants)+1, schedulingHint(t, logf), err)
				}
				if err := ensureProbeTable(t.Context, ten.Conn); err != nil {
					return fmt.Errorf("tenant %d probe table: %w", len(tenants)+1, err)
				}
				tenants = append(tenants, ten)
			}
			perTenant := time.Since(start).Seconds() / float64(params.BatchSize)
			s.Set("tenants_total", len(tenants))
			s.Set("seconds_per_tenant", fmt.Sprintf("%.1f", perTenant))
			createAt = append(createAt, core.ChartPoint{X: float64(len(tenants)), Y: perTenant})

			// Growing the population must not break the tenants already in it.
			return verifyTenantSample(t, s, tenants, params.VerifySample)
		})
		if err != nil {
			if len(tenants) <= params.BatchSize {
				return fmt.Errorf("the first batch of %d tenants did not stand up, so nothing was measured: %w", params.BatchSize, err)
			}
			stopReason = fmt.Sprintf("%d tenants: broke — %v", len(tenants), err)
			brokeAt = len(tenants)
			if kube != nil && len(tenants) > 0 {
				if diag := captureEndpointDiag(t, kube, tenants[len(tenants)-1], fmt.Sprintf("tenants%d", len(tenants)), logf); diag != "" {
					logf("scale_tenants: newest endpoint at the wall: %s", strings.ReplaceAll(diag, "\n", " | "))
				}
			}
			break
		}
		peakCount = len(tenants)
		logf("scale_tenants: %d tenants standing", peakCount)
	}

	return t.Step("report_ceiling", func(s *core.StepHandle) error {
		if peakCount == 0 {
			return fmt.Errorf("no tenant was created")
		}
		s.Set("max_tenants", peakCount)
		s.Set("broke_at_tenants", brokeAt)
		s.Set("stop_reason", stopReason)
		s.Set("endpoint_size", params.EndpointCPU+" / "+params.EndpointMem)
		if n := len(createAt); n > 0 {
			s.Set("first_seconds_per_tenant", fmt.Sprintf("%.1f", createAt[0].Y))
			s.Set("last_seconds_per_tenant", fmt.Sprintf("%.1f", createAt[n-1].Y))
		}
		gated := []core.Datapoint{
			{Name: "scale_tenants.max_tenants", Value: float64(peakCount), Unit: "tenants", Direction: core.HigherIsBetter},
		}
		var info []core.Datapoint
		if n := len(createAt); n > 0 {
			info = append(info, core.Datapoint{Name: "scale_tenants.last_seconds_per_tenant",
				Value: createAt[n-1].Y, Unit: "s", Direction: core.LowerIsBetter})
		}
		id := identityFromConfig(t.Options, "", frameworkCommit(),
			LoadFields{Script: "scale_tenants", Rows: params.SeedRows})
		t.SetPerfIdentity(id.core())
		gateErr := emitAndGate(t, perfFromConfig(t.Options), scaleVersion(t), "perf/scale_tenants", gated, info, id, logf)
		t.Chart(core.Chart{
			Title: "scale_tenants: seconds per tenant vs population", Kind: core.ChartLine,
			XLabel: "tenants", YLabel: "s",
			Series: []core.ChartSeries{{Name: "create", Points: createAt}},
		})
		return gateErr
	})
}

// verifyTenantSample writes and reads back on the oldest, middle and newest
// tenants. Checking every tenant every batch is quadratic and would spend the
// budget on verification instead of on growth; the oldest is the one most
// likely to have been forgotten, and the newest the one most likely to be sick.
func verifyTenantSample(t *core.T, s *core.StepHandle, tenants []tenant, sample int) error {
	if len(tenants) == 0 {
		return fmt.Errorf("no tenant to verify")
	}
	idx := []int{0, len(tenants) / 2, len(tenants) - 1}
	if sample < 3 {
		idx = idx[:max(1, sample)]
	}
	seen := map[int]bool{}
	for _, i := range idx {
		if seen[i] {
			continue
		}
		seen[i] = true
		ctx, cancel := context.WithTimeout(t.Context, tenantProbeTimeout)
		err := probeWriteRead(ctx, tenants[i], len(tenants))
		cancel()
		if err != nil {
			return fmt.Errorf("tenant %d of %d stopped serving while the population grew: %w", i+1, len(tenants), err)
		}
	}
	s.Set("verified_tenants", len(seen))
	return nil
}

const tenantProbeTimeout = 2 * time.Minute
