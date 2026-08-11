// Package kubemeta is the vocabulary ampqa stamps on the Kubernetes objects it
// owns. Constants only, no behaviour and no dependencies — the transport
// (framework/exec/k8s) is about talking to a cluster, and every app that creates
// objects needs this without importing the others.
package kubemeta

import "time"

// TestLabel marks every Kubernetes object the ampqa framework creates — the
// namespaces it stands environments up in, and the pods it runs inside them.
//
// It exists so an out-of-band janitor can find ampqa's leftovers without
// knowing which app created them. A run that is killed (aborted build, lost
// agent, test timeout) never reaches its own teardown, and what it abandons is
// expensive: an endpoint pod requests 2 CPU, and node autoprovisioning is capped
// cluster-wide, so a few stranded environments exhaust the ceiling and every
// later run's pods sit Pending.
//
// The existing ampqa-managed-by label cannot serve that purpose: its VALUE names
// the creating app (ampstack, ybastack, ybpod, ybk8s), so a janitor selecting on
// it has to enumerate them and silently misses any new one. This label is the
// same for all of them, deliberately.
const (
	TestLabel      = "ampqa-test"
	TestLabelValue = "true"
)

// TestLabels returns the labels every ampqa-created object carries, merged with
// the caller's extras. Extras cannot override the identifying pair: a namespace
// the janitor cannot recognise is a namespace nobody reclaims.
func TestLabels(extra map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range extra {
		out[k] = v
	}
	out["ampqa"] = "1"
	out[TestLabel] = TestLabelValue
	return out
}

// Keep markers record that a run was ASKED to leave an environment up
// (`ampqa run --env-mode keep`, `--setup-only`), so an out-of-band janitor can
// tell a held environment from an abandoned one and stop reaping it on age.
//
// They live on the namespace because that is the only durable place. The
// framework's own registry of kept environments is a file in the working
// directory, and a Jenkins build's container is destroyed when the build ends —
// so a kept environment from CI is invisible to `ampqa env rm` from then on, and
// the cluster is the sole record that it was deliberate.
//
// KeepLabel is a label so a selector can find it; KeepUntil is an ANNOTATION
// because a label value cannot contain the colons of an RFC3339 timestamp.
// An absent KeepUntil means "held until someone says otherwise" — a standing dev
// environment, which a janitor reports rather than reaps.
const (
	KeepLabel      = "ampqa-keep"
	KeepLabelValue = "true"
	KeepUntil      = "ampqa-keep-until"
)

// KeepMarkers are the label and annotation that mark a namespace as
// deliberately held. until is zero for an environment with no expiry.
func KeepMarkers(until time.Time) (labels, annotations map[string]string) {
	labels = map[string]string{KeepLabel: KeepLabelValue}
	annotations = map[string]string{}
	if !until.IsZero() {
		annotations[KeepUntil] = until.UTC().Format(time.RFC3339)
	}
	return labels, annotations
}
