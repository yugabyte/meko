package perf

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func init() {
	core.Register(core.Test{
		Name:        "perf/scale_branches",
		Suite:       "perf",
		Category:    core.Perf,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:        []string{scaleTier},
		OwningTeam:  core.TeamControlPlane,
		Description: "Fork ONE seeded branch again and again, more forks every stage, until a fork takes longer than the bound, and publish how many branches one cluster carried. The ceiling must be clean: the parent branch's own read latency stays within the configured multiple of what it was at the first stage.",
		Params:      []string{"perf.scale_branches"},
		Timeout:     4 * time.Hour,
		Run:         runScaleBranches,
	})
}

type scaleBranchesParams struct {
	ladderSteps
	MaxForkTime  time.Duration
	MaxReadRatio float64
	SeedRows     int
	Budget       time.Duration
	ReadTimeout  time.Duration
	EndpointCPU  string
	EndpointMem  string
}

func scaleBranchesParamsFromConfig(c *core.Config) scaleBranchesParams {
	return scaleBranchesParams{
		ladderSteps:  ladderStepsFromConfig(c, "perf.scale_branches", 5, 2, 7),
		MaxForkTime:  c.GetDuration("perf.scale_branches.stop_max_fork_time", 2*time.Minute),
		MaxReadRatio: c.GetFloat("perf.scale_branches.max_parent_read_ratio", 2),
		SeedRows:     c.GetInt("perf.scale_branches.seed_rows", 2000000),
		Budget:       c.GetDuration("perf.scale_branches.budget", 3*time.Hour),
		ReadTimeout:  c.GetDuration("perf.scale_branches.read_timeout", 5*time.Minute),
		EndpointCPU:  c.GetString("perf.scale_branches.endpoint_cpu", "4"),
		EndpointMem:  c.GetString("perf.scale_branches.endpoint_memory", "8Gi"),
	}
}

const branchParentTable = "branch_parent"

