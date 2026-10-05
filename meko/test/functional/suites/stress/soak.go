// Package stress holds the tests that load a dedicated stack from client pods
// inside the cluster for minutes and then check what it did to the stack.
package stress

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoload"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/loadtest"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

// dedicated is the environment every stress test asks for: a whole stack that
// no other test shares, and a slot of the cluster's capacity for it.
var dedicated = core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack, ClusterSlots: 1}

var paramPrefixes = []string{"stress", "apps.mekoload", "env.k8s", "env.mekostack", "timeouts"}

// soakSpec is one load test: the plan of its client pods, the workload they
// run and what the stack may do while they run it.
type soakSpec struct {
	key      string // config prefix, stress.<key>
	workload string
	plan     loadtest.Plan
	// config builds the meko-load config of pod i.
	config func(i int, pods int) map[string]any
	bounds loadtest.Bounds
	// during runs beside the load, after it has started. It returns what it
	// observed to judge once the load ends.
	during func(rig *loadtest.Rig, load *mekoload.Load, started time.Time, duration time.Duration) (judge func(mekoload.Aggregate) []string, err error)
}

func cfgDuration(t *core.T, key string, def time.Duration) time.Duration {
	return t.Options.GetDuration(key, def)
}

// runSoak is the shape of every stress test: start the clients, run the load
// for its duration, then judge the load itself, the datapacks it left, and
// the cleanup it does.
func runSoak(t *core.T, s soakSpec) error {
	duration := cfgDuration(t, "stress."+s.key+".duration", 15*time.Minute)
	drain := cfgDuration(t, "stress.drain_timeout", 10*time.Minute)

	rig, err := loadtest.Start(t, s.plan)
	if err != nil {
		return err
	}
	defer rig.Close()

	forwardedFor := t.Options.GetString("stress.forwarded_for", "per_request")
	var load *mekoload.Load
	started := time.Now()
	if err := t.Step("start_load", func(h *core.StepHandle) error {
		var err error
		load, err = rig.Launch(s.workload, forwardedFor, func(i int) map[string]any {
			c := s.config(i, s.plan.Pods)
			c["duration"] = duration.String()
			// The cleanup runs after the checks that need the datapacks alive.
			c["cleanup"] = false
			return c
		})
		if err != nil {
			return err
		}
		started = time.Now()
		h.Set("workload", s.workload)
		h.Set("duration", duration.String())
		h.Set("pods", s.plan.Pods)
		return nil
	}); err != nil {
		return err
	}
	defer func() {
		ctx, cancel := stacktest.CleanupCtx()
		defer cancel()
		_ = load.Stop(ctx)
		_ = load.Collect(ctx, loadtest.Sink(t))
	}()

	var judgeDuring func(mekoload.Aggregate) []string
	var duringErr error
	if s.during != nil {
		if err := t.Step("during_load", func(h *core.StepHandle) error {
			var err error
			judgeDuring, err = s.during(rig, load, started, duration)
			return err
		}); err != nil {
			duringErr = err
		}
	}

	var agg mekoload.Aggregate
	if err := t.Step("load", func(h *core.StepHandle) error {
		var err error
		agg, err = load.Wait(t, time.Until(started.Add(duration))+drain, 10*time.Second,
			loadtest.Progress(rig.Logf, s.workload, 30*time.Second))
		if err != nil {
			return err
		}
		h.Set("calls", agg.Total.Count)
		h.Set("error_rate", fmt.Sprintf("%.3f%%", agg.Total.ErrorRate*100))
		h.Set("http_5xx", agg.Http5xx)
		h.Set("elapsed", fmt.Sprintf("%.0fs", agg.Elapsed))
		loadtest.Report(t, "load", agg)
		return nil
	}); err != nil {
		return errors.Join(duringErr, err)
	}

	errs := []error{duringErr}
	if err := t.Step("judge_load", func(h *core.StepHandle) error {
		problems := loadtest.Judge(agg, s.bounds)
		if judgeDuring != nil {
			problems = append(problems, judgeDuring(agg)...)
		}
		if len(problems) == 0 {
			return nil
		}
		return errors.New(strings.Join(problems, "\n"))
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("datapacks_survive_the_load", func(*core.StepHandle) error {
		return rig.Survivors(t, load)
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("universe_counter_matches_datapacks", func(*core.StepHandle) error {
		return rig.UniverseCounter(t)
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("cleanup", func(h *core.StepHandle) error {
		res, err := load.Cleanup(t)
		if err != nil {
			return err
		}
		var problems []string
		for i, c := range res {
			h.Set(fmt.Sprintf("pod_%d", i), fmt.Sprintf("deleted %d, swept %d, leaked %d, errors %d", c.DatapacksDeleted, c.DatapacksSwept, len(c.LeakedDatapacks), len(c.Errors)))
			if !c.Clean() {
				problems = append(problems, fmt.Sprintf("pod %d: leaked datapacks %v, PATs %v, errors %v", i, c.LeakedDatapacks, c.LeakedPATs, c.Errors))
			}
		}
		if len(problems) > 0 {
			return errors.New(strings.Join(problems, "; "))
		}
		return rig.NothingLeft(t)
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("universe_counter_after_cleanup", func(*core.StepHandle) error {
		return rig.UniverseCounter(t)
	}); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
