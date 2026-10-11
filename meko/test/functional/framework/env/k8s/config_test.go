package k8s

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func load(t *testing.T, overrides ...string) *core.Config {
	t.Helper()
	cfg, err := core.LoadConfig(nil, nil, overrides)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestFromConfigReadsTheQuayCredentialFromTheSecret(t *testing.T) {
	cfg := load(t, "env.mekostack.quay_user=knob-user", "env.mekostack.quay_token=knob-token")
	resolve := func(name string) (string, bool, error) {
		if name == "quay" {
			return "yugabyte+robot:abc:def", true, nil
		}
		return "", false, nil
	}
	kc, err := FromConfig(cfg, resolve)
	if err != nil {
		t.Fatal(err)
	}
	if kc.Stack.QuayUser != "yugabyte+robot" || kc.Stack.QuayToken != "abc:def" {
		t.Errorf("credential = %q / %q, want the secret's, split at the first colon", kc.Stack.QuayUser, kc.Stack.QuayToken)
	}
}

func TestFromConfigFallsBackToTheKnobs(t *testing.T) {
	cfg := load(t, "env.mekostack.quay_user=knob-user", "env.mekostack.quay_token=knob-token")
	kc, err := FromConfig(cfg, func(string) (string, bool, error) { return "", false, nil })
	if err != nil {
		t.Fatal(err)
	}
	if kc.Stack.QuayUser != "knob-user" || kc.Stack.QuayToken != "knob-token" {
		t.Errorf("credential = %q / %q", kc.Stack.QuayUser, kc.Stack.QuayToken)
	}
}

func TestFromConfigRejectsABadSecret(t *testing.T) {
	cfg := load(t)
	if _, err := FromConfig(cfg, func(string) (string, bool, error) { return "no-colon-here", true, nil }); err == nil {
		t.Error("a secret with no user:token form was accepted")
	}
	if _, err := FromConfig(cfg, func(string) (string, bool, error) { return "", true, errors.New("env var unset") }); err == nil {
		t.Error("a declared secret that cannot be read was ignored")
	}
}

func TestFromConfigDefaultsAndPaths(t *testing.T) {
	out := t.TempDir()
	cfg := load(t, "report.out_dir="+out, "report.portal.run_id=ABCDEF12-3456", "owner=stepan", "env.k8s.context=itest-dev")
	kc, err := FromConfig(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if kc.RunID != "abcdef1234" || kc.Context != "itest-dev" || kc.Stack.Owner != "stepan" {
		t.Errorf("kc = %+v", kc)
	}
	if want := filepath.Join(out, "abcdef1234", "mekostack"); kc.Stack.ValuesDir != want {
		t.Errorf("ValuesDir = %q, want %q", kc.Stack.ValuesDir, want)
	}
	if want := filepath.Join(out, "abcdef1234", "diagnostics"); kc.Stack.DiagDir != want {
		t.Errorf("DiagDir = %q, want %q", kc.Stack.DiagDir, want)
	}
	if kc.Stack.APIReplicas != 3 || !kc.Stack.RagWorkers || kc.DeployTimeout.Minutes() != 60 {
		t.Errorf("stack defaults = %+v, deploy %s", kc.Stack, kc.DeployTimeout)
	}
	never := load(t, "report.out_dir="+out, "report.collect_diagnostics=never")
	if kc, _ := FromConfig(never, nil); kc.Stack.DiagDir != "" {
		t.Errorf("DiagDir = %q with collect_diagnostics=never", kc.Stack.DiagDir)
	}
}
