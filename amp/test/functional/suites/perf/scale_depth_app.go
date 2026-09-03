// Dataset escalation with a real workload instead of TPC-C: each rung grows the
// dataset with SqlDataLoadWithDDL, then measures SqlTransactions. The x-axis is
// the backing disk actually used, sampled per rung.
package perf

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func init() {
	core.Register(core.Test{
		Name:        "perf/scale_depth_sqltx",
		Suite:       "perf",
		Category:    core.Perf,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:        []string{scaleTier},
		OwningTeam:  core.TeamControlPlane,
		Description: "Grow one endpoint's dataset rung by rung with SqlDataLoadWithDDL, and after each rung measure SqlTransactions throughput on the same endpoint. Reports how a real workload's tps changes as the endpoint's data (sampled in GB on the backing) grows, and where it breaks.",
		Params:      []string{"perf.scale_depth_app"},
		Timeout:     5 * time.Hour,
		Run:         runDepthAppLadder,
	})
}

type depthAppParams struct {
	Rungs         int
	SeedDuration  time.Duration
	StageDuration time.Duration
	Budget        time.Duration
	MinTPS        float64
	ReadTimeout   time.Duration
	EndpointCPU   string
	EndpointMem   string
	// BulkRowsPerRung > 0 seeds server-side with generate_series instead of the
	// sample-app loader. The loader is bound by the compute (~110 rows/s at 2
	// vCPU regardless of client threads), which is far too slow to reach the
	// dataset sizes where TPC-C walls.
	BulkRowsPerRung int
}

func depthAppParamsFromConfig(c *core.Config) depthAppParams {
	return depthAppParams{
		Rungs:           c.GetInt("perf.scale_depth_app.rungs", 8),
		SeedDuration:    c.GetDuration("perf.scale_depth_app.seed_duration", 10*time.Minute),
		StageDuration:   c.GetDuration("perf.scale_depth_app.stage_duration", 5*time.Minute),
		Budget:          c.GetDuration("perf.scale_depth_app.budget", 4*time.Hour),
		MinTPS:          c.GetFloat("perf.scale_depth_app.min_tps", 1.0),
		ReadTimeout:     c.GetDuration("perf.scale_depth_app.read_timeout", 2*time.Minute),
		EndpointCPU:     c.GetString("perf.scale_depth_app.endpoint_cpu", "2"),
		EndpointMem:     c.GetString("perf.scale_depth_app.endpoint_memory", "4Gi"),
		BulkRowsPerRung: c.GetInt("perf.scale_depth_app.bulk_rows_per_rung", 0),
	}
}

// bulkSeed writes one fat table per rung server-side, so growth costs no client
// round-trips. ~130 bytes of payload per row before amp's amplification.
func bulkSeed(ctx context.Context, conn *pg.Client, rung, rows int) error {
	table := fmt.Sprintf("bulk_%03d", rung)
	if _, err := conn.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (id bigint PRIMARY KEY, v bigint, ts timestamptz, pad text)`, table)); err != nil {
		return fmt.Errorf("create %s: %w", table, err)
	}
	_, err := conn.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s SELECT g, g*7, now(), repeat(md5(g::text), 4) FROM generate_series(1, %d) g`, table, rows))
	if err != nil {
		return fmt.Errorf("bulk seed %s: %w", table, err)
	}
	return nil
}

