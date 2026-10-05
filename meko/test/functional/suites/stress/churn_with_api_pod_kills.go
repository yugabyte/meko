package stress

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoload"
	"github.com/yugabyte/meko/meko/test/functional/framework/chaos"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	execk8s "github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/loadtest"
)

const (
	apiPodSelector = "app=meko-api-server"
	apiDeployment  = "meko-api-server"
	recoveryWindow = 3
)

func init() {
	core.Register(core.Test{
		Name:       "stress/churn_with_api_pod_kills",
		Suite:      "stress",
		Category:   core.Stress,
		Tags:       []string{"stress"},
		OwningTeam: core.TeamAPIServer,
		Env:        dedicated,
		// A deleted pod is a container the guard sees go, and the replicas left
		// serve the requests the dead one drops.
		Tolerates: []string{core.GuardPodRestart, core.GuardHTTP5xx},
		Params:    append([]string{"chaos"}, paramPrefixes...),
		Description: "Client pods churn datapacks and PATs while an api-server pod is deleted every interval. After each kill the load is healthy again within a bound " +
			"(a window of seconds with successful calls and a low error rate), the datapacks the load still holds are on the server with Langfuse projects, " +
			"no invariant breaks, its cleanup leaves nothing, and the stack has all its replicas back at the end.",
		Run: churnWithAPIPodKills,
	})
}

type kill struct {
	pod string
	at  time.Time
}

func churnWithAPIPodKills(t *core.T) error {
	const key = "churn_with_api_pod_kills"
	o := t.Options
	prefix := "stress." + key + "."
	budget := o.GetInt(prefix+"max_active_pats", 1)

	interval := o.GetDuration(prefix+"kill_interval", 90*time.Second)
	firstAfter := o.GetDuration(prefix+"first_kill_after", 60*time.Second)
	recoverWithin := o.GetDuration(prefix+"recover_within", 90*time.Second)
	recoveryRate := o.GetFloat(prefix+"recovery_error_rate", 0.2)

	var kills []kill
	return runSoak(t, soakSpec{
		key:      key,
		workload: "mixed",
		plan:     loadtest.Plan{Pods: o.GetInt("stress.client_pods", 2), Credentials: o.GetInt("stress.credentials", 3), PATBudget: budget},
		bounds: loadtest.Bounds{
			MaxErrorRate: o.GetFloat(prefix+"max_error_rate", 0.10),
			Max5xx:       int64(o.GetInt(prefix+"http_5xx_allowed", o.GetInt("stress.http_5xx_allowed", 0))),
		},
		config: func(i, pods int) map[string]any {
			return map[string]any{
				"concurrency":     o.GetInt(prefix+"concurrency", 6),
				"think_time":      o.GetDuration(prefix+"think_time", 100*time.Millisecond).String(),
				"datapack_keep":   o.GetInt(prefix+"datapack_keep", 2),
				"max_active_pats": budget,
				"weights": map[string]int{
					"datapack_churn": o.GetInt(prefix+"datapack_weight", 6),
					"pat_churn":      o.GetInt(prefix+"pat_weight", 1),
				},
			}
		},
		during: func(rig *loadtest.Rig, load *mekoload.Load, started time.Time, duration time.Duration) (func(mekoload.Aggregate) []string, error) {
			kube := rig.Stack.Kube()
			if owner := chaos.FromConfig(o).OwnerPrefix; !strings.Contains(kube.Namespace(), owner) {
				return nil, fmt.Errorf("namespace %s does not carry %q, the chaos guard refuses to kill in it", kube.Namespace(), owner)
			}
			fault, err := chaos.DefaultRegistry().Build(chaos.ActionPodDelete, chaos.FromConfig(o))
			if err != nil {
				return nil, err
			}
			// The last kill needs room to be seen recovering.
			stop := started.Add(duration - recoverWithin - recoveryWindow*time.Second)
			next := started.Add(firstAfter)
			for n := 0; next.Before(stop); n++ {
				if err := sleepUntil(t, next); err != nil {
					return nil, err
				}
				k, err := killOne(t, kube, fault, n, recoverWithin+time.Minute)
				kills = append(kills, k)
				rig.Logf("churn_with_api_pod_kills: killed %s at %s, replicas back: %v", k.pod, k.at.Format("15:04:05"), err == nil)
				if err != nil {
					return nil, err
				}
				next = k.at.Add(interval)
				if n == 0 {
					t.Metric("kills.interval", interval.Seconds(), "s")
				}
			}
			return func(agg mekoload.Aggregate) []string {
				return judgeRecovery(t, load, kills, started.Add(duration), recoverWithin, recoveryRate)
			}, nil
		},
	})
}

