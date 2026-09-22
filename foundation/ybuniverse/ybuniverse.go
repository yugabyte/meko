// Package ybuniverse renders the operator's custom resources and interprets
// their status. It is pure: no I/O, no binaries, no Kubernetes client.
//
// That split is not tidiness. The amp controller ships helm but no kubectl and
// is explicitly "not a client-go deployer" (amp/deploy/controller/Dockerfile),
// and the BYOC agent is a distroless-static image with no shell at all — so a
// consumer that manages a foundation has to speak the k8s REST API directly.
// Keeping CR rendering and status interpretation free of transport is what lets
// the same code serve the shelling CLI here and a REST client later, without
// either one re-deriving what "Ready" means.
//
// The spec bodies are passed through VERBATIM rather than modelled as Go
// structs, so any field the pinned CRD has is reachable and a new operator
// release needs no code here. Nothing re-checks them against the CRD's schema:
// `kubectl apply` sends strict field validation, so the API server rejects an
// undeclared field, a wrong type and a bad enum on its own — and a second copy
// of that schema here would have to be regenerated on every chart bump just to
// say the same thing earlier.
package ybuniverse

import (
	"fmt"
	"strings"
	"time"
)

// APIVersion and the kinds this package renders.
const (
	APIVersion   = "operator.yugabyte.io/v1alpha1"
	KindUniverse = "YBUniverse"
	KindProvider = "YBProvider"
)

// Universe states. The CRD's description lists these four as examples ("e.g.")
// and declares no enum, so they are NOT the whole set — a real universe whose
// create was rejected reports "Error Creating", which is why FailureWatch
// matches on the prefix below rather than on a known value.
const (
	StateReady    = "Ready"
	StateCreating = "Creating"
	StateEditing  = "Editing"
	StateDeleting = "Deleting"
)

// StateErrorPrefix is what YBA prepends to the operation it could not do:
// "Error Creating", "Error Updating". Not in the CRD's list, and NOT a failure
// on sight — a healthy create flickers through one. See FailureWatch.
const StateErrorPrefix = "Error"

// Action statuses. Note there is no "succeeded": a finished action leaves the
// list, so absence is success and these three are the whole enum.
const (
	ActionQueued  = "queued"
	ActionRunning = "running"
	ActionFailed  = "failed"
)

// Render builds a CR object of kind from a spec body passed through UNTOUCHED.
//
// `name` is a parameter because it is metadata.name, which is a different
// field from whatever the body calls itself — YBUniverse has spec.universeName,
// and spec.name is a required field of KMSConfig, PitrConfig and DrConfig and a
// real one on StorageConfig and BackupSchedule. So the body's own `name` is the
// CR's, never the object's, and stripping it would break those kinds.
func Render(kind, name, namespace string, spec map[string]any) map[string]any {
	body := make(map[string]any, len(spec))
	for k, v := range spec {
		body[k] = v
	}
	return map[string]any{
		"apiVersion": APIVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": body,
	}
}

// Status is the subset of YBUniverse.status this package reads. Status is a
// subresource, written by the operator.
//
// Note what is NOT here: there is no observedGeneration. A wait that polls
// universeState alone is therefore racy — see Settled.
type Status struct {
	UniverseState string   `json:"universeState"`
	SQLEndpoints  []string `json:"sqlEndpoints"`
	CQLEndpoints  []string `json:"cqlEndpoints"`
	ResourceUUID  string   `json:"resourceUUID"`
	Actions       []Action `json:"actions"`
}

// Action is one entry of status.actions: the operator's in-flight or failed
// work, carrying the YBA task UUID behind it.
type Action struct {
	ActionType string `json:"action_type"`
	Message    string `json:"message"`
	Status     string `json:"status"`
	TaskUUID   string `json:"taskUUID"`
}

// ProviderStatus is YBProvider.status: a state, a message, and the UUID YBA
// gave the provider it created.
type ProviderStatus struct {
	State        string `json:"state"`
	Message      string `json:"message"`
	ResourceUUID string `json:"resourceUUID"`
}

