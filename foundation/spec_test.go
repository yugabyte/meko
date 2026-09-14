package foundation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadExampleAmp is the check that keeps examples/amp.yaml honest: it is
// the file an operator copies, and a spec that no longer parses (or silently
// loses a field to a rename) is a broken starting point.
func TestLoadExampleAmp(t *testing.T) {
	spec := loadExample(t, "amp.yaml")

	if spec.Namespace != "amp" {
		t.Errorf("namespace = %q, want amp", spec.Namespace)
	}
	if n, _ := intField(spec.Universe, "numNodes"); n != 3 {
		t.Errorf("universe.numNodes = %d, want 3", n)
	}
	if rf, _ := intField(spec.Universe, "replicationFactor"); rf != 3 {
		t.Errorf("universe.replicationFactor = %d, want 3", rf)
	}
	if got := spec.Databases; len(got) != 1 || got[0] != "yb_storage" {
		t.Errorf("databases = %v, want [yb_storage]", got)
	}
	// The controller's DSN is passwordless and sslmode=disable today, so both of
	// these staying off is what keeps the example working against it.
	if spec.Universe["enableYSQLAuth"] == true {
		t.Error("enableYSQLAuth is on; the existing controller DSN is passwordless")
	}
	// TLS cannot be changed after creation, and the existing DSNs are
	// sslmode=disable.
	if spec.Universe["enableNodeToNodeEncrypt"] == true || spec.Universe["enableClientToNodeEncrypt"] == true {
		t.Error("TLS is on; the existing DSNs are sslmode=disable")
	}
	// The provider is what carries the DB image registry, its pull secret and
	// the per-zone storage class — none of which the operator's auto-provider
	// can infer, so the example must ship one.
	if len(spec.Provider) == 0 {
		t.Fatal("the example has no provider block")
	}
	if spec.ProviderName() == "" {
		t.Error("the universe does not reference the provider")
	}
	ci, _ := spec.Provider["cloudInfo"].(map[string]any)
	if ci["kubernetesImageRegistry"] == nil {
		t.Error("the provider names no image registry; k8s universes pull the DB image from one")
	}
	// yb-standard is itest's class, and the PROVIDER's zone value is the one
	// that wins over any storageClass on the universe's volumes.
	if got := storageClasses(spec); len(got) != 1 || got[0] != "yb-standard" {
		t.Errorf("storage classes = %v, want exactly [yb-standard]", got)
	}
}

// TestLoadRejectsUnknownFields: a typo in a spec key would otherwise be
// ignored and the foundation would come up with a default nobody asked for —
// testVersion is the universe line every terse fixture needs: an empty or
// absent ybSoftwareVersion is a hard error, because YBA parses it and rejects
// the universe with no reason attached.
const testVersion = "  ybSoftwareVersion: 2026.1.1.1-b2\n"

// the worst kind of failure, because it looks like success.
func TestLoadRejectsUnknownFields(t *testing.T) {
	_, err := Load([]byte("namespace: amp\nuniverze: {}\n"))
	if err == nil {
		t.Fatal("Load accepted an unknown field")
	}
	if !strings.Contains(err.Error(), "univerze") {
		t.Errorf("error %q does not name the offending field", err)
	}
}

func TestDefaults(t *testing.T) {
	spec, err := Load([]byte("namespace: amp\nuniverse:\n  providerName: p\n" + testVersion))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if spec.ChartVersion != DefaultChartVersion {
		t.Errorf("chartVersion = %q, want %q", spec.ChartVersion, DefaultChartVersion)
	}
	if spec.ChartSource != ChartRepo {
		t.Errorf("chartSource = %q, want %q", spec.ChartSource, ChartRepo)
	}
	if spec.YBA.Chart != DefaultChartRepo {
		t.Errorf("yba.chart = %q, want %q", spec.YBA.Chart, DefaultChartRepo)
	}
	if spec.UniverseName() != DefaultUniverseName {
		t.Errorf("universeName = %q, want %q", spec.UniverseName(), DefaultUniverseName)
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		yaml    string
		wantErr string
	}{{
		name:    "namespace is required",
		yaml:    "universe:\n  numNodes: 3\n",
		wantErr: "namespace is required",
	}, {
		// RF 3 across 1 node cannot place replicas, and YBA would reject it
		// asynchronously — much later and much less clearly.
		name:    "numNodes below RF",
		yaml:    "namespace: amp\nuniverse:\n  providerName: p\n  numNodes: 1\n  replicationFactor: 3\n",
		wantErr: "below replicationFactor",
	}, {
		name:    "even RF",
		yaml:    "namespace: amp\nuniverse:\n  providerName: p\n  numNodes: 4\n  replicationFactor: 4\n",
		wantErr: "not supported",
	}, {
		name:    "bad chart source",
		yaml:    "namespace: amp\nchartSource: ftp\nuniverse:\n  providerName: p\n" + testVersion,
		wantErr: "not one of repo, s3, path",
	}, {
		name:    "path source needs a chart",
		yaml:    "namespace: amp\nchartSource: path\nuniverse:\n  providerName: p\n" + testVersion,
		wantErr: "needs yba.chart",
	}, {
		// Neither a provider block nor a providerName: the auto-provider cannot
		// supply the image registry, pull secret or storage class, so this has to
		// be an error rather than a surprise on the cluster.
		name:    "no provider at all",
		yaml:    "namespace: amp\nuniverse:\n  numNodes: 1\n  replicationFactor: 1\n" + testVersion,
		wantErr: "must name a preexisting provider",
	}, {
		name: "RF 1 single node is allowed",
		yaml: "namespace: amp\nuniverse:\n  providerName: p\n" + testVersion +
			"  numNodes: 1\n  replicationFactor: 1\n",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte(tc.yaml))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load accepted %q", tc.yaml)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func readExample(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("examples", name))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	return string(data)
}

func loadExample(t *testing.T, name string) *Spec {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("examples", name))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	spec, err := Load(data)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return spec
}

