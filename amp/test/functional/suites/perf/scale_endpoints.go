// Each endpoint gets its own branch: the controller holds one endpoint per role
// per branch, and a second read_write create on the same branch comes back
// "conflict" in seconds.
package perf

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func init() {
	core.Register(core.Test{
		Name:        "perf/scale_endpoints",
		Suite:       "perf",
		Category:    core.Perf,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:        []string{scaleTier},
		OwningTeam:  core.TeamControlPlane,
		Description: "Create endpoints on ONE cluster a batch at a time until a create fails or takes longer than the bound, and publish how many computes one control plane carried. The refusal at the ceiling must be clean: the failing task names a scheduling or quota reason rather than a bare deadline, and every endpoint created before it still serves SQL.",
		Params:      []string{"perf.scale_endpoints"},
		Timeout:     8 * time.Hour,
		Run:         runScaleEndpoints,
	})
}

type scaleEndpointsParams struct {
	ladderSteps
	MaxCreateTime time.Duration
	Budget        time.Duration
	ProbeTimeout  time.Duration
	EndpointCPU   string
	EndpointMem   string
}

func scaleEndpointsParamsFromConfig(c *core.Config) scaleEndpointsParams {
	return scaleEndpointsParams{
		ladderSteps:   ladderStepsFromConfig(c, "perf.scale_endpoints", 5, 1, 10),
		MaxCreateTime: c.GetDuration("perf.scale_endpoints.stop_max_create_time", 20*time.Minute),
		Budget:        c.GetDuration("perf.scale_endpoints.budget", 6*time.Hour),
		ProbeTimeout:  c.GetDuration("perf.scale_endpoints.probe_timeout", 60*time.Second),
		EndpointCPU:   c.GetString("perf.scale_endpoints.endpoint_cpu", "2"),
		EndpointMem:   c.GetString("perf.scale_endpoints.endpoint_memory", "4Gi"),
	}
}

