package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/fake"
	"github.com/yugabyte/amp/test/functional/framework/env/registry"
)

// A fake-backing setup-capable test, registered only for these CLI tests: its
// Setup provisions nothing real, just returns a standing-env handle whose ref a
// fake provider can attach+teardown. Lets the whole setup → env ls → env rm CLI
// flow run with no infrastructure.
func init() {
	core.Register(core.Test{
		Name:     "test/fake_standing",
		Suite:    "test",
		Category: core.Functional,
		Env:      core.EnvSpec{Sharing: core.Persistent, Shape: core.Universe},
		Setup: func(t *core.T) (*core.SetupResult, error) {
			return &core.SetupResult{
				Env: &core.StandingEnv{
					Name:     "cli-fake",
					Shape:    "fake",
					Provider: "fake",
					Ref:      "fake-env-1",
					Info:     map[string]string{"ysql_dsn": "host=fake port=5433"},
				},
				Teardown: func(context.Context) error { return nil },
			}, nil
		},
	})
}

// TestTeardownViaProviderReapsRef proves the env-rm reaping seam: reconstruct
// an environment from a serialized ref via the provider's Attach and tear it
// down. The fake provider records the Teardown call.
func TestTeardownViaProviderReapsRef(t *testing.T) {
	p := &fake.Provider{}
	if err := teardownViaProvider(context.Background(), p, "fake-env-1"); err != nil {
		t.Fatalf("teardownViaProvider: %v", err)
	}
	if p.TeardownCount() != 1 {
		t.Errorf("teardown count = %d, want 1", p.TeardownCount())
	}
}

// TestReapStandingEnvProviderPath exercises reapStandingEnv on a fake-provider
// entry (no run_tag): it rebuilds the fake provider from the entry and reaps.
func TestReapStandingEnvProviderPath(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	secrets, err := core.NewSecrets(cfg)
	if err != nil {
		t.Fatalf("secrets: %v", err)
	}
	masker := core.NewMaskerFromSecrets(secrets)
	entry := registry.Entry{
		ID: "abc", Provider: "fake", Shape: "fake",
		Data: map[string]string{"ref": "fake-env-1"},
	}
	if err := reapStandingEnv(context.Background(), entry, cfg, secrets, masker, "", core.DetectedVersions{}, false); err != nil {
		t.Fatalf("reapStandingEnv: %v", err)
	}
}

// TestReapStandingEnvPrefersReapPlan: an entry carrying a serialized reap.Plan
// is reaped via that plan (not the provider Attach path), even when a provider is
// also named. A plan with an unknown namespace-reap kind fails deterministically
// WITHOUT touching any infrastructure, so the surfaced error proves the plan path
// ran — and that a failed reap is reported loudly, never as silent success.
func TestReapStandingEnvPrefersReapPlan(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	secrets, err := core.NewSecrets(cfg)
	if err != nil {
		t.Fatalf("secrets: %v", err)
	}
	masker := core.NewMaskerFromSecrets(secrets)
	entry := registry.Entry{
		ID: "abc", Provider: "fake", Shape: "yba",
		Data: map[string]string{
			"ref":       "fake-env-1",
			"reap_plan": `{"namespaces":[{"kind":"bogus","namespace":"ampqa-yba-1"}]}`,
		},
	}
	err = reapStandingEnv(context.Background(), entry, cfg, secrets, masker, "", core.DetectedVersions{}, false)
	if err == nil {
		t.Fatal("reapStandingEnv returned nil; want the reap-plan error (plan path not taken, or silent success)")
	}
	if !strings.Contains(err.Error(), "unknown namespace reap kind") {
		t.Errorf("error %q does not show the reap-plan path ran", err.Error())
	}
}

// TestStandingEntryAppliesOverrides: --name and --ttl override the StandingEnv's
// own values; the ref and info land in Data.
func TestStandingEntryAppliesOverrides(t *testing.T) {
	env := &core.StandingEnv{
		Name:     "auto-name",
		Shape:    "yb_k8s",
		Provider: "functional",
		Ref:      "deadbeef",
		TTL:      time.Hour,
		Info:     map[string]string{"ysql_dsn": "host=x"},
	}
	e := standingEntry(env, "my-env", 2*time.Hour, true)
	if e.Name != "my-env" {
		t.Errorf("name = %q, want my-env (--name override)", e.Name)
	}
	if e.TTL != 2*time.Hour {
		t.Errorf("ttl = %s, want 2h (--ttl override)", e.TTL)
	}
	if e.Data["ref"] != "deadbeef" {
		t.Errorf("ref not stored in Data: %+v", e.Data)
	}
	if e.Data["ysql_dsn"] != "host=x" {
		t.Errorf("info not carried into Data: %+v", e.Data)
	}

	// Without the --ttl flag set, the env's own TTL wins.
	keep := standingEntry(env, "", 0, false)
	if keep.Name != "auto-name" {
		t.Errorf("name = %q, want auto-name (no override)", keep.Name)
	}
	if keep.TTL != time.Hour {
		t.Errorf("ttl = %s, want 1h (env default kept)", keep.TTL)
	}
}

