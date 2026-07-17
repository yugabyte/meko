package main

// Exercises the `run` command's flag/config interplay (--parallel,
// --per-env-parallelism, --out-dir, --junit): an explicit flag always wins;
// otherwise the value comes from the merged config (embedded defaults or a
// user -c file), per cobra's Flags().Changed(). Runs the real command against
// the fake provider (--fake-env) so nothing touches real infrastructure.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

// runData is the subset of the run command's JSON envelope this file checks.
type runEnvelope struct {
	Data struct {
		Parallel int `json:"parallel"`
	} `json:"data"`
}

func runCLI(t *testing.T, args ...string) runEnvelope {
	t.Helper()
	root := buildRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("run %v: %v (output: %s)", args, err, out.String())
	}
	var env runEnvelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (output: %s)", err, out.String())
	}
	return env
}

func TestRunParallelDefaultsFromConfigWhenFlagNotPassed(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "user.yaml")
	if err := os.WriteFile(cfgPath, []byte("sched:\n  parallel: 3\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	env := runCLI(t, "run", "--fake-env", "--json", "--suite", "canary", "--test", "canary/pass", "-c", cfgPath)
	if env.Data.Parallel != 3 {
		t.Errorf("parallel = %d, want 3 (from sched.parallel in -c file)", env.Data.Parallel)
	}
}

func TestRunParallelFlagWinsOverConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "user.yaml")
	if err := os.WriteFile(cfgPath, []byte("sched:\n  parallel: 3\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	env := runCLI(t, "run", "--fake-env", "--json", "--suite", "canary", "--test", "canary/pass", "-c", cfgPath, "--parallel", "7")
	if env.Data.Parallel != 7 {
		t.Errorf("parallel = %d, want 7 (explicit flag beats config)", env.Data.Parallel)
	}
}

func TestRunParallelDefaultsFromEmbeddedConfigWithNoUserFile(t *testing.T) {
	env := runCLI(t, "run", "--fake-env", "--json", "--suite", "canary", "--test", "canary/pass")
	if env.Data.Parallel != 1 {
		t.Errorf("parallel = %d, want 1 (embedded sched.parallel default)", env.Data.Parallel)
	}
}

// TestRunNoDefaultConfigStillSucceeds: --no-default-config must not break a
// run — every knob it would have supplied still has its Go-level fallback
// constant (sched.DefaultCapacity etc.), so a --fake-env works exactly as
// before the canonical-defaults file existed.
func TestRunNoDefaultConfigStillSucceeds(t *testing.T) {
	env := runCLI(t, "run", "--fake-env", "--json", "--suite", "canary", "--test", "canary/pass", "--no-default-config")
	if env.Data.Parallel != 1 {
		t.Errorf("parallel = %d, want 1 (flag's own Go-level default, embedded layer skipped)", env.Data.Parallel)
	}
}

// countsEnvelope decodes the run summary's per-status counts.
type countsEnvelope struct {
	Data struct {
		Counts map[string]int `json:"counts"`
	} `json:"data"`
}

func runCounts(t *testing.T, args ...string) map[string]int {
	t.Helper()
	root := buildRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("run %v: %v (output: %s)", args, err, out.String())
	}
	// The deprecated --dry-run alias prints a one-line deprecation notice before
	// the JSON envelope; decode from the first '{' so it doesn't break parsing.
	raw := out.Bytes()
	if i := bytes.IndexByte(raw, '{'); i > 0 {
		raw = raw[i:]
	}
	var env countsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v (output: %s)", err, out.String())
	}
	return env.Data.Counts
}

// TestFakeEnvAndDryRunBothSelectFakeProvider: both --fake-env and its
// deprecated --dry-run alias resolve the fake provider, so canary/pass runs its
// body and PASSES (a real-env selection would skip it with no provider).
func TestFakeEnvAndDryRunBothSelectFakeProvider(t *testing.T) {
	for _, flag := range []string{"--fake-env", "--dry-run"} {
		counts := runCounts(t, "run", flag, "--json", "--suite", "canary", "--test", "canary/pass")
		if counts["passed"] != 1 {
			t.Errorf("%s: passed = %d, want 1 (fake provider not selected: %v)", flag, counts["passed"], counts)
		}
		if counts["skipped"] != 0 {
			t.Errorf("%s: skipped = %d, want 0 (a skip means no provider was selected)", flag, counts["skipped"])
		}
	}
}