// Settled reports whether the universe has finished converging on the spec it
// was last given, and why not when it has not.
//
// The predicate is `universeState == Ready` AND no action queued or running —
// both halves are load-bearing. There is no observedGeneration in this CRD, so
// immediately after an apply the operator has not looked at the new spec and
// the state still reads Ready from the PREVIOUS generation: a wait on state
// alone returns success instantly, reporting an edit as applied that has not
// started. status.actions closes that hole because the operator enqueues an
// action before it begins work.
//
// This is the same failure ycp/cmd/agent/stack.go warns about for create-once
// applies ("reported applied, and never actually rolled out") and the one
// ycp/reconcile/engine.go's observed-generation echo exists to prevent.
func Settled(st Status) (bool, string) {
	for _, a := range st.Actions {
		switch a.Status {
		case ActionQueued, ActionRunning:
			return false, fmt.Sprintf("%s is %s (task %s)", describeAction(a), a.Status, a.TaskUUID)
		}
	}
	if st.UniverseState != StateReady {
		return false, fmt.Sprintf("universeState is %q", st.UniverseState)
	}
	return true, ""
}

// FailureSettleWindow is how long the one terminal signal must hold before the
// wait acts on it. It exists only to outlast the stale-generation read, so it
// is short.
const FailureSettleWindow = 2 * time.Minute

// FailureWatch reads what the CR says is wrong and decides the one case where
// the wait should stop early. It is deliberately not clever: a rejected spec
// fails asynchronously inside the operator, so without reading the CR back the
// failure presents as a wait that times out with the reason left somewhere
// nobody looked — and REPORTING that reason is the job. Adjudicating it is
// mostly not.
//
// A failed task is reported and nothing more, because this side cannot tell a
// dead create from one YBA is retrying. It retries with a backoff, and a retry
// keeps the SAME task UUID (only a re-run gets a new one), so neither elapsed
// time nor a count of tasks separates the two. Guessing costs a working
// install; waiting costs the rest of the universe timeout, which the operator
// can cut short themselves once the log has told them what broke.
//
// The exception is a spec YBA rejected BEFORE it submitted a task: the action
// stays "queued" with no UUID while universeState becomes "Error Creating" —
// observed on a spec whose ybSoftwareVersion was empty, where the only
// explanation was a RuntimeException in the yugaware log. Nothing will retry
// that, because YBA never accepted it, so the wait ends. It still has to
// outlast FailureSettleWindow: universeState has no observedGeneration behind
// it (see Settled), so a reading taken just after an apply can belong to the
// previous generation, and a healthy create was seen flickering through
// "Error Creating" seconds before its task appeared.
//
// The zero value is ready to use, and it is stateful: one per wait.
type FailureWatch struct {
	// Window overrides FailureSettleWindow. Zero uses it.
	Window time.Duration

	since  time.Time
	reason string
}

// Reason is the failure the CR currently shows, or "" when it shows none.
func (w *FailureWatch) Reason() string { return w.reason }

// Observe folds one status reading, taken at now, into the watch.
//
// report is non-empty the FIRST time a given failure is seen, for the caller to
// log — a failure is worth telling someone about whether or not it ends the
// wait. stop reports whether to give up.
//
// now is a parameter rather than time.Now() so this package stays free of
// ambient state, and so a test can advance time without sleeping.
func (w *FailureWatch) Observe(st Status, now time.Time) (report string, stop bool) {
	reason, terminal := failureSignal(st)
	if reason == "" {
		w.since, w.reason = time.Time{}, ""
		return "", false
	}
	if reason != w.reason {
		w.reason, w.since = reason, now
		report = reason
	}
	if !terminal {
		return report, false
	}
	window := w.Window
	if window <= 0 {
		window = FailureSettleWindow
	}
	return report, now.Sub(w.since) >= window
}

// failureSignal reports what the CR says is wrong, and whether it is a failure
// nothing can retry.
func failureSignal(st Status) (reason string, terminal bool) {
	for _, a := range st.Actions {
		if a.Status == ActionFailed {
			msg := a.Message
			if msg == "" {
				msg = "no message"
			}
			return fmt.Sprintf("%s failed: %s (task %s) — YBA may retry it; the cause is in the "+
				"yugaware log", describeAction(a), msg, a.TaskUUID), false
		}
	}
	// No task anywhere is what separates a rejection from a create in trouble:
	// YBA writes a UUID as soon as it accepts the work.
	if strings.HasPrefix(st.UniverseState, StateErrorPrefix) && !hasTask(st) {
		return fmt.Sprintf("universeState is %q and no action carries a task — YBA rejected the "+
			"spec before it created one, so the explanation is only in the yugaware log",
			st.UniverseState), true
	}
	return "", false
}

func hasTask(st Status) bool {
	for _, a := range st.Actions {
		if a.TaskUUID != "" {
			return true
		}
	}
	return false
}

func describeAction(a Action) string {
	if a.ActionType == "" {
		return "action"
	}
	return a.ActionType
}
