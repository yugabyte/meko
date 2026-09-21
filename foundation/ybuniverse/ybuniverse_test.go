package ybuniverse

import (
	"strings"
	"testing"
)

// TestSettledWaitsForQueuedAndRunningActions is the regression test for the
// trap this package exists to avoid.
//
// The CRD has no observedGeneration, so right after an apply the operator has
// not looked at the new spec and universeState still reads "Ready" from the
// PREVIOUS generation. A wait that polls state alone reports an edit as applied
// before it has started. status.actions is the only signal that closes the
// hole, so Settled must refuse to report settled while any action is queued or
// running — even when the state says Ready.
func TestSettledWaitsForQueuedAndRunningActions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status Status
		want   bool
	}{{
		name:   "ready with no actions is settled",
		status: Status{UniverseState: StateReady},
		want:   true,
	}, {
		// The load-bearing case: Ready is stale here.
		name: "ready but an action is queued is NOT settled",
		status: Status{UniverseState: StateReady, Actions: []Action{
			{ActionType: "EditUniverse", Status: ActionQueued, TaskUUID: "t-1"},
		}},
		want: false,
	}, {
		name: "ready but an action is running is NOT settled",
		status: Status{UniverseState: StateReady, Actions: []Action{
			{ActionType: "UpgradeSoftware", Status: ActionRunning, TaskUUID: "t-2"},
		}},
		want: false,
	}, {
		name:   "creating is not settled",
		status: Status{UniverseState: StateCreating},
		want:   false,
	}, {
		name:   "editing is not settled",
		status: Status{UniverseState: StateEditing},
		want:   false,
	}, {
		// A failed action is terminal, not in-flight: Settled says yes and
		// Failed is what the caller checks to decide the outcome. Blocking here
		// would turn a fast failure into a timeout.
		name: "ready with only a failed action is settled",
		status: Status{UniverseState: StateReady, Actions: []Action{
			{ActionType: "EditUniverse", Status: ActionFailed, Message: "boom"},
		}},
		want: true,
	}, {
		name:   "an empty status is not settled",
		status: Status{},
		want:   false,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := Settled(tc.status)
			if got != tc.want {
				t.Fatalf("Settled = %v (%s), want %v", got, why, tc.want)
			}
			if !got && why == "" {
				t.Error("not settled but gave no reason; a caller that times out has nothing to report")
			}
		})
	}
}

// TestFailedSurfacesTheOperatorsMessage: a rejected edit fails asynchronously
// inside the operator, so without reading it back the failure presents as a
// wait that timed out and the reason stays in the CR.
func TestFailedSurfacesTheOperatorsMessage(t *testing.T) {
	failed, msg := Failed(Status{UniverseState: StateReady, Actions: []Action{
		{ActionType: "EditGFlags", Status: ActionFailed, Message: "preview flag not allowed", TaskUUID: "t-9"},
	}})
	if !failed {
		t.Fatal("Failed = false, want true")
	}
	for _, want := range []string{"EditGFlags", "preview flag not allowed", "t-9"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q is missing %q", msg, want)
		}
	}

	if failed, _ := Failed(Status{UniverseState: StateReady}); failed {
		t.Error("Failed = true for a clean status")
	}
	// A failure with no message must still be reportable rather than blank.
	_, msg = Failed(Status{Actions: []Action{{Status: ActionFailed}}})
	if msg == "" {
		t.Error("a failed action with no message produced no text")
	}
}

// TestRenderPassesTheSpecThrough: the body is the CR's spec verbatim, so a
// field this package has never heard of reaches the cluster unchanged. That is
// the whole design — no Go struct to extend when the operator adds a field.
func TestRenderPassesTheSpecThrough(t *testing.T) {
	cr := Render(KindUniverse, "foundation", "amp", map[string]any{
		"universeName": "foundation",
		"numNodes":     3,
		// Nothing in this package knows these two; both must survive.
		"someFutureField": "kept",
		"nested":          map[string]any{"deep": true},
	})

	if cr["apiVersion"] != APIVersion || cr["kind"] != KindUniverse {
		t.Errorf("apiVersion/kind = %v/%v", cr["apiVersion"], cr["kind"])
	}
	md := cr["metadata"].(map[string]any)
	if md["name"] != "foundation" || md["namespace"] != "amp" {
		t.Errorf("metadata = %v", md)
	}
	spec := cr["spec"].(map[string]any)
	if spec["someFutureField"] != "kept" {
		t.Errorf("an unmodelled field was dropped: %v", spec)
	}
	if nested, ok := spec["nested"].(map[string]any); !ok || nested["deep"] != true {
		t.Errorf("a nested unmodelled field was dropped: %v", spec)
	}
}

// TestRenderKeepsTheBodysOwnName: metadata.name and spec.name are two
// different fields, and spec.name is REQUIRED by KMSConfig, PitrConfig and
// DrConfig and real on StorageConfig and BackupSchedule.
//
// Render used to strip it, which was harmless while only YBProvider and
// YBUniverse existed — neither declares one — and silently broke every one of
// those kinds the moment a spec could name them.
func TestRenderKeepsTheBodysOwnName(t *testing.T) {
	body := map[string]any{"name": "foundation-kms", "provider": "AWS"}
	cr := Render("KMSConfig", "foundation-kms-object", "amp", body)

	if got := cr["spec"].(map[string]any)["name"]; got != "foundation-kms" {
		t.Errorf("spec.name = %v, want the body's own name passed through", got)
	}
	if got := cr["metadata"].(map[string]any)["name"]; got != "foundation-kms-object" {
		t.Errorf("metadata.name = %v, want the object name the caller passed", got)
	}
	// Render is called twice by validate-then-render paths, so it must not
	// touch the caller's map.
	if body["name"] != "foundation-kms" {
		t.Error("Render mutated the caller's map")
	}
}

// TestFailedCatchesAnErrorStateWithNoFailedAction is a regression test for a
// wait that would otherwise run to timeout.
//
// YBA can reject a create BEFORE submitting a task: universeState becomes
// "Error Creating" while the action stays "queued" with no message and no task
// UUID. Observed exactly this on a spec whose ybSoftwareVersion was empty. A
// Failed that only looked at action status would call this "still queued" and
// wait it out.
func TestFailedCatchesAnErrorStateWithNoFailedAction(t *testing.T) {
	st := Status{
		UniverseState: "Error Creating",
		Actions:       []Action{{ActionType: "CreateKubernetesUniverse", Status: ActionQueued}},
	}
	failed, why := Failed(st)
	if !failed {
		t.Fatal("Failed did not report a universe in Error Creating")
	}
	if !strings.Contains(why, "Error Creating") || !strings.Contains(why, "yugaware log") {
		t.Errorf("reason %q does not name the state and where the explanation is", why)
	}
	// And it must not fire on the states that are merely in progress.
	for _, state := range []string{StateReady, StateCreating, StateEditing, StateDeleting} {
		if f, _ := Failed(Status{UniverseState: state}); f {
			t.Errorf("Failed fired on %q, which is not a failure", state)
		}
	}
}
