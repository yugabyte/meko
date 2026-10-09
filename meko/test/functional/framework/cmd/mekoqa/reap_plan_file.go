package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/spf13/cobra"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/reap"
)

// A run's reap plan on disk, so a process that shares no memory with it, a
// Jenkins post block after an aborted build, can still reclaim what it stood
// up. The runner tears its own environment down and removes the file; the file
// surviving IS the signal that it never got that far.

var reapPlanMu sync.Mutex

// planProvider is the provider surface that can describe its own teardown.
type planProvider interface{ ReapPlan() *reap.Plan }

// writeReapPlan records the provider's plan at path, and removes the file when
// nothing is live, so a stack an earlier attempt already tore down is not left
// named in it. Best-effort: failing to write it must not fail a run that is
// otherwise fine. Parallel tests call it, hence the lock and the rename.
func writeReapPlan(path string, provider any, logf func(string, ...any)) {
	pp, ok := provider.(planProvider)
	if !ok || path == "" {
		return
	}
	reapPlanMu.Lock()
	defer reapPlanMu.Unlock()
	plan := pp.ReapPlan()
	if plan.Empty() {
		dropReapPlan(path)
		return
	}
	b, err := json.Marshal(plan)
	if err != nil {
		logf("reap plan not recorded: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		logf("reap plan not recorded: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		logf("reap plan not recorded: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		logf("reap plan not recorded: %v", err)
		return
	}
	logf("reap plan recorded at %s", path)
}

// dropReapPlan removes the file after a teardown that succeeded.
// reapPlans keeps one run's plan file across provider rebuilds. A provider
// closed after a failed bring-up can leave something behind, and that stays in
// the file beside the live provider's plan.
type reapPlans struct {
	path string
	logf func(string, ...any)

	mu   sync.Mutex
	cur  core.EnvProvider
	left *reap.Plan
}

func (r *reapPlans) wire(p core.EnvProvider) {
	r.mu.Lock()
	if old, ok := r.cur.(planProvider); ok && r.cur != p {
		r.left = mergePlans(r.left, old.ReapPlan())
	}
	r.cur = p
	r.mu.Unlock()
	r.write()
	if sp, ok := p.(interface{ OnReapPlanChange(func()) }); ok {
		sp.OnReapPlanChange(r.write)
	}
}

func (r *reapPlans) write() {
	r.mu.Lock()
	p, left := r.cur, r.left
	r.mu.Unlock()
	if left.Empty() {
		writeReapPlan(r.path, p, r.logf)
		return
	}
	writeReapPlan(r.path, planFunc(func() *reap.Plan {
		var live *reap.Plan
		if pp, ok := p.(planProvider); ok {
			live = pp.ReapPlan()
		}
		return mergePlans(left, live)
	}), r.logf)
}

type planFunc func() *reap.Plan

func (f planFunc) ReapPlan() *reap.Plan { return f() }

func mergePlans(plans ...*reap.Plan) *reap.Plan {
	out := &reap.Plan{}
	seen := map[[3]string]bool{}
	for _, p := range plans {
		if p == nil {
			continue
		}
		out.AddCloud(p.Cloud)
		for i := range p.Clouds {
			out.AddCloud(&p.Clouds[i])
		}
		for _, ns := range p.Namespaces {
			key := [3]string{ns.Kind, ns.KubeContext, ns.Namespace}
			if !seen[key] {
				seen[key] = true
				out.Namespaces = append(out.Namespaces, ns)
			}
		}
	}
	return out
}

func dropReapPlan(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

// loadReapPlanFile decodes a plan a killed run left behind.
func loadReapPlanFile(path string) (*reap.Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read reap plan %s: %w", path, err)
	}
	return reap.FromJSON(string(b))
}

// reapPlanFile executes a plan a killed run left behind and removes the file
// once everything it named is gone. A partial reap keeps the file: a leak must
// stay visible to the next attempt.
func reapPlanFile(c *cobra.Command, path string, configFiles, jsonConfigs, overrides []string, noDefaults bool) error {
	plan, err := loadReapPlanFile(path)
	if err != nil {
		return &exitError{code: 2, err: err}
	}
	if plan.Empty() {
		fmt.Fprintf(c.OutOrStdout(), "reap plan %s names nothing\n", path)
		return nil
	}
	loadConfig := core.LoadConfig
	if noDefaults {
		loadConfig = core.LoadConfigNoDefaults
	}
	cfg, err := loadConfig(configFiles, jsonConfigs, overrides)
	if err != nil {
		return &exitError{code: 2, err: err}
	}
	secrets, err := core.NewSecrets(cfg)
	if err != nil {
		return &exitError{code: 2, err: err}
	}
	if err := reap.Execute(c.Context(), plan, cfg, secrets.Value, true); err != nil {
		return &exitError{code: 1, err: fmt.Errorf("reap %s: %w", path, err)}
	}
	dropReapPlan(path)
	fmt.Fprintf(c.OutOrStdout(), "reaped everything %s named\n", path)
	return nil
}
