package perf_test

import (
	"context"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/fake"
	_ "github.com/yugabyte/meko/meko/test/functional/suites/perf"
)

var tests = []string{"perf/api_latency", "perf/mcp_latency", "perf/recall_latency", "perf/write_path_latency",
	"perf/kb_ingest_throughput", "perf/ui_read_latency", "perf/chat_latency"}

func TestPerfTestsAreDedicatedAndTagged(t *testing.T) {
	for _, name := range tests {
		tc, ok := core.Default().Get(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if tc.Env.Shape != core.DedicatedStack || tc.Env.ClusterSlots < 1 || tc.Env.Sharing != core.Exclusive {
			t.Errorf("%s env %+v", name, tc.Env)
		}
		if tc.Category != core.Perf || len(tc.Tags) != 1 || tc.Tags[0] != "perf" {
			t.Errorf("%s category %v tags %v", name, tc.Category, tc.Tags)
		}
		if len(tc.Tolerates) != 0 {
			t.Errorf("%s tolerates %v", name, tc.Tolerates)
		}
	}
}

func TestDefaultsCarryEveryKey(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"perf.client_pods", "perf.credentials", "perf.forwarded_for", "perf.warmup", "perf.step_duration", "perf.min_samples", "perf.max_error_rate",
		"perf.api_latency.concurrency", "perf.api_latency.max_active_pats", "perf.mcp_latency.concurrency", "perf.mcp_latency.credentials",
		"perf.server_metrics.enabled", "perf.server_metrics.sample_interval", "perf.server_metrics.min_count",
		"perf.recall_latency.volumes", "perf.recall_latency.users", "perf.write_path_latency.llm_chat_latency", "perf.write_path_latency.freshness_timeout",
		"perf.kb_ingest_throughput.step_duration", "perf.ui_read_latency.memories", "perf.chat_latency.concurrency",
	} {
		if !cfg.Has(k) {
			t.Errorf("default.yaml has no %s", k)
		}
	}
	if got := cfg.GetStringSlice("perf.api_latency.concurrency", nil); len(got) != 3 || got[0] != "1" || got[2] != "32" {
		t.Errorf("api ladder %v", got)
	}
	if need := cfg.GetInt("perf.credentials", 0) + cfg.GetInt("perf.client_pods", 0)*cfg.GetInt("perf.api_latency.max_active_pats", 0); need > 8 {
		t.Errorf("api_latency needs %d active PATs", need)
	}
}

func TestWithoutAStackEveryTestSkips(t *testing.T) {
	var picked []core.Test
	for _, name := range tests {
		tc, _ := core.Default().Get(name)
		picked = append(picked, tc)
	}
	cfg, _ := core.LoadConfig(nil, nil, []string{"versions.product=1.0.0"})
	_, results := core.RunTests(context.Background(), picked, core.RunOptions{
		Parallel: 1, Seed: core.NewRunSeed(1, true), Config: cfg, Provider: &fake.Provider{},
		DetectedVersions: core.DetectedVersions{Product: "1.0.0"},
	})
	for _, r := range results {
		if r.Status != core.StatusSkipped {
			t.Errorf("%s = %s (%s)", r.TestName, r.Status, r.ErrorSummary)
		}
	}
}
