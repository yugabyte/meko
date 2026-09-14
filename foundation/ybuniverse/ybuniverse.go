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
)

// APIVersion and the kinds this package renders.
const (
	APIVersion   = "operator.yugabyte.io/v1alpha1"
	KindUniverse = "YBUniverse"
	KindProvider = "YBProvider"
)

// Universe states. The CRD's description lists these four as examples ("e.g.")
// and declares no enum, so they are NOT the whole set — a real universe whose
// create was rejected reports "Error Creating", which is why Failed matches on
// the prefix below rather than on a known value.
const (
	StateReady    = "Ready"
	StateCreating = "Creating"
	StateEditing  = "Editing"
	StateDeleting = "Deleting"
)

// StateErrorPrefix is what YBA prepends to the operation it could not do:
// "Error Creating", "Error Updating". Terminal, and not in the CRD's list.
const StateErrorPrefix = "Error"

// Action statuses. Note there is no "succeeded": a finished action leaves the
// list, so absence is success and these three are the whole enum.
const (
	ActionQueued  = "queued"
	ActionRunning = "running"
	ActionFailed  = "failed"
)

// Render builds a CR object of kind from a spec body passed through as given.
//
// `name` is a parameter rather than read from the body, because the two CRs
// spell their name differently — YBUniverse has spec.universeName, YBProvider
// has none at all. Keeping metadata.name and the body in step is the caller's
// job; render.go does it at both call sites.
func Render(kind, name, namespace string, spec map[string]any) map[string]any {
	body := make(map[string]any, len(spec))
	for k, v := range spec {
		body[k] = v
	}
	// `name` is the object's identity, not part of YBProvider.spec, so it is
	// lifted into metadata rather than sent as a spec field the CRD would reject.
	delete(body, "name")
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

// Failed reports whether the universe is in a terminal failure, with whatever
// reason is available. A rejected spec fails asynchronously inside the
// operator, so without this a bad spec presents as a wait that times out and
// the actual reason stays in a CR nobody read.
//
// Two shapes, and the second is not optional. An action can carry status
// "failed" with a message and a task UUID — the good case. But YBA can also
// reject a create BEFORE it submits a task, and then the action stays "queued"
// with no message and no UUID while universeState becomes "Error Creating":
// observed on a spec whose ybSoftwareVersion was empty, where the only
// explanation was a RuntimeException in the yugaware log. Matching just the
// action status would wait that out for the full timeout.
func Failed(st Status) (bool, string) {
	for _, a := range st.Actions {
		if a.Status == ActionFailed {
			msg := a.Message
			if msg == "" {
				msg = "no message"
			}
			return true, fmt.Sprintf("%s failed: %s (task %s)", describeAction(a), msg, a.TaskUUID)
		}
	}
	if strings.HasPrefix(st.UniverseState, StateErrorPrefix) {
		// No message to quote: YBA writes the state and nothing else, so say
		// where the reason actually is rather than implying we have it.
		return true, fmt.Sprintf("universeState is %q and no action carries a reason — YBA rejected "+
			"the spec before it created a task, so the explanation is only in the yugaware log "+
			"(kubectl logs statefulset/<release>-yugaware -c yugaware)", st.UniverseState)
	}
	return false, ""
}

func describeAction(a Action) string {
	if a.ActionType == "" {
		return "action"
	}
	return a.ActionType
}