func runScaleEndpoints(t *core.T) error {
	env, ok := t.Env.(ampMultitenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, ok := t.Env.(interface{ AmpKube() *k8s.Kubectl })
	if !ok || kubeEnv.AmpKube() == nil {
		t.Skip("proving the older endpoints still serve needs a kube context for the control-plane namespace")
		return nil
	}
	params := scaleEndpointsParamsFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)
	kube := kubeEnv.AmpKube()
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
				logf("scale_endpoints: could not delete cluster %s: %v", tenants[i].ClusterID, err)
			}
		}
	}()

	var baseHealth endpointHealth
	var firstPod string
	if err := t.Step("create_cluster", func(s *core.StepHandle) error {
		ten, tm, err := createTenant(t, cl, env, poolID, 0, multitenantParams{
			SeedRows:       1,
			EndpointCPU:    params.EndpointCPU,
			EndpointMemory: params.EndpointMem,
		})
		if err != nil {
			return fmt.Errorf("%w%s", err, schedulingHint(t, logf))
		}
		tenants = append(tenants, ten)
		if firstPod, err = endpointPodByID(t.Context, kube, ten.Endpoint.ID); err != nil {
			return err
		}
		if baseHealth, err = snapshotEndpointHealth(t.Context, kube, firstPod); err != nil {
			return fmt.Errorf("baseline endpoint health: %w", err)
		}
		s.Set("cluster", ten.ClusterID)
		s.Set("first_endpoint_ms", fmt.Sprintf("%.0f", tm.endpointMS))
		s.Set("endpoint_size", params.EndpointCPU+" / "+params.EndpointMem)
		return nil
	}); err != nil {
		return err
	}
	ten := tenants[0]

	var (
		standing   = []string{ten.Endpoint.ID}
		createAt   []core.ChartPoint
		reached    int
		brokeAt    int
		stopReason string
		failTask   controller.Task
		failErr    error
		deadTime   time.Duration
	)
	deadline := time.Now().Add(params.Budget)

	for i := 0; i < params.MaxStages; i++ {
		batch := params.rung(i)
		if i > 0 && time.Now().After(deadline) {
			stopReason = fmt.Sprintf("%d endpoints: out of the %s budget before a create refused", len(standing), params.Budget)
			break
		}
		stage := fmt.Sprintf("batch_%02d_endpoints%d", i+1, len(standing)+batch)
		var slowest time.Duration
		err := t.Step(stage, func(s *core.StepHandle) error {
			for n := 0; n < batch; n++ {
				branchID, err := forkBranchID(t, cl, poolID, ten.ClusterID, ten.BranchID,
					fmt.Sprintf("ep-%d", len(standing)+1))
				if err != nil {
					return wallError{fmt.Errorf("branch for endpoint %d: %w", len(standing)+1, err)}
				}
				// Not bounded by MaxCreateTime: a create cancelled on the test's
				// own deadline reports "context deadline exceeded" and hides what
				// the control plane would have said. The bound is read off the
				// elapsed time instead.
				start := time.Now()
				task, err := cl.CreateEndpointAndWait(t.Context, poolID, ten.ClusterID, branchID,
					controller.CreateEndpointRequest{
						Role:      "read_write",
						Resources: &controller.EndpointResources{CPU: params.EndpointCPU, Memory: params.EndpointMem},
					})
				took := time.Since(start)
				slowest = max(slowest, took)
				if err != nil {
					failTask, failErr, deadTime = task, err, took
					diag := captureEndpointDiag(t, kube, ten, fmt.Sprintf("endpoints%d", len(standing)+1), logf)
					if diag != "" {
						s.Set("endpoint_state", strings.ReplaceAll(diag, "\n", " | "))
					}
					return wallError{fmt.Errorf("endpoint %d refused after %s%s: %w",
						len(standing)+1, took.Round(time.Second), schedulingHint(t, logf), err)}
				}
				eps, err := cl.ListAllEndpoints(t.Context, poolID, ten.ClusterID, branchID)
				if err != nil || len(eps) == 0 {
					return fmt.Errorf("the branch for endpoint %d lists no endpoint after a successful create: %v", len(standing)+1, err)
				}
				standing = append(standing, eps[len(eps)-1].ID)
				s.Set("endpoints_total", len(standing))
				if took > params.MaxCreateTime {
					deadTime = took
					return wallError{fmt.Errorf("endpoint %d took %s, past the %s bound", len(standing), took.Round(time.Second), params.MaxCreateTime)}
				}
			}
			s.Set("endpoints_total", len(standing))
			s.Set("slowest_create_seconds", fmt.Sprintf("%.0f", slowest.Seconds()))
			createAt = append(createAt, core.ChartPoint{X: float64(len(standing)), Y: slowest.Seconds()})
			return serveCheck(t.Context, kube, standing, params.ProbeTimeout)
		})
		if err != nil {
			var wall wallError
			if !errors.As(err, &wall) {
				return fmt.Errorf("batch %d (standing %d): %w", i+1, len(standing), err)
			}
			if reached == 0 {
				return fmt.Errorf("the first batch never completed, so nothing was measured: %w", err)
			}
			stopReason = fmt.Sprintf("%d endpoints: %v", len(standing), err)
			brokeAt = len(standing)
			break
		}
		reached = len(standing)
		logf("scale_endpoints: %d endpoints standing, slowest create %s", reached, slowest.Round(time.Second))
	}

	return t.Step("report_ceiling", func(s *core.StepHandle) error {
		if reached == 0 {
			return fmt.Errorf("no batch of endpoints stood up")
		}
		s.Set("stop_reason", stopReason)
		if brokeAt == 0 {
			return noCeilingReached(params.MaxStages, fmt.Sprintf("%d endpoints still created inside the bound", reached), stopReason)
		}
		s.Set("max_endpoints", reached)
		s.Set("broke_at_endpoints", brokeAt)
		if failTask.Error != nil {
			s.Set("task_error", *failTask.Error)
		}
		clean := assertCleanCeiling(t.Context, s, kube, firstPod, baseHealth,
			cleanCheck{"the refusal", refusalVerdict(failTask, failErr, deadTime, params.MaxCreateTime)},
			cleanCheck{"the endpoints created before it", serveCheck(t.Context, kube, standing[:reached], params.ProbeTimeout)})
		t.Chart(core.Chart{
			Title: "scale_endpoints: slowest create vs endpoints on the cluster", Kind: core.ChartLine,
			XLabel: "endpoints", YLabel: "s",
			Series: []core.ChartSeries{{Name: "create", Points: createAt}},
		})
		gated := []core.Datapoint{
			{Name: "endpoints.per_controller", Value: float64(reached), Unit: "endpoints", Direction: core.HigherIsBetter},
		}
		var info []core.Datapoint
		if n := len(createAt); n > 0 {
			info = append(info, core.Datapoint{Name: "endpoints.slowest_create_seconds",
				Value: createAt[n-1].Y, Unit: "s", Direction: core.LowerIsBetter})
		}
		id := identityFromConfig(t.Options, "", frameworkCommit(),
			LoadFields{Script: "scale_endpoints:" + params.EndpointCPU + "/" + params.EndpointMem, Clients: params.Start})
		t.SetPerfIdentity(id.core())
		if gateErr := emitAndGateAuto(t, perfFromConfig(t.Options), scaleVersion(t), "perf/scale_endpoints", gated, info, id, logf); gateErr != nil {
			return gateErr
		}
		return clean
	})
}

