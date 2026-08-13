package perf

import (
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// TestVariantsFromMapsShorthand: the portal's {image_version, yb_version}
// shorthand translates to the config keys that actually change a variant arm,
// auto-labels by the versions, and gets the run's own config prepended as the
// "primary" reference.
func TestVariantsFromMapsShorthand(t *testing.T) {
	vs, err := variantsFromMaps([]map[string]any{
		{"yb_version": "2026.1.2.0-b10"},
		{"image_version": "0.260805.1-daad9bd8f", "yb_version": "2025.2.1.0-b141"},
	})
	if err != nil {
		t.Fatalf("variantsFromMaps: %v", err)
	}
	if len(vs) != 3 {
		t.Fatalf("got %d variants, want 3 (primary + 2 shorthand)", len(vs))
	}
	if vs[0].Label != primaryVariantLabel || len(vs[0].Overrides) != 0 {
		t.Errorf("variant[0] = %+v, want the empty %q reference arm", vs[0], primaryVariantLabel)
	}
	if vs[1].Label != "2026.1.2.0-b10" {
		t.Errorf("yb-only label = %q, want the version string", vs[1].Label)
	}
	if got, want := vs[1].Overrides[variantYBVersionKey], "2026.1.2.0-b10"; got != want {
		t.Errorf("%s = %q, want %q", variantYBVersionKey, got, want)
	}
	if len(vs[1].Overrides) != 1 {
		t.Errorf("yb-only overrides = %v, want only %s", vs[1].Overrides, variantYBVersionKey)
	}
	if vs[2].Label != "0.260805.1-daad9bd8f+2025.2.1.0-b141" {
		t.Errorf("both-versions label = %q, want the joined versions", vs[2].Label)
	}
	want := map[string]string{
		variantYBVersionKey:       "2025.2.1.0-b141",
		variantControllerImageKey: "quay.io/yugabyte/amp-controller:0.260805.1-daad9bd8f",
		variantEndpointImageKey:   "quay.io/yugabyte/amp-endpoint:0.260805.1-daad9bd8f",
	}
	for k, v := range want {
		if got := vs[2].Overrides[k]; got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if len(vs[2].Overrides) != len(want) {
		t.Errorf("both-versions overrides = %v, want exactly %v", vs[2].Overrides, want)
	}
}

// TestVariantsFromMapsImageOnlyRejected: the comparison benchmarks the raw YB
// backing, so an amp image alone cannot make an arm differ — it fails fast
// instead of producing two identical arms.
func TestVariantsFromMapsImageOnlyRejected(t *testing.T) {
	_, err := variantsFromMaps([]map[string]any{{"image_version": "0.260805.1-daad9bd8f"}})
	if err == nil {
		t.Fatal("image_version-only variant accepted, want an error")
	}
	if !strings.Contains(err.Error(), "no amp control plane is provisioned") {
		t.Errorf("error = %v, want it to name why an amp image alone is meaningless", err)
	}

	if _, err := variantsFromMaps([]map[string]any{{"yb_version": ""}}); err == nil {
		t.Error("empty-version shorthand accepted, want an error")
	}
}

// TestVariantsFromMapsLabelDedup: two arms pinning the same versions still get
// unique labels (the resolver requires uniqueness), and neither can claim the
// reserved primary label.
func TestVariantsFromMapsLabelDedup(t *testing.T) {
	vs, err := variantsFromMaps([]map[string]any{
		{"yb_version": "2026.1.2.0-b10"},
		{"yb_version": "2026.1.2.0-b10"},
		{"yb_version": "primary"},
	})
	if err != nil {
		t.Fatalf("variantsFromMaps: %v", err)
	}
	got := []string{vs[0].Label, vs[1].Label, vs[2].Label, vs[3].Label}
	want := []string{"primary", "2026.1.2.0-b10", "2026.1.2.0-b10-2", "primary-2"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("label[%d] = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

// TestVariantsFromMapsFullFormUntouched: the {label, overrides} form keeps its
// exact labels and overrides, with no primary arm injected.
func TestVariantsFromMapsFullFormUntouched(t *testing.T) {
	vs, err := variantsFromMaps([]map[string]any{
		{"label": "base", "overrides": map[string]any{"apps.ybk8s.image_tag": "a"}},
		{"label": "candidate", "overrides": map[string]any{"apps.ybk8s.image_tag": "b"}},
	})
	if err != nil {
		t.Fatalf("variantsFromMaps: %v", err)
	}
	if len(vs) != 2 {
		t.Fatalf("got %d variants, want the 2 given (no primary injection)", len(vs))
	}
	if vs[0].Label != "base" || vs[1].Label != "candidate" {
		t.Errorf("labels = %q/%q, want base/candidate", vs[0].Label, vs[1].Label)
	}
	if vs[1].Overrides["apps.ybk8s.image_tag"] != "b" {
		t.Errorf("overrides = %v, want the given keys verbatim", vs[1].Overrides)
	}
}

// TestVariantsFromMapsMixedRejected: mixing the two shapes in one list is
// ambiguous (which arm is the reference?) and rejected.
func TestVariantsFromMapsMixedRejected(t *testing.T) {
	_, err := variantsFromMaps([]map[string]any{
		{"label": "base", "overrides": map[string]any{"apps.ybk8s.image_tag": "a"}},
		{"yb_version": "2026.1.2.0-b10"},
	})
	if err == nil {
		t.Fatal("mixed shorthand + full form accepted, want an error")
	}
	if !strings.Contains(err.Error(), "mixes") {
		t.Errorf("error = %v, want it to name the mixed shapes", err)
	}
}

// TestComparePortalPayloadResolves: the EXACT payload the report portal triggers
// with — one shorthand variant as a json_text override — travels through the real
// config loader and resolves into 2 arms (primary + the variant), with the
// variant's backing pinned to its yb_version.
func TestComparePortalPayloadResolves(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, []string{
		`perf.compare.variants=json_text:[{"image_version":"0.260805.1-daad9bd8f","yb_version":"2026.1.2.0-b10"}]`,
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cc, err := compareConfigFromConfig(cfg)
	if err != nil {
		t.Fatalf("compareConfigFromConfig: %v", err)
	}
	if len(cc.Variants) != 2 {
		t.Fatalf("got %d variants, want 2 (primary + the portal variant)", len(cc.Variants))
	}

	base := perfFromConfig(cfg)
	vs, err := resolveVariantList(&core.T{Options: cfg}, base, cc.Variants, LoadFields{Scale: 10}, "commit", "comparison")
	if err != nil {
		t.Fatalf("resolveVariantList: %v", err)
	}
	if len(vs) != 2 {
		t.Fatalf("resolved %d variants, want 2", len(vs))
	}
	if vs[0].Label != primaryVariantLabel {
		t.Errorf("reference arm = %q, want %q", vs[0].Label, primaryVariantLabel)
	}
	if vs[1].YBK8s.ChartVersion != "2026.1.2.0-b10" {
		t.Errorf("variant chart version = %q, want the shorthand's yb_version", vs[1].YBK8s.ChartVersion)
	}
	if vs[0].YBK8s.ChartVersion == vs[1].YBK8s.ChartVersion {
		t.Errorf("both arms deploy %q — the shorthand did not change the backing", vs[0].YBK8s.ChartVersion)
	}
	if vs[1].Identity.AmpControllerImage != "quay.io/yugabyte/amp-controller:0.260805.1-daad9bd8f" {
		t.Errorf("variant controller image = %q, want the shorthand's amp tag", vs[1].Identity.AmpControllerImage)
	}
}
