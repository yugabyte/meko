package ampbackend

import (
	"encoding/json"
	"testing"

	"meko/ycp/internal/api"
)

// controllerTargetVersion is the controller image tag the generation tests
// declare.
const controllerTargetVersion = "0.260902.0-def5678"

func fleetRes(gen int64) api.DesiredResource {
	return api.DesiredResource{Kind: KindFleetVersion, Name: fleetName, Generation: gen,
		Spec: json.RawMessage(`{"version":"` + controllerTargetVersion + `"}`)}
}

func controllerRes(gen int64) api.DesiredResource {
	return api.DesiredResource{Kind: KindController, Name: controllerName, Generation: gen,
		Spec: json.RawMessage(`{"version":"` + controllerTargetVersion + `"}`)}
}

// assertObservable is the observable-kind half of the enumeration guard in
// TestUpdatableKindsEchoTheirGeneration.
//
// An updatable kind proves it converges by echoing the generation it applied. An
// observable one has no echo — every field of its spec is readable on the remote
// — so it proves the same thing the other way round: a declaration the remote
// has not reached issues the work, and one it has reached issues nothing. Without
// the second half a kind could "converge" by acting on every tick forever, which
// for this kind is a helm upgrade of the release the agent runs in.
func assertObservable(t *testing.T, kind string, res api.DesiredResource, diverged *genServer) {
	t.Helper()
	switch kind {
	case KindController:
		ups := diverged.helm.upgrades()
		if len(ups) != 1 {
			t.Fatalf("%s: helm upgrades = %v, want exactly one", kind, ups)
		}
		if got := valueAt(diverged.helm.upgradeValues(t, 0), "controller.image.tag"); got != controllerTargetVersion {
			t.Errorf("%s: the upgrade's values set controller.image.tag = %v, want %s", kind, got, controllerTargetVersion)
		}

		converged := &genServer{branchGen: gen64(1), endpointGen: gen64(1),
			controllerVersion: controllerTargetVersion,
			helm:              deployedAt(controllerTargetVersion)}
		b := converged.start(t)
		if err := b.Update(t.Context(), res); err != nil {
			t.Fatalf("%s: Update on an already-converged controller = %v, want nil", kind, err)
		}
		if got := converged.helm.upgrades(); len(got) != 0 {
			t.Errorf("%s: converged Update ran %v, want no upgrade at all", kind, got)
		}
	case KindFleetVersion:
		if diverged.fleetRolls != 1 {
			t.Fatalf("%s: rolls = %d, want exactly one", kind, diverged.fleetRolls)
		}
		converged := &genServer{branchGen: gen64(1), endpointGen: gen64(1),
			fleetVersion: controllerTargetVersion, fleetPin: controllerTargetVersion}
		b := converged.start(t)
		if err := b.Update(t.Context(), res); err != nil {
			t.Fatalf("%s: Update on an already-converged fleet = %v, want nil", kind, err)
		}
		if converged.fleetRolls != 0 {
			t.Errorf("%s: converged Update rolled %d times, want none", kind, converged.fleetRolls)
		}
	default:
		t.Fatalf("%s is observable but this test does not know what to observe for it", kind)
	}
}
