package features

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// The valid overrides the test creates its endpoint with. Each is a value no
// deployment defaults to, so a read-back can only come from this request.
const (
	createSettingsWorkMem     = "23MB"
	createSettingsPitr        = "12 hours"
	createSettingsPitrKey     = "tenant_config.pitr_interval"
	createSettingsWalCacheKey = "wal_cache_size_mib"
	createSettingsWalCacheMiB = 96
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_settings_create",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "The settings API at endpoint CREATE (features/amp_endpoint_settings covers the reload half): an unknown pageserver key, a protected identity key, a nested value and an unknown safekeeper key are each rejected with 400 leaving no endpoint behind, and a valid create's pg/pageserver/safekeeper settings are echoed back with the pg override live in SHOW.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpEndpointSettingsCreate,
	})
}

func runAmpEndpointSettingsCreate(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"endpoint_settings_create", func(ctx context.Context) error {
			cl, err := a.f.controllerClient(ctx)
			if err != nil {
				return err
			}
			return endpointSettingsCreateSteps(ctx, cl, a.proj, a.f.endpointSQL, a.f.t.Skip)
		}}}
	})
}

// settingsRejectionProblem classifies the error a doomed settings create came
// back with. "" means the rejection is the documented one: a synchronous 400
// from validation. Anything else is what the message describes — the daemons'
// key-set validation must refuse before a task is planned, not fail a pod later.
func settingsRejectionProblem(err error) string {
	var apiErr *controller.APIError
	if !errors.As(err, &apiErr) {
		return fmt.Sprintf("rejected, but not by validation: want a synchronous 400 from the daemon key-set check, got %v", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		return fmt.Sprintf("rejected with HTTP %d, want 400 from the daemon key-set check: %v", apiErr.StatusCode, err)
	}
	return ""
}

func endpointSettingsCreateSteps(ctx context.Context, cl ampCtl, proj ampCluster, connect endpointConnector, skip func(string)) error {
	// The invalid creates run first, against a branch with no endpoint: a
	// rejection here can only be validation, never the one-writer guardrail.
	rejects := []struct {
		name string
		req  controller.CreateEndpointRequest
	}{
		{"an unknown pageserver key", controller.CreateEndpointRequest{
			Role:               roleReadWrite,
			PageserverSettings: map[string]any{"tenant_config.no_such_knob": true},
		}},
		{"a protected identity key", controller.CreateEndpointRequest{
			Role:               roleReadWrite,
			PageserverSettings: map[string]any{"branch_id": "not-yours-to-set"},
		}},
		{"a nested object value", controller.CreateEndpointRequest{
			Role:               roleReadWrite,
			PageserverSettings: map[string]any{"tenant_config": map[string]any{"enable_gc": true}},
		}},
		{"an unknown safekeeper key", controller.CreateEndpointRequest{
			Role:               roleReadWrite,
			SafekeeperSettings: map[string]any{"no_such_knob": 1},
		}},
	}
	for i, tc := range rejects {
		_, err := cl.CreateEndpointAndWait(ctx, proj.poolID, proj.projID, proj.branchID, tc.req)
		if err == nil {
			// A controller predating the settings API ignores the unknown JSON
			// fields and happily creates a plain endpoint — detectable only on the
			// first case. Nothing here is broken; the capability is absent.
			if i == 0 {
				if ep, ok := lastEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID); ok {
					_, _ = cl.DeleteEndpointAndWait(ctx, proj.poolID, proj.projID, proj.branchID, ep.ID)
				}
				skip("this controller accepts an unknown pageserver_settings key, so it predates the endpoint settings API")
				return nil
			}
			return fmt.Errorf("creating an endpoint with %s succeeded, want it rejected with 400 (the daemons' generated key set must refuse it)", tc.name)
		}
		if problem := settingsRejectionProblem(err); problem != "" {
			return fmt.Errorf("creating an endpoint with %s: %s", tc.name, problem)
		}
		if eps, lerr := cl.ListAllEndpoints(ctx, proj.poolID, proj.projID, proj.branchID); lerr != nil {
			return fmt.Errorf("list endpoints after the rejected create (%s): %w", tc.name, lerr)
		} else if len(eps) != 0 {
			return fmt.Errorf("the create rejected for %s left %d endpoint record(s) on branch %s, want 0 — a validation 400 must not leak a partial endpoint", tc.name, len(eps), proj.branchID)
		}
	}

	ep, err := createEndpointReq(ctx, cl, proj.poolID, proj.projID, proj.branchID, controller.CreateEndpointRequest{
		Role:               roleReadWrite,
		PgSettings:         map[string]string{"work_mem": createSettingsWorkMem},
		PageserverSettings: map[string]any{createSettingsPitrKey: createSettingsPitr},
		SafekeeperSettings: map[string]any{createSettingsWalCacheKey: createSettingsWalCacheMiB},
	})
	if err != nil {
		return fmt.Errorf("create endpoint with valid pg/pageserver/safekeeper settings: %w", err)
	}

	// The settings round-trip through the registry verbatim.
	got, err := cl.GetEndpoint(ctx, proj.poolID, proj.projID, proj.branchID, ep.ID)
	if err != nil {
		return fmt.Errorf("read endpoint %s back: %w", ep.ID, err)
	}
	if v := got.PgSettings["work_mem"]; v != createSettingsWorkMem {
		return fmt.Errorf("endpoint %s echoes pg_settings[work_mem] = %q, want %q — the create-time pg settings did not persist", ep.ID, v, createSettingsWorkMem)
	}
	if v, ok := got.PageserverSettings[createSettingsPitrKey]; !ok || fmt.Sprint(v) != createSettingsPitr {
		return fmt.Errorf("endpoint %s echoes pageserver_settings[%s] = %v, want %q", ep.ID, createSettingsPitrKey, v, createSettingsPitr)
	}
	if v, ok := got.SafekeeperSettings[createSettingsWalCacheKey]; !ok || fmt.Sprint(v) != fmt.Sprint(createSettingsWalCacheMiB) {
		return fmt.Errorf("endpoint %s echoes safekeeper_settings[%s] = %v, want %d", ep.ID, createSettingsWalCacheKey, v, createSettingsWalCacheMiB)
	}

	// The pg override is live in the running endpoint, not just stored.
	c, err := connect(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	defer c.Close()
	workMem, err := c.QueryString(ctx, "SHOW work_mem")
	if err != nil {
		return fmt.Errorf("SHOW work_mem: %w", err)
	}
	if workMem != createSettingsWorkMem {
		return fmt.Errorf("SHOW work_mem = %q on an endpoint created with pg_settings[work_mem] = %q — the stored setting never reached the running postgres", workMem, createSettingsWorkMem)
	}
	return nil
}
