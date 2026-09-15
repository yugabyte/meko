package features

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	// Not dotted: Postgres treats an unknown dotted name as a placeholder and starts anyway.
	badSettingName   = "wal_levl"
	badSettingValue  = "logical"
	goodSettingName  = "work_mem"
	goodSettingValue = "64MB"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_task_redpath_bad_setting_restart",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "PATCH an endpoint's settings with a misspelled GUC: the API must refuse it naming the setting, the compute must still serve after a restart (the point at which the unrefused version bricked it), the bystander cluster must be untouched, and a valid settings change afterwards must apply.",
		Params:      []string{"functional.redpath", "functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     60 * time.Minute,
		Run:         runAmpTaskRedpathBadSetting,
	})
}

func runAmpTaskRedpathBadSetting(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"redpath_bad_setting_restart", a.redpathBadSetting}}
	})
}

func (a *ampFeatureRun) redpathBadSetting(ctx context.Context) error {
	r, err := a.newRedpath(ctx)
	if err != nil {
		return err
	}
	ep, err := createEndpoint(ctx, r.cl, r.target.poolID, r.target.projID, r.target.branchID)
	if err != nil {
		return fmt.Errorf("create the endpoint the bad setting is aimed at: %w", err)
	}

	name, value := badSettingName, badSettingValue
	if !r.cfg.Inject {
		name, value = goodSettingName, goodSettingValue
	}
	_, err = r.cl.UpdateEndpointSettings(ctx, r.target.poolID, r.target.projID, r.target.branchID, ep.ID,
		pgSettings(map[string]string{name: value}))
	accepted := err == nil
	if err != nil {
		if status := apiErrorStatus(err); status != http.StatusBadRequest {
			return fmt.Errorf("PATCH settings %s=%s returned HTTP %d (%v), want 400", name, value, status, err)
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(name)) {
			return fmt.Errorf("the refusal of %s=%s does not name the setting it refused: %v", name, value, err)
		}
		r.logf("PATCH settings %s=%s refused 400: %v", name, value, err)
	}

	if err := r.restartAndServe(ctx, ep); err != nil {
		if accepted {
			return fmt.Errorf("PATCH settings %s=%s was ACCEPTED and the compute did not come back from its next restart: an unknown setting must be refused at the API, not written into a config that stops Postgres starting (meko#918): %w", name, value, err)
		}
		return err
	}
	if accepted {
		return fmt.Errorf("PATCH settings %s=%s was accepted; an unknown setting must be refused with 400 while the caller is still there to read it, not written into the endpoint's config to bite at its next start (meko#918)", name, value)
	}
	if err := r.stillServing(ctx); err != nil {
		return err
	}
	return r.retrySucceeds(ctx, "PATCH settings "+goodSettingName, func(ctx context.Context) error {
		_, err := r.cl.UpdateEndpointSettings(ctx, r.target.poolID, r.target.projID, r.target.branchID, ep.ID,
			pgSettings(map[string]string{goodSettingName: goodSettingValue}))
		return err
	})
}

func (r *redpathRun) restartAndServe(ctx context.Context, ep controller.Endpoint) error {
	kube := r.a.f.ampKube()
	if kube == nil {
		return fmt.Errorf("no kubectl for the amp namespace, so the compute cannot be restarted")
	}
	roller := &kubeComputeRoller{kube: kube, timeout: restartReadyTimeout}
	pod, err := roller.podName(ctx, ep.ID)
	if err != nil {
		return err
	}
	if err := roller.deletePod(ctx, pod); err != nil {
		return fmt.Errorf("delete compute pod %s: %w", pod, err)
	}
	if err := roller.waitReady(ctx, ep.ID); err != nil {
		return fmt.Errorf("the compute of endpoint %s never became ready again after a restart: %w (%s)", ep.ID, err, kube.PodTrouble(ctx, pod))
	}
	c, err := r.a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("the endpoint does not accept connections after the restart: %w", err)
	}
	defer c.Close()
	if _, err := c.QueryInt(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("the endpoint does not answer SQL after the restart: %w", err)
	}
	return nil
}
