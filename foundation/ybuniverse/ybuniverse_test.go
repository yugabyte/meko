package ybuniverse

import (
	"strings"
	"testing"
	"time"
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

// TestWatchReportsAFailedTaskWithoutEndingTheWait.
//
// A failed task is reported, and that is all: YBA retries with a backoff, and a
// retry keeps the SAME task UUID, so nothing on this side separates a dead
// create from one that is about to succeed. Ending the wait on it would fail
// installs that recover. The report is what the operator actually needs.
func TestWatchReportsAFailedTaskWithoutEndingTheWait(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := Status{UniverseState: "Error Creating", Actions: []Action{
		{ActionType: "EditGFlags", Status: ActionFailed, Message: "preview flag not allowed", TaskUUID: "t-9"},
	}}
	var w FailureWatch
	report, stop := w.Observe(st, t0)
	if stop {
		t.Fatal("ended the wait on a failed task, which YBA may still be retrying")
	}
	for _, want := range []string{"EditGFlags", "preview flag not allowed", "t-9", "yugaware log"} {
		if !strings.Contains(report, want) {
			t.Errorf("report %q is missing %q", report, want)
		}
	}
	// However long it sits there, it is still not this side's call to make.
	if _, stop := w.Observe(st, t0.Add(FailureSettleWindow*10)); stop {
		t.Error("ended the wait on a failed task after waiting a while, which is the same guess")
	}
	// And it is reported ONCE, not on every poll.
	if report, _ := w.Observe(st, t0.Add(time.Minute)); report != "" {
		t.Errorf("re-reported the same failure: %q", report)
	}
	// A clean status clears it, so a later failure is reported again.
	if report, _ := w.Observe(Status{UniverseState: StateReady}, t0.Add(2*time.Minute)); report != "" {
		t.Errorf("reported a failure for a clean status: %q", report)
	}
	if w.Reason() != "" {
		t.Errorf("Reason = %q after the failure cleared", w.Reason())
	}
	if report, _ := w.Observe(st, t0.Add(3*time.Minute)); report == "" {
		t.Error("a failure that came back was not reported again")
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

// TestWatchCatchesAnErrorStateWithNoFailedAction is the one failure the wait
// does end on, and a regression test for a wait that would otherwise run to
// timeout.
//
// YBA can reject a create BEFORE submitting a task: universeState becomes
// "Error Creating" while the action stays "queued" with no message and no task
// UUID. Observed exactly this on a spec whose ybSoftwareVersion was empty.
// Nothing retries it, because YBA never accepted it.
func TestWatchCatchesAnErrorStateWithNoFailedAction(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := Status{
		UniverseState: "Error Creating",
		Actions:       []Action{{ActionType: "CreateKubernetesUniverse", Status: ActionQueued}},
	}
	var w FailureWatch
	report, stop := w.Observe(st, t0)
	if stop {
		t.Fatal("ended the wait on the first reading, before the stale-generation window")
	}
	if !strings.Contains(report, "Error Creating") || !strings.Contains(report, "yugaware log") {
		t.Errorf("report %q does not name the state and where the explanation is", report)
	}
	if _, stop := w.Observe(st, t0.Add(FailureSettleWindow)); !stop {
		t.Error("a rejection that held past the window did not end the wait")
	}
	// And it must not fire on the states that are merely in progress.
	for _, state := range []string{StateReady, StateCreating, StateEditing, StateDeleting} {
		var w FailureWatch
		w.Observe(Status{UniverseState: state}, t0)
		if _, stop := w.Observe(Status{UniverseState: state}, t0.Add(FailureSettleWindow)); stop {
			t.Errorf("watch fired on %q, which is not a failure", state)
		}
	}
}

// TestWatchRidesOutATransientErrorState is why even that one case waits.
//
// A healthy create was seen reporting "Error Creating" for a few seconds before
// the operator enqueued its task — universeState has no observedGeneration
// behind it, so a reading taken right after an apply can belong to the previous
// generation. Failing on it aborted an install that went on to reach Ready with
// six healthy pods.
func TestWatchRidesOutATransientErrorState(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var w FailureWatch
	if _, stop := w.Observe(Status{UniverseState: "Error Creating"}, t0); stop {
		t.Fatal("ended the wait on a single Error Creating reading")
	}
	// The operator picks the generation up: the state clears and work starts.
	running := Status{
		UniverseState: StateCreating,
		Actions:       []Action{{ActionType: "CreateKubernetesUniverse", Status: ActionRunning, TaskUUID: "t-1"}},
	}
	if _, stop := w.Observe(running, t0.Add(15*time.Second)); stop {
		t.Fatal("ended the wait on a running create")
	}
	if _, stop := w.Observe(Status{UniverseState: StateReady}, t0.Add(30*time.Second)); stop {
		t.Fatal("ended the wait on a Ready universe")
	}
	// The window must have been reset, not merely not yet elapsed: a second
	// flicker much later is a fresh window, not the tail of the first one.
	if _, stop := w.Observe(Status{UniverseState: "Error Creating"}, t0.Add(FailureSettleWindow*2)); stop {
		t.Fatal("the first flicker's elapsed time was carried into the second")
	}
}

// A create YBA accepted is never the rejection case, however bad its state
// looks: the task UUID is the evidence that it got that far.
func TestWatchDoesNotCallAnAcceptedCreateARejection(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := Status{
		UniverseState: "Error Creating",
		Actions:       []Action{{ActionType: "CreateKubernetesUniverse", Status: ActionQueued, TaskUUID: "t-1"}},
	}
	var w FailureWatch
	w.Observe(st, t0)
	if _, stop := w.Observe(st, t0.Add(FailureSettleWindow*5)); stop {
		t.Error("treated a queued task as a spec YBA never accepted")
	}
}
