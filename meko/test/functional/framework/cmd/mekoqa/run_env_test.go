package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/env/registry"
)

// hasOverride reports whether overrides carries the given typed key=value entry.
func hasOverride(overrides []string, want string) bool {
	for _, o := range overrides {
		if o == want {
			return true
		}
	}
	return false
}

// TestAttachFromStackEntryMapsToK8s: a standing stack (kube context + namespace)
// resolves to the k8s provider with its location injected under env.k8s.*.
func TestAttachFromStackEntryMapsToK8s(t *testing.T) {
	entry := registry.Entry{
		ID:    "abc123",
		Shape: "single_stack",
		Data:  map[string]string{"namespace": "mekoqa-st-x", "context": "itest-dev"},
	}
	prov, ov, err := attachFromEntry(entry)
	if err != nil {
		t.Fatalf("attachFromEntry: %v", err)
	}
	if prov != "k8s" {
		t.Fatalf("provider = %q, want k8s", prov)
	}
	for _, want := range []string{"env.k8s.context=itest-dev", "env.k8s.namespace=mekoqa-st-x"} {
		if !hasOverride(ov, want) {
			t.Errorf("missing override %q in %v", want, ov)
		}
	}
}

// TestAttachFromNonAttachableEntryErrors: a bare VM host / crash-recovery reap
// record has no kube context and namespace and must error clearly.
func TestAttachFromNonAttachableEntryErrors(t *testing.T) {
	for _, entry := range []registry.Entry{
		{ID: "vm1", Shape: "yugabyted", Provider: "vm", Data: map[string]string{"run_tag": "abcd", "cloud": "aws"}},
		{ID: "rr1", Shape: "functional-run", Provider: "functional", Data: map[string]string{"reap_plan": "{}"}},
	} {
		if _, _, err := attachFromEntry(entry); err == nil {
			t.Errorf("entry %s (shape %q) must be rejected as non-attachable", entry.ID, entry.Shape)
		}
	}
}

// TestAttachIsStatelessAndCollisionFree: resolving the same entry twice yields
// identical overrides carrying no per-run identifier.
func TestAttachIsStatelessAndCollisionFree(t *testing.T) {
	entry := registry.Entry{ID: "abc123", Shape: "single_stack", Data: map[string]string{"namespace": "ns", "context": "ctx"}}
	_, ov1, err := attachFromEntry(entry)
	if err != nil {
		t.Fatalf("attachFromEntry #1: %v", err)
	}
	_, ov2, err := attachFromEntry(entry)
	if err != nil {
		t.Fatalf("attachFromEntry #2: %v", err)
	}
	if strings.Join(ov1, ",") != strings.Join(ov2, ",") {
		t.Fatalf("overrides differ across calls: %v vs %v", ov1, ov2)
	}
}

// --- end-to-end through the run command (usage-error paths, no infra) ---

func runErr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := buildRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// TestRunEnvConflictsWithFakeEnv: --env and --fake-env together is a usage error.
func TestRunEnvConflictsWithFakeEnv(t *testing.T) {
	out, err := runErr(t, "run", "--env", "whatever", "--fake-env", "--suite", "canary")
	if err == nil {
		t.Fatalf("expected a usage error, got none (output: %s)", out)
	}
	if !strings.Contains(err.Error(), "conflicts with --fake-env") {
		t.Errorf("error = %v, want a --fake-env conflict", err)
	}
}

// TestRunEnvConflictsWithEnvProvider: --env and --env-provider together is a usage
// error (the provider is selected from the standing env).
func TestRunEnvConflictsWithEnvProvider(t *testing.T) {
	out, err := runErr(t, "run", "--env", "whatever", "--env-provider", "k8s", "--suite", "canary")
	if err == nil {
		t.Fatalf("expected a usage error, got none (output: %s)", out)
	}
	if !strings.Contains(err.Error(), "do not also pass --env-provider") {
		t.Errorf("error = %v, want an --env-provider conflict", err)
	}
}

// TestRunEnvUnknownIDErrors: --env with an id absent from the registry errors.
func TestRunEnvUnknownIDErrors(t *testing.T) {
	regFile := filepath.Join(t.TempDir(), "envs.json")
	out, err := runErr(t, "run", "--env", "missing", "--registry", regFile, "--suite", "canary")
	if err == nil {
		t.Fatalf("expected a not-found error, got none (output: %s)", out)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want a not-found error", err)
	}
}

// TestRunEnvNonAttachableEntryErrors: --env pointing at a non-attachable record
// (a crash-recovery reap entry) surfaces the attachFromEntry error through run.
func TestRunEnvNonAttachableEntryErrors(t *testing.T) {
	regFile := filepath.Join(t.TempDir(), "envs.json")
	reg := registry.New(regFile)
	stored, err := reg.Add(registry.Entry{Shape: "functional-run", Provider: "functional", Data: map[string]string{"reap_plan": "{}"}})
	if err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	out, rerr := runErr(t, "run", "--env", stored.ID, "--registry", regFile, "--suite", "canary")
	if rerr == nil {
		t.Fatalf("expected a not-attachable error, got none (output: %s)", out)
	}
	if !strings.Contains(rerr.Error(), "not attachable") {
		t.Errorf("error = %v, want a not-attachable error", rerr)
	}
}
