// The control plane holds every endpoint to a memory-per-vCPU floor and derives
// memory for a cpu-only request (meko#957). The ratio is deployment config, so
// the test reads it back from a cpu-only create and then walks the whole shape
// matrix around it: on the floor, above, below, malformed, and the resize paths.
package features

import (
	"context"
	"fmt"
	"net/http"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const memoryFloorCPU = "500m"

// shapeCase is one create request the API must refuse.
type shapeCase struct {
	name   string
	cpu    string
	memory func(floorBytes int64) string
}

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_memory_floor",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Derive the deployment's GiB-per-vCPU ratio from a cpu-only create, then hold create and resize to it: on-floor and above-floor shapes are accepted and persisted verbatim, below-floor, zero and malformed quantities are refused with 400 and leave nothing behind, a cpu-only grow under the floor is refused, and a grow that names both dimensions lands.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpEndpointMemoryFloor,
	})
}

func runAmpEndpointMemoryFloor(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"endpoint_memory_floor", a.endpointMemoryFloor}}
	})
}

func (a *ampFeatureRun) endpointMemoryFloor(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, err := createEndpointReq(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID, controller.CreateEndpointRequest{
		Role:      roleReadWrite,
		Resources: &controller.EndpointResources{CPU: memoryFloorCPU},
	})
	if err != nil {
		return fmt.Errorf("create a cpu-only endpoint: %w", err)
	}
	got, err := a.endpointResources(ctx, cl, ep.ID)
	if err != nil {
		return err
	}
	if got.Memory == "" {
		return fmt.Errorf("a cpu-only create left endpoint %s with no memory reservation; the control plane must derive it from the GiB-per-vCPU ratio", ep.ID)
	}
	memBytes, err := parseMemBytes(got.Memory)
	if err != nil {
		return fmt.Errorf("derived memory %q: %w", got.Memory, err)
	}
	cpuMilli, err := parseCPUMilli(got.CPU)
	if err != nil {
		return fmt.Errorf("stored cpu %q: %w", got.CPU, err)
	}
	const gib = int64(1 << 30)
	perCore := memBytes * 1000 / cpuMilli
	if perCore%gib != 0 || perCore < gib {
		return fmt.Errorf("endpoint %s got memory %s for cpu %s, which is %d bytes per core and not a whole GiB-per-vCPU ratio", ep.ID, got.Memory, got.CPU, perCore)
	}
	a.f.logf("memory floor: cpu %s derived memory %s (%d GiB per vCPU)", got.CPU, got.Memory, perCore/gib)
	floor := memBytes

	// Refusals are synchronous and must not add an endpoint to the branch.
	cases := []shapeCase{
		{"below_floor", memoryFloorCPU, func(f int64) string { return fmt.Sprintf("%dMi", f/2/(1<<20)) }},
		{"one_mib_below", memoryFloorCPU, func(f int64) string { return fmt.Sprintf("%dMi", f/(1<<20)-1) }},
		{"zero_memory", memoryFloorCPU, func(int64) string { return "0" }},
		{"malformed_memory", memoryFloorCPU, func(int64) string { return "lots" }},
		{"zero_cpu", "0", func(f int64) string { return fmt.Sprintf("%dMi", f/(1<<20)) }},
		{"malformed_cpu", "two", func(f int64) string { return fmt.Sprintf("%dMi", f/(1<<20)) }},
	}
	for _, tc := range cases {
		mem := tc.memory(floor)
		_, _, err := cl.CreateEndpoint(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, controller.CreateEndpointRequest{
			Role:      roleReadWrite,
			Resources: &controller.EndpointResources{CPU: tc.cpu, Memory: mem},
		})
		if err == nil {
			return fmt.Errorf("%s: creating an endpoint with cpu %q memory %q was accepted, want 400", tc.name, tc.cpu, mem)
		}
		if status := apiErrorStatus(err); status != http.StatusBadRequest {
			return fmt.Errorf("%s: cpu %q memory %q failed with HTTP %d, want 400: %w", tc.name, tc.cpu, mem, status, err)
		}
	}
	eps, err := cl.ListAllEndpoints(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID)
	if err != nil {
		return fmt.Errorf("list endpoints after the refused creates: %w", err)
	}
	if len(eps) != 1 {
		return fmt.Errorf("the branch lists %d endpoint(s) after %d refused creates, want only the one accepted", len(eps), len(cases))
	}

	// A cpu-only grow leaves memory where it is, so it lands under the floor.
	doubled := fmt.Sprintf("%dm", cpuMilli*2)
	_, _, err = cl.ResizeEndpoint(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, ep.ID,
		controller.ResizeEndpointRequest{Resources: controller.EndpointResources{CPU: doubled}})
	if err == nil {
		return fmt.Errorf("a cpu-only resize of endpoint %s to %s was accepted though its memory %s stays on the old floor, want 400", ep.ID, doubled, got.Memory)
	}
	if status := apiErrorStatus(err); status != http.StatusBadRequest {
		return fmt.Errorf("a cpu-only resize under the floor failed with HTTP %d, want 400: %w", status, err)
	}
	belowMem := fmt.Sprintf("%dMi", floor/2/(1<<20))
	_, _, err = cl.ResizeEndpoint(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, ep.ID,
		controller.ResizeEndpointRequest{Resources: controller.EndpointResources{Memory: belowMem}})
	if err == nil {
		return fmt.Errorf("a memory-only resize of endpoint %s to %s (half the floor) was accepted, want 400", ep.ID, belowMem)
	}
	if status := apiErrorStatus(err); status != http.StatusBadRequest {
		return fmt.Errorf("a memory-only resize under the floor failed with HTTP %d, want 400: %w", status, err)
	}
	if err := a.assertResources(ctx, cl, ep.ID, got.CPU, got.Memory); err != nil {
		return fmt.Errorf("after the refused resizes: %w", err)
	}

	// Above the floor is a workload that needs it, and a grow naming both
	// dimensions is exactly on the new floor: both must land verbatim.
	above := fmt.Sprintf("%dMi", floor*3/2/(1<<20))
	if _, err := a.resize(ctx, cl, ep.ID, controller.EndpointResources{Memory: above}); err != nil {
		return fmt.Errorf("resize memory above the floor to %s: %w", above, err)
	}
	if err := a.assertResources(ctx, cl, ep.ID, got.CPU, above); err != nil {
		return err
	}
	onFloor := fmt.Sprintf("%dMi", floor*2/(1<<20))
	if _, err := a.resize(ctx, cl, ep.ID, controller.EndpointResources{CPU: doubled, Memory: onFloor}); err != nil {
		return fmt.Errorf("resize to cpu %s memory %s (exactly on the floor): %w", doubled, onFloor, err)
	}
	return a.assertResources(ctx, cl, ep.ID, doubled, onFloor)
}
