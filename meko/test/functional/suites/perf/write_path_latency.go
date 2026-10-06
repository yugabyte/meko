package perf

import (
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/loadtest"
)

func init() {
	core.Register(core.Test{
		Name:       "perf/write_path_latency",
		Suite:      "perf",
		Category:   core.Perf,
		Tags:       []string{"perf"},
		OwningTeam: core.TeamMCPServer,
		Env:        dedicated,
		Params:     paramPrefixes,
		Description: "What a captured agent session costs, at a ladder of virtual users (1, 8 and 32 by default) with the LLM mock answering as slowly as a model: " +
			"conversation_create and turns of conversation_add_message, and for every turn the time until memory_search finds the fact the server extracted from it " +
			"(freshness_memory_extracted). Records latency per op and level, freshness, and the servers' CPU, memory, request latency and LLM calls per turn.",
		Run: writePathLatency,
	})
}

func writePathLatency(t *core.T) error {
	const key = "write_path_latency"
	o := t.Options
	prefix := "perf." + key + "."
	remove, err := llmProfile(t, o.GetDuration(prefix+"llm_chat_latency", 800*time.Millisecond), o.GetDuration(prefix+"llm_embed_latency", 20*time.Millisecond))
	if err != nil {
		return err
	}
	defer remove()
	return runLadder(t, ladderSpec{
		name:     "write",
		key:      key,
		workload: "write_path",
		plan:     loadtest.Plan{Pods: o.GetInt("perf.client_pods", 1), Credentials: o.GetInt(prefix+"credentials", 4)},
		levels:   loadtest.IntList(o, prefix+"concurrency", []int{1, 8, 32}),
		config: func() map[string]any {
			return map[string]any{
				"think_time":                "0s",
				"datapacks":                 1,
				"reuse_session":             true,
				"messages_per_conversation": o.GetInt(prefix+"messages", 3),
				"freshness_timeout":         o.GetDuration(prefix+"freshness_timeout", 2*time.Minute).String(),
				"freshness_every":           o.GetInt(prefix+"freshness_every", 1),
			}
		},
	})
}
