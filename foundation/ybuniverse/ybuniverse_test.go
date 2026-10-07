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

// TestWatchReportsAFailedTaskAndNothingMore.
//
// A failed task is reported, and that is all: YBA retries with a backoff, and a
// retry keeps the SAME task UUID, so nothing on this side separates a dead
// create from one that is about to succeed. The report is what the operator
// actually needs.
func TestWatchReportsAFailedTaskAndNothingMore(t *testing.T) {
	st := Status{UniverseState: "Error Creating", Actions: []Action{
		{ActionType: "EditGFlags", Status: ActionFailed, Message: "preview flag not allowed", TaskUUID: "t-9"},
	}}
	var w FailureWatch
	report := w.Observe(st)
	for _, want := range []string{"EditGFlags", "preview flag not allowed", "t-9", "yugaware log"} {
		if !strings.Contains(report, want) {
			t.Errorf("report %q is missing %q", report, want)
		}
	}
	// Reported ONCE, not on every poll.
	if report := w.Observe(st); report != "" {
		t.Errorf("re-reported the same failure: %q", report)
	}
	// A clean status clears it, so a later failure is reported again.
	if report := w.Observe(Status{UniverseState: StateReady}); report != "" {
		t.Errorf("reported a failure for a clean status: %q", report)
	}
	if w.Reason() != "" {
		t.Errorf("Reason = %q after the failure cleared", w.Reason())
	}
	if report := w.Observe(st); report == "" {
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

// TestWatchNeverEndsTheWait is the regression test for the verdict this used to
// make.
//
// YBA can reject a create BEFORE submitting a task: universeState becomes
// "Error Creating" while the action stays "queued" with no task UUID. That was
// treated as terminal on the grounds that YBA never accepted the spec and so
// nothing would retry it — which is false. The reconciler throws before
// submitting, AbstractReconciler swallows it, and the NO_OP enqueued alongside
// the CREATE requeues CREATE; OperatorWorkQueue caps the backoff at 1800s and
// the retry count at nothing. Ending the wait abandoned installs that recover.
//
// Observe now reports and returns, so the only thing this can assert is that
// every shape reports once and keeps reporting changes.
func TestWatchNeverEndsTheWait(t *testing.T) {
	rejected := Status{
		UniverseState: "Error Creating",
		Actions:       []Action{{ActionType: "CreateKubernetesUniverse", Status: ActionQueued}},
	}
	var w FailureWatch
	report := w.Observe(rejected)
	if !strings.Contains(report, "Error Creating") || !strings.Contains(report, "yugaware log") {
		t.Errorf("report %q does not name the state and where the explanation is", report)
	}
	if !strings.Contains(report, "retries") {
		t.Errorf("report %q does not say YBA retries it, which is why the wait continues", report)
	}
	// However long it sits there, it stays a report.
	for i := 0; i < 100; i++ {
		if r := w.Observe(rejected); r != "" {
			t.Fatalf("re-reported an unchanged rejection on poll %d: %q", i, r)
		}
	}

	// The states that are merely in progress report nothing at all.
	for _, state := range []string{StateReady, StateCreating, StateEditing, StateDeleting} {
		var w FailureWatch
		if r := w.Observe(Status{UniverseState: state}); r != "" {
			t.Errorf("reported %q, which is not a failure: %q", state, r)
		}
	}
}

// A transient "Error Creating" is why ending the wait on it was wrong twice
// over: a healthy create was seen reporting it for a few seconds before the
// operator enqueued its task, because universeState has no observedGeneration
// behind it and a reading taken right after an apply can belong to the previous
// generation.
func TestWatchRidesOutATransientErrorState(t *testing.T) {
	var w FailureWatch
	if r := w.Observe(Status{UniverseState: "Error Creating"}); r == "" {
		t.Fatal("the first Error Creating reading was not reported")
	}
	// The operator picks the generation up: the state clears and work starts.
	running := Status{
		UniverseState: StateCreating,
		Actions:       []Action{{ActionType: "CreateKubernetesUniverse", Status: ActionRunning, TaskUUID: "t-1"}},
	}
	if r := w.Observe(running); r != "" {
		t.Errorf("reported a failure for a running create: %q", r)
	}
	if w.Reason() != "" {
		t.Errorf("Reason = %q once the universe was progressing", w.Reason())
	}
	if r := w.Observe(Status{UniverseState: StateReady}); r != "" {
		t.Errorf("reported a failure for a Ready universe: %q", r)
	}
}

// A create YBA accepted reports the accepted-but-failing wording, not the
// rejection wording: the task UUID is the evidence that it got that far.
func TestWatchDistinguishesAnAcceptedCreateFromARejection(t *testing.T) {
	accepted := Status{
		UniverseState: "Error Creating",
		Actions:       []Action{{ActionType: "CreateKubernetesUniverse", Status: ActionQueued, TaskUUID: "t-1"}},
	}
	var w FailureWatch
	if r := w.Observe(accepted); r != "" {
		t.Errorf("a queued task with a UUID is not a failure to report: %q", r)
	}
}
