// A memory decrease is the resize the apiserver rejected before Kubernetes
// 1.35 ("memory limits cannot be decreased unless resizePolicy is
// RestartContainer"); the resize tests above only ever grow memory.
package features

import (
	"context"
	"fmt"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	memoryDownCPU    = "500m"
	memoryDownCreate = "8Gi"
	// memoryDownTarget stays above the 8 GiB/vCPU floor for memoryDownCPU, so
	// the only thing that can refuse it is the runtime.
	memoryDownTarget = "6Gi"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_resize_memory_down",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Shrink a running endpoint's memory and assert the decrease is applied in place: the task reports completed, the same pod keeps serving with no container restart, its cgroup limit is the new size, and SQL still answers.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpEndpointResizeMemoryDown,
	})
}

func runAmpEndpointResizeMemoryDown(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"endpoint_resize_memory_down", a.endpointResizeMemoryDown}}
	})
}

func (a *ampFeatureRun) endpointResizeMemoryDown(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, err := createEndpointReq(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID, controller.CreateEndpointRequest{
		Role:      roleReadWrite,
		Resources: &controller.EndpointResources{CPU: memoryDownCPU, Memory: memoryDownCreate},
	})
	if err != nil {
		return fmt.Errorf("create endpoint: %w", err)
	}
	if _, err := a.endpointResources(ctx, cl, ep.ID); err != nil {
		return err
	}
	kube := a.f.ampKube()
	if kube == nil {
		a.f.t.Skip("no kube handle for the endpoint pod, so the live limit cannot be compared with the persisted one")
		return nil
	}
	probe := &kubeComputeRoller{kube: kube, timeout: restartReadyTimeout}
	before, err := endpointPodIdentity(ctx, probe, ep.ID)
	if err != nil {
		return err
	}
	if err := assertPodCgroup(ctx, kube, before.name, memoryDownCPU, memoryDownCreate); err != nil {
		return fmt.Errorf("before the resize: %w", err)
	}

	outcome, err := a.resize(ctx, cl, ep.ID, controller.EndpointResources{Memory: memoryDownTarget})
	if err != nil {
		return err
	}
	a.f.logf("resized memory %s -> %s: in-place outcome %q (pod %s)", memoryDownCreate, memoryDownTarget, outcome, before.name)
	if err := a.assertResources(ctx, cl, ep.ID, memoryDownCPU, memoryDownTarget); err != nil {
		return err
	}
	if outcome != controller.ResizeCompleted {
		_, mem, cgErr := podCgroupLimits(ctx, kube, before.name)
		if cgErr != nil {
			return fmt.Errorf("memory decrease %s -> %s reported in-place outcome %q, want %q; %v", memoryDownCreate, memoryDownTarget, outcome, controller.ResizeCompleted, cgErr)
		}
		return fmt.Errorf("memory decrease %s -> %s reported in-place outcome %q, want %q: pod %s still runs with a %d-byte memory limit while the API reports %s", memoryDownCreate, memoryDownTarget, outcome, controller.ResizeCompleted, before.name, mem, memoryDownTarget)
	}
	after, err := endpointPodIdentity(ctx, probe, ep.ID)
	if err != nil {
		return err
	}
	if after.name != before.name {
		return fmt.Errorf("memory decrease reported %q but the endpoint moved from pod %s to %s", outcome, before.name, after.name)
	}
	if after.restarts != before.restarts {
		return fmt.Errorf("memory decrease reported %q but pod %s went from %d to %d container restarts", outcome, after.name, before.restarts, after.restarts)
	}
	if err := assertPodCgroup(ctx, kube, after.name, memoryDownCPU, memoryDownTarget); err != nil {
		return err
	}

	c, err := a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("after the memory decrease the endpoint no longer accepts connections: %w", err)
	}
	defer c.Close()
	if _, err := c.QueryInt(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("after the memory decrease the endpoint no longer answers SQL: %w", err)
	}
	return nil
}
