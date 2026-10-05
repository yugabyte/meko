package stress

import (
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/loadtest"
)

func init() {
	core.Register(core.Test{
		Name:       "stress/datapack_churn_soak",
		Suite:      "stress",
		Category:   core.Stress,
		Tags:       []string{"stress"},
		OwningTeam: core.TeamAPIServer,
		Env:        dedicated,
		Params:     paramPrefixes,
		Description: "Client pods inside the cluster churn datapacks over REST (create, get, list, delete) and mint and revoke PATs for the configured duration. " +
			"The error rate stays under a bound with no 5xx, no invariant breaks (a datapack is listed after its create and gone after its delete), " +
			"every datapack the load still holds is on the server with a Langfuse project, its cleanup leaves nothing, and the service guard stays clean.",
		Run: datapackChurnSoak,
	})
}

func datapackChurnSoak(t *core.T) error {
	const key = "datapack_churn_soak"
	o := t.Options
	prefix := "stress." + key + "."
	pods := o.GetInt("stress.client_pods", 2)
	budget := o.GetInt(prefix+"max_active_pats", 2)
	return runSoak(t, soakSpec{
		key:      key,
		workload: "mixed",
		plan:     loadtest.Plan{Pods: pods, Credentials: o.GetInt("stress.credentials", 3), PATBudget: budget},
		bounds: loadtest.Bounds{
			MaxErrorRate: o.GetFloat("stress.max_error_rate", 0.02),
			Max5xx:       int64(o.GetInt("stress.http_5xx_allowed", 0)),
		},
		config: func(i, pods int) map[string]any {
			return map[string]any{
				"concurrency":     o.GetInt(prefix+"concurrency", 8),
				"think_time":      o.GetDuration(prefix+"think_time", 100*time.Millisecond).String(),
				"datapack_keep":   o.GetInt(prefix+"datapack_keep", 2),
				"max_active_pats": budget,
				"weights": map[string]int{
					"datapack_churn": o.GetInt(prefix+"datapack_weight", 6),
					"pat_churn":      o.GetInt(prefix+"pat_weight", 1),
				},
			}
		},
	})
}
