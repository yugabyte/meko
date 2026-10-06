package perf

import (
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/loadtest"
)

func init() {
	core.Register(core.Test{
		Name:       "perf/ui_read_latency",
		Suite:      "perf",
		Category:   core.Perf,
		Tags:       []string{"perf"},
		OwningTeam: core.TeamAPIServer,
		Env:        dedicated,
		Params:     paramPrefixes,
		Description: "The REST reads behind the web app's pages at a ladder of virtual users (1, 8 and 32 by default), on datapacks seeded with memories, conversations and documents: " +
			"auth/me, the datapack list and page, memories and their count, conversations and their count (read from Langfuse), knowledge-base counts, summary and documents, " +
			"metrics, token savings, members, agents, artifacts and projects. Records latency per page and level, and the servers' CPU, memory and request latency.",
		Run: uiReadLatency,
	})
}

func uiReadLatency(t *core.T) error {
	const key = "ui_read_latency"
	o := t.Options
	prefix := "perf." + key + "."
	return runLadder(t, ladderSpec{
		name:     "ui",
		key:      key,
		workload: "ui_reads",
		plan:     loadtest.Plan{Pods: o.GetInt("perf.client_pods", 1), Credentials: o.GetInt(prefix+"credentials", 1)},
		levels:   loadtest.IntList(o, prefix+"concurrency", []int{1, 8, 32}),
		config: func() map[string]any {
			return map[string]any{
				"think_time":                "0s",
				"datapacks":                 o.GetInt(prefix+"datapacks", 2),
				"seed_memories":             o.GetInt(prefix+"memories", 200),
				"seed_conversations":        o.GetInt(prefix+"conversations", 10),
				"messages_per_conversation": 3,
				"seed_docs":                 o.GetInt(prefix+"docs", 3),
				"seed_concurrency":          16,
			}
		},
	})
}
