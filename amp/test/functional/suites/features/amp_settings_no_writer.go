// The two settings paths amp_endpoint_settings never takes: a branch with no
// writer to ask (the check is skipped, the row is written, the next endpoint
// picks it up), and a writer with no pod at all (reported in `unreachable`, the
// row still written, applied at the next start). Both are the common case for a
// user editing settings before or while their compute is down.
package features

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	noWriterSetting      = "work_mem"
	noWriterValue        = "111MB"
	unreachableValue     = "121MB"
	noWriterProbeSetting = "amp_probe.set_with_no_writer"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_settings_no_writer",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "gapcoverage"},
		OwningTeam:  core.TeamCompute,
		Description: "Branch settings written with no writer on the branch are stored (applied=true, no endpoint outcomes) and in force on the first endpoint created afterwards, an unknown dotted name is accepted unchecked; an endpoint settings write (one reloadable, one postmaster setting) made while the endpoint's Deployment is scaled to zero is stored and reported in `unreachable`, and both values are in force once the compute is back.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     30 * time.Minute,
		Run:         runAmpSettingsNoWriter,
	})
}

func runAmpSettingsNoWriter(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{
			{"settings_without_writer", a.settingsWithoutWriter},
			{"settings_unreachable_writer", a.settingsUnreachableWriter},
		}
	})
}

func (a *ampFeatureRun) settingsWithoutWriter(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	proj := a.proj
	eps, err := cl.ListAllEndpoints(ctx, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("list endpoints: %w", err)
	}
	if len(eps) != 0 {
		return fmt.Errorf("the fresh cluster's default branch already has %d endpoint(s); the no-writer path cannot be taken", len(eps))
	}
	reply, err := cl.UpdateBranchSettings(ctx, proj.poolID, proj.projID, proj.branchID, pgSettings(map[string]string{
		noWriterSetting:      noWriterValue,
		noWriterProbeSetting: "1",
	}))
	if err != nil {
		return fmt.Errorf("write branch settings with no writer: %w", err)
	}
	if !reply.Applied {
		return fmt.Errorf("applied=false with no writer on the branch: the doc says the row is written and the check skipped")
	}
	if len(reply.Endpoints) != 0 {
		return fmt.Errorf("a branch with no endpoints reported %d endpoint outcomes", len(reply.Endpoints))
	}
	ep, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create the first writer: %w", err)
	}
	c, err := a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect the writer: %w", err)
	}
	defer c.Close()
	got, err := c.QueryString(ctx, "SHOW "+noWriterSetting)
	if err != nil {
		return fmt.Errorf("SHOW %s: %w", noWriterSetting, err)
	}
	if got != noWriterValue {
		return fmt.Errorf("%s = %s on the first endpoint after the branch stored %s with no writer to ask; the stored row did not reach the compute spec", noWriterSetting, got, noWriterValue)
	}
	return nil
}

func (a *ampFeatureRun) settingsUnreachableWriter(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	kube := a.f.ampKube()
	if kube == nil {
		a.f.t.Skip("the control plane is reached by a direct URL with no kube access, so the compute pod cannot be taken away")
		return nil
	}
	proj := a.proj
	ep, ok := lastEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if !ok {
		return fmt.Errorf("the branch has no writer from the previous phase")
	}
	roller := &kubeComputeRoller{kube: kube, timeout: restartReadyTimeout}
	deploy := endpointDeployName(ep.ID)
	scale := func(n int) error {
		return kube.Patch(ctx, "deployment", deploy, "merge", fmt.Sprintf(`{"spec":{"replicas":%d}}`, n))
	}
	if err := scale(0); err != nil {
		return fmt.Errorf("scale endpoint Deployment %s to zero: %w", deploy, err)
	}
	scaledDown := true
	defer func() {
		if scaledDown {
			_ = scale(1)
		}
	}()
	err = pollUntil(ctx, 3*time.Minute, 2*time.Second, func() (bool, error) {
		pods, err := kube.GetSelector(ctx, "pod", roller.instanceSelector(ep.ID), "{.items[*].metadata.name}")
		return err == nil && strings.TrimSpace(pods) == "", nil
	})
	if err != nil {
		return fmt.Errorf("endpoint %s still has a pod after its Deployment was scaled to zero: %w", ep.ID, err)
	}
	want := map[string]string{noWriterSetting: unreachableValue, settingsRestartName: settingsRestartValue}
	reply, err := cl.UpdateEndpointSettings(ctx, proj.poolID, proj.projID, proj.branchID, ep.ID, pgSettings(want))
	if err != nil {
		return fmt.Errorf("write a reloadable and a postmaster setting while the endpoint has no pod: %w (the controller stores the change whether or not the compute answers)", err)
	}
	if !reply.Applied {
		return fmt.Errorf("applied=false while the compute was down: the row must be written regardless")
	}
	if len(reply.Endpoints) != 1 {
		return fmt.Errorf("want one endpoint outcome, got %d", len(reply.Endpoints))
	}
	if out := reply.Endpoints[0]; out.Unreachable == "" {
		return fmt.Errorf("the endpoint has no pod, yet its outcome does not report it unreachable: %+v", out)
	}
	if err := scale(1); err != nil {
		return fmt.Errorf("scale endpoint Deployment %s back to one: %w", deploy, err)
	}
	scaledDown = false
	if err := roller.waitReady(ctx, ep.ID); err != nil {
		return fmt.Errorf("compute not ready after scaling back up: %w", err)
	}
	c, err := reconnect(ctx, a.f.endpointSQL, ep)
	if err != nil {
		return fmt.Errorf("reconnect after scaling back up: %w", err)
	}
	defer c.Close()
	for name, value := range want {
		got, err := c.QueryString(ctx, "SHOW "+name)
		if err != nil {
			return fmt.Errorf("SHOW %s: %w", name, err)
		}
		if got != value {
			return fmt.Errorf("%s = %q after the compute came back, want %s: a write stored while the endpoint had no pod must be in force when it next starts", name, got, value)
		}
	}
	return nil
}
