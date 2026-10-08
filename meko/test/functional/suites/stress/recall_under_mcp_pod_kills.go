package stress

import (
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoload"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/loadtest"
)

func init() {
	core.Register(core.Test{
		Name:       "stress/recall_under_mcp_pod_kills",
		Suite:      "stress",
		Category:   core.Stress,
		Tags:       []string{"stress"},
		OwningTeam: core.TeamMCPServer,
		Env:        withMCPReplicas("recall_under_mcp_pod_kills"),
		Tolerates:  []string{core.GuardPodRestart, core.GuardHTTP5xx},
		Params:     append([]string{"chaos"}, paramPrefixes...),
		Description: "Client pods recall from seeded datapacks (memory_search, context_search, knowledgebase_search, describe, the memories pages), " +
			"capture sessions with conversation_add_message and wait for the extracted fact to be searchable, while a meko-mcp-server pod is deleted every interval. " +
			"After each kill the load is healthy again within a bound, between kills it fails under 2% of calls, every search for a seeded fact returns it, " +
			"the datapacks the load holds survive with Langfuse projects, and its cleanup leaves nothing.",
		Run: recallUnderMCPPodKills,
	})
}

func recallUnderMCPPodKills(t *core.T) error {
	const key = "recall_under_mcp_pod_kills"
	o := t.Options
	prefix := "stress." + key + "."
	var events []event
	return runSoak(t, soakSpec{
		key:      key,
		workload: "mixed",
		plan:     loadtest.Plan{Pods: o.GetInt("stress.client_pods", 2), Credentials: o.GetInt("stress.credentials", 3)},
		bounds: loadtest.Bounds{
			MaxErrorRate: o.GetFloat(prefix+"max_error_rate", 0.10),
			Max5xx:       int64(o.GetInt(prefix+"http_5xx_allowed", 100000)),
		},
		config: func(i, pods int) map[string]any {
			return map[string]any{
				"concurrency":       o.GetInt(prefix+"concurrency", 12),
				"think_time":        o.GetDuration(prefix+"think_time", 50*time.Millisecond).String(),
				"datapacks":         1,
				"seed_memories":     o.GetInt(prefix+"seed_memories", 300),
				"seed_docs":         o.GetInt(prefix+"seed_docs", 5),
				"seed_concurrency":  16,
				"freshness_timeout": o.GetDuration(prefix+"freshness_timeout", 3*time.Minute).String(),
				"freshness_every":   5,
				"weights": map[string]int{
					"recall":            o.GetInt(prefix+"recall_weight", 6),
					"write_path":        o.GetInt(prefix+"write_weight", 2),
					"mcp_conversations": o.GetInt(prefix+"conversation_weight", 2),
				},
			}
		},
		during: func(rig *loadtest.Rig, load *mekoload.Load, started time.Time, duration time.Duration) (func(mekoload.Aggregate) []string, error) {
			mcp := workload{name: "meko-mcp-server", kube: rig.Stack.Kube(), selector: "app=meko-mcp-server", prefix: "meko-mcp-server", owner: "deployment/meko-mcp-server"}
			sched := faultSchedule{
				faults:     []fault{podDelete(o, mcp)},
				firstAfter: o.GetDuration(prefix+"first_kill_after", 2*time.Minute),
				interval:   o.GetDuration(prefix+"kill_interval", 3*time.Minute),
				recover:    o.GetDuration(prefix+"recover_within", 2*time.Minute),
			}
			var err error
			events, err = sched.run(t, rig, started, duration)
			return func(mekoload.Aggregate) []string {
				return judgeFaults(t, load, events, started.Add(duration), sched.recover,
					o.GetFloat(prefix+"recovery_error_rate", 0.2), o.GetFloat(prefix+"quiet_error_rate", 0.02))
			}, err
		},
	})
}