func forkBranchID(t *core.T, cl *controller.Client, poolID, clusterID, parentID, name string) (string, error) {
	full := name + "-" + t.ResourceSuffix()
	if _, err := cl.CreateBranchAndWait(t.Context, poolID, clusterID, controller.CreateBranchRequest{
		Name: full, ParentBranchID: parentID,
	}); err != nil {
		return "", err
	}
	branches, err := cl.ListAllBranches(t.Context, poolID, clusterID)
	if err != nil {
		return "", err
	}
	for i := len(branches) - 1; i >= 0; i-- {
		if branches[i].Name == full {
			return branches[i].BranchID, nil
		}
	}
	return "", fmt.Errorf("branch %q is absent after a successful create", full)
}

func serveCheck(ctx context.Context, kube *k8s.Kubectl, ids []string, timeout time.Duration) error {
	for _, id := range ids {
		pod, err := endpointPodByID(ctx, kube, id)
		if err != nil {
			return fmt.Errorf("endpoint %s: %w", id, err)
		}
		res, err := kube.ForPod(pod, "endpoint").Run(ctx,
			fmt.Sprintf("timeout %d %s 'select 1'", int(timeout.Seconds()), endpointPsql))
		if err != nil || res.ExitCode != 0 {
			return fmt.Errorf("endpoint %s stopped serving SQL (exit %d, %v): %s",
				id, res.ExitCode, err, strings.TrimSpace(res.Stderr))
		}
	}
	return nil
}

// "exceed" and "limit" are deliberately absent: they also match "context
// deadline exceeded" and "connection limit", which are not scheduling refusals.
var schedulingMarkers = []string{
	"insufficient", "unschedulable", "failedscheduling", "exceeded quota", "quota",
	"no nodes", "nodes are available", "didn't match", "too many pods",
}

// A ladder that ended on a create merely being slow has no refusal to judge.
func refusalVerdict(task controller.Task, err error, took, bound time.Duration) error {
	if err == nil {
		return nil
	}
	msg := ""
	if task.Error != nil {
		msg = *task.Error
	}
	if err != nil {
		msg += " " + err.Error()
	}
	low := strings.ToLower(msg)
	if strings.TrimSpace(msg) == "" {
		return fmt.Errorf("the create that ended the ladder recorded no reason at all (it took %s against the %s bound)", took.Round(time.Second), bound)
	}
	// Before the markers: a helm deploy out of its own 35m clock says "context deadline exceeded", which several of them would otherwise match.
	if strings.Contains(low, "context deadline exceeded") || strings.Contains(low, "timeout") {
		return fmt.Errorf("the create died on a deadline instead of reporting a reason: %q", strings.TrimSpace(msg))
	}
	for _, m := range schedulingMarkers {
		if strings.Contains(low, m) {
			return nil
		}
	}
	return fmt.Errorf("the create failed for a reason that names neither scheduling nor a quota: %q", strings.TrimSpace(msg))
}
