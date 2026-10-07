package foundation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestLoadExampleAmp is the check that keeps examples/foundation-cli.yaml honest: it is
// the file an operator copies, and a spec that no longer parses (or silently
// loses a field to a rename) is a broken starting point.
func TestLoadExampleAmp(t *testing.T) {
	spec := loadExample(t, "foundation-cli.yaml")

	if spec.Namespace != "amp" {
		t.Errorf("namespace = %q, want amp", spec.Namespace)
	}
	if n, _ := intField(spec.universe(), "numNodes"); n != 3 {
		t.Errorf("universe.numNodes = %d, want 3", n)
	}
	if rf, _ := intField(spec.universe(), "replicationFactor"); rf != 3 {
		t.Errorf("universe.replicationFactor = %d, want 3", rf)
	}
	// The controller's DSN is passwordless, and enableYSQLAuth is create-time
	// only, so this staying off is what keeps the example working against it.
	// It is also what leaves YugabyteDB's trailing trust rule in place after
	// the hba rules below, which is why an encrypted universe does not yet
	// refuse a plaintext client.
	if spec.universe()["enableYSQLAuth"] == true {
		t.Error("enableYSQLAuth is on; the existing controller DSN is passwordless")
	}
	// TLS on both hops, and a CA that is not YBA's own — the three together are
	// what make the reported DSN verify-full. All three are create-time only,
	// so a spec that lost one would need the universe rebuilt to put it back.
	if spec.universe()["enableNodeToNodeEncrypt"] != true {
		t.Error("enableNodeToNodeEncrypt is off; the compute hot path's RPC lane would be plaintext")
	}
	if spec.universe()["enableClientToNodeEncrypt"] != true {
		t.Error("enableClientToNodeEncrypt is off; the YSQL lane would be plaintext")
	}
	if ca, _ := spec.universe()["rootCA"].(string); ca == "" {
		t.Error("no rootCA: YBA would invent its own CA, which nothing else trusts, and the best " +
			"DSN available would be sslmode=require")
	}
	if got := spec.SSLMode(); got != "verify-full" {
		t.Errorf("SSLMode = %q, want verify-full", got)
	}
	// The provider is what carries the DB image registry, its pull secret and
	// the per-zone storage class — none of which the operator's auto-provider
	// can infer, so the example must ship one.
	if len(spec.provider()) == 0 {
		t.Fatal("the example has no provider block")
	}
	if spec.ProviderName() == "" {
		t.Error("the universe does not reference the provider")
	}
	ci, _ := spec.provider()["cloudInfo"].(map[string]any)
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
// testVersion is the minimum a universe body needs to pass Validate, so that a
// terse fixture can be about the one thing it is testing.
//
// ybSoftwareVersion because an empty or absent one is a hard error — YBA parses
// it and rejects the universe with no reason attached — and the rest because
// the deployer fills nothing in: a spec has to state the address contract and
// say what it wants about encryption, or it is refused. endpointBody is the
// same block without the encryption flags, for tables that set those.
const testVersion = versionLine + contractBody

// versionLine alone, for the few fixtures that state the contract themselves.
const versionLine = "    ybSoftwareVersion: 2026.1.1.1-b2\n"

// minimalSpec is the smallest valid spec: both required kinds in order, plus
// the universe body the caller cares about.
func minimalSpec(universeBody string) string {
	return bareSpec(contractBody + universeBody)
}

// contractBody is what every spec has to say for itself now: a stated answer on
// encryption, plus endpointBody.
const contractBody = `    enableClientToNodeEncrypt: false
    enableNodeToNodeEncrypt: false` + endpointBody

// endpointBody is the address contract alone, for tables that state the
// encryption flags themselves.
const endpointBody = `
    enableLoadBalancer: true
    kubernetesOverrides:
      serviceEndpoints:
        - name: yb-tserver-conn
          type: ClusterIP
          scope: Namespaced
          app: yb-tserver
          ports:
            tcp-ysql-port: "5433"
            tcp-rpc-port: "9100"
`

// bareSpec builds a spec whose universe body is EXACTLY what is passed, with no
// testVersion in front — for the tests about Validate's own rules.
func bareSpec(universeBody string) string {
	return "namespace: amp\n" +
		"YBProvider:\n  name: p\n  spec: {}\n" +
		"YBUniverse:\n  name: foundation\n  spec:\n" + versionLine + universeBody
}

// the worst kind of failure, because it looks like success.
func TestLoadRejectsUnknownFields(t *testing.T) {
	_, err := Load([]byte(minimalSpec("") + "univerze: {}\n"))
	if err == nil {
		t.Fatal("Load accepted an unknown field")
	}
	if !strings.Contains(err.Error(), "univerze") {
		t.Errorf("error %q does not name the offending field", err)
	}
}

func TestDefaults(t *testing.T) {
	spec, err := Load([]byte(minimalSpec("")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if spec.ChartVersion != DefaultChartVersion {
		t.Errorf("chartVersion = %q, want %q", spec.ChartVersion, DefaultChartVersion)
	}
	if spec.ChartSource != DefaultChartSource {
		t.Errorf("chartSource = %q, want %q", spec.ChartSource, DefaultChartSource)
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
		yaml:    strings.Replace(minimalSpec(""), "namespace: amp\n", "", 1),
		wantErr: "namespace is required",
	}, {
		// RF 3 across 1 node cannot place replicas, and YBA would reject it
		// asynchronously — much later and much less clearly.
		name:    "numNodes below RF",
		yaml:    minimalSpec("    numNodes: 1\n    replicationFactor: 3\n"),
		wantErr: "below replicationFactor",
	}, {
		name:    "bad chart source",
		yaml:    "chartSource: ftp\n" + minimalSpec(""),
		wantErr: "not one of repo, path",
	}, {
		// The s3 source only ever built a releases.yugabyte.com URL, which is
		// an ordinary path to helm. A spec still saying it should be told
		// what to write, not just that the value is unknown.
		name:    "the retired s3 source names its replacement",
		yaml:    "chartSource: s3\n" + minimalSpec(""),
		wantErr: "use chartSource: path with yba.chart",
	}, {
		name:    "path source needs a chart",
		yaml:    "chartSource: path\n" + minimalSpec(""),
		wantErr: "needs yba.chart",
	}, {
		name:    "no ybSoftwareVersion",
		yaml:    "namespace: amp\nYBProvider:\n  name: p\n  spec: {}\nYBUniverse:\n  name: u\n  spec: {}\n",
		wantErr: "ybSoftwareVersion is required",
	}, {
		name: "RF 1 single node is allowed",
		yaml: minimalSpec("    numNodes: 1\n    replicationFactor: 1\n"),
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
	spec := loadExample(t, "foundation-cli.yaml")
	if got := universeInt(spec.universe(), "numNodes"); got != 3 {
		t.Errorf("numNodes = %d, want 3", got)
	}
	if got := universeInt(spec.universe(), "replicationFactor"); got != 3 {
		t.Errorf("replicationFactor = %d, want 3", got)
	}
	placement, ok := spec.universe()["placementInfo"].(map[string]any)
	if !ok {
		t.Fatal("the example declares no placementInfo")
	}
	total := 0
	for _, r := range asList(placement["regions"]) {
		region, _ := r.(map[string]any)
		for _, z := range asList(region["zones"]) {
			zone, _ := z.(map[string]any)
			code, _ := zone["code"].(string)
			if !providerZones(spec.provider())[code] {
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
	spec := loadExample(t, "foundation-cli.yaml")
	g, _ := spec.universe()["gFlags"].(map[string]any)
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
	spec := loadExample(t, "foundation-cli.yaml")
	spec.universe()["placementInfo"] = placement("us-west1-c", 3)
	err := spec.Validate()
	if err == nil {
		t.Fatal("Validate accepted a placement naming a zone the provider does not define")
	}
	if !strings.Contains(err.Error(), "the provider does not have") {
		t.Errorf("error %q does not say which zone is missing", err)
	}
}

// TestValidateLeavesYBAsOwnRulesToYBA: which replication factors YBA supports,
// and how it wants a placement to add up, are YBA's to decide and move between
// releases. A copy of either here goes stale and starts rejecting specs YBA
// would accept — the same argument that keeps the CRD schema check and the
// preview-gflag check out of this package.
//
// This is here so the checks are not quietly reinstated: a removal needs a
// test as much as an addition does.
func TestValidateLeavesYBAsOwnRulesToYBA(t *testing.T) {
	spec := loadExample(t, "foundation-cli.yaml")
	spec.universe()["replicationFactor"] = 4
	spec.universe()["numNodes"] = 4
	spec.universe()["placementInfo"] = map[string]any{"regions": []any{map[string]any{
		"code":  "us-west1",
		"zones": []any{map[string]any{"code": "us-west1-b", "numNodes": 99}},
	}}}
	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate second-guessed YBA: %v", err)
	}
}

// TestStorageClassesIncludesYugawareOwn: yugaware's PVC is a different class
// from the universe's, and unset it takes the cluster default — an
// Immediate-binding one zone-pins the ~4.5 CPU pod before it schedules, which
// is the whole reason preflight checks binding modes.
func TestStorageClassesIncludesYugawareOwn(t *testing.T) {
	spec := loadExample(t, "foundation-cli.yaml")
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
		"{}",                        // absent
		"{ybSoftwareVersion: \"\"}", // explicitly empty
	} {
		_, err := Load([]byte("namespace: amp\n" +
			"YBProvider:\n  name: p\n  spec: {}\n" +
			"YBUniverse:\n  name: u\n  spec: " + universe + "\n"))
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
	spec := loadExample(t, "foundation-cli.yaml")
	version := universeString(spec.universe(), "ybSoftwareVersion")
	// The yugaware a chart installs bundles only its own release, so the two
	// have to agree or the universe is created against a version YBA does not
	// have. How they agree depends on what chartVersion names:
	//
	//   a published line ("2026.1.1")  -> the DB version is ON it ("2026.1.1.2-b9")
	//   a full build ("2026.1.2.0-b137") -> the chart's appVersion IS that build,
	//                                       so the two are equal
	onTheLine := strings.HasPrefix(version, spec.ChartVersion+".")
	if version != spec.ChartVersion && !onTheLine {
		t.Errorf("ybSoftwareVersion %q does not match chartVersion %q; the yugaware that chart "+
			"installs bundles only its own release, so the universe would be created "+
			"against a version YBA does not have", version, spec.ChartVersion)
	}
}

// TestTheSpecIsTheRenderedBody is the structural guard for the class of bug
// that produced a plaintext universe with an sslmode=require DSN.
//
// Every default this deployer owns for the universe is applied at parse time,
// so the YBUniverse entry's Spec and the rendered body are the SAME map. Anything
// reading the Spec — Connect's sslMode, the storage-class preflight, Validate —
// therefore sees exactly what the cluster gets. Move a default back into
// rendering and this fails.
func TestTheSpecIsTheRenderedBody(t *testing.T) {
	spec := loadExample(t, "foundation-cli.yaml")
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
		if _, ok := spec.universe()[key]; !ok {
			t.Errorf("the CR has %q but the Spec does not: a default applied at render time is "+
				"invisible to everything that reads the Spec", key)
		}
	}
	for key := range spec.universe() {
		if _, ok := cr.Spec[key]; !ok {
			t.Errorf("the Spec has %q but the CR does not", key)
		}
	}
}

// TestLoadIsIdempotent: withDefaults writes into the maps it was given, so
// running it twice must not accumulate — a second serviceEndpoints entry would
// mean two Services fighting over one Namespaced endpoint.
func TestLoadIsIdempotent(t *testing.T) {
	yaml := readExample(t, "foundation-cli.yaml")
	first, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	first.withDefaults()
	first.withDefaults()

	overrides, _ := first.universe()["kubernetesOverrides"].(map[string]any)
	eps := asList(overrides["serviceEndpoints"])
	if len(eps) != 1 {
		t.Errorf("serviceEndpoints has %d entries after repeated defaulting, want 1", len(eps))
	}
	second, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a, _ := json.Marshal(first.universe())
	b, _ := json.Marshal(second.universe())
	if string(a) != string(b) {
		t.Errorf("defaulting twice differs from once:\n once: %s\ntwice: %s", b, a)
	}
}

// TestATildePathIsExpandedInChartAndCrdPath pins the one thing that makes a
// locally downloaded release usable from a spec written the way a person
// writes a path.
//
// Neither Go, helm nor kubectl expands ~: a YAML value is not a shell word. So
// before this, "~/yb/yugaware.tgz" was looked up as a directory literally named
// "~" and the error named a path that visibly exists.
func TestATildePathIsExpandedInChartAndCrdPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	spec, err := Load([]byte("chartSource: path\n" +
		"crdPath: ~/yb/concatenated_crd.yaml\n" +
		"yba:\n  chart: ~/yb/yugaware-2026.1.1.tgz\n" + minimalSpec("")))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if want := filepath.Join(home, "yb/yugaware-2026.1.1.tgz"); spec.YBA.Chart != want {
		t.Errorf("yba.chart = %q, want %q", spec.YBA.Chart, want)
	}
	if want := filepath.Join(home, "yb/concatenated_crd.yaml"); spec.CRDPath != want {
		t.Errorf("crdPath = %q, want %q", spec.CRDPath, want)
	}
}

// TestOnlyALeadingTildeIsExpanded keeps the expansion from touching a chart
// NAME, which is what yba.chart holds under chartSource repo, or a URL.
func TestOnlyALeadingTildeIsExpanded(t *testing.T) {
	for _, in := range []string{
		"yugabytedb/yugaware",
		"https://example.test/yugaware.tgz",
		"/abs/~/yugaware.tgz",
		"~user/yugaware.tgz",
		"",
	} {
		if got := expandHome(in); got != in {
			t.Errorf("expandHome(%q) = %q, want it untouched", in, got)
		}
	}
}

// TestLoadKeepsDocumentOrder is the contract the whole apply and teardown
// order rests on: keys are applied top to bottom and deleted bottom to top, so
// the order the file was written in has to survive parsing.
//
// Go randomises map iteration, so decoding the document into a map would pass
// this once in a while and fail the rest of the time. It walks a yaml.Node
// instead, and this is the test that catches a map creeping back in.
func TestLoadKeepsDocumentOrder(t *testing.T) {
	both := func(first, second string) []string {
		spec, err := Load([]byte("namespace: amp\n" +
			"YBProvider:\n  name: p\n  spec: {}\n" +
			first + "\n" +
			"YBUniverse:\n  name: u\n  spec:\n" + testVersion +
			second + "\n"))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		var kinds []string
		for _, r := range spec.Resources {
			kinds = append(kinds, r.Kind)
		}
		return kinds
	}
	release := "Release:\n  name: r\n  spec: {}"
	backup := "BackupSchedule:\n  name: b\n  spec: {}"

	// Repeated, because a map would give the right answer some of the time.
	for i := 0; i < 20; i++ {
		if got := both(release, backup); !reflect.DeepEqual(got,
			[]string{"YBProvider", "Release", "YBUniverse", "BackupSchedule"}) {
			t.Fatalf("kinds = %v, want document order", got)
		}
		if got := both(backup, release); !reflect.DeepEqual(got,
			[]string{"YBProvider", "BackupSchedule", "YBUniverse", "Release"}) {
			t.Fatalf("kinds = %v, want the reordered document's order", got)
		}
	}
}

// TestUppercaseKeysAreCRKinds and TestLowercaseTyposAreStillParseErrors are
// the two halves of the rule that lets any kind be declared without this
// package knowing it.
//
// The case split is what keeps KnownFields working: without it a mistyped
// `namespaec:` would quietly become a CR named "namespaec" instead of an
// error, which is the worst failure shape there is.
func TestUppercaseKeysAreCRKinds(t *testing.T) {
	spec, err := Load([]byte(minimalSpec("") +
		"KMSConfig:\n  name: kms\n  spec:\n    name: kms\n    provider: AWS\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := spec.find("KMSConfig")
	if r == nil {
		t.Fatal("KMSConfig was not read as a CR kind")
	}
	if r.Name != "kms" || r.Spec["provider"] != "AWS" {
		t.Errorf("entry = %+v, want the body passed through", r)
	}
	// The kind's own spec.name must survive: it is required by this CRD.
	if r.Spec["name"] != "kms" {
		t.Errorf("spec.name = %v, want it kept", r.Spec["name"])
	}
}

func TestLowercaseTyposAreStillParseErrors(t *testing.T) {
	_, err := Load([]byte(minimalSpec("") + "namespaec: amp\n"))
	if err == nil {
		t.Fatal("Load accepted a lowercase typo")
	}
	if !strings.Contains(err.Error(), "namespaec") {
		t.Errorf("error %q does not name the offending key", err)
	}
}

// TestSeveralOfOneKindAsAList: two backup targets are an ordinary thing to
// want, and a mapping key can only appear once.
func TestSeveralOfOneKindAsAList(t *testing.T) {
	spec, err := Load([]byte(minimalSpec("") +
		"StorageConfig:\n" +
		"  - name: backups\n    spec: {bucket: a}\n" +
		"  - name: archive\n    spec: {bucket: b}\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var names []string
	for _, r := range spec.Resources {
		if r.Kind == "StorageConfig" {
			names = append(names, r.Name)
		}
	}
	if !reflect.DeepEqual(names, []string{"backups", "archive"}) {
		t.Errorf("names = %v, want the list's own order", names)
	}
}

func TestASingleEntryNeedNotBeAList(t *testing.T) {
	spec, err := Load([]byte(minimalSpec("") + "Release:\n  name: r\n  spec: {version: v}\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if spec.count("Release") != 1 {
		t.Fatalf("Release count = %d, want 1", spec.count("Release"))
	}
}

func TestAnUnknownKeyInsideAnEntryIsAParseError(t *testing.T) {
	_, err := Load([]byte(minimalSpec("") + "Release:\n  nmae: r\n  spec: {}\n"))
	if err == nil || !strings.Contains(err.Error(), "nmae") {
		t.Fatalf("err = %v, want it to name the misspelled key", err)
	}
}

func TestACRValueMustBeAMapOrAListOfMaps(t *testing.T) {
	for _, bad := range []string{
		"Release: just-a-string\n",
		"Release:\n  - just-a-string\n",
	} {
		_, err := Load([]byte(minimalSpec("") + bad))
		if err == nil || !strings.Contains(err.Error(), "Release") {
			t.Errorf("err = %v for %q, want it to name the kind", err, bad)
		}
	}
}

// TestLegacyKeysNameTheirReplacement: provider: and universe: were spec fields
// until they became CR kinds like every other. KnownFields would reject them
// anyway, but with "field not found", which does not tell you what to write.
func TestLegacyKeysNameTheirReplacement(t *testing.T) {
	for old, want := range map[string]string{"provider": "YBProvider", "universe": "YBUniverse"} {
		_, err := Load([]byte("namespace: amp\n" + old + ":\n  name: x\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v for %q, want it to name %q", err, old, want)
		}
	}
}

// TestValidateStructure covers the rules about the CR set itself, each of
// which is something the API server either accepts happily or rejects while
// naming nothing the author can find in their file.
func TestValidateStructure(t *testing.T) {
	const prov = "YBProvider:\n  name: p\n  spec: {}\n"
	const univ = "YBUniverse:\n  name: u\n  spec:\n" + testVersion

	for _, tc := range []struct {
		name, yaml, wantErr string
	}{{
		// A provider is optional: the operator has an auto-provider, and a
		// universe can name one somebody else registered.
		name: "no provider is fine",
		yaml: "namespace: amp\n" + univ,
	}, {
		name:    "two providers",
		yaml:    "namespace: amp\n" + prov + "YBProvider:\n  name: p2\n  spec: {}\n" + univ,
		wantErr: "declared more than once",
	}, {
		name:    "no universe",
		yaml:    "namespace: amp\n" + prov,
		wantErr: "exactly one YBUniverse",
	}, {
		name:    "two universes",
		yaml:    "namespace: amp\n" + prov + univ + "YBUniverse:\n  name: u2\n  spec: {}\n",
		wantErr: "exactly one YBUniverse",
	}, {
		name:    "provider below universe",
		yaml:    "namespace: amp\n" + univ + prov,
		wantErr: "must be declared above",
	}, {
		name:    "an entry with no name",
		yaml:    "namespace: amp\n" + prov + univ + "Release:\n  spec: {}\n",
		wantErr: "name is required",
	}, {
		name:    "an entry with no spec",
		yaml:    "namespace: amp\n" + prov + univ + "Release:\n  name: r\n",
		wantErr: "spec is required",
	}, {
		name: "the same kind and name twice",
		yaml: "namespace: amp\n" + prov + univ +
			"StorageConfig:\n  - name: dup\n    spec: {}\n  - name: dup\n    spec: {}\n",
		wantErr: "declared twice",
	}, {
		name: "several distinct extras are fine",
		yaml: "namespace: amp\n" + prov + "Release:\n  name: r\n  spec: {}\n" + univ +
			"StorageConfig:\n  - name: a\n    spec: {}\n  - name: b\n    spec: {}\n",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte(tc.yaml))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestChartVersionIsRequiredForTheRepoSource guards the rule against a future
// change to withDefaults. It goes through Validate rather than Load because
// the default is filled in first, so a spec cannot express the empty case.
// TestTheDefaultChartIsResolvable: the default version and the default source
// have to agree, and the pairing is easy to break one half at a time. The
// repo source resolves a PUBLISHED version, and only released lines are
// published — so a default naming a build would be a helm lookup that cannot
// succeed.
func TestTheDefaultChartIsResolvable(t *testing.T) {
	spec, err := Load([]byte(minimalSpec("")))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if spec.ChartSource != ChartRepo {
		t.Fatalf("the default chart source is %q; this test only knows how to check repo",
			spec.ChartSource)
	}
	if len(strings.Split(spec.ChartVersion, ".")) > 3 {
		t.Errorf("chartVersion %q is a build, and the repo source can only resolve a published "+
			"line — a build is deployed by naming its tarball in yba.chart", spec.ChartVersion)
	}
	if spec.YBA.Chart == "" {
		t.Error("the repo source needs a chart name")
	}
}

func TestChartVersionIsRequiredForTheRepoSource(t *testing.T) {
	spec := loadExample(t, "foundation-cli.yaml")
	spec.ChartSource = ChartRepo
	spec.ChartVersion = ""
	if err := spec.Validate(); err == nil || !strings.Contains(err.Error(), "chartVersion is required") {
		t.Fatalf("err = %v, want it to require chartVersion", err)
	}
	spec.ChartSource = ChartPath
	spec.YBA.Chart = "/tmp/chart.tgz"
	if err := spec.Validate(); err != nil {
		t.Fatalf("a path source should not need a chartVersion: %v", err)
	}
}

// TestNamesInTheBodyMustMatchTheEntry: metadata.name and the body's own name
// field are different fields that both name the same object, and nothing
// couples them — Render names the CR from the entry while YBA reads the body.
//
// Spelling them differently is the natural mistake when migrating from the old
// provider:/universe: blocks, where the name lived in the body. Uncaught, it
// burns the full universe timeout reporting "waiting for universe X to appear"
// while X is healthy under the other name.
func TestNamesInTheBodyMustMatchTheEntry(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, wantErr string
	}{{
		name: "universeName disagrees with the entry",
		yaml: "namespace: amp\nYBProvider:\n  name: p\n  spec: {}\n" +
			"YBUniverse:\n  name: foundation\n  spec:\n" + testVersion +
			"    universeName: something-else\n",
		wantErr: "spec.universeName says",
	}, {
		name: "providerName disagrees with the provider entry",
		yaml: "namespace: amp\nYBProvider:\n  name: foundation\n  spec: {}\n" +
			"YBUniverse:\n  name: u\n  spec:\n" + testVersion +
			"    providerName: something-else\n",
		wantErr: "providerName says",
	}, {
		name: "agreeing is fine",
		yaml: "namespace: amp\nYBProvider:\n  name: foundation\n  spec: {}\n" +
			"YBUniverse:\n  name: u\n  spec:\n" + testVersion +
			"    universeName: u\n    providerName: foundation\n",
	}, {
		// No provider of our own: providerName names someone else's, and
		// there is no entry to disagree with.
		name: "a preexisting provider is not cross-checked",
		yaml: "namespace: amp\nYBUniverse:\n  name: u\n  spec:\n" + testVersion +
			"    providerName: shared-gke\n",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte(tc.yaml))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestKubectlNamesComeFromTheEntry: every lookup addresses metadata.name, so
// the accessors have to return the entry's name rather than the body's.
func TestKubectlNamesComeFromTheEntry(t *testing.T) {
	spec, err := Load([]byte("namespace: amp\nYBProvider:\n  name: prov\n  spec: {}\n" +
		"YBUniverse:\n  name: univ\n  spec:\n" + testVersion))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if spec.UniverseName() != "univ" {
		t.Errorf("UniverseName = %q, want the entry's name", spec.UniverseName())
	}
	if spec.ProviderName() != "prov" {
		t.Errorf("ProviderName = %q, want the entry's name", spec.ProviderName())
	}
}

// TestPlacementIsNotCheckedAgainstAProviderWeDoNotDeclare: the zone check is a
// cross-reference between two halves of the spec file, so it needs both
// halves. With a preexisting provider the zones live in YBA, where nothing
// here can see them — checking against an empty set rejected every zone with
// "which the provider does not have (it has )".
func TestPlacementIsNotCheckedAgainstAProviderWeDoNotDeclare(t *testing.T) {
	placement := "    placementInfo:\n      regions:\n        - code: us-west1\n" +
		"          zones:\n            - {code: us-west1-b, numNodes: 3}\n"

	// No YBProvider: the placement is YBA's to judge.
	if _, err := Load([]byte("namespace: amp\nYBUniverse:\n  name: u\n  spec:\n" +
		testVersion + "    providerName: shared-gke\n" + placement)); err != nil {
		t.Fatalf("a placement against a preexisting provider was rejected: %v", err)
	}
	// With one, the cross-reference is checkable and still checked.
	_, err := Load([]byte("namespace: amp\n" +
		"YBProvider:\n  name: p\n  spec:\n    regions:\n      - code: us-west1\n" +
		"        zones:\n          - {code: us-west1-a}\n" +
		"YBUniverse:\n  name: u\n  spec:\n" + testVersion + placement))
	if err == nil || !strings.Contains(err.Error(), "the provider does not have") {
		t.Fatalf("err = %v, want the zone mismatch", err)
	}
}

// TestResourcesListRendersIdenticallyToTopLevelKinds.
//
// Both forms exist at once so a spec can move without a flag day. That is only
// true if they mean exactly the same thing, so this renders a spec twice — once
// each way — and compares the manifests byte for byte, order included.
func TestResourcesListRendersIdenticallyToTopLevelKinds(t *testing.T) {
	const topLevel = `
namespace: amp
chartVersion: "2026.1.3.0-b21"
chartSource: path
yba:
  chart: https://example.invalid/yugaware.tgz
  values: {yugaware: {defaultUser: {password: hunter2}}}
YBProvider:
  name: gke
  spec:
    cloudInfo: {kubernetes: {kubernetesProvider: gke}}
    regions: [{code: us-west1, zones: [{code: us-west1-b}]}]
YBUniverse:
  name: foundation
  spec:
    universeName: foundation
    numNodes: 1
    replicationFactor: 1
    ybSoftwareVersion: 2026.1.3.0-b21
    providerName: gke
    enableLoadBalancer: true
    placementInfo:
      cloudList: [{code: kubernetes, regionList: [{code: us-west1, azList: [{name: us-west1-b, numNodesInAZ: 1, replicationFactor: 1}]}]}]
    kubernetesOverrides:
      serviceEndpoints:
        - {name: yb-tserver-conn, type: ClusterIP, scope: Namespaced, app: yb-tserver, ports: {tcp-ysql-port: "5433", tcp-rpc-port: "9100"}}
`
	const asList = `
namespace: amp
chartVersion: "2026.1.3.0-b21"
chartSource: path
yba:
  chart: https://example.invalid/yugaware.tgz
  values: {yugaware: {defaultUser: {password: hunter2}}}
resources:
  - kind: YBProvider
    name: gke
    spec:
      cloudInfo: {kubernetes: {kubernetesProvider: gke}}
      regions: [{code: us-west1, zones: [{code: us-west1-b}]}]
  - kind: YBUniverse
    name: foundation
    spec:
      universeName: foundation
      numNodes: 1
      replicationFactor: 1
      ybSoftwareVersion: 2026.1.3.0-b21
      providerName: gke
      enableLoadBalancer: true
      placementInfo:
        cloudList: [{code: kubernetes, regionList: [{code: us-west1, azList: [{name: us-west1-b, numNodesInAZ: 1, replicationFactor: 1}]}]}]
      kubernetesOverrides:
        serviceEndpoints:
          - {name: yb-tserver-conn, type: ClusterIP, scope: Namespaced, app: yb-tserver, ports: {tcp-ysql-port: "5433", tcp-rpc-port: "9100"}}
`
	render := func(src string) []Manifest {
		t.Helper()
		spec, err := Load([]byte(src))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		m, err := spec.Render()
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		return m
	}
	a, b := render(topLevel), render(asList)
	if len(a) != len(b) {
		t.Fatalf("rendered %d manifests from keys and %d from the list", len(a), len(b))
	}
	for i := range a {
		// Name carries the index, so comparing it pins apply order too.
		if a[i].Name != b[i].Name || a[i].Kind != b[i].Kind {
			t.Errorf("manifest %d: keys gave %s/%s, list gave %s/%s",
				i, a[i].Kind, a[i].Name, b[i].Kind, b[i].Name)
		}
		if string(a[i].JSON) != string(b[i].JSON) {
			t.Errorf("manifest %d (%s) differs between the two forms:\nkeys: %s\nlist: %s",
				i, a[i].Kind, a[i].JSON, b[i].JSON)
		}
	}
}

// Mixing the forms is refused rather than guessed at: apply order is document
// order, and nothing says whether a list sits before or after the keys around
// it.
func TestMixingResourceFormsIsRefused(t *testing.T) {
	_, err := Load([]byte(`
namespace: amp
chartVersion: "2026.1.3.0-b21"
yba: {values: {yugaware: {defaultUser: {password: hunter2}}}}
YBCertificate:
  name: amp-ca
  spec: {certType: K8S_CERT_MANAGER}
resources:
  - kind: YBUniverse
    name: foundation
    spec: {ybSoftwareVersion: 2026.1.3.0-b21}
`))
	if err == nil {
		t.Fatal("a spec using both forms was accepted; its apply order is undefined")
	}
	for _, want := range []string{"YBCertificate", "YBUniverse", "resources"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q, so the fix is not obvious", err, want)
		}
	}
}

// An entry with no kind is the one mistake the list form makes possible that
// the key form does not, so it gets its own message.
func TestResourcesListRequiresAKind(t *testing.T) {
	_, err := Load([]byte(`
namespace: amp
chartVersion: "2026.1.3.0-b21"
yba: {values: {yugaware: {defaultUser: {password: hunter2}}}}
resources:
  - name: foundation
    spec: {ybSoftwareVersion: 2026.1.3.0-b21}
`))
	if err == nil {
		t.Fatal("an entry with no kind was accepted")
	}
	if !strings.Contains(err.Error(), "no kind") {
		t.Errorf("error %q does not say the kind is missing", err)
	}
}
