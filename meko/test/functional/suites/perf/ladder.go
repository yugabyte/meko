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
var dedicated = core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack, ClusterSlots: 1}

var paramPrefixes = []string{"perf", "apps.mekoload", "env.k8s", "env.mekostack", "timeouts"}

// ladderSpec is a latency test: the same closed-loop workload at each
// concurrency of a ladder.
type ladderSpec struct {
	// name is the datapoint prefix, api or mcp.
	name     string
	key      string // config prefix, perf.<key>
	workload string
	plan     loadtest.Plan
	levels   []int
	// config is the workload's own knobs, the same at every level.
	config func() map[string]any
}

// Datapoint names carry the level and the op: <name>.c<users>.<op>.<stat>.
func dpName(prefix string, users int, op, stat string) string {
	return fmt.Sprintf("%s.c%d.%s.%s", prefix, users, op, stat)
}

func runLadder(t *core.T, s ladderSpec) error {
	o := t.Options
	step := o.GetDuration("perf.step_duration", time.Minute)
	warmup := o.GetDuration("perf.warmup", 30*time.Second)
	minSamples := int64(o.GetInt("perf.min_samples", 20))
	maxErr := o.GetFloat("perf.max_error_rate", 0.01)
	drain := o.GetDuration("perf.drain_timeout", 10*time.Minute)
	forwardedFor := o.GetString("perf.forwarded_for", "per_request")
	if len(s.levels) == 0 {
		return fmt.Errorf("perf.%s.concurrency is empty", s.key)
	}

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

	// Run once, unmeasured: the first calls pay for connections and cold caches.
	if warmup > 0 {
		if err := t.Step("warmup", func(h *core.StepHandle) error {
			_, err := runLevel(t, rig, s, forwardedFor, minInt(s.levels), warmup, drain, "")
			return err
		}); err != nil {
			return err
		}
	}

	var errs []error
	for _, users := range s.levels {
		name := fmt.Sprintf("users_%d", users)
		if err := t.Step(name, func(h *core.StepHandle) error {
			agg, err := runLevel(t, rig, s, forwardedFor, users, step, drain, name)
			if err != nil {
				return err
			}
			h.Set("calls", agg.Total.Count)
			h.Set("throughput", fmt.Sprintf("%.1f ops/s", agg.Total.Throughput))
			h.Set("error_rate", fmt.Sprintf("%.3f%%", agg.Total.ErrorRate*100))
			problems := loadtest.Judge(agg, loadtest.Bounds{MaxErrorRate: maxErr, Max5xx: 0})
			if len(problems) > 0 {
				return fmt.Errorf("a latency measured over failing calls says nothing about the product, so none is recorded: %s", strings.Join(problems, "\n"))
			}
			emit(t, s.name, users, agg, minSamples)
			return rig.UniverseCounter(t)
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// runLevel runs the workload at users virtual users, closed loop, for d.
func runLevel(t *core.T, rig *loadtest.Rig, s ladderSpec, forwardedFor string, users int, d, drain time.Duration, label string) (mekoload.Aggregate, error) {
	share := loadtest.Split(users, rig.Plan.Pods)
	load, err := rig.Launch(s.workload, forwardedFor, func(i int) map[string]any {
		c := s.config()
		c["concurrency"] = share[i]
		c["duration"] = d.String()
		c["rate_limit_ops"] = 0
		c["cleanup"] = true
		return c
	})
	if err != nil {
		return mekoload.Aggregate{}, err
	}
	defer func() {
		ctx, cancel := stacktest.CleanupCtx()
		defer cancel()
		_ = load.Stop(ctx)
		if label != "" {
			_ = load.Collect(ctx, loadtest.Sink(t))
		}
	}()
	agg, err := load.Wait(t, d+drain, 5*time.Second, loadtest.Progress(rig.Logf, fmt.Sprintf("%s %d users", s.workload, users), 20*time.Second))
	if err != nil {
		return agg, err
	}
	if label != "" {
		loadtest.Report(t, label, agg)
	}
	return agg, nil
}

// emit records the level's datapoints. An op with too few successful calls has
// no stable percentile, and records none.
func emit(t *core.T, prefix string, users int, agg mekoload.Aggregate, minSamples int64) {
	t.Datapoint(core.Datapoint{Name: dpName(prefix, users, "total", "throughput_ops"), Value: agg.Total.OKThroughput, Unit: "ops/s", Direction: core.HigherIsBetter, N: 1})
	t.Datapoint(core.Datapoint{Name: dpName(prefix, users, "total", "error_rate_pct"), Value: agg.Total.ErrorRate * 100, Unit: "%", Direction: core.LowerIsBetter, N: 1})
	for _, op := range agg.OpNames() {
		o := agg.Ops[op]
		if o.OK < minSamples {
			continue
		}
		for _, p := range []struct {
			stat string
			v    float64
		}{{"p50_ms", o.Latency.P50}, {"p95_ms", o.Latency.P95}, {"p99_ms", o.Latency.P99}} {
			t.Datapoint(core.Datapoint{Name: dpName(prefix, users, op, p.stat), Value: p.v, Unit: "ms", Direction: core.LowerIsBetter, N: int(min(o.OK, 1<<30))})
		}
		t.Datapoint(core.Datapoint{Name: dpName(prefix, users, op, "throughput_ops"), Value: o.OKThroughput, Unit: "ops/s", Direction: core.HigherIsBetter, N: 1})
	}
}

// loadSignature hashes everything that shapes the load, so two results are
// comparable exactly when it matches.
func loadSignature(s ladderSpec, step time.Duration, forwardedFor string, pods int) (string, error) {
	b, err := json.Marshal(map[string]any{
		"workload": s.workload, "levels": s.levels, "step": step.String(), "pods": pods,
		"forwarded_for": forwardedFor, "config": s.config(),
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]), nil
}

func minInt(v []int) int {
	m := v[0]
	for _, x := range v {
		m = min(m, x)
	}
	return m
}
