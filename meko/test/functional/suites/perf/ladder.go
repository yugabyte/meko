// Package perf holds the tests that measure a dedicated stack under a fixed,
// closed-loop load and record the numbers as datapoints a baseline gate can compare.
package perf

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoload"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/loadtest"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

// dedicated is a stack of its own, so the numbers are the product's and no
// other test's, and a slot of the cluster's capacity for it.
var dedicated = core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack, ClusterSlots: 1, BackingConfig: loadtest.StackConfig}

var paramPrefixes = []string{"perf", "apps.mekoload", "env.k8s", "env.mekostack", "timeouts"}

// ladderSpec is a latency test: the same closed-loop workload at each level of
// a ladder. A level is a concurrency, or with steps a whole config change such
// as the amount of seeded data.
type ladderSpec struct {
	// name is the datapoint prefix, api or mcp.
	name     string
	key      string // config prefix, perf.<key>
	workload string
	plan     loadtest.Plan
	levels   []int
	// steps replaces levels when set.
	steps []step
	// config is the workload's own knobs, the same at every level.
	config func() map[string]any
}

// step is one level: its label in datapoint names, its virtual users, and the
// config it changes.
type step struct {
	label string
	users int
	extra map[string]any
}

func (s ladderSpec) ladder() []step {
	if len(s.steps) > 0 {
		return s.steps
	}
	out := make([]step, len(s.levels))
	for i, u := range s.levels {
		out[i] = step{label: fmt.Sprintf("c%d", u), users: u}
	}
	return out
}

// Datapoint names carry the level and the op: <name>.<level>.<op>.<stat>.
func dpName(prefix, level, op, stat string) string {
	return fmt.Sprintf("%s.%s.%s.%s", prefix, level, op, stat)
}

