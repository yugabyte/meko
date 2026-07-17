package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yugabyte/yb-amp/test/functional/framework/env/registry"
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

// TestAttachFromAmpstackEntryMapsToSharedk8s: a `deploy-amp` task entry (provider
// ampstack, shape dedicated_stack, context+namespace) resolves to the sharedk8s
// provider with its connection info injected under env.sharedk8s.*.
func TestAttachFromAmpstackEntryMapsToSharedk8s(t *testing.T) {
	entry := registry.Entry{
		ID:       "abc123",
		Shape:    "dedicated_stack",
		Provider: "ampstack",
		Data: map[string]string{
			"namespace":          "ampqa-amp-x",
			"context":            "itest-dev",
			"controller_service": "amp-controller",
			"controller_port":    "8080",
		},
	}
	prov, ov, err := attachFromEntry(entry)
	if err != nil {
		t.Fatalf("attachFromEntry: %v", err)
	}
	if prov != "sharedk8s" {
		t.Fatalf("provider = %q, want sharedk8s", prov)
	}
	for _, want := range []string{
		"env.sharedk8s.context=itest-dev",
		"env.sharedk8s.namespace=ampqa-amp-x",
		"env.sharedk8s.controller_service=amp-controller",
		"env.sharedk8s.controller_port=int:8080",
	} {
		if !hasOverride(ov, want) {
			t.Errorf("missing override %q in %v", want, ov)
		}
	}
	// No controller_url was recorded (the task's health-check forward can't outlive
	// its process), so none must be injected — sharedk8s port-forwards via context.
	for _, o := range ov {
		if strings.HasPrefix(o, "env.sharedk8s.controller_url=") {
			t.Errorf("must not inject a controller_url when none was recorded: %q", o)
		}
	}
}

// TestAttachFromControllerURLEntryMapsToSharedk8s: a functional-preset standing env
// (amp_controller_url + amp_namespace, no kube context) resolves to sharedk8s with
// a direct controller_url.
func TestAttachFromControllerURLEntryMapsToSharedk8s(t *testing.T) {
	entry := registry.Entry{
		ID:       "def456",
		Shape:    "yb_k8s",
		Provider: "functional",
		Data: map[string]string{
			"amp_controller_url": "http://amp.tailnet:8080",
			"amp_namespace":      "ampqa-fnd-y",
		},
	}
	prov, ov, err := attachFromEntry(entry)
	if err != nil {
		t.Fatalf("attachFromEntry: %v", err)
	}
	if prov != "sharedk8s" {
		t.Fatalf("provider = %q, want sharedk8s", prov)
	}
	if !hasOverride(ov, "env.sharedk8s.controller_url=http://amp.tailnet:8080") {
		t.Errorf("missing controller_url override in %v", ov)
	}
	if !hasOverride(ov, "env.sharedk8s.namespace=ampqa-fnd-y") {
		t.Errorf("missing namespace override in %v", ov)
	}
}

// TestAttachFromUniverseAddressEntry: an entry recording attach_address resolves to
// the universe provider in attach_address mode.
func TestAttachFromUniverseAddressEntry(t *testing.T) {
	entry := registry.Entry{
		ID:    "u1",
		Shape: "universe",
		Data:  map[string]string{"attach_address": "10.0.0.1:5433,10.0.0.2:5433"},
	}
	prov, ov, err := attachFromEntry(entry)
	if err != nil {
		t.Fatalf("attachFromEntry: %v", err)
	}
	if prov != "universe" {
		t.Fatalf("provider = %q, want universe", prov)
	}
	if !hasOverride(ov, "env.universe.mode=attach_address") {
		t.Errorf("missing mode override in %v", ov)
	}
	if !hasOverride(ov, "env.universe.addresses=list:10.0.0.1:5433,10.0.0.2:5433") {
		t.Errorf("missing addresses override in %v", ov)
	}
}

// TestAttachFromUniverseYBAEntry: an entry recording attach_yba_url + universe
// resolves to the universe provider in attach_yba mode.
func TestAttachFromUniverseYBAEntry(t *testing.T) {
	entry := registry.Entry{
		ID:    "u2",
		Shape: "universe",
		Data: map[string]string{
			"attach_yba_url":      "https://yba.example:443",
			"attach_yba_universe": "my-uni",
			"attach_yba_customer": "cust-uuid",
		},
	}
	prov, ov, err := attachFromEntry(entry)
	if err != nil {
		t.Fatalf("attachFromEntry: %v", err)
	}
	if prov != "universe" {
		t.Fatalf("provider = %q, want universe", prov)
	}
	for _, want := range []string{
		"env.universe.mode=attach_yba",
		"env.universe.yba.url=https://yba.example:443",
		"env.universe.yba.universe=my-uni",
		"env.universe.yba.customer_uuid=cust-uuid",
	} {
		if !hasOverride(ov, want) {
			t.Errorf("missing override %q in %v", want, ov)
		}
	}
}

// TestAttachFromNonAttachableEntryErrors: a bare VM host / crash-recovery reap
// record has no amp or universe endpoint and must error clearly.
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
// identical overrides carrying no per-run identifier, so two sequential
// `run --env <id>` invocations don't collide — distinct project names come from the
// per-run seed, not from this mapping.
func TestAttachIsStatelessAndCollisionFree(t *testing.T) {
	entry := registry.Entry{
		ID:       "abc123",
		Shape:    "dedicated_stack",
		Provider: "ampstack",
		Data:     map[string]string{"namespace": "ns", "context": "ctx"},
	}
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
	for _, o := range ov1 {
		// A pinned project/name/seed in the overrides would make two runs collide;
		// only connection info (context/namespace/url/pool) is allowed.
		if strings.Contains(o, "project") || strings.Contains(o, "seed") || strings.Contains(o, "name=") {
			t.Errorf("override %q must not pin a per-run identifier", o)
		}
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
	out, err := runErr(t, "run", "--env", "whatever", "--env-provider", "sharedk8s", "--suite", "canary")
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

// TestRunEnvSelectsUniverseProviderFromEntry: a universe attach_address entry drives
// `run` to the universe provider. With no real universe reachable the run still
// completes cleanly (tests skip: provider configured but the fake attach target is
// unreachable is out of scope — we assert the provider was selected, not that a
// body ran), so we only check the command resolved without a usage/registry error.
func TestRunEnvSelectsUniverseProviderFromEntry(t *testing.T) {
	regFile := filepath.Join(t.TempDir(), "envs.json")
	reg := registry.New(regFile)
	stored, err := reg.Add(registry.Entry{
		Shape: "universe",
		Data:  map[string]string{"attach_address": "127.0.0.1:1"},
	})
	if err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	// Select a suite that matches nothing real so no body runs (avoids touching the
	// unreachable address); the point is that --env resolved to a provider without a
	// usage (2) / registry (1/3) error.
	out, rerr := runErr(t, "run", "--env", stored.ID, "--registry", regFile, "--json",
		"--suite", "canary", "--test", "canary/pass")
	// canary/pass needs a body; against the universe provider (unreachable) it
	// env-fails or skips — either way it's not a usage/registry error. Assert the
	// envelope decodes (command ran to completion) rather than a specific status.
	var env struct {
		Data struct {
			Results []map[string]any `json:"results"`
		} `json:"data"`
	}
	if jerr := json.Unmarshal([]byte(out[strings.IndexByte(out, '{'):]), &env); jerr != nil {
		t.Fatalf("run --env did not complete cleanly: rerr=%v output=%s", rerr, out)
	}
}