// TestExampleIsThreeNodeRF3WithPlacement pins the shape the foundation CI
// deploys: RF3 over three nodes, placed explicitly on a zone the provider
// defines. Left implicit, YBA picks the layout and a zone typo surfaces as a
// failed task tens of minutes into a bring-up.
func TestExampleIsThreeNodeRF3WithPlacement(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	if got := universeInt(spec.Universe, "numNodes"); got != 3 {
		t.Errorf("numNodes = %d, want 3", got)
	}
	if got := universeInt(spec.Universe, "replicationFactor"); got != 3 {
		t.Errorf("replicationFactor = %d, want 3", got)
	}
	placement, ok := spec.Universe["placementInfo"].(map[string]any)
	if !ok {
		t.Fatal("the example declares no placementInfo")
	}
	total := 0
	for _, r := range asList(placement["regions"]) {
		region, _ := r.(map[string]any)
		for _, z := range asList(region["zones"]) {
			zone, _ := z.(map[string]any)
			code, _ := zone["code"].(string)
			if !providerZones(spec.Provider)[code] {
				t.Errorf("placement zone %q is not one the provider defines", code)
			}
			n, _ := intField(zone, "numNodes")
			total += n
		}
	}
	if total != 3 {
		t.Errorf("placementInfo places %d nodes, want 3", total)
	}
}

// TestExampleSeedsTheMageGFlag: MAGE is a START-TIME gflag on both processes,
// so a universe created without it needs a rolling restart to gain it. It is
// also a preview flag, which YBA refuses unless allowed_preview_flags_csv names
// it — so the two always travel together.
//
// This checks the spec FILE, not a rule: nothing here validates preview flags,
// because which flags are preview is YB's to decide and moves between releases.
// YBA rejects a CSV that does not cover them, and that is the right authority.
func TestExampleSeedsTheMageGFlag(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	g, _ := spec.Universe["gFlags"].(map[string]any)
	for _, role := range []string{"masterGFlags", "tserverGFlags"} {
		flags, _ := g[role].(map[string]any)
		if fmt.Sprint(flags["ysql_yb_enable_mage"]) != "true" {
			t.Errorf("%s does not enable mage: %v", role, flags)
		}
		csv := fmt.Sprint(flags["allowed_preview_flags_csv"])
		if !strings.Contains(csv, "ysql_yb_enable_mage") {
			t.Errorf("%s sets mage but allowed_preview_flags_csv is %q; YBA would reject the universe",
				role, csv)
		}
	}
}

// TestValidateRejectsAPlacementTheProviderCannotSatisfy covers both halves,
// each of which YBA otherwise rejects asynchronously from inside the operator.
func TestValidateRejectsAPlacementTheProviderCannotSatisfy(t *testing.T) {
	placement := func(zone string, nodes int) map[string]any {
		return map[string]any{"regions": []any{map[string]any{
			"code":  "us-west1",
			"zones": []any{map[string]any{"code": zone, "numNodes": nodes}},
		}}}
	}
	for _, tc := range []struct {
		name, zone, want string
		nodes            int
	}{
		{"zone the provider lacks", "us-west1-c", "the provider does not have", 3},
		{"node count that does not add up", "us-west1-b", "does not add up", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := loadExample(t, "amp.yaml")
			spec.Universe["placementInfo"] = placement(tc.zone, tc.nodes)
			err := spec.Validate()
			if err == nil {
				t.Fatal("Validate accepted a placement YBA would reject")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
		})
	}
}

