package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// captureCLI executes the root command with args and returns stdout + any error
// (unlike execCLI, which fatals) so a caller can assert on the error/exit code.
func captureCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := buildRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// describeListEnvelope decodes `describe --json` (list mode).
type describeListEnvelope struct {
	Kind string `json:"kind"`
	Data struct {
		Count int `json:"count"`
		Tests []struct {
			Name        string `json:"name"`
			Suite       string `json:"suite"`
			Description string `json:"description"`
		} `json:"tests"`
	} `json:"data"`
}

// describeTestEnvelope decodes `describe <test> --json` (single mode).
type describeTestEnvelope struct {
	Kind string `json:"kind"`
	Data struct {
		Test struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"test"`
		Description  string `json:"description"`
		GlobalParams []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"global_params"`
		GlobalNote string `json:"global_note"`
		TestParams []struct {
			Prefix string `json:"prefix"`
			Keys   []struct {
				Key         string `json:"key"`
				Default     string `json:"default"`
				Description string `json:"description"`
			} `json:"keys"`
		} `json:"test_params"`
		TestNote string `json:"test_note"`
	} `json:"data"`
}

// TestDescribeListGroupsBySuiteWithDescriptions: the no-arg list carries each
// test's description, both in the JSON envelope and the human text.
func TestDescribeListGroupsBySuiteWithDescriptions(t *testing.T) {
	out, err := captureCLI(t, "describe", "--suite", "perf", "--json")
	if err != nil {
		t.Fatalf("describe --json: %v (%s)", err, out)
	}
	var env describeListEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	if env.Kind != "ampqa.describe.list" {
		t.Errorf("kind = %q", env.Kind)
	}
	if env.Data.Count == 0 {
		t.Fatal("no perf tests listed")
	}
	for _, tc := range env.Data.Tests {
		if tc.Suite != "perf" {
			t.Errorf("test %s has suite %q, want perf (filter leaked)", tc.Name, tc.Suite)
		}
		if tc.Description == "" {
			t.Errorf("test %s has no description in the list", tc.Name)
		}
	}

	// Human text: a suite header and the description under a test.
	text, err := captureCLI(t, "describe", "--suite", "canary")
	if err != nil {
		t.Fatalf("describe text: %v (%s)", err, text)
	}
	if !strings.Contains(text, "canary (") {
		t.Errorf("text missing canary suite header:\n%s", text)
	}
	if !strings.Contains(text, "always passes") {
		t.Errorf("text missing canary/pass description:\n%s", text)
	}
}

// TestDescribeTestResolvesParams: `describe <test>` shows the description, the
// global run flags, and the test-specific params resolved to key/default/doc.
func TestDescribeTestResolvesParams(t *testing.T) {
	out, err := captureCLI(t, "describe", "perf/pgbench", "--json")
	if err != nil {
		t.Fatalf("describe perf/pgbench: %v (%s)", err, out)
	}
	var env describeTestEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	if env.Kind != "ampqa.describe.test" {
		t.Errorf("kind = %q", env.Kind)
	}
	if env.Data.Test.Name != "perf/pgbench" || env.Data.Description == "" {
		t.Errorf("test name/description = %q/%q", env.Data.Test.Name, env.Data.Description)
	}
	if env.Data.GlobalNote == "" {
		t.Error("global_note empty")
	}

	// Global params must include the run flags (a representative pair).
	globalNames := map[string]bool{}
	for _, f := range env.Data.GlobalParams {
		globalNames[f.Name] = true
	}
	for _, want := range []string{"parallel", "option", "seed"} {
		if !globalNames[want] {
			t.Errorf("global params missing --%s", want)
		}
	}

	// Test params: the declared prefixes, each resolved to real config keys.
	prefixes := map[string]bool{}
	var scale struct {
		found       bool
		def         string
		description string
	}
	for _, g := range env.Data.TestParams {
		prefixes[g.Prefix] = true
		for _, k := range g.Keys {
			if k.Key == "perf.pgbench.scale" {
				scale.found = true
				scale.def = k.Default
				scale.description = k.Description
			}
		}
	}
	for _, want := range []string{"perf", "apps.ybk8s"} {
		if !prefixes[want] {
			t.Errorf("test params missing prefix %q", want)
		}
	}
	if !scale.found {
		t.Fatal("perf.pgbench.scale not resolved under the perf prefix")
	}
	if scale.def != "10" {
		t.Errorf("perf.pgbench.scale default = %q, want 10", scale.def)
	}
	if !strings.Contains(scale.description, "pgbench -s") {
		t.Errorf("perf.pgbench.scale doc = %q, want the comment from default.yaml", scale.description)
	}
}

// TestDescribeTestNoParams: a test with no Params says so explicitly.
func TestDescribeTestNoParams(t *testing.T) {
	out, err := captureCLI(t, "describe", "canary/pass", "--json")
	if err != nil {
		t.Fatalf("describe canary/pass: %v (%s)", err, out)
	}
	var env describeTestEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	if len(env.Data.TestParams) != 0 {
		t.Errorf("canary/pass has test params %v, want none", env.Data.TestParams)
	}
	if env.Data.TestNote != "no test-specific parameters (uses global config only)" {
		t.Errorf("test_note = %q", env.Data.TestNote)
	}
}

// TestDescribeUnknownTest exits 3 (not found).
func TestDescribeUnknownTest(t *testing.T) {
	_, err := captureCLI(t, "describe", "no/such_test")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 {
		t.Fatalf("err = %v, want exitError code 3", err)
	}
}

// TestCollectCarriesDescription: the collect envelope now carries each test's
// description so JSON self-discovery includes it.
func TestCollectCarriesDescription(t *testing.T) {
	out, err := captureCLI(t, "collect", "--suite", "stress", "--json")
	if err != nil {
		t.Fatalf("collect: %v (%s)", err, out)
	}
	var env describeListEnvelope // same {count, tests[{name,suite,description}]} shape
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	if env.Data.Count == 0 {
		t.Fatal("no stress tests")
	}
	for _, tc := range env.Data.Tests {
		if tc.Description == "" {
			t.Errorf("collect: %s has no description", tc.Name)
		}
	}
}

// TestManifestCarriesDescription: the manifest testNode now carries description.
func TestManifestCarriesDescription(t *testing.T) {
	out, err := captureCLI(t, "manifest", "--json")
	if err != nil {
		t.Fatalf("manifest: %v (%s)", err, out)
	}
	var env struct {
		Data struct {
			Tests []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tests"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	var checked bool
	for _, tc := range env.Data.Tests {
		if tc.Name == "perf/tpcc" {
			checked = true
			if tc.Description == "" {
				t.Error("manifest: perf/tpcc has no description")
			}
		}
	}
	if !checked {
		t.Error("perf/tpcc not in manifest")
	}
}
