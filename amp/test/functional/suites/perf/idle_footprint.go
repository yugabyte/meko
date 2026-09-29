package perf

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// idleCPUFloorMilli is the smallest CPU change worth a verdict. The safekeeper
// idles near 3 mcores, so +1.5 mcores read as +43% (run b76ac4b9), and one
// 100 Hz tick over a 30 s interval is already 0.33 mcores.
const idleCPUFloorMilli = 2.0

const (
	testNameIdleFootprint = "perf/idle_footprint"
	idlePrefix            = "idle."
	// "2" = CPU and RSS are the median over the hold's intervals, not its mean.
	idleHarnessRev = "2"
)

const (
	DefaultIdleHold        = 10 * time.Minute
	DefaultIdleInterval    = 30 * time.Second
	DefaultIdleSettle      = 1 * time.Minute
	DefaultIdleEndpointCPU = "2"
	DefaultIdleEndpointMem = "4Gi"
)

type idleParams struct {
	Hold        time.Duration
	Interval    time.Duration
	Settle      time.Duration
	EndpointCPU string
	EndpointMem string
}

func idleParamsFromConfig(c *core.Config) idleParams {
	return idleParams{
		Hold:        c.GetDuration("perf.idle_footprint.hold", DefaultIdleHold),
		Interval:    c.GetDuration("perf.idle_footprint.interval", DefaultIdleInterval),
		Settle:      c.GetDuration("perf.idle_footprint.settle", DefaultIdleSettle),
		EndpointCPU: c.GetString("perf.idle_footprint.endpoint_cpu", DefaultIdleEndpointCPU),
		EndpointMem: c.GetString("perf.idle_footprint.endpoint_memory", DefaultIdleEndpointMem),
	}
}

func init() {
	core.Register(core.Test{
		Name:       testNameIdleFootprint,
		Suite:      "perf",
		Category:   core.Perf,
		Env:        core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:       []string{"perf"},
		OwningTeam: core.TeamCompute,
		Description: "Idle cost of one endpoint (meko#461): create it, drop every connection, leave it untouched for perf.idle_footprint.hold and sample /proc inside its container every perf.idle_footprint.interval. " +
			"Reports gated idle.cpu_mcores@<process> and idle.rss_mib@<process> datapoints for postgres, compute-ctl, the supervisor, the pageserver and the safekeeper, plus the CPU total. Everything else in the container lands in an ungated 'other' " +
			"group, which the sweep's own shell is part of, and nothing else samples the pod during the hold, so the gate never measures the harness. The per-process RSS sum is ungated too: /proc counts a shared page in every process mapping it.",
		Params:  []string{"perf.idle_footprint", "perf.gate", "apps.metrics"},
		Timeout: 2 * time.Hour,
		Run:     runIdleFootprint,
	})
}

func runIdleFootprint(t *core.T) error {
	env, ok := t.Env.(ampMultitenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, ok := t.Env.(interface{ AmpKube() *k8s.Kubectl })
	if !ok || kubeEnv.AmpKube() == nil {
		t.Skip("the per-process split is read from /proc inside the endpoint container, which needs a kube context")
		return nil
	}
	kube := kubeEnv.AmpKube()
	params := idleParamsFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)
	cl, err := env.AmpController(t.Context)
	if err != nil {
		return err
	}
	id := identityFromConfig(t.Options, "", frameworkCommit(), loadFieldsIdle(params))
	t.SetPerfIdentity(id.core())

	poolID := env.AmpPoolID()
	td := &teardownStack{}
	var ten tenant
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		if ten.Conn != nil {
			ten.Conn.Close()
		}
		_ = td.run(ctx, logf)
		if ten.ClusterID != "" {
			if _, err := cl.DeleteClusterAndWait(ctx, poolID, ten.ClusterID); err != nil {
				logf("idle_footprint: could not delete cluster %s: %v", ten.ClusterID, err)
			}
		}
	}()

	if err := t.Step("create_tenant", func(s *core.StepHandle) error {
		got, _, err := createTenant(t, cl, env, poolID, 0, multitenantParams{
			EndpointCPU: params.EndpointCPU, EndpointMemory: params.EndpointMem})
		if err != nil {
			return fmt.Errorf("%w%s", err, schedulingHint(t, logf))
		}
		ten = got
		s.Set("cluster", ten.ClusterID)
		s.Set("endpoint", ten.Endpoint.ID)
		ten.Conn.Close()
		ten.Conn = nil
		return nil
	}); err != nil {
		return err
	}

	var footprint []procUsage
	if err := t.Step("hold_idle", func(s *core.StepHandle) error {
		s.Set("hold", params.Hold.String())
		s.Set("interval", params.Interval.String())
		if err := sleepCtx(t.Context, params.Settle); err != nil {
			return err
		}
		pod, err := ampstack.EndpointPodName(t.Context, kube, ten.Endpoint.ID)
		if err != nil {
			return err
		}
		s.Set("pod", pod)
		got, err := sampleIdleProcesses(t.Context, kube.ForPod(pod, chainEndpointContainer),
			params.Hold, params.Interval, logf)
		footprint = got
		s.Set("processes", len(footprint))
		return err
	}); err != nil {
		return err
	}

	return t.Step("gate_idle_footprint", func(s *core.StepHandle) error {
		gated, info := idleDatapoints(footprint)
		s.Set("datapoints", len(gated))
		t.AttachBytes("idle-footprint.txt", "log", []byte(formatIdle(footprint)))
		t.Chart(idleChart(footprint))
		return emitAndGateAuto(t, perfFromConfig(t.Options), scaleVersion(t), testNameIdleFootprint, gated, info, id, logf)
	})
}

