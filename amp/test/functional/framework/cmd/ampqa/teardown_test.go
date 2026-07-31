package main

import (
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/env/reap"
	"github.com/yugabyte/amp/test/functional/framework/env/registry"
)

// TestAmpstackNSPlanFromRecordedNamespace: an ampstack standing env with no
// serialized reap_plan (a pre-reap-plan setup, e.g. the CE-1655 dev sandbox) is
// still reapable — env rm reconstructs a KindAmp namespace reap from the recorded
// namespace + context rather than failing on the non-existent "ampstack" provider.
func TestAmpstackNSPlanFromRecordedNamespace(t *testing.T) {
	entry := registry.Entry{
		ID:       "2cfa5957",
		Provider: "ampstack",
		Data: map[string]string{
			"namespace": "ampqa-dev",
			"context":   "gke_yugabyte_us-west1_itest-dev",
		},
	}
	plan, err := ampstackNSPlan(entry)
	if err != nil {
		t.Fatalf("ampstackNSPlan: %v", err)
	}
	if plan.Empty() {
		t.Fatal("plan is empty; want one namespace reap")
	}
	if len(plan.Namespaces) != 1 {
		t.Fatalf("namespaces = %d, want 1", len(plan.Namespaces))
	}
	n := plan.Namespaces[0]
	if n.Kind != reap.KindAmp {
		t.Errorf("kind = %q, want %q", n.Kind, reap.KindAmp)
	}
	if n.Namespace != "ampqa-dev" {
		t.Errorf("namespace = %q, want ampqa-dev", n.Namespace)
	}
	if n.KubeContext != "gke_yugabyte_us-west1_itest-dev" {
		t.Errorf("kubecontext = %q, want the recorded context", n.KubeContext)
	}
}

// TestAmpstackNSPlanNoNamespaceErrors: without a recorded namespace there is
// nothing to reap, so env rm must fail loudly (record kept) rather than silently
// dropping the entry and orphaning infrastructure.
func TestAmpstackNSPlanNoNamespaceErrors(t *testing.T) {
	entry := registry.Entry{ID: "abc123", Provider: "ampstack", Data: map[string]string{}}
	if _, err := ampstackNSPlan(entry); err == nil {
		t.Fatal("want an error when no namespace is recorded, got nil")
	}
}
