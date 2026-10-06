package stress

import (
	"fmt"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoload"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/loadtest"
)

func init() {
	core.Register(core.Test{
		Name:       "stress/kb_ingest_with_worker_kills",
		Suite:      "stress",
		Category:   core.Stress,
		Tags:       []string{"stress"},
		OwningTeam: core.TeamAPIServer,
		Env:        dedicated,
		Timeout:    90 * time.Minute,
		Tolerates:  []string{core.GuardPodRestart},
		Params:     append([]string{"chaos"}, paramPrefixes...),
		Description: "Client pods upload knowledge-base documents without pause and search the knowledge base while the text rag worker is deleted every interval, " +
			"mid-ingest. Every uploaded document must still reach COMPLETED within the ingest timeout (the lease of a task the dead worker held expires and another " +
			"worker takes it), none may end FAILED, each must be searchable once indexed, and the upload API must not fail while the worker is down.",
		Run: kbIngestWithWorkerKills,
	})
}

func kbIngestWithWorkerKills(t *core.T) error {
	const key = "kb_ingest_with_worker_kills"
	o := t.Options
	prefix := "stress." + key + "."
	var events []event
	return runSoak(t, soakSpec{
		key:      key,
		workload: "mixed",
		plan:     loadtest.Plan{Pods: o.GetInt("stress.client_pods", 2), Credentials: o.GetInt("stress.credentials", 3)},
		bounds: loadtest.Bounds{
			MaxErrorRate: o.GetFloat(prefix+"max_error_rate", 0.02),
			Max5xx:       int64(o.GetInt(prefix+"http_5xx_allowed", 0)),
		},
		config: func(i, pods int) map[string]any {
			return map[string]any{
				"concurrency":    o.GetInt(prefix+"concurrency", 6),
				"think_time":     o.GetDuration(prefix+"think_time", 200*time.Millisecond).String(),
				"datapacks":      1,
				"seed_docs":      o.GetInt(prefix+"seed_docs", 3),
				"doc_bytes":      o.GetInt(prefix+"doc_bytes", 8192),
				"ingest_timeout": o.GetDuration(prefix+"ingest_timeout", 15*time.Minute).String(),
				"op_weights":     map[string]int{"knowledgebase_search": 1},
				"weights": map[string]int{
					"kb_ingest": o.GetInt(prefix+"ingest_weight", 3),
					"recall":    o.GetInt(prefix+"search_weight", 1),
				},
			}
		},
		during: func(rig *loadtest.Rig, load *mekoload.Load, started time.Time, duration time.Duration) (func(mekoload.Aggregate) []string, error) {
			worker := workload{name: "rag-agent-text", kube: rig.Stack.Kube(), selector: "app.kubernetes.io/name=rag-agent", prefix: "rag-agent-text-", owner: "deployment/rag-agent-text"}
			sched := faultSchedule{
				faults:     []fault{podDelete(o, worker)},
				firstAfter: o.GetDuration(prefix+"first_kill_after", 3*time.Minute),
				interval:   o.GetDuration(prefix+"kill_interval", 5*time.Minute),
				recover:    o.GetDuration(prefix+"recover_within", 2*time.Minute),
			}
			var err error
			events, err = sched.run(t, rig, started, duration)
			return func(agg mekoload.Aggregate) []string {
				var problems []string
				for _, ev := range events {
					if ev.settleErr != nil {
						problems = append(problems, fmt.Sprintf("%s on %s: %v", ev.name, ev.target, ev.settleErr))
					}
				}
				if len(events) == 0 {
					problems = append(problems, "no worker was killed, the duration is too short for the kill interval")
				}
				ready := agg.Ops["kb_time_to_ready"]
				t.Metric("kb.documents_indexed", float64(ready.OK), "docs")
				t.Metric("kb.time_to_ready_p99_ms", ready.Latency.P99, "ms")
				if ready.Errors > 0 {
					problems = append(problems, fmt.Sprintf("%d of %d uploaded documents never reached COMPLETED (by class %v): a worker killed mid-ingest left them behind",
						ready.Errors, ready.Count, ready.ErrorsByClass))
				}
				if ready.OK == 0 {
					problems = append(problems, "no document was indexed")
				}
				return problems
			}, err
		},
	})
}