func runLadder(t *core.T, s ladderSpec) error {
	o := t.Options
	step := o.GetDuration("perf."+s.key+".step_duration", o.GetDuration("perf.step_duration", time.Minute))
	warmup := o.GetDuration("perf.warmup", 30*time.Second)
	minSamples := int64(o.GetInt("perf.min_samples", 20))
	maxErr := o.GetFloat("perf.max_error_rate", 0.01)
	drain := o.GetDuration("perf.drain_timeout", 10*time.Minute)
	forwardedFor := o.GetString("perf.forwarded_for", "per_request")
	ladder := s.ladder()
	if len(ladder) == 0 {
		return fmt.Errorf("perf.%s has no levels", s.key)
	}
	minCount := int64(o.GetInt("perf.server_metrics.min_count", 20))

	rig, err := loadtest.Start(t, s.plan)
	if err != nil {
		return err
	}
	defer rig.Close()

	sig, err := loadSignature(s, step, forwardedFor, rig.Plan.Pods)
	if err != nil {
		return err
	}
	product, database := rig.Stack.Versions(t)
	t.SetPerfIdentity(core.PerfIdentity{
		YBImage:       database,
		Images:        map[string]string{"product": product},
		LoadSignature: sig,
	})

	probe := loadtest.NewProbe(rig)

	// Run once, unmeasured: the first calls pay for connections and cold caches.
	if warmup > 0 {
		if err := t.Step("warmup", func(h *core.StepHandle) error {
			first := ladder[0]
			first.users = minUsers(ladder)
			_, _, err := runLevel(t, rig, s, forwardedFor, first, warmup, drain, "", nil)
			return err
		}); err != nil {
			return err
		}
	}

	var errs []error
	for _, lv := range ladder {
		name := "level_" + lv.label
		if err := t.Step(name, func(h *core.StepHandle) error {
			agg, srv, err := runLevel(t, rig, s, forwardedFor, lv, step, drain, name, probe)
			if err != nil {
				return err
			}
			h.Set("users", lv.users)
			h.Set("calls", agg.Total.Count)
			h.Set("throughput", fmt.Sprintf("%.1f ops/s", agg.Total.Throughput))
			h.Set("error_rate", fmt.Sprintf("%.3f%%", agg.Total.ErrorRate*100))
			problems := loadtest.Judge(agg, loadtest.Bounds{MaxErrorRate: maxErr, Max5xx: 0})
			if len(problems) > 0 {
				return fmt.Errorf("a latency measured over failing calls says nothing about the product, so none is recorded: %s", strings.Join(problems, "\n"))
			}
			emit(t, s.name, lv.label, agg, minSamples)
			srv.Emit(t, s.name+"."+lv.label, measuredOK(agg), minCount)
			return rig.UniverseCounter(t)
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// measuredOK is the successful calls of the load itself, setup excluded.
func measuredOK(agg mekoload.Aggregate) int64 {
	var n int64
	for op, o := range agg.Ops {
		if !strings.HasPrefix(op, "setup_") {
			n += o.OK
		}
	}
	return n
}

func minUsers(ladder []step) int {
	m := ladder[0].users
	for _, l := range ladder {
		m = min(m, l.users)
	}
	return m
}

// runLevel runs the workload at the level's virtual users, closed loop, for d.
// With a probe, the servers are measured from the moment every pod is past its
// setup until the load ends.
func runLevel(t *core.T, rig *loadtest.Rig, s ladderSpec, forwardedFor string, lv step, d, drain time.Duration, label string, probe *loadtest.Probe) (mekoload.Aggregate, loadtest.ServerReport, error) {
	share := loadtest.Split(lv.users, rig.Plan.Pods)
	load, err := rig.Launch(s.workload, forwardedFor, func(i int) map[string]any {
		c := s.config()
		for k, v := range lv.extra {
			c[k] = v
		}
		c["concurrency"] = share[i]
		c["duration"] = d.String()
		c["rate_limit_ops"] = 0
		c["cleanup"] = true
		return c
	})
	if err != nil {
		return mekoload.Aggregate{}, loadtest.ServerReport{}, err
	}
	defer func() {
		ctx, cancel := stacktest.CleanupCtx()
		defer cancel()
		_ = load.Stop(ctx)
		if label != "" {
			_ = load.Collect(ctx, loadtest.Sink(t))
		}
	}()
	progress := loadtest.Progress(rig.Logf, fmt.Sprintf("%s %s %d users", s.workload, lv.label, lv.users), 20*time.Second)
	var win *loadtest.Window
	agg, err := load.Wait(t, d+drain+setupAllowance(lv), 5*time.Second, func(a mekoload.Aggregate) {
		progress(a)
		if probe != nil && win == nil && a.States["running"] == len(rig.Fleet.Pods()) {
			win = probe.Begin(t)
		}
	})
	srv := win.End(t)
	if err != nil {
		return agg, srv, err
	}
	if label != "" {
		loadtest.Report(t, label, agg)
		for op, o := range agg.Ops {
			if strings.HasPrefix(op, "setup_") && o.Count > 0 {
				t.Metric(fmt.Sprintf("%s.%s.p50_ms", label, op), o.Latency.P50, "ms")
				t.Metric(fmt.Sprintf("%s.%s.calls", label, op), float64(o.Count), "calls")
			}
		}
	}
	return agg, srv, nil
}

// setupAllowance is how long a level's seeding may take on top of its measured
// duration: one minute plus a second for every hundred seeded items.
func setupAllowance(lv step) time.Duration {
	n := 0
	for _, k := range []string{"seed_memories", "seed_docs", "seed_conversations"} {
		if v, ok := lv.extra[k].(int); ok {
			n += v
		}
	}
	return time.Minute + time.Duration(n/100)*time.Second
}

// emit records the level's datapoints. An op with too few successful calls has
// no stable percentile, and records none.
func emit(t *core.T, prefix, level string, agg mekoload.Aggregate, minSamples int64) {
	t.Datapoint(core.Datapoint{Name: dpName(prefix, level, "total", "throughput_ops"), Value: agg.Total.OKThroughput, Unit: "ops/s", Direction: core.HigherIsBetter, N: 1})
	t.Datapoint(core.Datapoint{Name: dpName(prefix, level, "total", "error_rate_pct"), Value: agg.Total.ErrorRate * 100, Unit: "%", Direction: core.LowerIsBetter, N: 1})
	for _, op := range agg.OpNames() {
		o := agg.Ops[op]
		if o.OK < minSamples || strings.HasPrefix(op, "setup_") {
			continue
		}
		for _, p := range []struct {
			stat string
			v    float64
		}{{"p50_ms", o.Latency.P50}, {"p95_ms", o.Latency.P95}, {"p99_ms", o.Latency.P99}} {
			t.Datapoint(core.Datapoint{Name: dpName(prefix, level, op, p.stat), Value: p.v, Unit: "ms", Direction: core.LowerIsBetter, N: int(min(o.OK, 1<<30))})
		}
		t.Datapoint(core.Datapoint{Name: dpName(prefix, level, op, "throughput_ops"), Value: o.OKThroughput, Unit: "ops/s", Direction: core.HigherIsBetter, N: 1})
	}
}

// loadSignature hashes everything that shapes the load, so two results are
// comparable exactly when it matches.
func loadSignature(s ladderSpec, step time.Duration, forwardedFor string, pods int) (string, error) {
	b, err := json.Marshal(map[string]any{
		"workload": s.workload, "levels": s.levels, "steps": stepSignature(s.steps), "step": step.String(), "pods": pods,
		"forwarded_for": forwardedFor, "config": s.config(),
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]), nil
}

func stepSignature(steps []step) []map[string]any {
	out := make([]map[string]any, len(steps))
	for i, st := range steps {
		out[i] = map[string]any{"label": st.label, "users": st.users, "extra": st.extra}
	}
	return out
}
