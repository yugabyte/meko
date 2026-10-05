package yba

import "testing"

// The run override is what `--yb-version` becomes (`-o versions.yb=…`). Reading
// only versions.yugabytedb here silently dropped it on the yba backing: the
// universe came up on the YBA's bundled release while the run recorded the
// version the caller asked for.
func TestDBVersionPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cfg         map[string]any
		explicitKey string
		want        string
	}{
		{
			name:        "the run override is honoured",
			cfg:         map[string]any{"versions.yb": "2026.1.2.0-b72"},
			explicitKey: "apps.yba.universe.yb_version",
			want:        "2026.1.2.0-b72",
		},
		{
			name:        "the run override beats versions.yugabytedb",
			cfg:         map[string]any{"versions.yb": "2026.1.2.0-b72", "versions.yugabytedb": "2025.2.1.0-b0"},
			explicitKey: "apps.yba.universe.yb_version",
			want:        "2026.1.2.0-b72",
		},
		{
			name:        "an explicit per-app version beats the run override",
			cfg:         map[string]any{"apps.yba.universe.yb_version": "2026.1.0.0-b1", "versions.yb": "2026.1.2.0-b72"},
			explicitKey: "apps.yba.universe.yb_version",
			want:        "2026.1.0.0-b1",
		},
		{
			name:        "versions.yugabytedb is still the last resort",
			cfg:         map[string]any{"versions.yugabytedb": "2025.2.1.0-b0"},
			explicitKey: "apps.yba.universe.yb_version",
			want:        "2025.2.1.0-b0",
		},
		{
			name: "no explicit key (release URL derivation) still sees the override",
			cfg:  map[string]any{"versions.yb": "2026.1.2.0-b72", "versions.yugabytedb": "2025.2.1.0-b0"},
			want: "2026.1.2.0-b72",
		},
		{
			name:        "nothing configured resolves empty",
			cfg:         map[string]any{},
			explicitKey: "apps.yba.universe.yb_version",
			want:        "",
		},
	} {
		if got := DBVersion(fakeConfig{m: tc.cfg}, tc.explicitKey); got != tc.want {
			t.Errorf("%s: DBVersion = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Every universe builder must resolve through DBVersion, so a run override
// reaches the spec YBA is actually handed.
func TestUniverseSpecsHonourTheRunOverride(t *testing.T) {
	cfg := fakeConfig{m: map[string]any{"versions.yb": "2026.1.2.0-b72", "versions.yugabytedb": "2025.2.1.0-b0"}}
	for name, got := range map[string]string{
		"k8s": BuildK8sUniverseSpec(cfg).YBSoftwareVersion,
		"gcp": BuildGCPUniverseSpec(cfg).YBSoftwareVersion,
		"aws": BuildUniverseSpec(cfg).YBSoftwareVersion,
	} {
		if got != "2026.1.2.0-b72" {
			t.Errorf("%s universe spec version = %q, want the versions.yb override", name, got)
		}
	}
}
