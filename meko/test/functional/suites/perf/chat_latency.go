package perf

import (
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/loadtest"
)

func init() {
	core.Register(core.Test{
		Name:       "perf/chat_latency",
		Suite:      "perf",
		Category:   core.Perf,
		Tags:       []string{"perf"},
		OwningTeam: core.TeamInferenceGateway,
		Env:        dedicated,
		Params:     paramPrefixes,
		Description: "Playground chat through the inference gateway at a ladder of virtual users (1, 4 and 16 by default) on a datapack with seeded memories, with the LLM mock answering " +
			"as slowly as a model. Records the turn's latency and every processing step the gateway times itself (context search, LLM, saving the turn), " +
			"and the servers' CPU, memory, request latency and LLM calls per turn.",
		Run: chatLatency,
	})
}

func chatLatency(t *core.T) error {
	const key = "chat_latency"
	o := t.Options
	prefix := "perf." + key + "."
	remove, err := llmProfile(t, o.GetDuration(prefix+"llm_chat_latency", 800*time.Millisecond), o.GetDuration(prefix+"llm_embed_latency", 20*time.Millisecond))
	if err != nil {
		return err
	}
	defer remove()
	return runLadder(t, ladderSpec{
		name:     "chat",
		key:      key,
		workload: "chat",
		plan:     loadtest.Plan{Pods: o.GetInt("perf.client_pods", 1), Credentials: o.GetInt(prefix+"credentials", 2)},
		levels:   loadtest.IntList(o, prefix+"concurrency", []int{1, 4, 16}),
		config: func() map[string]any {
			return map[string]any{
				"think_time":                "0s",
				"datapacks":                 1,
				"seed_memories":             o.GetInt(prefix+"memories", 50),
				"messages_per_conversation": o.GetInt(prefix+"messages", 2),
			}
		},
	})
}
