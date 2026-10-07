package foundation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Why Events as well as conditions.
//
// A condition says what is true NOW and is overwritten in place, so a failure
// that healed leaves nothing behind: `kubectl get foundation` shows Ready and
// the half hour it spent rejected is gone. Events are the record of what
// HAPPENED, they are what `kubectl describe` prints, and they are where an
// operator looks first. The pair is the Kubernetes convention, and neither
// substitutes for the other.
//
// Emitted on CHANGE only, never per poll. An event every 30s would push the
// one that matters out of the window — the API server keeps them an hour by
// default — which is the usual way an event stream becomes useless.
const (
	// EventReasonConverging and friends are the vocabulary. Kept short and
	// CamelCase because that is what `kubectl describe` columns and every
	// convention expect.
	EventReasonConverging   = "Converging"
	EventReasonConverged    = "Converged"
	EventReasonFailed       = "ConvergeFailed"
	EventReasonSpecInvalid  = "SpecInvalid"
	EventReasonTearingDown  = "TearingDown"
	EventReasonTornDown     = "TornDown"
	EventReasonTeardownKept = "TeardownIncomplete"

	eventTypeNormal  = "Normal"
	eventTypeWarning = "Warning"
)

// reportingController identifies the writer, which is what lets an operator
// tell our events from the YugabyteDB operator's on a neighbouring object.
const reportingController = "meko.yugabyte.com/foundation"

// emit writes one Event about the Foundation.
//
// Best effort, and deliberately so: an event that cannot be written must never
// fail a converge. The status and the pod log still carry the same fact, so
// the cost of losing one is a less convenient history, not a blind operator.
func (r *Reconciler) emit(ctx context.Context, cr *FoundationCR, kind, reason, note string) {
	// The note lands in `kubectl describe`'s Message column, and the API server
	// caps it at 1KiB — a converge error carrying a kubectl stderr can exceed
	// that, and an over-long event is REJECTED rather than truncated.
	if len(note) > 900 {
		note = note[:900] + "… (truncated; see the object's conditions)"
	}
	now := r.now().UTC()
	ev := map[string]any{
		"apiVersion": "events.k8s.io/v1",
		"kind":       "Event",
		"metadata": map[string]any{
			// Unique per event: the API server will not take two with one name,
			// and a collision would silently drop the later one.
			"name":      fmt.Sprintf("%s.%x", cr.Metadata.Name, now.UnixNano()),
			"namespace": cr.Metadata.Namespace,
		},
		"regarding": map[string]any{
			"apiVersion": FoundationGroup + "/v1alpha1",
			"kind":       "Foundation",
			"name":       cr.Metadata.Name,
			"namespace":  cr.Metadata.Namespace,
		},
		"reason": reason,
		"note":   note,
		"type":   kind,
		// MicroTime, not RFC3339: events.k8s.io/v1 declares eventTime as
		// MicroTime and the API server rejects a plain second-resolution one.
		"eventTime":           now.Format("2006-01-02T15:04:05.000000Z"),
		"reportingController": reportingController,
		"reportingInstance":   reportingController,
		"action":              "Converge",
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return
	}
	res, err := r.cr.run(ctx, body, r.cr.argv("apply", "-f", "-"))
	if err != nil || res.ExitCode != 0 {
		r.log("could not record the %s event: %s", reason, strings.TrimSpace(res.Stderr))
	}
}

// emitOnChange records a transition and nothing else. It returns true when it
// emitted, which the tests use to pin that a steady state is silent.
func (r *Reconciler) emitOnChange(ctx context.Context, cr *FoundationCR, c Condition) bool {
	key := c.Reason + "/" + c.Status
	if r.lastEvent == key {
		return false
	}
	r.lastEvent = key

	kind, reason := eventTypeNormal, c.Reason
	note := c.Message
	switch c.Reason {
	case "ConvergeFailed", "SpecInvalid":
		kind = eventTypeWarning
	case "Converged":
		note = "the foundation is converged and reachable"
	case "Converging":
		if note == "" {
			note = "converging"
		}
	default:
		// Pending and NeverConverged are states, not events worth a line.
		return false
	}
	r.emit(ctx, cr, kind, reason, note)
	return true
}

// argv builds a kubectl command for this object's namespace.
func (c *crClient) argv(args ...string) []string {
	out := []string{"kubectl"}
	if c.kubeContext != "" {
		out = append(out, "--context", c.kubeContext)
	}
	return append(append(out, "-n", c.namespace), args...)
}
