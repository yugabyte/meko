package foundation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// emittedEvents decodes every Event the loop applied.
func emittedEvents(t *testing.T, rec *Recorder) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, c := range rec.Calls {
		if c.Stdin == "" || !strings.Contains(c.Stdin, `"kind":"Event"`) {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(c.Stdin), &ev); err != nil {
			t.Fatalf("an emitted event is not valid JSON: %v\n%s", err, c.Stdin)
		}
		out = append(out, ev)
	}
	return out
}

// TestAFailureIsRecordedAsAWarningEvent.
//
// Conditions say what is true NOW and are overwritten in place, so a failure
// that later heals leaves nothing behind. Events are the record that it
// happened, and they are what `kubectl describe foundation` prints — which is
// where an operator looks first.
func TestAFailureIsRecordedAsAWarningEvent(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, nil)}},
	}}
	f := &fakeConverger{applyErr: errFromApply("YBUniverse", "numNodez: field not declared in schema")}
	r := testReconciler(t, rec, f)

	r.tick(context.Background())
	r.settle(t)
	r.tick(context.Background()) // reaps the failure and writes status + event

	evs := emittedEvents(t, rec)
	if len(evs) == 0 {
		t.Fatal("a failed converge produced no event; `kubectl describe` would show nothing")
	}
	last := evs[len(evs)-1]
	if last["type"] != "Warning" {
		t.Errorf("type = %v, want Warning — a failure listed as Normal is filtered out by "+
			"every `--field-selector type=Warning` an operator uses", last["type"])
	}
	if last["reason"] != EventReasonFailed {
		t.Errorf("reason = %v, want %s", last["reason"], EventReasonFailed)
	}
	if note, _ := last["note"].(string); !strings.Contains(note, "numNodez") {
		t.Errorf("the note does not carry the reason:\n%s", note)
	}
	// It has to point AT the Foundation, or `kubectl describe foundation`
	// shows nothing however correct the event is.
	reg, _ := last["regarding"].(map[string]any)
	if reg["kind"] != "Foundation" || reg["name"] != "foundation" {
		t.Errorf("regarding = %v, want the Foundation", reg)
	}
	if reg["namespace"] != "amp" {
		t.Errorf("regarding.namespace = %v, want the object's", reg["namespace"])
	}
}

// A steady state is SILENT. One event per poll would be 120 an hour, pushing
// the one that matters out of the API server's retention window — the usual
// way an event stream stops being worth reading.
func TestASteadyStateEmitsNothingFurther(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, nil)}},
	}}
	f := &fakeConverger{}
	r := testReconciler(t, rec, f)

	for range 5 {
		r.tick(context.Background())
		r.settle(t)
	}
	evs := emittedEvents(t, rec)
	if len(evs) > 2 {
		var reasons []string
		for _, e := range evs {
			reasons = append(reasons, e["reason"].(string))
		}
		t.Errorf("five passes emitted %d events (%s); a steady state should settle",
			len(evs), strings.Join(reasons, ", "))
	}
}

// And a transition back is recorded too, so the history reads as a story
// rather than only the bad half.
func TestRecoveryIsRecorded(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, nil)}, Once: true},
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, nil)}, Once: true},
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 2, nil)}},
	}}
	f := &fakeConverger{applyErr: errFromApply("YBUniverse", "bad")}
	r := testReconciler(t, rec, f)

	r.tick(context.Background()) // converge gen 1
	r.settle(t)
	r.tick(context.Background()) // reap the failure -> Warning

	f.mu.Lock()
	f.applyErr = nil // the operator fixes it
	f.mu.Unlock()

	r.tick(context.Background()) // gen 2 converges
	r.settle(t)
	r.tick(context.Background()) // reap the success -> Normal

	var reasons []string
	for _, e := range emittedEvents(t, rec) {
		reasons = append(reasons, e["reason"].(string))
	}
	joined := strings.Join(reasons, ",")
	if !strings.Contains(joined, EventReasonFailed) || !strings.Contains(joined, EventReasonConverged) {
		t.Errorf("events = [%s], want the failure AND the recovery", joined)
	}
}

// The note is capped. events.k8s.io caps it at 1KiB and REJECTS an over-long
// one rather than truncating, so a converge error carrying a whole kubectl
// stderr would silently produce no event at all.
func TestALongNoteIsTruncatedRatherThanRejected(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, nil)}},
	}}
	r := testReconciler(t, rec, &fakeConverger{})
	cr := &FoundationCR{}
	cr.Metadata.Name, cr.Metadata.Namespace = "foundation", "amp"

	r.emit(context.Background(), cr, eventTypeWarning, EventReasonFailed, strings.Repeat("x", 4000))
	evs := emittedEvents(t, rec)
	if len(evs) != 1 {
		t.Fatalf("emitted %d events, want 1", len(evs))
	}
	note, _ := evs[0]["note"].(string)
	if len(note) >= 1024 {
		t.Errorf("note is %d bytes; the API server rejects over 1KiB outright", len(note))
	}
	if !strings.Contains(note, "truncated") {
		t.Error("a truncated note does not say it was truncated")
	}
}

// errFromApply is the error applyCRs returns for a rejected CR.
func errFromApply(kind, stderr string) error {
	return &IncompleteError{
		Step: "apply the custom resources",
		ID:   StepApplyResources,
		Err:  errString("apply " + kind + ": " + stderr),
	}
}

type errString string

func (e errString) Error() string { return string(e) }