func runScaleBranches(t *core.T) error {
	env, ok := t.Env.(ampMultitenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, ok := t.Env.(interface{ AmpKube() *k8s.Kubectl })
	if !ok || kubeEnv.AmpKube() == nil {
		t.Skip("the clean-failure verdict reads the endpoint pod, which needs a kube context")
		return nil
	}
	kube := kubeEnv.AmpKube()
	params := scaleBranchesParamsFromConfig(t.Options)
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
				logf("scale_branches: could not delete cluster %s: %v", tenants[i].ClusterID, err)
			}
		}
	}()

	var pod string
	var baseHealth endpointHealth
	if err := t.Step("seed_parent", func(s *core.StepHandle) error {
		ten, _, err := createTenant(t, cl, env, poolID, 0, multitenantParams{
			SeedRows:       1,
			EndpointCPU:    params.EndpointCPU,
			EndpointMemory: params.EndpointMem,
		})
		if err != nil {
			return fmt.Errorf("%w%s", err, schedulingHint(t, logf))
		}
		tenants = append(tenants, ten)
		start := time.Now()
		if _, err := ten.Conn.Exec(t.Context, fmt.Sprintf(
			`CREATE TABLE %s (id bigint PRIMARY KEY, v bigint, pad text)`, branchParentTable)); err != nil {
			return fmt.Errorf("create the parent table: %w", err)
		}
		if _, err := ten.Conn.Exec(t.Context, fmt.Sprintf(
			`INSERT INTO %s SELECT g, g * 7, repeat('x', 64) FROM generate_series(1, %d) g`,
			branchParentTable, params.SeedRows)); err != nil {
			return fmt.Errorf("seed %d rows into the parent: %w", params.SeedRows, err)
		}
		s.Set("seed_rows", params.SeedRows)
		s.Set("seed_ms", fmt.Sprintf("%.0f", msSince(start)))
		if kube != nil {
			if pod, err = endpointPodByID(t.Context, kube, ten.Endpoint.ID); err != nil {
				return err
			}
			if baseHealth, err = snapshotEndpointHealth(t.Context, kube, pod); err != nil {
				return fmt.Errorf("baseline endpoint health: %w", err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	ten := tenants[0]

	var (
		branches    int
		forkAt      []core.ChartPoint
		readAt      []core.ChartPoint
		firstRead   float64
		lastRead    float64
		reached     int
		brokeAt     int
		stopReason  string
		readFailure error
	)
	deadline := time.Now().Add(params.Budget)

	for i := 0; i < params.MaxStages; i++ {
		batch := params.rung(i)
		if i > 0 && time.Now().After(deadline) {
			stopReason = fmt.Sprintf("%d branches: out of the %s budget before a fork ran long", branches, params.Budget)
			break
		}
		stage := fmt.Sprintf("stage_%02d_branches%d", i+1, branches+batch)
		var slowest, mean float64
		err := t.Step(stage, func(s *core.StepHandle) error {
			total := 0.0
			for n := 0; n < batch; n++ {
				ms, err := timeFork(t, cl, poolID, ten.ClusterID, ten.BranchID, fmt.Sprintf("fork-%d", branches+1))
				if err != nil {
					return wallError{fmt.Errorf("fork %d: %w", branches+1, err)}
				}
				branches++
				total += ms
				slowest = max(slowest, ms)
			}
			// The stage's MEAN, not its worst fork: one tail outlier is not a
			// limit, and a 2.7s fork against a flat 2.0s mean ended this ladder
			// at stage 1 once.
			mean = total / float64(batch)
			s.Set("branches", branches)
			s.Set("mean_fork_ms", fmt.Sprintf("%.0f", mean))
			s.Set("slowest_fork_ms", fmt.Sprintf("%.0f", slowest))
			forkAt = append(forkAt, core.ChartPoint{X: float64(branches), Y: mean})

			readMS, err := parentReadMS(t, ten, params.ReadTimeout)
			if err != nil {
				if isTransportErr(err) {
					return fmt.Errorf("the parent read lost its connection at %d forks, which is the harness tunnel and not a ceiling: %w", branches, err)
				}
				readFailure = err
				diag := captureEndpointDiag(t, kube, ten, fmt.Sprintf("branches%d", branches), logf)
				if diag != "" {
					s.Set("endpoint_state", strings.ReplaceAll(diag, "\n", " | "))
				}
				return wallError{fmt.Errorf("the parent branch stopped answering at %d forks: %w", branches, err)}
			}
			lastRead = readMS
			if firstRead == 0 {
				firstRead = readMS
			}
			readAt = append(readAt, core.ChartPoint{X: float64(branches), Y: readMS})
			s.Set("parent_read_ms", fmt.Sprintf("%.0f", readMS))
			if mean > float64(params.MaxForkTime.Milliseconds()) {
				return wallError{fmt.Errorf("a fork took %.0fms on average (slowest %.0fms), past the %s bound", mean, slowest, params.MaxForkTime)}
			}
			return nil
		})
		if err != nil {
			var wall wallError
			if !errors.As(err, &wall) {
				return fmt.Errorf("stage %d (%d branches): %w", i+1, branches, err)
			}
			if reached == 0 {
				return fmt.Errorf("the first batch of forks never completed, so nothing was measured: %w", err)
			}
			stopReason = fmt.Sprintf("%d branches: %v", branches, err)
			brokeAt = branches
			break
		}
		reached = branches
		logf("scale_branches: %d branches, mean fork %.0fms, parent read %.0fms", branches, mean, lastRead)
	}

	return t.Step("report_ceiling", func(s *core.StepHandle) error {
		if reached == 0 {
			return fmt.Errorf("no fork completed")
		}
		s.Set("stop_reason", stopReason)
		if brokeAt == 0 {
			return noCeilingReached(params.MaxStages, fmt.Sprintf("%d branches still forked inside the bound", reached), stopReason)
		}
		s.Set("max_branches", reached)
		s.Set("broke_at_branches", brokeAt)
		s.Set("first_parent_read_ms", fmt.Sprintf("%.0f", firstRead))
		s.Set("last_parent_read_ms", fmt.Sprintf("%.0f", lastRead))
		readErr := readFailure
		if readErr == nil && firstRead > 0 && lastRead > firstRead*params.MaxReadRatio {
			readErr = fmt.Errorf("the parent's read went from %.0fms to %.0fms, past %.1fx",
				firstRead, lastRead, params.MaxReadRatio)
		}
		clean := assertCleanCeiling(t.Context, s, kube, pod, baseHealth,
			cleanCheck{"the parent branch's reads", readErr})
		t.Chart(core.Chart{
			Title: "scale_branches: mean fork time vs branches on the cluster", Kind: core.ChartLine,
			XLabel: "branches", YLabel: "ms",
			Series: []core.ChartSeries{{Name: "mean fork", Points: forkAt}},
		})
		t.Chart(core.Chart{
			Title: "scale_branches: parent read latency vs branches", Kind: core.ChartLine,
			XLabel: "branches", YLabel: "ms",
			Series: []core.ChartSeries{{Name: "parent read", Points: readAt}},
		})
		gated := []core.Datapoint{
			{Name: "branches.per_cluster", Value: float64(reached), Unit: "branches", Direction: core.HigherIsBetter},
		}
		info := []core.Datapoint{
			{Name: "branches.parent_read_ms_at_ceiling", Value: lastRead, Unit: "ms", Direction: core.LowerIsBetter},
			{Name: "branches.mean_fork_ms_at_ceiling", Value: forkAt[len(forkAt)-1].Y, Unit: "ms", Direction: core.LowerIsBetter},
		}
		id := identityFromConfig(t.Options, "", frameworkCommit(),
			LoadFields{Script: "scale_branches", Rows: params.SeedRows, Clients: params.Start})
		t.SetPerfIdentity(id.core())
		if gateErr := emitAndGateAuto(t, perfFromConfig(t.Options), scaleVersion(t), "perf/scale_branches", gated, info, id, logf); gateErr != nil {
			return gateErr
		}
		return clean
	})
}

// The tunnel's ~29ms rides in both measurements, so their ratio is conservative.
func parentReadMS(t *core.T, ten tenant, timeout time.Duration) (float64, error) {
	ctx, cancel := context.WithTimeout(t.Context, timeout)
	defer cancel()
	start := time.Now()
	if _, err := ten.Conn.QueryString(ctx, fmt.Sprintf(
		"SELECT sum(v)::text FROM %s", branchParentTable)); err != nil {
		return 0, err
	}
	return msSince(start), nil
}
