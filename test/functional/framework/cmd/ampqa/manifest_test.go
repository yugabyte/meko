package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

// manifestEnvelope decodes the discovery fields of the manifest envelope.
type manifestEnvelope struct {
	Data struct {
		Suites []string `json:"suites"`
		Tags   []string `json:"tags"`
		Tests  []struct {
			Name     string `json:"name"`
			Suite    string `json:"suite"`
			HasRun   bool   `json:"has_run"`
			HasSetup bool   `json:"has_setup"`
		} `json:"tests"`
		Config map[string]any `json:"config"`
	} `json:"data"`
}

func decodeManifest(t *testing.T) manifestEnvelope {
	t.Helper()
	root := buildRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"manifest", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var m manifestEnvelope
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("decode manifest: %v (%s)", err, out.String())
	}
	return m
}

// TestManifestSurfacesTestsAndTags: an agent must be able to discover WHAT it
// can run — the registered suites, tags, and per-test metadata (including
// whether a test is setup-only).
func TestManifestSurfacesTestsAndTags(t *testing.T) {
	m := decodeManifest(t)

	if !containsStr(m.Data.Suites, "features") || !containsStr(m.Data.Suites, "presets") {
		t.Errorf("suites %v missing features/presets", m.Data.Suites)
	}
	if !containsStr(m.Data.Tags, "regression") || !containsStr(m.Data.Tags, "nightly") {
		t.Errorf("tags %v missing regression/nightly", m.Data.Tags)
	}
	if len(m.Data.Tests) == 0 {
		t.Fatal("manifest lists no tests")
	}

	var sawPreset, sawFoundation bool
	for _, tc := range m.Data.Tests {
		if tc.Suite == "presets" {
			sawPreset = true
			if tc.HasRun || !tc.HasSetup {
				t.Errorf("preset %s: has_run=%v has_setup=%v, want setup-only (false/true)", tc.Name, tc.HasRun, tc.HasSetup)
			}
		}
		if tc.Name == "features/amp_on_yb_k8s" {
			sawFoundation = true
			if !tc.HasRun {
				t.Errorf("%s: has_run=false, want a runnable body", tc.Name)
			}
		}
	}
	if !sawPreset {
		t.Error("no presets test surfaced in manifest")
	}
	if !sawFoundation {
		t.Error("features/amp_on_yb_k8s not surfaced in manifest")
	}
}

// TestManifestSurfacesConfigKeys: an agent must be able to discover WHAT to
// configure — the flattened config keys with their defaults across the provider
// and app namespaces.
func TestManifestSurfacesConfigKeys(t *testing.T) {
	m := decodeManifest(t)
	for _, key := range []string{
		"sched.capacity",
		"env.docker.yb_image",
		"functional.enabled",
		"apps.yba.provider_type",
		"versions.yugabytedb",
		"report.max_steps_retained",
	} {
		if _, ok := m.Data.Config[key]; !ok {
			t.Errorf("config key %q missing from manifest (have %d keys)", key, len(m.Data.Config))
		}
	}
	// A known scalar default must round-trip.
	if got := m.Data.Config["env.docker.yb_image"]; got != "yugabytedb/yugabyte:latest" {
		t.Errorf("env.docker.yb_image = %v, want the default image", got)
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
