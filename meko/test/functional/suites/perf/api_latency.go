package perf

import (
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/loadtest"
)

func init() {
	core.Register(core.Test{
		Name:       "perf/api_latency",
		Suite:      "perf",
		Category:   core.Perf,
		Tags:       []string{"perf"},
		OwningTeam: core.TeamAPIServer,
		Env:        dedicated,
		Params:     paramPrefixes,
		Description: "Closed-loop REST load from client pods at a ladder of virtual users (1, 8 and 32 by default) over datapack create, get, list and delete and PAT mint, list and revoke. " +
			"Records p50, p95 and p99 latency and throughput per op and level as datapoints, and fails a level whose calls fail, since a latency over errors measures nothing.",
		Run: apiLatency,
	})
}

func apiLatency(t *core.T) error {
	const key = "api_latency"
	o := t.Options
	prefix := "perf." + key + "."
	budget := o.GetInt(prefix+"max_active_pats", 2)
	pods := o.GetInt("perf.client_pods", 1)
	return runLadder(t, ladderSpec{
		name:     "api",
		key:      key,
		workload: "mixed",
		plan:     loadtest.Plan{Pods: pods, Credentials: o.GetInt("perf.credentials", 3), PATBudget: budget},
		levels:   loadtest.IntList(o, prefix+"concurrency", []int{1, 8, 32}),
		config: func() map[string]any {
			return map[string]any{
				"think_time":      o.GetDuration(prefix+"think_time", 0).String(),
				"max_active_pats": budget,
				"weights": map[string]int{
					"datapack_churn": o.GetInt(prefix+"datapack_weight", 4),
					"pat_churn":      o.GetInt(prefix+"pat_weight", 2),
				},
			}
		},
	})
}
