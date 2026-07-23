package ybdeploy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybdeploy"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

func loadCfg(t *testing.T, overrides ...string) *core.Config {
	t.Helper()
	cfg, err := core.LoadConfig(nil, nil, overrides)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func TestResolvePrecedence(t *testing.T) {
	cases := []struct {
		name      string
		overrides []string
		spec      core.EnvSpec
		want      string
	}{
		{"knob beats preference", []string{"env.yb_deploy=yb_k8s"}, core.EnvSpec{PreferredYBDeploy: ybdeploy.ModeExternal}, ybdeploy.ModeYBK8s},
		{"knob beats no preference", []string{"env.yb_deploy=yba_universe"}, core.EnvSpec{}, ybdeploy.ModeYBAUniverse},
		{"yb_pod knob", []string{"env.yb_deploy=yb_pod"}, core.EnvSpec{}, ybdeploy.ModeYBPod},
		{"empty knob honours preference", []string{"env.yb_deploy="}, core.EnvSpec{PreferredYBDeploy: ybdeploy.ModeExternal}, ybdeploy.ModeExternal},
		{"empty knob, no preference → builtin", []string{"env.yb_deploy="}, core.EnvSpec{}, ybdeploy.ModeBuiltin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadCfg(t, tc.overrides...)
			if got := ybdeploy.Resolve(cfg, tc.spec); got != tc.want {
				t.Errorf("Resolve = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProvisionBuiltinIsNoop(t *testing.T) {
	store, teardown, err := ybdeploy.Provision(context.Background(), loadCfg(t), ybdeploy.ModeBuiltin, "tag", nil)
	if err != nil {
		t.Fatalf("Provision(builtin) err = %v", err)
	}
	if store != nil {
		t.Errorf("Provision(builtin) store = %v, want nil", store)
	}
	if teardown != nil {
		t.Errorf("Provision(builtin) teardown = non-nil, want nil")
	}
}

func TestProvisionUnknownModeErrors(t *testing.T) {
	store, teardown, err := ybdeploy.Provision(context.Background(), loadCfg(t), "bogus", "tag", nil)
	if err == nil {
		t.Fatal("Provision(bogus) err = nil, want an error")
	}
	if store != nil || teardown != nil {
		t.Errorf("Provision(bogus) returned non-nil store/teardown")
	}
	for _, mode := range ybdeploy.ValidModes {
		if !strings.Contains(err.Error(), mode) {
			t.Errorf("error %q does not list valid mode %q", err.Error(), mode)
		}
	}
}

// TestProvisionExternalWiring exercises the store → ampstack.Backing seam
// (ybbacking.ExternalBacking) without touching real infra: external mode reads
// the env.ampstack.backing.* knobs and the mapped Backing must carry them.
func TestProvisionExternalWiring(t *testing.T) {
	cfg := loadCfg(t,
		"env.yb_deploy=external",
		"env.ampstack.backing.host_ports=h1:5433,h2:5433",
		"env.ampstack.backing.user=stress",
		"env.ampstack.backing.password=pw",
		"env.ampstack.backing.sslmode=require",
	)
	mode := ybdeploy.Resolve(cfg, core.EnvSpec{})
	if mode != ybdeploy.ModeExternal {
		t.Fatalf("Resolve = %q, want external", mode)
	}
	store, teardown, err := ybdeploy.Provision(context.Background(), cfg, mode, "tag", nil)
	if err != nil {
		t.Fatalf("Provision(external) err = %v", err)
	}
	if teardown != nil {
		t.Errorf("external teardown = non-nil, want nil (nothing provisioned to reap)")
	}
	if store == nil {
		t.Fatal("external store = nil")
	}
	b := ybbacking.ExternalBacking(store)
	if b.Mode != ampstack.BackingExternal {
		t.Errorf("Backing.Mode = %q, want external", b.Mode)
	}
	if b.HostPorts != "h1:5433,h2:5433" {
		t.Errorf("Backing.HostPorts = %q, want the configured CSV", b.HostPorts)
	}
	if b.User != "stress" || b.Password != "pw" || b.SSLMode != "require" {
		t.Errorf("Backing creds = %q/%q/%q, want stress/pw/require", b.User, b.Password, b.SSLMode)
	}
}

func TestProvisionExternalMissingHostPortsErrors(t *testing.T) {
	cfg := loadCfg(t, "env.yb_deploy=external")
	_, _, err := ybdeploy.Provision(context.Background(), cfg, ybdeploy.ModeExternal, "tag", nil)
	if err == nil {
		t.Fatal("external without host_ports err = nil, want an error")
	}
}