func idleDatapoints(usage []procUsage) (gated, info []core.Datapoint) {
	var rss float64
	var total []float64
	var named int
	for _, u := range usage {
		cpuDP := core.Datapoint{Name: idlePrefix + "cpu_mcores@" + u.Name, Value: u.CPUMilli, Unit: "mcores", Direction: core.LowerIsBetter,
			MinDelta: idleCPUFloorMilli, N: len(u.CPURates), Samples: u.CPURates}
		rssDP := core.Datapoint{Name: idlePrefix + "rss_mib@" + u.Name, Value: u.RSSMiB, Unit: "MiB", Direction: core.LowerIsBetter,
			N: len(u.RSSRuns), Samples: u.RSSRuns}
		info = append(info, core.Datapoint{Name: idlePrefix + "procs@" + u.Name, Value: float64(u.Procs), Unit: "procs", Direction: core.LowerIsBetter})
		if u.Name == otherProcGroup {
			info = append(info, cpuDP, rssDP)
			continue
		}
		named++
		for i, r := range u.CPURates {
			if i == len(total) {
				total = append(total, 0)
			}
			total[i] += r
		}
		rss += u.RSSMiB
		gated = append(gated, cpuDP, rssDP)
	}
	if named == 0 {
		return gated, info
	}
	gated = append(gated, core.Datapoint{Name: idlePrefix + "cpu_mcores_total", Value: median(total), Unit: "mcores", Direction: core.LowerIsBetter,
		MinDelta: idleCPUFloorMilli, N: len(total), Samples: total})
	info = append(info, core.Datapoint{Name: idlePrefix + "rss_mib_sum", Value: rss, Unit: "MiB", Direction: core.LowerIsBetter})
	return gated, info
}

func formatIdle(usage []procUsage) string {
	var b []byte
	b = fmt.Appendf(b, "%-14s %12s %12s %8s\n", "process", "cpu (mcores)", "rss (MiB)", "procs")
	for _, u := range usage {
		b = fmt.Appendf(b, "%-14s %12.1f %12.1f %8d\n", u.Name, u.CPUMilli, u.RSSMiB, u.Procs)
	}
	return string(b)
}

func idleChart(usage []procUsage) core.Chart {
	series := make([]core.ChartSeries, 0, len(usage))
	for i, u := range usage {
		series = append(series, core.ChartSeries{Name: u.Name, Points: []core.ChartPoint{{X: float64(i + 1), Y: u.RSSMiB}}})
	}
	return core.Chart{Title: "Idle RSS by process", Kind: core.ChartBar, XLabel: "process", YLabel: "RSS (MiB)", Series: series}
}

func loadFieldsIdle(p idleParams) LoadFields {
	return LoadFields{
		Script:        fmt.Sprintf("idle_footprint@%s/%s", p.EndpointCPU, p.EndpointMem),
		MeasurementMS: p.Hold.Milliseconds(),
		Harness:       idleHarnessRev,
	}
}

func sortUsage(usage []procUsage) {
	sort.Slice(usage, func(i, j int) bool { return usage[i].Name < usage[j].Name })
}
