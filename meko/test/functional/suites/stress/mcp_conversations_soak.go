package stress

import (
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/loadtest"
)

func init() {
	core.Register(core.Test{
		Name:       "stress/mcp_conversations_soak",
		Suite:      "stress",
		Category:   core.Stress,
		Tags:       []string{"stress"},
		OwningTeam: core.TeamMCPServer,
		Env:        dedicated,
		Params:     paramPrefixes,
		Description: "Client pods inside the cluster hold MCP sessions as several PATs and run conversation_create, conversation_add_message and datapack_list, with memory_add and memory_search against the LLM mock, " +
			"for the configured duration. The error rate stays under a bound with no 5xx, datapack_list always names the datapack it was asked about, " +
			"the datapacks the load made have Langfuse projects, its cleanup leaves nothing, and the service guard stays clean.",
		Run: mcpConversationsSoak,
	})
}

func mcpConversationsSoak(t *core.T) error {
	const key = "mcp_conversations_soak"
	o := t.Options
	prefix := "stress." + key + "."
	return runSoak(t, soakSpec{
		key:      key,
		workload: "mixed",
		plan:     loadtest.Plan{Pods: o.GetInt("stress.client_pods", 2), Credentials: o.GetInt(prefix+"credentials", 4)},
		bounds: loadtest.Bounds{
			MaxErrorRate: o.GetFloat(prefix+"max_error_rate", o.GetFloat("stress.max_error_rate", 0.02)),
			Max5xx:       int64(o.GetInt("stress.http_5xx_allowed", 0)),
		},
		config: func(i, pods int) map[string]any {
			return map[string]any{
				"concurrency":               o.GetInt(prefix+"concurrency", 8),
				"think_time":                o.GetDuration(prefix+"think_time", 100*time.Millisecond).String(),
				"messages_per_conversation": o.GetInt(prefix+"messages", 3),
				"datapacks":                 o.GetInt(prefix+"datapacks", 1),
				"reuse_session":             o.GetBool(prefix+"reuse_session", false),
				"weights": map[string]int{
					"mcp_conversations": o.GetInt(prefix+"conversation_weight", 8),
					"memory":            o.GetInt(prefix+"memory_weight", 1),
				},
			}
		},
	})
}
