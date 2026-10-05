package perf

import (
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/loadtest"
)

func init() {
	core.Register(core.Test{
		Name:       "perf/mcp_latency",
		Suite:      "perf",
		Category:   core.Perf,
		Tags:       []string{"perf"},
		OwningTeam: core.TeamMCPServer,
		Env:        dedicated,
		Params:     paramPrefixes,
		Description: "Closed-loop MCP load from client pods at a ladder of virtual users (1, 8 and 32 by default): initialize, conversation_create, conversation_add_message and datapack_list as several PATs. " +
			"Records p50, p95 and p99 latency and throughput per tool and level as datapoints, and fails a level whose calls fail.",
		Run: mcpLatency,
	})
}

func mcpLatency(t *core.T) error {
	const key = "mcp_latency"
	o := t.Options
	prefix := "perf." + key + "."
	return runLadder(t, ladderSpec{
		name:     "mcp",
		key:      key,
		workload: "mcp_conversations",
		plan:     loadtest.Plan{Pods: o.GetInt("perf.client_pods", 1), Credentials: o.GetInt(prefix+"credentials", 4)},
		levels:   loadtest.IntList(o, prefix+"concurrency", []int{1, 8, 32}),
		config: func() map[string]any {
			return map[string]any{
				"think_time":                o.GetDuration(prefix+"think_time", 0).String(),
				"messages_per_conversation": o.GetInt(prefix+"messages", 2),
				"datapacks":                 o.GetInt(prefix+"datapacks", 1),
				"reuse_session":             o.GetBool(prefix+"reuse_session", false),
			}
		},
	})
}
