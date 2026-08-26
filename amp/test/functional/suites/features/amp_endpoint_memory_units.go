// This file carries the validation test for the endpoint MEMORY quantity. A
// unitless value is a valid Kubernetes quantity but means BYTES: an endpoint
// created with memory "4" reserves four bytes, and every later in-place resize of
// it wedges in the kubelet ("cannot decrease memory limits ... below current
// usage") because a cpu-only resize carries the bad memory forward. The control
// plane must reject a unitless memory on both create and resize.
package features

import (
	"context"
	"fmt"
	"net/http"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	// memoryUnitless is the foot-gun value: a valid k8s quantity that means 4
	// bytes, not 4 GiB.
	memoryUnitless = "4"
	// memoryWithUnit is the same intent expressed correctly, and must be accepted.
	memoryWithUnit = "4Gi"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_memory_units",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Assert a unitless endpoint memory quantity is rejected with 400 on both create and resize, while the same value with a unit is accepted and persisted.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpEndpointMemoryUnits,
	})
}

func runAmpEndpointMemoryUnits(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"endpoint_memory_units", a.endpointMemoryUnits}}
	})
}

// endpointMemoryUnits asserts the three things #492 asks for: a unitless memory
// is rejected at create, the same value with a unit is accepted and persisted,
// and a unitless memory is rejected at resize too (the path that actually wedges
// the kubelet, since a cpu-only resize carries the stored memory forward).
func (a *ampFeatureRun) endpointMemoryUnits(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}

	// Plan the create WITHOUT waiting out its task. The assertion is about the
	// synchronous reply, and an ...AndWait here would turn a regression into a ten
	// minute readiness timeout instead of an immediate red: the create is accepted,
	// the four-byte compute never comes up, and the wait eats the test's whole
	// budget before it can say why.
	_, _, err = cl.CreateEndpoint(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, controller.CreateEndpointRequest{
		Role:      roleReadWrite,
		Resources: &controller.EndpointResources{Memory: memoryUnitless},
	})
	if err == nil {
		return fmt.Errorf("creating an endpoint with memory %q was accepted, want a 400 rejection — a unitless quantity is a byte count, so the endpoint reserves %s bytes and every later in-place resize of it wedges in the kubelet",
			memoryUnitless, memoryUnitless)
	}
	if status := apiErrorStatus(err); status != http.StatusBadRequest {
		return fmt.Errorf("creating an endpoint with memory %q failed with HTTP status %d, want a synchronous 400 naming the missing unit: %w", memoryUnitless, status, err)
	}
	// A rejected create must leave nothing behind.
	eps, err := cl.ListAllEndpoints(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID)
	if err != nil {
		return fmt.Errorf("list endpoints after the rejected create: %w", err)
	}
	if len(eps) != 0 {
		return fmt.Errorf("the branch lists %d endpoint(s) after a create that was rejected for a unitless memory, want 0", len(eps))
	}

	ep, err := createEndpointReq(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID, controller.CreateEndpointRequest{
		Role:      roleReadWrite,
		Resources: &controller.EndpointResources{Memory: memoryWithUnit},
	})
	if err != nil {
		return fmt.Errorf("creating an endpoint with memory %q was rejected, want it accepted (an explicit unit is exactly what the validation must require): %w", memoryWithUnit, err)
	}
	got, err := a.endpointResources(ctx, cl, ep.ID)
	if err != nil {
		return err
	}
	if got.Memory != memoryWithUnit {
		return fmt.Errorf("endpoint %s reports memory %q, want %q as requested at create", ep.ID, got.Memory, memoryWithUnit)
	}

	_, _, err = cl.ResizeEndpoint(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, ep.ID,
		controller.ResizeEndpointRequest{Resources: controller.EndpointResources{Memory: memoryUnitless}})
	if err == nil {
		return fmt.Errorf("resizing endpoint %s to memory %q was accepted, want a 400 rejection", ep.ID, memoryUnitless)
	}
	if status := apiErrorStatus(err); status != http.StatusBadRequest {
		return fmt.Errorf("resizing endpoint %s to memory %q failed with HTTP status %d, want a synchronous 400: %w", ep.ID, memoryUnitless, status, err)
	}
	// The rejected resize must not have moved the stored reservation.
	return a.assertResources(ctx, cl, ep.ID, got.CPU, memoryWithUnit)
}
