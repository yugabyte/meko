package stress

import (
	"fmt"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoload"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/loadtest"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

func init() {
	core.Register(core.Test{
		Name:       "stress/dependency_faults_under_load",
		Suite:      "stress",
		Category:   core.Stress,
		Tags:       []string{"stress"},
		OwningTeam: core.TeamPlatform,
		Env:        withMCPReplicas("dependency_faults_under_load"),
		Timeout:    90 * time.Minute,
		Tolerates:  []string{core.GuardPodRestart, core.GuardHTTP5xx},
		Params:     append([]string{"chaos"}, paramPrefixes...),
		Description: "Every kind of traffic at once (recall, captured sessions, conversations, datapack and PAT churn, the web app's reads, playground chat) while the stack's " +
			"dependencies fail in turn: Langfuse frozen, Valkey deleted, the LLM slow then failing, an api-server, MCP server, gateway and database pod deleted. " +
			"After each fault the load is healthy again within a bound, between faults it fails under 2% of calls, no invariant breaks, the datapacks the load holds " +
			"survive with Langfuse projects, the universe counter matches the datapacks, and the cleanup leaves nothing.",
		Run: dependencyFaultsUnderLoad,
	})
}

func dependencyFaultsUnderLoad(t *core.T) error {
	const key = "dependency_faults_under_load"
	o := t.Options
	prefix := "stress." + key + "."
	var events []event
	return runSoak(t, soakSpec{
		key:      key,
		workload: "mixed",
		plan:     loadtest.Plan{Pods: o.GetInt("stress.client_pods", 2), Credentials: o.GetInt("stress.credentials", 3), PATBudget: 1},
		bounds: loadtest.Bounds{
			MaxErrorRate: o.GetFloat(prefix+"max_error_rate", 0.15),
			Max5xx:       int64(o.GetInt(prefix+"http_5xx_allowed", 100000)),
		},
		config: func(i, pods int) map[string]any {
			return map[string]any{
				"concurrency":       o.GetInt(prefix+"concurrency", 12),
				"think_time":        o.GetDuration(prefix+"think_time", 100*time.Millisecond).String(),
				"datapacks":         1,
				"datapack_keep":     2,
				"max_active_pats":   1,
				"seed_memories":     o.GetInt(prefix+"seed_memories", 100),
				"seed_docs":         2,
				"freshness_timeout": o.GetDuration(prefix+"freshness_timeout", 3*time.Minute).String(),
				"freshness_every":   5,
				"weights": map[string]int{
					"recall": 4, "write_path": 2, "mcp_conversations": 2, "datapack_churn": 2, "pat_churn": 1, "ui_reads": 2, "chat": 1,
				},
			}
		},
		during: func(rig *loadtest.Rig, load *mekoload.Load, started time.Time, duration time.Duration) (func(mekoload.Aggregate) []string, error) {
			kube := rig.Stack.Kube()
			faults := []fault{
				podFreeze(o, workload{name: "langfuse", kube: kube, selector: "app=langfuse-web-worker", prefix: "langfuse-web-worker", owner: "deployment/langfuse-web-worker"}),
				podDelete(o, workload{name: "valkey", kube: kube, selector: "app.kubernetes.io/name=valkey", prefix: "valkey", owner: "deployment/valkey"}),
				llmFault("slow LLM", stacktest.LLM(t), llmmock.Fault{Route: llmmock.RouteChat, Kind: llmmock.KindLatency, DelayMS: int(o.GetDuration(prefix+"llm_slow", 8*time.Second) / time.Millisecond)}),
				llmFault("failing LLM", stacktest.LLM(t), llmmock.Fault{Kind: llmmock.KindStatus, Status: 503}),
				podDelete(o, workload{name: "meko-api-server", kube: kube, selector: "app=meko-api-server", prefix: "meko-api-server", owner: "deployment/meko-api-server"}),
				podDelete(o, workload{name: "meko-mcp-server", kube: kube, selector: "app=meko-mcp-server", prefix: "meko-mcp-server", owner: "deployment/meko-mcp-server"}),
				podDelete(o, workload{name: "inference-gateway", kube: kube, selector: "app.kubernetes.io/name=inference-gateway", prefix: "inference-gateway", owner: "deployment/inference-gateway"}),
			}
			if b, err := rig.Stack.Backing(t); err == nil && b.Namespace != "" && b.Replication >= 3 {
				db := kube.InNamespace(b.Namespace)
				sts, _ := db.GetSelector(t, "statefulset", "app=yugabyted", "{.items[0].metadata.name}")
				if sts != "" {
					faults = append(faults, podDelete(o, workload{name: "yugabyted", kube: db, selector: "app=yugabyted", prefix: "", owner: "statefulset/" + sts, settle: o.GetDuration(prefix+"db_settle_within", 10*time.Minute)}))
				}
			} else {
				rig.Logf("stress: the database is not RF3 in its own namespace (%v), no database pod is deleted", err)
			}
			sched := faultSchedule{
				faults:     faults,
				firstAfter: o.GetDuration(prefix+"first_fault_after", 2*time.Minute),
				interval:   o.GetDuration(prefix+"fault_interval", 3*time.Minute),
				hold:       o.GetDuration(prefix+"hold", 60*time.Second),
				recover:    o.GetDuration(prefix+"recover_within", 2*time.Minute),
			}
			t.Metric("faults.kinds", float64(len(faults)), "faults")
			var err error
			events, err = sched.run(t, rig, started, duration)
			return func(mekoload.Aggregate) []string {
				problems := judgeFaults(t, load, events, started.Add(duration), sched.recover,
					o.GetFloat(prefix+"recovery_error_rate", 0.2), o.GetFloat(prefix+"quiet_error_rate", 0.02))
				if len(events) < len(faults) {
					problems = append(problems, fmt.Sprintf("only %d of the %d faults ran, the duration is too short for the schedule", len(events), len(faults)))
				}
				return problems
			}, err
		},
	})
}
