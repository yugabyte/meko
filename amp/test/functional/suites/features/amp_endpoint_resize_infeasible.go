// meko#905: an in-place resize the kubelet refuses as Infeasible is recorded as
// success and the new size is persisted, so the API, the pod and the node
// disagree about one endpoint until someone notices.
package features

import (
	"context"
	"fmt"
	"net/http"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	infeasibleCPU    = "500m"
	infeasibleMemory = "4Gi"
	// infeasibleTarget is more memory than any node in a test pool has.
	infeasibleTarget = "4000Gi"
	// infeasibleRecover is the valid size an operator would move back to.
	infeasibleRecover = "6Gi"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_resize_infeasible",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Ask for more memory than any node has and assert the control plane refuses or fails the resize rather than recording success while the pod keeps its old cgroup limit; then assert the endpoint still serves SQL and a valid resize lands on the pod afterwards.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpEndpointResizeInfeasible,
	})
}

func runAmpEndpointResizeInfeasible(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"endpoint_resize_infeasible", a.endpointResizeInfeasible}}
	})
}

func (a *ampFeatureRun) endpointResizeInfeasible(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, err := createEndpointReq(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID, controller.CreateEndpointRequest{
		Role:      roleReadWrite,
		Resources: &controller.EndpointResources{CPU: infeasibleCPU, Memory: infeasibleMemory},
	})
	if err != nil {
		return fmt.Errorf("create endpoint: %w", err)
	}
	before, err := a.endpointResources(ctx, cl, ep.ID)
	if err != nil {
		return err
	}
	kube := a.f.ampKube()
	if kube == nil {
		a.f.t.Skip("no kube handle for the endpoint pod, so the live limit cannot be compared with the persisted one")
		return nil
	}
	probe := &kubeComputeRoller{kube: kube, timeout: restartReadyTimeout}
	pod, err := probe.podName(ctx, ep.ID)
	if err != nil {
		return err
	}

	task, err := cl.ResizeEndpointAndWait(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, ep.ID,
		controller.ResizeEndpointRequest{Resources: controller.EndpointResources{Memory: infeasibleTarget}})
	if err != nil {
		if status := apiErrorStatus(err); status == http.StatusBadRequest {
			a.f.logf("resize to %s refused synchronously with 400: %v", infeasibleTarget, err)
			return a.assertResources(ctx, cl, ep.ID, before.CPU, before.Memory)
		}
		a.f.logf("resize to %s failed as a task: %v", infeasibleTarget, err)
		return a.assertResources(ctx, cl, ep.ID, before.CPU, before.Memory)
	}
	outcome, message := task.InPlaceResize()
	after, err := a.endpointResources(ctx, cl, ep.ID)
	if err != nil {
		return err
	}
	_, mem, err := podCgroupLimits(ctx, kube, pod)
	if err != nil {
		return err
	}
	wantBytes, err := parseMemBytes(infeasibleTarget)
	if err != nil {
		return err
	}
	a.f.logf("resize to %s: task %s succeeded, in-place outcome %q (%q), persisted memory %s, pod memory limit %d bytes", infeasibleTarget, task.ID, outcome, message, after.Memory, mem)
	wedged := after.Memory == infeasibleTarget && mem != wantBytes
	if err := a.recoverFromInfeasible(ctx, cl, ep, pod); err != nil {
		return fmt.Errorf("after the infeasible resize (task %s, outcome %q, persisted %s, pod at %d bytes): %w", task.ID, outcome, after.Memory, mem, err)
	}
	if wedged {
		return fmt.Errorf("resize task %s succeeded with in-place outcome %q and persisted memory %s, but pod %s still ran with a %d-byte memory limit: an infeasible resize is recorded as success and the endpoint sits between what the API says and what the node can give (meko#905)", task.ID, outcome, after.Memory, pod, mem)
	}
	return nil
}

// recoverFromInfeasible is the operator's next move: a valid size must land on
// whatever pod serves afterwards, in place or by replacement, and the endpoint
// must still serve SQL before it.
func (a *ampFeatureRun) recoverFromInfeasible(ctx context.Context, cl *controller.Client, ep controller.Endpoint, pod string) error {
	c, err := a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("the endpoint no longer accepts connections: %w", err)
	}
	defer c.Close()
	if _, err := c.QueryInt(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("the endpoint no longer answers SQL: %w", err)
	}
	outcome, err := a.resize(ctx, cl, ep.ID, controller.EndpointResources{CPU: infeasibleCPU, Memory: infeasibleRecover})
	if err != nil {
		return fmt.Errorf("a valid resize back to %s/%s is refused, so the endpoint cannot be recovered through the API: %w", infeasibleCPU, infeasibleRecover, err)
	}
	if err := a.assertResources(ctx, cl, ep.ID, infeasibleCPU, infeasibleRecover); err != nil {
		return err
	}
	// meko#905 asks for a rolling replace when in place cannot apply, so the
	// pod serving now may be a new one; either way its limits must be the size.
	probe := &kubeComputeRoller{kube: a.f.ampKube(), timeout: restartReadyTimeout}
	if err := probe.waitReady(ctx, ep.ID); err != nil {
		return fmt.Errorf("after the recovery resize (outcome %q) the endpoint never became ready: %w", outcome, err)
	}
	current, err := probe.podName(ctx, ep.ID)
	if err != nil {
		return err
	}
	if current != pod {
		a.f.logf("the recovery resize replaced pod %s with %s (in-place outcome %q)", pod, current, outcome)
	} else if outcome != controller.ResizeCompleted {
		return fmt.Errorf("the valid resize to %s/%s reported in-place outcome %q and pod %s was neither resized nor replaced", infeasibleCPU, infeasibleRecover, outcome, pod)
	}
	return assertPodCgroup(ctx, a.f.ampKube(), current, infeasibleCPU, infeasibleRecover)
}
