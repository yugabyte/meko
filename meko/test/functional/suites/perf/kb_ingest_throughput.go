package perf

import (
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/loadtest"
)

func init() {
	core.Register(core.Test{
		Name:       "perf/kb_ingest_throughput",
		Suite:      "perf",
		Category:   core.Perf,
		Tags:       []string{"perf"},
		OwningTeam: core.TeamAPIServer,
		Env:        dedicated,
		Params:     paramPrefixes,
		Description: "Knowledge-base ingest at a ladder of concurrent uploaders (1, 4 and 16 by default): each uploads a text document, waits until the rag worker has indexed it " +
			"(kb_time_to_ready) and knowledgebase_search finds it. Records upload latency, time to ready, documents per second, and the servers' CPU, memory and request latency.",
		Run: kbIngestThroughput,
	})
}

func kbIngestThroughput(t *core.T) error {
	const key = "kb_ingest_throughput"
	o := t.Options
	prefix := "perf." + key + "."
	remove, err := llmProfile(t, 0, o.GetDuration(prefix+"llm_embed_latency", 20*time.Millisecond))
	if err != nil {
		return err
	}
	defer remove()
	return runLadder(t, ladderSpec{
		name:     "ingest",
		key:      key,
		workload: "kb_ingest",
		plan:     loadtest.Plan{Pods: o.GetInt("perf.client_pods", 1), Credentials: o.GetInt(prefix+"credentials", 2)},
		levels:   loadtest.IntList(o, prefix+"concurrency", []int{1, 4, 16}),
		config: func() map[string]any {
			return map[string]any{
				"think_time":     "0s",
				"datapacks":      1,
				"reuse_session":  true,
				"doc_bytes":      o.GetInt(prefix+"doc_bytes", 16384),
				"ingest_timeout": o.GetDuration(prefix+"ingest_timeout", 10*time.Minute).String(),
			}
		},
	})
}
