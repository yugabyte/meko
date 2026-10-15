package foundation

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	operatorChartPath = "deploy/helm/meko-foundation/Chart.yaml"
	crdChartPath      = "deploy/helm/meko-foundation-crd/Chart.yaml"
	testedWithYBAKey  = "meko.yugabyte.com/tested-with-yba"
)

type chartYAML struct {
	Version     string            `yaml:"version"`
	AppVersion  string            `yaml:"appVersion"`
	Annotations map[string]string `yaml:"annotations"`
}

func readChart(t *testing.T, path string) chartYAML {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var c chartYAML
	if err := yaml.Unmarshal(data, &c); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return c
}

// The annotation names the YBA build this release targets, and the example is
// what makes that true — it is the spec the GKE lane deploys. Two files, one
// fact, so they are compared rather than trusted.
//
// This does NOT establish that a universe came up on that build: the lane is
// directive-gated and nightly, and publishing does not wait on it. The claim
// the annotation is allowed to make is exactly what this test checks, which is
// why Chart.yaml and the README word it that way.
//
// Nothing else would catch a drift. A stale annotation renders, packages,
// publishes and installs; it is only ever read by a human deciding whether a
// chart is safe for their YBA, which is exactly when being wrong is expensive.
func TestTheAnnotationMatchesTheExample(t *testing.T) {
	chart := readChart(t, operatorChartPath)
	got := chart.Annotations[testedWithYBAKey]
	if got == "" {
		t.Fatalf("%s carries no %s annotation; publish-chart.sh and the README both "+
			"present it as the answer to \"which YBA is this qualified with\"",
			operatorChartPath, testedWithYBAKey)
	}

	// Read as plain YAML rather than through Load: this is the CR form, and
	// what matters is the two fields a reader would compare the annotation
	// against, not that the whole spec still parses (other tests cover that).
	data, err := os.ReadFile("examples/foundation-cr.yaml")
	if err != nil {
		t.Fatalf("read the example: %v", err)
	}
	var cr struct {
		Spec struct {
			ChartVersion string `yaml:"chartVersion"`
			Resources    []struct {
				Kind string         `yaml:"kind"`
				Spec map[string]any `yaml:"spec"`
			} `yaml:"resources"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &cr); err != nil {
		t.Fatalf("parse the example: %v", err)
	}

	if cr.Spec.ChartVersion != got {
		t.Errorf("annotation says tested with %q, the example deploys chartVersion %q — "+
			"a reader choosing a chart for their YBA would believe the annotation",
			got, cr.Spec.ChartVersion)
	}

	// The DB build travels with the chart build on this lane, and the annotation
	// names one string for both. A spec pinning a different ybSoftwareVersion
	// would make the annotation true of YBA and false of the database.
	var db string
	for _, r := range cr.Spec.Resources {
		if r.Kind == "YBUniverse" {
			if v, ok := r.Spec["ybSoftwareVersion"].(string); ok {
				db = v
			}
		}
	}
	if db == "" {
		t.Fatal("the example declares no YBUniverse ybSoftwareVersion; this check needs rewriting")
	}
	if db != got {
		t.Errorf("annotation says tested with %q, the example's ybSoftwareVersion is %q", got, db)
	}
}

// scripts/version.sh reads the annotation's value out of the example, so the
// image's yba-* alias and the chart annotation cannot disagree either — but
// only if the example keeps the field the script reads.
func TestTheExampleKeepsTheFieldVersionShReads(t *testing.T) {
	data, err := os.ReadFile("scripts/version.sh")
	if err != nil {
		t.Fatalf("read version.sh: %v", err)
	}
	if !strings.Contains(string(data), `spec["chartVersion"]`) {
		t.Fatal("version.sh no longer reads chartVersion from the example; " +
			"TestTheAnnotationMatchesTheExample is comparing the wrong field")
	}
}

// Both placeholders have to name something that EXISTS, because they are what a
// checkout install resolves to — `helm install ./meko-foundation` with no
// overrides. publish-chart.sh stamps the real pair onto the published chart, so
// these are only ever the developer's path.
//
// appVersion was once the chart's own version, which named an image nothing
// published: the install succeeded and the pod sat in ImagePullBackOff.
func TestTheCheckoutPlaceholdersNameAPublishedTag(t *testing.T) {
	chart := readChart(t, operatorChartPath)
	if chart.AppVersion != "latest" {
		t.Errorf("appVersion is %q; a checkout install pulls that tag, and `latest` is the only "+
			"floating tag build-image.sh publishes", chart.AppVersion)
	}
}

// The CRD chart is NOT versioned with the operator: it is cluster-scoped and
// shared, so a version bump has to mean the schema changed. publish-chart.sh
// takes its version from Chart.yaml for that reason, and refuses to republish
// an existing one — which is what forces the bump.
func TestTheCRDChartIsVersionedOnItsOwnSchema(t *testing.T) {
	chart := readChart(t, crdChartPath)
	if chart.Annotations[testedWithYBAKey] != "" {
		t.Error("the CRD chart carries a tested-with-yba annotation. The schema is YBA-agnostic " +
			"by design — resources[].spec is preserve-unknown-fields — so the claim is noise here")
	}

	// appVersion is the served API version, since this chart deploys no image.
	// A new served version with a stale appVersion would misreport what the
	// cluster got, and nothing else compares the two.
	crd, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read the CRD: %v", err)
	}
	var doc struct {
		Spec struct {
			Versions []struct {
				Name string `yaml:"name"`
			} `yaml:"versions"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(crd, &doc); err != nil {
		t.Fatalf("parse the CRD: %v", err)
	}
	if len(doc.Spec.Versions) != 1 {
		t.Fatalf("the CRD serves %d versions; appVersion can no longer name just one",
			len(doc.Spec.Versions))
	}
	if chart.AppVersion != doc.Spec.Versions[0].Name {
		t.Errorf("the CRD chart's appVersion is %q but the CRD serves %q",
			chart.AppVersion, doc.Spec.Versions[0].Name)
	}
}