// setupEnvelope is the subset of the setup command's JSON envelope tests read.
type setupEnvelope struct {
	Data struct {
		Env struct {
			ID string `json:"id"`
		} `json:"env"`
	} `json:"data"`
}

type envListEnvelope struct {
	Data struct {
		Count int `json:"count"`
	} `json:"data"`
}

func execCLI(t *testing.T, args ...string) string {
	t.Helper()
	root := buildRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("cli %v: %v (output: %s)", args, err, out.String())
	}
	return out.String()
}

// TestSetupThenEnvLsThenEnvRm is the full standing-env round trip against the
// fake backing: setup registers a standing env and prints it, env ls lists it,
// and env rm reconstructs its teardown (fake provider Attach+Teardown) and
// removes it. jsonOutput is a package global cobra sets; reset it after.
func TestSetupThenEnvLsThenEnvRm(t *testing.T) {
	defer func() { jsonOutput = false }()
	reg := filepath.Join(t.TempDir(), "envs.json")

	out := execCLI(t, "setup", "--json", "-t", "test/fake_standing", "--registry", reg)
	var se setupEnvelope
	if err := json.Unmarshal([]byte(out), &se); err != nil {
		t.Fatalf("decode setup envelope: %v (%s)", err, out)
	}
	id := se.Data.Env.ID
	if id == "" {
		t.Fatalf("setup returned no env id: %s", out)
	}

	lsOut := execCLI(t, "env", "ls", "--json", "--registry", reg)
	var le envListEnvelope
	if err := json.Unmarshal([]byte(lsOut), &le); err != nil {
		t.Fatalf("decode env ls envelope: %v (%s)", err, lsOut)
	}
	if le.Data.Count != 1 {
		t.Errorf("env ls count = %d, want 1", le.Data.Count)
	}

	execCLI(t, "env", "rm", id, "--json", "--registry", reg, "--env-provider", "fake")

	lsOut2 := execCLI(t, "env", "ls", "--json", "--registry", reg)
	var le2 envListEnvelope
	if err := json.Unmarshal([]byte(lsOut2), &le2); err != nil {
		t.Fatalf("decode env ls envelope: %v (%s)", err, lsOut2)
	}
	if le2.Data.Count != 0 {
		t.Errorf("env ls count after rm = %d, want 0", le2.Data.Count)
	}
}

// TestSetupCommandInManifest confirms `ampqa setup` and `env rm --skip-teardown`
// are discoverable via the manifest (agents' self-discovery).
func TestSetupCommandInManifest(t *testing.T) {
	root := buildRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"manifest", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	for _, want := range []string{`"name": "setup"`, "skip-teardown"} {
		if !bytes.Contains(out.Bytes(), []byte(want)) {
			t.Errorf("manifest missing %q: %s", want, out.String())
		}
	}
}

// TestSetupRecordFailureRunsTeardown: when recording the standing env fails, its
// teardown MUST run — a registry-write failure drops the only in-memory handle
// to the live environment, so recordStandingEnv tears it down rather than orphan
// it. Upholds "never leave provisioned infra unrecoverable".
func TestSetupRecordFailureRunsTeardown(t *testing.T) {
	// Point the registry at a path whose parent is a FILE, so Add's MkdirAll fails.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	reg := registry.New(filepath.Join(blocker, "envs.json"))

	var torn bool
	sr := &core.SetupResult{
		Env:      &core.StandingEnv{Name: "x", Shape: "yb_k8s", Provider: "functional"},
		Teardown: func(context.Context) error { torn = true; return nil },
	}
	entry := standingEntry(sr.Env, "", 0, false)

	_, err := recordStandingEnv(reg, sr, entry, time.Minute, nil)
	if err == nil {
		t.Fatal("recordStandingEnv returned nil despite a failing registry write")
	}
	if !torn {
		t.Error("teardown must run when recording fails, to avoid orphaning the live env")
	}
}

// TestSetupRecordSuccessKeepsEnv: on a successful record the env is NOT torn down
// (the standing env must stay up for manual use).
func TestSetupRecordSuccessKeepsEnv(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	var torn bool
	sr := &core.SetupResult{
		Env:      &core.StandingEnv{Name: "x", Shape: "yb_k8s", Provider: "functional"},
		Teardown: func(context.Context) error { torn = true; return nil },
	}
	entry := standingEntry(sr.Env, "", 0, false)

	stored, err := recordStandingEnv(reg, sr, entry, time.Minute, nil)
	if err != nil {
		t.Fatalf("recordStandingEnv: %v", err)
	}
	if torn {
		t.Error("a successfully recorded standing env must NOT be torn down")
	}
	if _, ok, _ := reg.Get(stored.ID); !ok {
		t.Error("recorded env not found in the registry")
	}
}
