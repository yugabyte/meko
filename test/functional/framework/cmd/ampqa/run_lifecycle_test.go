package main

import (
	"strings"
	"testing"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
	"github.com/yugabyte/yb-amp/test/functional/framework/env/registry"
)

func TestResolveEnvMode(t *testing.T) {
	cases := []struct {
		name      string
		mode      string
		setupOnly bool
		envID     string
		modeSet   bool
		want      string
		wantErr   string
	}{
		{name: "default is build", mode: "build", want: envModeBuild},
		{name: "explicit keep", mode: "keep", modeSet: true, want: envModeKeep},
		{name: "setup-only forces keep", mode: "build", setupOnly: true, want: envModeKeep},
		{name: "attach requires env", mode: "attach", modeSet: true, wantErr: "requires --env"},
		{name: "env id implies attach", mode: "build", envID: "abc123", want: envModeAttach},
		{name: "invalid mode", mode: "wat", modeSet: true, wantErr: "invalid --env-mode"},
		{name: "setup-only conflicts with env", mode: "build", setupOnly: true, envID: "x", wantErr: "conflicts with --env"},
		{name: "setup-only conflicts with explicit non-keep", mode: "attach", setupOnly: true, modeSet: true, wantErr: "implies --env-mode keep"},
		{name: "env with explicit keep conflicts", mode: "keep", modeSet: true, envID: "x", wantErr: "attaches to a standing environment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveEnvMode(tc.mode, tc.setupOnly, tc.envID, tc.modeSet)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("mode = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestKeepRecordAttachRoundTrip: the registry entry a self-built control plane
// is recorded as (provider "ampstack" + context/namespace/controller_url, the
// shape sharedk8s.StandingEnv produces) is recognized by attachFromEntry and
// routed back to the sharedk8s provider in ATTACH mode — closing the
// build/keep → attach loop.
func TestKeepRecordAttachRoundTrip(t *testing.T) {
	entry := registry.Entry{
		ID:       "cp-1",
		Name:     "ampqa-cp-abc",
		Shape:    "dedicated_stack",
		Provider: "ampstack",
		Data: map[string]string{
			"namespace":          "ampqa-cp-abc",
			"context":            "gke_x",
			"controller_service": "amp-controller",
			"controller_port":    "8080",
			"controller_url":     "http://10.0.0.9:8080",
			"pool_name":          "ampqa",
		},
	}
	prov, overrides, err := attachFromEntry(entry)
	if err != nil {
		t.Fatalf("attachFromEntry: %v", err)
	}
	if prov != "sharedk8s" {
		t.Fatalf("provider = %q, want sharedk8s", prov)
	}
	joined := strings.Join(overrides, " ")
	for _, want := range []string{
		"env.sharedk8s.controller_url=http://10.0.0.9:8080",
		"env.sharedk8s.context=gke_x",
		"env.sharedk8s.namespace=ampqa-cp-abc",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("overrides missing %q; got %v", want, overrides)
		}
	}
}

func TestResolveEnvModeKeepFailed(t *testing.T) {
	got, err := resolveEnvMode("keep-failed", false, "", true)
	if err != nil || got != envModeKeepFailed {
		t.Fatalf("resolveEnvMode(keep-failed) = %q, %v", got, err)
	}
}

func TestSummaryHasFailures(t *testing.T) {
	green := core.RunSummary{Counts: map[core.Status]int{core.StatusPassed: 3, core.StatusSkipped: 1}}
	if summaryHasFailures(green) {
		t.Error("green run reported failures")
	}
	red := core.RunSummary{Counts: map[core.Status]int{core.StatusPassed: 2, core.StatusEnvFailed: 1}}
	if !summaryHasFailures(red) {
		t.Error("env_failed run reported green")
	}
}
