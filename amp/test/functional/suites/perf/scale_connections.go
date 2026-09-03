// Escalation by CONNECTION COUNT on one endpoint. Connections are opened INSIDE
// the pod (psql against localhost): coordinator-side ones would all ride a
// single kubectl port-forward and find its limits instead of the endpoint's.
package perf

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func init() {
	core.Register(core.Test{
		Name:        "perf/scale_connections",
		Suite:       "perf",
		Category:    core.Perf,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:        []string{scaleTier},
		OwningTeam:  core.TeamCompute,
		Description: "Open connections against ONE endpoint batch by batch, each held on a long pg_sleep, until the endpoint refuses or stops answering a fresh probe connection. Reports how many concurrent connections one compute carried, what each cost in memory, and the first refusal verbatim.",
		Params:      []string{"perf.scale_connections"},
		Timeout:     2 * time.Hour,
		Run:         runScaleConnections,
	})
}

type scaleConnParams struct {
	BatchSize int
	// MaxConns is the backstop; the ladder is meant to end on the product.
	MaxConns int
	Budget   time.Duration
	// ProbeTimeout bounds the fresh-connection probe after each batch: an
	// endpoint that accepts held connections but cannot serve a new one has
	// hit the wall a customer sees.
	ProbeTimeout time.Duration
	// HoldSeconds is how long each held connection sleeps server-side; sized
	// past the budget so they never drain mid-ladder.
	HoldSeconds int
	EndpointCPU string
	EndpointMem string
}

func scaleConnParamsFromConfig(c *core.Config) scaleConnParams {
	return scaleConnParams{
		BatchSize:    c.GetInt("perf.scale_connections.batch_size", 50),
		MaxConns:     c.GetInt("perf.scale_connections.max_connections", 5000),
		Budget:       c.GetDuration("perf.scale_connections.budget", 1*time.Hour),
		ProbeTimeout: c.GetDuration("perf.scale_connections.probe_timeout", 30*time.Second),
		HoldSeconds:  c.GetInt("perf.scale_connections.hold_seconds", 7200),
		EndpointCPU:  c.GetString("perf.scale_connections.endpoint_cpu", "2"),
		EndpointMem:  c.GetString("perf.scale_connections.endpoint_memory", "4Gi"),
	}
}

const connLadderApp = "connladder"