func sleepUntil(t *core.T, at time.Time) error {
	select {
	case <-time.After(time.Until(at)):
		return nil
	case <-t.Done():
		return t.Err()
	}
}

// killOne deletes one api-server pod and waits until the Deployment has all its
// replicas ready again with the deleted pod gone.
func killOne(t *core.T, kube *execk8s.Kubectl, fault chaos.ChaosAction, n int, within time.Duration) (kill, error) {
	pods, err := kube.GetSelector(t, "pods", apiPodSelector, "{.items[*].metadata.name}")
	if err != nil {
		return kill{}, fmt.Errorf("list the api-server pods: %w", err)
	}
	names := strings.Fields(pods)
	if len(names) == 0 {
		return kill{}, fmt.Errorf("no pod matches %s", apiPodSelector)
	}
	victim := names[n%len(names)]
	k := kill{pod: victim, at: time.Now()}
	if err := fault.Inject(t, chaos.NewPodTarget("api-server", kube, victim, "")); err != nil {
		return k, fmt.Errorf("delete %s: %w", victim, err)
	}
	return k, t.WaitFor("the api-server Deployment to have all its replicas back after "+victim+" was deleted", within, 2*time.Second, func() (bool, error) {
		return replicasRestored(t, kube, victim)
	})
}

func replicasRestored(ctx context.Context, kube *execk8s.Kubectl, victim string) (bool, error) {
	want, err := kube.Get(ctx, "deployment", apiDeployment, "{.spec.replicas}")
	if err != nil {
		return false, err
	}
	ready, err := kube.Get(ctx, "deployment", apiDeployment, "{.status.readyReplicas}")
	if err != nil {
		return false, err
	}
	pods, err := kube.GetSelector(ctx, "pods", apiPodSelector, "{.items[*].metadata.name}")
	if err != nil {
		return false, err
	}
	for _, p := range strings.Fields(pods) {
		if p == victim {
			return false, nil
		}
	}
	return strings.TrimSpace(want) != "" && strings.TrimSpace(want) == strings.TrimSpace(ready), nil
}

// judgeRecovery reads the per-second timeline once the load has ended and
// checks each kill was followed by a healthy window within the bound.
func judgeRecovery(t *core.T, load *mekoload.Load, kills []kill, end time.Time, within time.Duration, maxErr float64) []string {
	if len(kills) == 0 {
		return []string{"no api-server pod was killed, the duration is too short for the kill interval"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tls, err := load.Timelines(ctx)
	if err != nil {
		return []string{"read the load timeline: " + err.Error()}
	}
	sec := loadtest.Seconds(tls)
	var problems []string
	for i, k := range kills {
		until := end
		if i+1 < len(kills) {
			until = kills[i+1].at
		}
		took, ok := loadtest.Recovered(sec, k.at, until, recoveryWindow, maxErr)
		rate, errs := loadtest.Damage(sec, k.at)
		t.Metric(fmt.Sprintf("kill.%d.error_rate_30s", i), rate*100, "%")
		t.Metric(fmt.Sprintf("kill.%d.errors_30s", i), float64(errs), "calls")
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("after %s was deleted at %s the load was not healthy again (%d seconds with calls and under %.0f%% errors) before the next fault or the end of the run",
				k.pod, k.at.Format("15:04:05"), recoveryWindow, maxErr*100))
		default:
			t.Metric(fmt.Sprintf("kill.%d.recovery_seconds", i), took.Seconds(), "s")
			if took > within {
				problems = append(problems, fmt.Sprintf("after %s was deleted the load took %s to recover, %s allowed", k.pod, took.Round(time.Second), within))
			}
		}
	}
	return problems
}