// TestStorageClassesIncludesYugawareOwn: yugaware's PVC is a different class
// from the universe's, and unset it takes the cluster default — an
// Immediate-binding one zone-pins the ~4.5 CPU pod before it schedules, which
// is the whole reason preflight checks binding modes.
func TestStorageClassesIncludesYugawareOwn(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	yw, _ := spec.YBA.Values["yugaware"].(map[string]any)
	if yw["storageClass"] != "yb-standard" {
		t.Fatalf("the example leaves yugaware.storageClass = %v; it would take the cluster default",
			yw["storageClass"])
	}
	yw["storageClass"] = "some-other-class"
	got := storageClasses(spec)
	found := false
	for _, sc := range got {
		if sc == "some-other-class" {
			found = true
		}
	}
	if !found {
		t.Errorf("storageClasses = %v; preflight would never check yugaware's own class", got)
	}
}

// TestValidateRejectsAnEmptyDBVersion. Observed on the first real run: YBA
// parses ybSoftwareVersion against its own version and an empty string is a
// parse failure, not "use the default". The universe came back universeState
// "Error Creating" with a still-QUEUED action, no message and no task UUID —
// the only explanation was a RuntimeException in the yugaware log.
func TestValidateRejectsAnEmptyDBVersion(t *testing.T) {
	for _, universe := range []string{
		"  providerName: p\n",                          // absent
		"  providerName: p\n  ybSoftwareVersion: \"\"", // explicitly empty
	} {
		_, err := Load([]byte("namespace: amp\nuniverse:\n" + universe))
		if err == nil {
			t.Fatalf("Load accepted a spec with no DB version: %q", universe)
		}
		if !strings.Contains(err.Error(), "ybSoftwareVersion") {
			t.Errorf("error %q does not name the field", err)
		}
	}
}

// TestExampleDBVersionMatchesTheChart: the version has to be a release this
// YBA has, and the only one it ships is its own — so moving chartVersion moves
// this too. Both are read off the same release line, which is what this pins.
func TestExampleDBVersionMatchesTheChart(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	version := universeString(spec.Universe, "ybSoftwareVersion")
	// 2026.1.1 -> 2026.1.1.1-b2: the release line has to agree, or the version
	// is not one `ls /opt/yugabyte/releases` in that yugaware would print.
	if !strings.HasPrefix(version, spec.ChartVersion+".") {
		t.Errorf("ybSoftwareVersion %q is not on chartVersion %q's release line; the yugaware that "+
			"chart installs bundles only its own release, so the universe would be created "+
			"against a version YBA does not have", version, spec.ChartVersion)
	}
}

// TestTheSpecIsTheRenderedBody is the structural guard for the class of bug
// that produced a plaintext universe with an sslmode=require DSN.
//
// Every default this deployer owns for the universe is applied at parse time,
// so s.Universe and the rendered YBUniverse spec are the SAME map. Anything
// reading the Spec — Connect's sslMode, the storage-class preflight, Validate —
// therefore sees exactly what the cluster gets. Move a default back into
// rendering and this fails.
func TestTheSpecIsTheRenderedBody(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	manifests, err := spec.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var cr struct {
		Spec map[string]any `json:"spec"`
	}
	for _, m := range manifests {
		if m.Kind == "YBUniverse" {
			if err := json.Unmarshal(m.JSON, &cr); err != nil {
				t.Fatalf("parse the universe: %v", err)
			}
		}
	}
	if len(cr.Spec) == 0 {
		t.Fatal("no YBUniverse rendered")
	}
	// Same key set, both ways: a field only the CR has is a default applied too
	// late for a Spec reader to see, and one only the Spec has would be dropped.
	for key := range cr.Spec {
		if _, ok := spec.Universe[key]; !ok {
			t.Errorf("the CR has %q but the Spec does not: a default applied at render time is "+
				"invisible to everything that reads the Spec", key)
		}
	}
	for key := range spec.Universe {
		if _, ok := cr.Spec[key]; !ok {
			t.Errorf("the Spec has %q but the CR does not", key)
		}
	}
}

// TestLoadIsIdempotent: withDefaults writes into the maps it was given, so
// running it twice must not accumulate — a second serviceEndpoints entry would
// mean two Services fighting over one Namespaced endpoint.
func TestLoadIsIdempotent(t *testing.T) {
	yaml := readExample(t, "amp.yaml")
	first, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	first.withDefaults()
	first.withDefaults()

	overrides, _ := first.Universe["kubernetesOverrides"].(map[string]any)
	eps := asList(overrides["serviceEndpoints"])
	if len(eps) != 1 {
		t.Errorf("serviceEndpoints has %d entries after repeated defaulting, want 1", len(eps))
	}
	second, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a, _ := json.Marshal(first.Universe)
	b, _ := json.Marshal(second.Universe)
	if string(a) != string(b) {
		t.Errorf("defaulting twice differs from once:\n once: %s\ntwice: %s", b, a)
	}
}
