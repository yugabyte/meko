package stress_test

import (
	"context"
	"testing"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/fake"
	_ "github.com/yugabyte/meko/meko/test/functional/suites/stress"
)

var tests = []string{"stress/datapack_churn_soak", "stress/mcp_conversations_soak", "stress/churn_with_api_pod_kills",
	"stress/recall_under_mcp_pod_kills", "stress/kb_ingest_with_worker_kills", "stress/dependency_faults_under_load"}

func TestStressTestsAreDedicatedAndTagged(t *testing.T) {
	for _, name := range tests {
		tc, ok := core.Default().Get(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if tc.Env.Shape != core.DedicatedStack || tc.Env.ClusterSlots < 1 || tc.Env.Sharing != core.Exclusive {
			t.Errorf("%s env %+v: a load test needs a stack of its own and a cluster slot", name, tc.Env)
		}
		if tc.Category != core.Stress || len(tc.Tags) != 1 || tc.Tags[0] != "stress" {
			t.Errorf("%s category %v tags %v", name, tc.Category, tc.Tags)
		}
		if tc.Description == "" || tc.OwningTeam == "" {
			t.Errorf("%s has no description or owner", name)
		}
	}
	kills, _ := core.Default().Get("stress/churn_with_api_pod_kills")
	want := map[string]bool{core.GuardPodRestart: true, core.GuardHTTP5xx: true}
	for _, k := range kills.Tolerates {
		delete(want, k)
	}
	if len(want) != 0 {
		t.Errorf("the pod-kill test must tolerate %v", want)
	}
	for _, name := range tests[:2] {
		tc, _ := core.Default().Get(name)
		if len(tc.Tolerates) != 0 {
			t.Errorf("%s tolerates %v, a soak without kills tolerates nothing", name, tc.Tolerates)
		}
	}
}

func TestDefaultsCarryEveryKeyAndFitThePATCap(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"stress.client_pods", "stress.credentials", "stress.forwarded_for", "stress.max_error_rate", "stress.http_5xx_allowed", "stress.drain_timeout",
		"stress.datapack_churn_soak.duration", "stress.datapack_churn_soak.concurrency", "stress.datapack_churn_soak.max_active_pats",
		"stress.mcp_conversations_soak.duration", "stress.mcp_conversations_soak.credentials", "stress.mcp_conversations_soak.messages",
		"stress.churn_with_api_pod_kills.duration", "stress.churn_with_api_pod_kills.kill_interval", "stress.churn_with_api_pod_kills.recover_within",
		"stress.churn_with_api_pod_kills.first_kill_after", "stress.churn_with_api_pod_kills.recovery_error_rate",
		"stress.recall_under_mcp_pod_kills.duration", "stress.recall_under_mcp_pod_kills.kill_interval", "stress.recall_under_mcp_pod_kills.quiet_error_rate",
		"stress.kb_ingest_with_worker_kills.duration", "stress.kb_ingest_with_worker_kills.ingest_timeout", "stress.kb_ingest_with_worker_kills.kill_interval",
		"stress.dependency_faults_under_load.duration", "stress.dependency_faults_under_load.hold", "stress.dependency_faults_under_load.fault_interval",
		"stress.dependency_faults_under_load.mcp_replicas", "stress.dependency_faults_under_load.db_settle_within", "stress.recall_under_mcp_pod_kills.mcp_replicas",
		"apps.mekoload.image", "apps.mekoload.port", "apps.mekoload.release.repo",
	} {
		if !cfg.Has(k) {
			t.Errorf("default.yaml has no %s", k)
		}
	}
	pods := cfg.GetInt("stress.client_pods", 0)
	for _, p := range []struct {
		name string
		plan func() (int, int, int)
	}{
		{"datapack_churn_soak", func() (int, int, int) {
			return pods, cfg.GetInt("stress.credentials", 0), cfg.GetInt("stress.datapack_churn_soak.max_active_pats", 0)
		}},
		{"mcp_conversations_soak", func() (int, int, int) { return pods, cfg.GetInt("stress.mcp_conversations_soak.credentials", 0), 0 }},
		{"churn_with_api_pod_kills", func() (int, int, int) {
			return pods, cfg.GetInt("stress.credentials", 0), cfg.GetInt("stress.churn_with_api_pod_kills.max_active_pats", 0)
		}},
	} {
		pods, creds, budget := p.plan()
		if need := creds + pods*budget; need > 8 {
			t.Errorf("%s needs %d active PATs, the plan check allows 8", p.name, need)
		}
	}
	// A kill test's run must fit its duration, the last fault's recovery and the
	// drain inside the test timeout.
	for _, name := range []string{"recall_under_mcp_pod_kills", "kb_ingest_with_worker_kills", "dependency_faults_under_load"} {
		tc, _ := core.Default().Get("stress/" + name)
		timeout := tc.Timeout
		if timeout == 0 {
			timeout = 60 * time.Minute
		}
		if need := cfg.GetDuration("stress."+name+".duration", 0) + cfg.GetDuration("stress.drain_timeout", 0) + 10*time.Minute; need > timeout {
			t.Errorf("%s needs %s and its timeout is %s", name, need, timeout)
		}
	}
	if d := cfg.GetDuration("stress.datapack_churn_soak.duration", 0); d.Minutes() != 15 {
		t.Errorf("soak duration %v, want 15m", d)
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
	if len(results) != len(tests) {
		t.Fatalf("%d results", len(results))
	}
	for _, r := range results {
		if r.Status != core.StatusSkipped {
			t.Errorf("%s = %s (%s), want skipped on a provider with no stack", r.TestName, r.Status, r.ErrorSummary)
		}
	}
}