func runDepthAppLadder(t *core.T) error {
	env, ok := t.Env.(ampMultitenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, ok := t.Env.(interface{ AmpKube() *k8s.Kubectl })
	if !ok || kubeEnv.AmpKube() == nil {
		t.Skip("driving real clients needs a kube context for the control-plane namespace")
		return nil
	}
	params := depthAppParamsFromConfig(t.Options)
	cfg := perfFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)
	kube := kubeEnv.AmpKube()
	cl, err := env.AmpController(t.Context)
	if err != nil {
		return err
	}
	poolID := env.AmpPoolID()

	var tenants []tenant
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		for i := range tenants {
			if tenants[i].Conn != nil {
				tenants[i].Conn.Close()
			}
			if _, err := cl.DeleteClusterAndWait(ctx, poolID, tenants[i].ClusterID); err != nil {
				logf("scale_depth_sqltx: could not delete cluster %s: %v", tenants[i].ClusterID, err)
			}
		}
	}()
	td := &teardownStack{}
	defer func() {
		tctx, cancel := context.WithTimeout(context.Background(), perfTeardownBudget)
		defer cancel()
		_ = td.run(tctx, logf)
	}()

	var measure, seeder Workload
	var ex exec.RemoteExecutor
	if err := t.Step("create_endpoint", func(s *core.StepHandle) error {
		ten, _, err := createTenant(t, cl, env, poolID, 0, multitenantParams{
			SeedRows:       1,
			EndpointCPU:    params.EndpointCPU,
			EndpointMemory: params.EndpointMem,
		})
		if err != nil {
			return fmt.Errorf("%w%s", err, schedulingHint(t, logf))
		}
		tenants = append(tenants, ten)
		if err := ensureProbeTable(t.Context, ten.Conn); err != nil {
			return fmt.Errorf("create the probe table: %w", err)
		}
		conn := endpointConn(kube.Namespace(), ten.Endpoint.ID, ten.Endpoint.Address)
		podName := "ampqa-da-" + t.ResourceSuffix()
		ex, err = startPerfClientPod(t.Context, kube, attachSink{t: t, logf: logf}, td,
			podName, cfg.ClientImage, cfg.ClientPullSecret)
		if err != nil {
			return err
		}
		jarPath, err := deliverDriversOnce(t.Context, t, cfg, []string{mixSqlTransactions}, []exec.RemoteExecutor{ex})
		if err != nil {
			return err
		}
		deps := mixDeps{jarPath: jarPath, marker: "da-" + t.ResourceSuffix(), logf: logf}
		if measure, err = mixWorkloadFor(mixSqlTransactions, cfg, t.Options, conn, deps); err != nil {
			return err
		}
		if seeder, err = mixWorkloadFor(mixDataLoadDDL, cfg, t.Options, conn, deps); err != nil {
			return err
		}
		// The seeder exists to move gigabytes; at the measure workload's thread
		// count it managed ~2 GB in 80 minutes and never reached the region
		// where TPC-C walls.
		if sw, ok := seeder.(*sampleappWorkload); ok {
			n := t.Options.GetInt("perf.scale_depth_app.seed_threads", 48)
			sw.readers, sw.writers = 0, n
		}
		s.Set("endpoint", ten.Endpoint.ID)
		s.Set("endpoint_size", params.EndpointCPU+" / "+params.EndpointMem)
		return nil
	}); err != nil {
		return err
	}

	var tpsAt []core.ChartPoint
	var reached int
	stopReason := fmt.Sprintf("completed all %d rungs without breaking", params.Rungs)
	brokeAt := 0
	deadline := time.Now().Add(params.Budget)

	for i := 1; i <= params.Rungs; i++ {
		if i > 1 && time.Now().After(deadline) {
			stopReason = fmt.Sprintf("rung %d: ran out of the %s budget before anything broke", i-1, params.Budget)
			break
		}
		stage := fmt.Sprintf("stage_%02d", i)
		var usedGB float64
		err := t.Step(stage, func(s *core.StepHandle) error {
			if params.BulkRowsPerRung > 0 {
				start := time.Now()
				if err := bulkSeed(t.Context, tenants[0].Conn, i, params.BulkRowsPerRung); err != nil {
					diag := captureEndpointDiag(t, kube, tenants[0], fmt.Sprintf("depth-sqltx-%d-bulk", i), logf)
					return wallError{fmt.Errorf("bulk seed rung %d [endpoint: %s]: %w", i, strings.ReplaceAll(diag, "\n", " | "), err)}
				}
				s.Set("bulk_seed_ms", fmt.Sprintf("%.0f", msSince(start)))
			}
			seed, err := seeder.RunOnce(t.Context, ex, RunParams{Duration: params.SeedDuration, SampleInterval: time.Second})
			recordDriverRun(t, logf, "seed", stage, seed, params.SeedDuration)
			if err != nil {
				if isTransportErr(err) {
					return fmt.Errorf("seed rung %d: lost the exec transport: %w", i, err)
				}
				diag := captureEndpointDiag(t, kube, tenants[0], fmt.Sprintf("depth-sqltx-%d-seed", i), logf)
				return wallError{fmt.Errorf("seed rung %d stopped [endpoint: %s]: %w", i, strings.ReplaceAll(diag, "\n", " | "), err)}
			}
			s.Set("seed_ops_per_sec", fmt.Sprintf("%.1f", seed.TPS))

			sample, err := measure.RunOnce(t.Context, ex, RunParams{Duration: params.StageDuration, SampleInterval: time.Second})
			recordDriverRun(t, logf, "sqltx", stage, sample, params.StageDuration)
			if err != nil {
				if isTransportErr(err) {
					return fmt.Errorf("measure rung %d: lost the exec transport: %w", i, err)
				}
				diag := captureEndpointDiag(t, kube, tenants[0], fmt.Sprintf("depth-sqltx-%d", i), logf)
				return wallError{fmt.Errorf("measure rung %d stopped [endpoint: %s]: %w", i, strings.ReplaceAll(diag, "\n", " | "), err)}
			}
			if used, avail, ok := backingDiskGB(t, env); ok {
				usedGB = used
				s.Set("backing_used_gb", fmt.Sprintf("%.1f", used))
				s.Set("backing_avail_gb", fmt.Sprintf("%.1f", avail))
			}
			s.Set("tps", fmt.Sprintf("%.1f", sample.TPS))
			s.Set("latency_ms", fmt.Sprintf("%.1f", sample.LatencyAvgMS))
			if sample.TPS <= params.MinTPS {
				diag := captureEndpointDiag(t, kube, tenants[0], fmt.Sprintf("depth-sqltx-%d-zero", i), logf)
				if strings.TrimSpace(sample.Raw) == "" {
					return fmt.Errorf("rung %d: the driver produced no output at all; a measurement that did not happen, not a wall [endpoint: %s]",
						i, strings.ReplaceAll(diag, "\n", " | "))
				}
				return wallError{fmt.Errorf("rung %d: throughput fell to %.1f tps [endpoint: %s]",
					i, sample.TPS, strings.ReplaceAll(diag, "\n", " | "))}
			}
			tpsAt = append(tpsAt, core.ChartPoint{X: usedGB, Y: sample.TPS})
			// An endpoint that stops answering after a rung IS the wall, and the
			// pod state is the only evidence of which kind, so capture it here
			// rather than reporting a bare timeout.
			if err := assertStorageAdvances(t, s, kube, env, tenants[0], i, params.ReadTimeout, logf); err != nil {
				diag := captureEndpointDiag(t, kube, tenants[0], fmt.Sprintf("depth-sqltx-%d-probe", i), logf)
				if diag != "" {
					s.Set("endpoint_state", strings.ReplaceAll(diag, "\n", " | "))
				}
				return wallError{fmt.Errorf("%w [endpoint: %s]", err, strings.ReplaceAll(diag, "\n", " | "))}
			}
			return nil
		})
		if err != nil {
			var wall wallError
			if !errors.As(err, &wall) {
				return fmt.Errorf("rung %d (reached %d): %w", i, reached, err)
			}
			if i == 1 {
				return fmt.Errorf("the first rung could not be measured: %w", err)
			}
			stopReason = fmt.Sprintf("rung %d: broke — %v", i, err)
			brokeAt = i
			break
		}
		reached = i
	}

	return t.Step("report_ceiling", func(s *core.StepHandle) error {
		if reached == 0 {
			return fmt.Errorf("no rung completed")
		}
		s.Set("stop_reason", stopReason)
		s.Set("max_rungs", reached)
		s.Set("broke_at_rung", brokeAt)
		if n := len(tpsAt); n > 0 {
			s.Set("last_backing_used_gb", fmt.Sprintf("%.1f", tpsAt[n-1].X))
			s.Set("last_tps", fmt.Sprintf("%.1f", tpsAt[n-1].Y))
		}
		t.Chart(core.Chart{
			Title: "sqltx throughput vs backing dataset (" + params.EndpointCPU + "/" + params.EndpointMem + ")",
			Kind:  core.ChartLine, XLabel: "backing used GB", YLabel: "tps",
			Series: []core.ChartSeries{{Name: "sqltx", Points: tpsAt}},
		})
		for _, p := range tpsAt {
			logf("scale_depth_sqltx: %.1f GB on backing: %.1f tps", p.X, p.Y)
		}
		return nil
	})
}