func runScaleConnections(t *core.T) error {
	env, ok := t.Env.(ampMultitenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, ok := t.Env.(interface{ AmpKube() *k8s.Kubectl })
	if !ok || kubeEnv.AmpKube() == nil {
		t.Skip("opening connections inside the endpoint pod needs a kube context")
		return nil
	}
	params := scaleConnParamsFromConfig(t.Options)
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
				logf("scale_connections: could not delete cluster %s: %v", tenants[i].ClusterID, err)
			}
		}
	}()

	var pod string
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
		pod, err = endpointPodFor(t, kube, ten)
		if err != nil {
			return err
		}
		s.Set("endpoint", ten.Endpoint.ID)
		s.Set("endpoint_size", params.EndpointCPU+" / "+params.EndpointMem)
		s.Set("pod", pod)
		return nil
	}); err != nil {
		return err
	}
	ex := kube.ForPod(pod, "endpoint")
	psql := "psql -U cloud_admin -h 127.0.0.1 -p 55433 -d postgres -Atc"

	// The server's own ceiling, so the measured wall can be read against what
	// the endpoint was configured to allow.
	maxConns := ""
	if res, err := ex.Run(t.Context, psql+" 'show max_connections'"); err == nil && res.ExitCode == 0 {
		maxConns = strings.TrimSpace(res.Stdout)
	}

	var (
		held      int
		memAt     []core.ChartPoint
		probeAt   []core.ChartPoint
		firstMem  int64
		lastMem   int64
		brokeAt   int
		lastError string
	)
	stopReason := fmt.Sprintf("carried all %d connections without breaking", params.MaxConns)
	deadline := time.Now().Add(params.Budget)

	for batch := 1; held < params.MaxConns; batch++ {
		if batch > 1 && time.Now().After(deadline) {
			stopReason = fmt.Sprintf("%d connections: ran out of the %s ladder budget before anything broke", held, params.Budget)
			break
		}
		stage := fmt.Sprintf("batch_%02d_conns%d", batch, held+params.BatchSize)
		err := t.Step(stage, func(s *core.StepHandle) error {
			spawn := fmt.Sprintf(
				"for i in $(seq 1 %d); do (PGAPPNAME=%s %s 'select pg_sleep(%d)' >/dev/null 2>>/tmp/connladder.err &); done; sleep 3; echo spawned",
				params.BatchSize, connLadderApp, psql, params.HoldSeconds)
			if res, err := ex.Run(t.Context, spawn); err != nil || res.ExitCode != 0 {
				return fmt.Errorf("spawn batch: %v (%s)", err, strings.TrimSpace(res.Stderr))
			}
			// Count the holder PROCESSES, not with a SQL query: asking the
			// server how many clients it has needs a client, and at the wall
			// that connection is refused too. The first run reported the wall
			// at 50 because its own counter lost the race for the last slot,
			// while the real limit was max_connections.
			res, err := ex.Run(t.Context, "pgrep -fc 'pg_sleep' || echo 0")
			if err != nil {
				return fmt.Errorf("count holder processes: %w", err)
			}
			n, _ := strconv.Atoi(strings.TrimSpace(res.Stdout))
			gained := n - held
			held = n
			s.Set("connections_held", held)
			s.Set("gained", gained)

			if mem, err := podMemoryBytes(ex, t); err == nil {
				if firstMem == 0 {
					firstMem = mem
				}
				lastMem = mem
				s.Set("endpoint_memory_mb", mem/(1<<20))
				memAt = append(memAt, core.ChartPoint{X: float64(held), Y: float64(mem) / (1 << 20)})
			}

			// A fresh connection is what a customer opens; time it.
			start := time.Now()
			pres, perr := ex.Run(t.Context, fmt.Sprintf("timeout %d %s 'select 1'",
				int(params.ProbeTimeout.Seconds()), psql))
			probeMS := float64(time.Since(start).Milliseconds())
			s.Set("probe_ms", fmt.Sprintf("%.0f", probeMS))
			probeAt = append(probeAt, core.ChartPoint{X: float64(held), Y: probeMS})

			if gained < params.BatchSize || perr != nil || pres.ExitCode != 0 {
				tail, _ := ex.Run(t.Context, "tail -3 /tmp/connladder.err 2>/dev/null")
				lastError = strings.TrimSpace(tail.Stdout + " " + pres.Stderr)
				s.Set("first_refusal", lastError)
				return fmt.Errorf("wall at %d connections (batch gained %d of %d; fresh probe exit %d): %s",
					held, gained, params.BatchSize, pres.ExitCode, lastError)
			}
			return nil
		})
		if err != nil {
			if held == 0 {
				return fmt.Errorf("not a single connection was established: %w", err)
			}
			stopReason = fmt.Sprintf("%d connections: broke — %v", held, err)
			brokeAt = held
			break
		}
		logf("scale_connections: %d held, endpoint %dMi", held, lastMem/(1<<20))
	}

	return t.Step("report_ceiling", func(s *core.StepHandle) error {
		s.Set("max_connections_held", held)
		s.Set("server_max_connections", maxConns)
		s.Set("broke_at_connections", brokeAt)
		s.Set("stop_reason", stopReason)
		if held > 0 && lastMem > firstMem {
			perConn := (lastMem - firstMem) / int64(held)
			s.Set("memory_per_connection_kb", perConn/(1<<10))
		}
		gated := []core.Datapoint{
			{Name: "scale_connections.max_connections", Value: float64(held), Unit: "conns", Direction: core.HigherIsBetter},
		}
		var info []core.Datapoint
		if n := len(probeAt); n > 0 {
			info = append(info, core.Datapoint{Name: "scale_connections.final_probe_ms",
				Value: probeAt[n-1].Y, Unit: "ms", Direction: core.LowerIsBetter})
		}
		id := identityFromConfig(t.Options, "", frameworkCommit(),
			LoadFields{Script: "scale_connections", Clients: params.BatchSize})
		t.SetPerfIdentity(id.core())
		gateErr := emitAndGateAuto(t, perfFromConfig(t.Options), scaleVersion(t), "perf/scale_connections", gated, info, id, logf)
		t.Chart(core.Chart{
			Title: "scale_connections: endpoint memory vs held connections", Kind: core.ChartLine,
			XLabel: "connections", YLabel: "MiB",
			Series: []core.ChartSeries{{Name: "memory", Points: memAt}},
		})
		t.Chart(core.Chart{
			Title: "scale_connections: fresh-connection latency vs held connections", Kind: core.ChartLine,
			XLabel: "connections", YLabel: "ms",
			Series: []core.ChartSeries{{Name: "probe", Points: probeAt}},
		})
		return gateErr
	})
}

// endpointPodFor names the endpoint's compute pod.
func endpointPodFor(t *core.T, kube *k8s.Kubectl, ten tenant) (string, error) {
	pods, err := kube.ListPods(t.Context)
	if err != nil {
		return "", fmt.Errorf("list pods: %w", err)
	}
	want := "yb-ep-" + ten.Endpoint.ID
	for _, p := range pods {
		if strings.HasPrefix(p, want) {
			return p, nil
		}
	}
	return "", fmt.Errorf("no pod matches endpoint %s", ten.Endpoint.ID)
}

// podMemoryBytes reads the endpoint container's cgroup v2 usage from inside.
func podMemoryBytes(ex *k8s.Kubectl, t *core.T) (int64, error) {
	res, err := ex.Run(t.Context, "cat /sys/fs/cgroup/memory.current 2>/dev/null || cat /sys/fs/cgroup/memory/memory.usage_in_bytes")
	if err != nil || res.ExitCode != 0 {
		return 0, fmt.Errorf("read cgroup memory: %v", err)
	}
	return strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64)
}