// TestRunExcludesSetupOnlyPresets: the presets are setup-only (Run==nil) and
// belong to `ampqa setup`; selecting them for `ampqa run` matches nothing to
// execute, so the command reports the no-match usage error (exit 3), never
// silently trying to run a body-less test.
func TestRunExcludesSetupOnlyPresets(t *testing.T) {
	root := buildRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"run", "--fake-env", "--json", "--suite", "presets"})
	err := root.Execute()
	if err == nil {
		t.Fatalf("run --suite presets returned nil; want a no-match error (setup-only tests excluded). output: %s", out.String())
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 {
		t.Fatalf("error = %v (code path), want exitError code 3", err)
	}
}

// TestRunExitCodeAllSkippedDistinctFromAllPass: a run where every selected test
// skipped (nothing executed) must exit with the distinct all-skipped code, not
// 0, so an agent checking only the exit code can't mistake "nothing ran" for
// "all green". An all-pass exits 0.
func TestRunExitCodeAllSkippedDistinctFromAllPass(t *testing.T) {
	allPass := core.RunSummary{Total: 2, Counts: map[core.Status]int{core.StatusPassed: 2}}
	if got := runExitCode(allPass); got != 0 {
		t.Errorf("all-pass exit = %d, want 0", got)
	}
	allSkip := core.RunSummary{Total: 2, Counts: map[core.Status]int{core.StatusSkipped: 2}}
	if got := runExitCode(allSkip); got != exitAllSkipped {
		t.Errorf("all-skipped exit = %d, want %d", got, exitAllSkipped)
	}
	mixedPassSkip := core.RunSummary{Total: 2, Counts: map[core.Status]int{core.StatusPassed: 1, core.StatusSkipped: 1}}
	if got := runExitCode(mixedPassSkip); got != 0 {
		t.Errorf("passed+skipped exit = %d, want 0 (something ran and passed)", got)
	}
	withFailure := core.RunSummary{Total: 2, Counts: map[core.Status]int{core.StatusFailed: 1, core.StatusSkipped: 1}}
	if got := runExitCode(withFailure); got != 1 {
		t.Errorf("failure exit = %d, want 1", got)
	}
}

// TestRunAllSkippedEndToEndExits4: selecting a real-env test with no provider
// available skips it cleanly, and the command exits with the all-skipped code.
func TestRunAllSkippedEndToEndExits4(t *testing.T) {
	root := buildRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	// canary/fail-env would fail; use canary/pass but force no provider via a
	// non-fake, unconfigured env-provider so the fake isn't selected and the test
	// skips with ProviderUnavailableReason.
	root.SetArgs([]string{"run", "--json", "--suite", "canary", "--test", "canary/pass", "--env-provider", "docker"})
	err := root.Execute()
	if err == nil {
		t.Fatalf("want all-skipped exit error, got nil. output: %s", out.String())
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != exitAllSkipped {
		t.Fatalf("exit = %v, want exitError code %d (all skipped). output: %s", err, exitAllSkipped, out.String())
	}
}

// TestRunHTMLFlagWritesReport: `run --html <path>` attaches the HTML sink and
// renders a self-contained report on the spot when the run finishes.
func TestRunHTMLFlagWritesReport(t *testing.T) {
	root := buildRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	htmlPath := filepath.Join(t.TempDir(), "report.html")
	root.SetArgs([]string{"run", "--suite", "canary", "--test", "canary/pass", "--fake-env", "--html", htmlPath})
	if err := root.Execute(); err != nil {
		t.Fatalf("run failed: %v\noutput: %s", err, out.String())
	}
	data, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("HTML report not written to %s: %v", htmlPath, err)
	}
	if !bytes.Contains(bytes.ToLower(data), []byte("<html")) && !bytes.Contains(bytes.ToLower(data), []byte("<!doctype")) {
		t.Fatalf("HTML report has no html document tag; got prefix: %.200s", data)
	}
	if !bytes.Contains(data, []byte("canary/pass")) {
		t.Errorf("HTML report does not mention the test it ran (canary/pass)")
	}
}

// TestManifestListsNoDefaultConfigFlag confirms `ampqa manifest` (self-
// discovery for humans and agents) surfaces the new escape hatch.
func TestManifestListsNoDefaultConfigFlag(t *testing.T) {
	root := buildRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"manifest", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("no-default-config")) {
		t.Errorf("manifest output does not mention --no-default-config: %s", out.String())
	}
}
