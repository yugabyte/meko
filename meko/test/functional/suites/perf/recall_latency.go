package perf

import (
	"fmt"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/loadtest"
)

func init() {
	core.Register(core.Test{
		Name:       "perf/recall_latency",
		Suite:      "perf",
		Category:   core.Perf,
		Tags:       []string{"perf"},
		OwningTeam: core.TeamMCPServer,
		Env:        dedicated,
		Params:     paramPrefixes,
		Description: "Recall latency as a datapack grows. At each volume (100, 1000 and 5000 memories plus knowledge-base documents by default) client pods seed a fresh datapack, " +
			"then run memory_search, context_search and knowledgebase_search for seeded text, datapack_describe, the REST datapack page and the memories list and count, with some memory_add. " +
			"A search for a seeded fact must return it. Records latency per op and volume, and the servers' CPU, memory, request latency and LLM calls per op.",
		Run: recallLatency,
	})
}

func recallLatency(t *core.T) error {
	const key = "recall_latency"
	o := t.Options
	prefix := "perf." + key + "."
	users := o.GetInt(prefix+"users", 8)
	var steps []step
	for _, v := range loadtest.IntList(o, prefix+"volumes", []int{100, 1000, 5000}) {
		steps = append(steps, step{label: fmt.Sprintf("v%d", v), users: users, extra: map[string]any{"seed_memories": v}})
	}
	remove, err := llmProfile(t, 0, o.GetDuration(prefix+"llm_embed_latency", 20*time.Millisecond))
	if err != nil {
		return err
	}
	defer remove()
	return runLadder(t, ladderSpec{
		name:     "recall",
		key:      key,
		workload: "recall",
		plan:     loadtest.Plan{Pods: o.GetInt("perf.client_pods", 1), Credentials: o.GetInt(prefix+"credentials", 1)},
		steps:    steps,
		config: func() map[string]any {
			return map[string]any{
				"think_time":       "0s",
				"datapacks":        1,
				"reuse_session":    true,
				"seed_docs":        o.GetInt(prefix+"docs", 20),
				"doc_bytes":        o.GetInt(prefix+"doc_bytes", 4096),
				"seed_concurrency": o.GetInt(prefix+"seed_concurrency", 16),
				"ingest_timeout":   o.GetDuration(prefix+"ingest_timeout", 10*time.Minute).String(),
			}
		},
	})
}
