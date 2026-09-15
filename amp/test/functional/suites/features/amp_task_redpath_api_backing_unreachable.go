package features

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

const apiBlockBudget = 4 * time.Minute

func init() {
	core.Register(core.Test{
		Name:        "features/amp_task_redpath_api_backing_unreachable",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane, FaultsSharedInfra: true},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Drop the controller's route to its backing and time one GET /clusters: the API must answer 5xx within functional.redpath.api_deadline rather than blocking, must leave no half-created resource, and must serve the same read again once the route is restored.",
		Params:      []string{"functional.redpath", "functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     45 * time.Minute,
		Run:         runAmpTaskRedpathAPIBackingUnreachable,
	})
}

func runAmpTaskRedpathAPIBackingUnreachable(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"redpath_api_backing_unreachable", a.redpathAPIBackingUnreachable}}
	})
}

func (a *ampFeatureRun) redpathAPIBackingUnreachable(ctx context.Context) error {
	r, err := a.newRedpath(ctx)
	if err != nil {
		return err
	}
	var cut *backingCut
	if r.cfg.Inject {
		if cut, err = r.cutControllerFromBacking(ctx); err != nil {
			return core.AsEnvFailure(fmt.Errorf("cut the controller off from the backing: %w", err))
		}
		a.f.register("heal the controller-to-backing cut", cut.teardown)
	}

	// One request, not the client's retry loop: what is measured is how long the API holds a caller.
	once := *r.cl
	once.UnavailableBudget = 0
	rctx, cancel := context.WithTimeout(ctx, apiBlockBudget)
	start := time.Now()
	_, listErr := once.ListClusters(rctx, a.proj.poolID, 100, 0)
	elapsed := time.Since(start)
	cancel()
	status := apiErrorStatus(listErr)
	a.f.t.Metric("api_response_seconds", elapsed.Seconds(), "s")
	r.logf("GET /clusters with the backing unreachable answered in %s: HTTP %d (%v)", elapsed.Round(time.Millisecond), status, listErr)

	healErr := cut.heal(ctx)

	if elapsed > r.cfg.APIDeadline {
		return fmt.Errorf("GET /clusters took %s to answer with the backing unreachable, want an answer within %s: a read the controller cannot serve must fail fast, not hold the client for minutes on a request that is never going to succeed (meko#1242). It answered HTTP %d (%v)",
			elapsed.Round(time.Millisecond), r.cfg.APIDeadline, status, listErr)
	}
	if listErr == nil {
		return fmt.Errorf("GET /clusters SUCCEEDED in %s with the controller cut off from its backing; the reply cannot have come from the backing, so the API answered from somewhere that is not the source of truth", elapsed.Round(time.Millisecond))
	}
	if status != 0 && status != http.StatusServiceUnavailable {
		return fmt.Errorf("GET /clusters answered HTTP %d (%v) with the backing unreachable, want 503: a client has to be able to tell a dependency being away from its own request being wrong", status, listErr)
	}
	if healErr != nil {
		return core.AsEnvFailure(fmt.Errorf("restore the controller's route to the backing: %w", healErr))
	}
	if err := r.noOrphans(ctx, r.target.branchID, r.cfg.OrphanTimeout); err != nil {
		return err
	}
	if err := r.stillServing(ctx); err != nil {
		return err
	}
	return r.retrySucceeds(ctx, "GET /clusters", func(ctx context.Context) error {
		_, err := r.cl.ListClusters(ctx, a.proj.poolID, 100, 0)
		return err
	})
}
