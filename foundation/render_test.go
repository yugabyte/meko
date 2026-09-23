package foundation

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"meko/foundation/ybuniverse"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// TestRenderGolden compares the rendered manifests for examples/amp.yaml
// against golden files.
//
// This is the main testing dividend of having a `render` verb: what CI checks
// and what a cluster receives are the same bytes, and a reviewer can read the
// golden file against the CRD instead of reconstructing a manifest from argv
// assertions. Regenerate with `go test ./... -update` and READ THE DIFF — a
// changed golden file is a changed thing applied to a cluster.
func TestRenderGolden(t *testing.T) {
	for _, example := range []string{"amp.yaml", "resources.yaml"} {
		t.Run(example, func(t *testing.T) {
			checkGolden(t, example, strings.TrimSuffix(example, ".yaml"))
		})
	}
}

func checkGolden(t *testing.T, example, dir string) {
	t.Helper()
	spec := loadExample(t, example)
	manifests, err := spec.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(manifests) == 0 {
		t.Fatal("Render produced nothing")
	}
	// Cleared first, so a renamed or removed manifest does not leave an
	// orphan golden behind. An orphan is invisible otherwise: nothing compares
	// against it, and `git diff` sees an unchanged tracked file.
	if *update {
		if err := os.RemoveAll(filepath.Join("testdata", dir)); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range manifests {
		golden := filepath.Join("testdata", dir, m.Name)
		if *update {
			if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(golden, m.JSON, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("read golden %s (regenerate with -update): %v", golden, err)
		}
		if string(m.JSON) != string(want) {
			t.Errorf("%s does not match %s\n--- got ---\n%s\n--- want ---\n%s",
				m.Name, golden, m.JSON, want)
		}
	}
}

// TestRenderIsDeterministic: a manifest whose key order moved between renders
// would show up as a spurious diff in review and would make GitOps re-apply an
// unchanged object.
func TestRenderIsDeterministic(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	spec.universe()["gFlags"] = map[string]any{"tserverGFlags": map[string]any{
		"allowed_preview_flags_csv": "ysql_yb_enable_mage",
		"ysql_yb_enable_mage":       "true",
		"ysql_log_statement":        "all",
	}}
	first, err := spec.Render()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := spec.Render()
		if err != nil {
			t.Fatal(err)
		}
		for j := range first {
			if string(first[j].JSON) != string(again[j].JSON) {
				t.Fatalf("%s is not byte-stable across renders", first[j].Name)
			}
		}
	}
}

// TestServiceEndpointOverrideAsksTheChartForTheService covers the mechanism
// that replaced a hand-rolled Service.
//
// The chart already knows how to create namespaced Services, so the CR declares
// a serviceEndpoints entry and the chart renders it — selector included. That
// deletes the whole class of bug a hand-written selector had: the chart's own
// selector cannot be wrong about which pods are tservers.
//
// Two chart values gate it and both are asserted here, because the endpoint
// silently renders NOTHING if either is off:
//   - enableLoadBalancer, which the whole serviceEndpoints template is wrapped in
//   - scope Namespaced, which is skipped unless oldNamingStyle is off (YBA's
//     choice, and not in the CRD's kubernetesOverrides schema)
func TestServiceEndpointOverrideAsksTheChartForTheService(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	crSpec := renderedSpec(t, spec, ybuniverse.KindUniverse)

	// Required gate, and NOT a request for public exposure: overriding the
	// endpoint list replaces the chart's three LoadBalancer defaults, so nothing
	// is exposed despite this being true.
	if crSpec["enableLoadBalancer"] != true {
		t.Error("enableLoadBalancer is not true; the chart renders no serviceEndpoints at all without it")
	}

	eps, ok := crSpec["kubernetesOverrides"].(map[string]any)["serviceEndpoints"].([]any)
	if !ok || len(eps) != 1 {
		t.Fatalf("kubernetesOverrides.serviceEndpoints = %v, want exactly one entry", crSpec["kubernetesOverrides"])
	}
	ep := eps[0].(map[string]any)
	for field, want := range map[string]any{
		"name":  TServerEndpointName,
		"type":  "ClusterIP",
		"scope": "Namespaced",
		// Must equal one of the chart's Services[].label values, or the endpoint
		// matches no workload and renders nothing.
		"app": "yb-tserver",
	} {
		if ep[field] != want {
			t.Errorf("serviceEndpoint.%s = %v, want %v", field, ep[field], want)
		}
	}
	// BOTH client ports on the one Service, keyed by the chart's port NAMES and
	// rendered as strings like its own defaults. This is what lets a single
	// address drive both compute lanes: YSQL uses the host as given, and the
	// direct-RPC lane derives the same host on 9100 — the deploy path cannot set
	// tserver_rpc_addrs, so that derivation is the only route.
	ports := ep["ports"].(map[string]any)
	if got := ports["tcp-ysql-port"]; got != "5433" {
		t.Errorf("ports = %v, want tcp-ysql-port 5433", ports)
	}
	if got := ports["tcp-rpc-port"]; got != "9100" {
		t.Errorf("ports = %v, want tcp-rpc-port 9100", ports)
	}
	if len(ports) != 2 {
		t.Errorf("ports = %v, want exactly the two client ports", ports)
	}

	// The Service is found by label, never by a constructed name: YBA prefixes
	// a Namespaced endpoint with the universe name it generated (suffix and
	// all), so a template would match nothing. These two labels come from
	// values we set, which is what makes them usable as an identity.
	for _, want := range []string{"serviceName=" + TServerEndpointName, "scope=Namespaced"} {
		if !strings.Contains(TServerServiceLabels, want) {
			t.Errorf("TServerServiceLabels = %q, missing %q", TServerServiceLabels, want)
		}
	}
	// Not the chart's own default endpoint name: if our override ever failed to
	// apply, an endpoint of that name would be a LoadBalancer with no 9100, and
	// the Service's presence would stop being proof the override took.
	if TServerEndpointName == "yb-tserver-service" {
		t.Error("the endpoint reuses the chart's default name")
	}
}

// TestHelmSets covers the three settings that make this an operator-driven
// foundation rather than a plain YBA install.
func TestHelmSets(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	sets := spec.HelmSets()

	if sets["yugaware.kubernetesOperatorEnabled"] != "true" {
		t.Error("kubernetesOperatorEnabled is not true; without it there is no operator and no CR reconciliation")
	}
	// The operator watches the release namespace, not a separate one: yugaware,
	// the operator, the CR, the DB pods and the amp pods all share one
	// namespace, which is what up-gke.sh already does.
	if got := sets["yugaware.kubernetesOperatorNamespace"]; got != spec.Namespace {
		t.Errorf("kubernetesOperatorNamespace = %q, want the release namespace %q", got, spec.Namespace)
	}
	// The chart's customer-create job is gated on this and the chart defaults it
	// OFF, so setting the defaultUser fields without it does nothing at all —
	// and YBA then comes up with zero customers, which the operator refuses
	// every CR over.
	if sets["yugaware.defaultUser.enabled"] != "true" {
		t.Error("yugaware.defaultUser.enabled is not true; YBA would come up with no customer " +
			"and the operator would refuse every CR")
	}
	// rbac.create must NOT be forced. The operator does need the ClusterRoles,
	// but the chart already defaults it on and already fails loudly when the
	// operator is enabled with neither it nor a yugaware.serviceAccount —
	// forcing it here would only block that legitimate alternative on a cluster
	// where cluster-scoped RBAC cannot be created.
	if _, forced := sets["rbac.create"]; forced {
		t.Error("rbac.create is forced; that blocks the pre-made-ServiceAccount path the chart " +
			"supports, and it belongs in yba.values where a spec can override it")
	}
}

// TestExampleEnablesRBAC: dropped from the forced set, so the spec is what has
// to carry it — and the operator cannot reconcile a CR without those
// ClusterRoles.
func TestExampleEnablesRBAC(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	rbac, _ := spec.YBA.Values["rbac"].(map[string]any)
	if rbac["create"] != true {
		t.Errorf("yba.values.rbac.create = %v, want true; the operator needs the ClusterRoles "+
			"the chart makes", rbac["create"])
	}
}

// TestSortedSetArgsIsStableAndEscapesCommas: helm splits an unescaped comma
// into a second assignment, so a value containing one would silently become two
// settings.
func TestSortedSetArgsIsStableAndEscapesCommas(t *testing.T) {
	argv := SortedSetArgs(map[string]string{
		"b.key": "has,comma",
		"a.key": "plain",
	})
	want := `--set-string a.key=plain --set-string b.key=has\,comma`
	if got := strings.Join(argv, " "); got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

// TestCRDURLUsesRawGithubusercontent: the docs give raw.github.com, which is a
// redirect kubectl will not follow — the apply fails with unparseable YAML
// rather than a redirect error.
func TestCRDURLUsesRawGithubusercontent(t *testing.T) {
	u := CRDURL("2026.1.1")
	if !strings.HasPrefix(u, "https://raw.githubusercontent.com/yugabyte/charts/2026.1.1/") {
		t.Errorf("CRDURL = %q", u)
	}
	if strings.Contains(u, "raw.github.com/") {
		t.Error("CRDURL uses raw.github.com, which is a redirect kubectl will not follow")
	}
}

// renderedSpec is the spec body of the one rendered manifest of a kind.
func renderedSpec(t *testing.T, spec *Spec, kind string) map[string]any {
	t.Helper()
	manifests, err := spec.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, m := range manifests {
		if m.Kind != kind {
			continue
		}
		var cr map[string]any
		if err := json.Unmarshal(m.JSON, &cr); err != nil {
			t.Fatalf("parse %s: %v", m.Name, err)
		}
		body, _ := cr["spec"].(map[string]any)
		return body
	}
	t.Fatalf("Render produced no %s", kind)
	return nil
}

// TestPassthroughReachesTheCR: the point of the design. A field this package
// does not model, set in the spec file, must arrive in the CR unchanged — that
// is what makes a new operator release need no code here.
func TestPassthroughReachesTheCR(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	// Real CRD fields this package has no knowledge of.
	spec.universe()["paused"] = false
	spec.universe()["useYbdbInbuiltYbc"] = true
	spec.universe()["masterResourceSpec"] = map[string]any{"cpu": 2, "memory": 8}

	body := renderedSpec(t, spec, ybuniverse.KindUniverse)
	if body["paused"] != false || body["useYbdbInbuiltYbc"] != true {
		t.Errorf("passthrough fields did not reach the CR: %v", body)
	}
	res, _ := body["masterResourceSpec"].(map[string]any)
	if res["cpu"] != float64(2) {
		t.Errorf("masterResourceSpec = %v", res)
	}
}

// TestProviderZonesGetTheSpecNamespace: with kubeNamespace unset, YBA generates
// a namespace of its own ("yb-<customer code>-<universe>-<suffix>") and puts the
// DB pods there. Observed on a real run — universe Ready in
// yb-operator-foundation-1954916679 — and it breaks three things at once: the
// pull secret kubernetesOverrides.Image.pullSecretName names is minted in the
// SPEC's namespace and so does not exist there, the tserver Service consumers
// dial is somewhere nothing predicted, and the single-namespace design the rest
// of this module documents is false.
func TestProviderZonesGetTheSpecNamespace(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	spec.Namespace = "ci-pr-fnd-7"

	zones := renderedZones(t, spec)
	if len(zones) == 0 {
		t.Fatal("the example renders no provider zones")
	}
	for _, zone := range zones {
		ci, _ := zone["cloudInfo"].(map[string]any)
		if ci["kubeNamespace"] != "ci-pr-fnd-7" {
			t.Errorf("zone %v has kubeNamespace %v, want the spec's namespace",
				zone["code"], ci["kubeNamespace"])
		}
	}

	// Render must not mutate the caller's spec: the CLI renders and then applies
	// the same Spec, and `up` renders twice.
	original, _ := spec.provider()["regions"].([]any)
	region, _ := original[0].(map[string]any)
	zs, _ := region["zones"].([]any)
	z0, _ := zs[0].(map[string]any)
	ci0, _ := z0["cloudInfo"].(map[string]any)
	if _, leaked := ci0["kubeNamespace"]; leaked {
		t.Error("Render wrote kubeNamespace back into the caller's spec")
	}
}

// TestAZoneCanPutTheUniverseInItsOwnNamespace: kubeNamespace is what YBA obeys,
// so a zone naming its own is how you ask for a universe outside the
// foundation's namespace. The deployer then follows it — UniverseNamespace is
// what every universe-facing lookup and the DSN use.
func TestAZoneCanPutTheUniverseInItsOwnNamespace(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	zoneCloudInfo(t, spec)["kubeNamespace"] = "yb-amp"

	if got := spec.UniverseNamespace(); got != "yb-amp" {
		t.Errorf("UniverseNamespace = %q, want the zone's own %q", got, "yb-amp")
	}
	// And it survives rendering rather than being defaulted over.
	for _, zone := range renderedZones(t, spec) {
		ci, _ := zone["cloudInfo"].(map[string]any)
		if ci["kubeNamespace"] != "yb-amp" {
			t.Errorf("kubeNamespace = %v, want the zone's own value", ci["kubeNamespace"])
		}
	}
	// Unset, it is the foundation's namespace — co-location, which is what
	// every consumer in this repo wants.
	plain := loadExample(t, "amp.yaml")
	if got := plain.UniverseNamespace(); got != plain.Namespace {
		t.Errorf("UniverseNamespace = %q, want the foundation's %q", got, plain.Namespace)
	}
}

// Zones must agree on one namespace: every universe-facing lookup runs in
// exactly one, so zones spread over several would resolve to whichever came
// first. That is a genuinely different feature (a per-zone address), and
// refusing it beats picking silently.
func TestProviderZonesMustAgreeOnOneNamespace(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	regions, _ := spec.provider()["regions"].([]any)
	region, _ := regions[0].(map[string]any)
	zoneCloudInfo(t, spec)["kubeNamespace"] = "yb-amp-a"
	region["zones"] = append(asList(region["zones"]), map[string]any{
		"code":      "us-west1-c",
		"cloudInfo": map[string]any{"kubeNamespace": "yb-amp-c"},
	})

	err := spec.Validate()
	if err == nil {
		t.Fatal("Validate accepted zones in two different namespaces")
	}
	for _, want := range []string{"yb-amp-a", "yb-amp-c", "kubeNamespace"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// zoneCloudInfo returns the first zone's cloudInfo, for tests that need to poke
// at it.
func zoneCloudInfo(t *testing.T, spec *Spec) map[string]any {
	t.Helper()
	regions, _ := spec.provider()["regions"].([]any)
	region, _ := regions[0].(map[string]any)
	zs, _ := region["zones"].([]any)
	z0, _ := zs[0].(map[string]any)
	ci, ok := z0["cloudInfo"].(map[string]any)
	if !ok {
		t.Fatal("the example's first zone has no cloudInfo")
	}
	return ci
}

// renderedZones pulls the zones out of the rendered YBProvider.
func renderedZones(t *testing.T, spec *Spec) []map[string]any {
	t.Helper()
	manifests, err := spec.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var cr map[string]any
	for _, m := range manifests {
		if m.Kind == "YBProvider" {
			if err := json.Unmarshal(m.JSON, &cr); err != nil {
				t.Fatalf("parse the provider: %v", err)
			}
		}
	}
	body, _ := cr["spec"].(map[string]any)
	var out []map[string]any
	for _, r := range asList(body["regions"]) {
		region, _ := r.(map[string]any)
		for _, z := range asList(region["zones"]) {
			if zone, ok := z.(map[string]any); ok {
				out = append(out, zone)
			}
		}
	}
	return out
}

// TestRenderFollowsDocumentOrder: Render walks the spec's resources in the
// order they were written, and nothing sorts. Two specs differing only in the
// position of one kind must render in different orders.
func TestRenderFollowsDocumentOrder(t *testing.T) {
	kinds := func(yaml string) []string {
		spec, err := Load([]byte(yaml))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		manifests, err := spec.Render()
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		var out []string
		for _, m := range manifests {
			out = append(out, m.Kind)
		}
		return out
	}
	const prov = "YBProvider:\n  name: p\n  spec: {}\n"
	const univ = "YBUniverse:\n  name: u\n  spec:\n" + testVersion
	const rel = "Release:\n  name: r\n  spec: {}\n"

	if got := kinds("namespace: amp\n" + prov + rel + univ); !reflect.DeepEqual(got,
		[]string{"YBProvider", "Release", "YBUniverse"}) {
		t.Errorf("kinds = %v, want the Release in the middle", got)
	}
	if got := kinds("namespace: amp\n" + prov + univ + rel); !reflect.DeepEqual(got,
		[]string{"YBProvider", "YBUniverse", "Release"}) {
		t.Errorf("kinds = %v, want the Release last", got)
	}
}

// TestResourceFileNamesCarryTheApplyOrder: the file names are what `render -o
// DIR` writes and what the goldens are compared against, so they are indexed —
// an alphabetical `ls` would otherwise misreport apply order.
func TestResourceFileNamesCarryTheApplyOrder(t *testing.T) {
	spec, err := Load([]byte("namespace: amp\n" +
		"YBProvider:\n  name: p\n  spec: {}\n" +
		"YBUniverse:\n  name: u\n  spec:\n" + testVersion +
		"StorageConfig:\n  - name: backups\n    spec: {}\n  - name: archive\n    spec: {}\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	manifests, err := spec.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var names []string
	for _, m := range manifests {
		names = append(names, m.Name)
	}
	want := []string{
		"1-ybprovider-p.json",
		"2-ybuniverse-u.json",
		"3-storageconfig-backups.json",
		"4-storageconfig-archive.json",
	}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
}

// TestAResourceSpecKeepsItsOwnName is the regression test for the one real bug
// this change had to fix: Render stripped `name` from every body, which is
// right for the provider and fatal for KMSConfig, PitrConfig and DrConfig,
// where spec.name is a REQUIRED field.
func TestAResourceSpecKeepsItsOwnName(t *testing.T) {
	spec, err := Load([]byte(minimalSpec("") +
		"KMSConfig:\n  name: foundation-kms\n  spec:\n    name: foundation-kms\n    provider: AWS\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	manifests, err := spec.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, m := range manifests {
		if m.Kind != "KMSConfig" {
			continue
		}
		var cr map[string]any
		if err := json.Unmarshal(m.JSON, &cr); err != nil {
			t.Fatal(err)
		}
		if got := cr["spec"].(map[string]any)["name"]; got != "foundation-kms" {
			t.Errorf("spec.name = %v, want it passed through — the CRD requires it", got)
		}
		if got := cr["metadata"].(map[string]any)["name"]; got != "foundation-kms" {
			t.Errorf("metadata.name = %v", got)
		}
		return
	}
	t.Fatal("Render produced no KMSConfig")
}

// TestTheBodyIsPassedThroughEvenWhenItHasAName: nothing lifts `name` out of a
// spec body any more, for any kind. That is required for KMSConfig,
// PitrConfig and DrConfig, where spec.name is a field of the CRD.
//
// The cost is that a spec migrated by moving the old provider: block under
// YBProvider.spec still carries a body-level name, which the YBProvider CRD
// does not declare — and that is the API server's to reject on apply, with a
// message naming the field, rather than something to re-implement here.
func TestTheBodyIsPassedThroughEvenWhenItHasAName(t *testing.T) {
	spec, err := Load([]byte("namespace: amp\n" +
		"YBProvider:\n  name: entry\n  spec:\n    name: in-the-body\n" +
		"YBUniverse:\n  name: u\n  spec:\n" + testVersion))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	body := renderedSpec(t, spec, ybuniverse.KindProvider)
	if body["name"] != "in-the-body" {
		t.Errorf("spec.name = %v, want the body passed through untouched", body["name"])
	}
	// The object is still named from the entry, which is what kubectl
	// addresses and what ProviderName returns.
	if spec.ProviderName() != "entry" {
		t.Errorf("ProviderName = %q, want the entry's name", spec.ProviderName())
	}
}

// TestTheExampleKeepsItsNamesOutOfTheBodies guards the migration the examples
// went through: with the body passed through verbatim, a leftover body-level
// name would reach the API server and be rejected there rather than here.
func TestTheExampleKeepsItsNamesOutOfTheBodies(t *testing.T) {
	for _, example := range []string{"amp.yaml", "resources.yaml"} {
		spec := loadExample(t, example)
		for _, kind := range []string{ybuniverse.KindProvider, ybuniverse.KindUniverse} {
			if body := renderedSpec(t, spec, kind); body["name"] != nil {
				t.Errorf("%s: %s body carries a name the CRD does not declare: %v",
					example, kind, body["name"])
			}
		}
	}
}

// TestCrdSourceHonoursTheOverride: `render` prints the CRD apply command it is
// deliberately not running, and it used to print CRDURL directly — which
// ignored crdPath and told the reader to apply a bundle the spec had
// overridden. The two must not be able to disagree.
func TestCrdSourceHonoursTheOverride(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	if got := spec.CRDSource(); got != CRDURL(spec.ChartVersion) {
		t.Errorf("CRDSource = %q, want the version's bundle when no override is set", got)
	}
	spec.CRDPath = "https://example.test/crds.yaml"
	if got := spec.CRDSource(); got != spec.CRDPath {
		t.Errorf("CRDSource = %q, want the override %q", got, spec.CRDPath)
	}
	d := New(spec, (&Recorder{}).Runner())
	if got := d.crdSource(); got != spec.CRDPath {
		t.Errorf("the deployer and the CLI disagree: %q vs %q", got, spec.CRDPath)
	}
}

// TestMetricsExportIsDeclaredNotAssembled pins the shape of YugabyteDB metrics
// export (meko#1405), which is entirely CR fields: YBA renders the collector
// config from these, and nothing in this repo writes a scrape job or a relabel
// rule.
//
// Three of these fields are the whole reason the feature is safe to turn on, and
// each fails silently rather than loudly if it drifts:
//
//   - collectionLevel MINIMAL is the smallest set that actually takes effect.
//     NOT TABLE_OFF, whose name says what we want: YBA's OTel export sends only
//     priority_regex, metrics and show_help, and TABLE_OFF's params file defines
//     neither of the first two, so it degrades to no filter at all and emits
//     every per-table series. The CRD accepts it either way, so the guard has to
//     live here.
//   - the target list omits CQL_EXPORT, which amp's computes never speak.
//   - an EMPTY exporters list is not "scrape but ship nowhere": YBA gates the
//     sidecar on there being an exporter, so it means no collector at all. Since
//     the identity tags hang off the exporter entry, that is also what makes it
//     impossible to ship a series with no identity on it.
func TestMetricsExportIsDeclaredNotAssembled(t *testing.T) {
	spec := loadExample(t, "resources.yaml")
	crSpec := renderedSpec(t, spec, "YBUniverse")

	metrics, ok := crSpec["telemetry"].(map[string]any)["metrics"].(map[string]any)
	if !ok {
		t.Fatalf("telemetry.metrics = %v, want a map", crSpec["telemetry"])
	}
	if got := metrics["collectionLevel"]; got != "MINIMAL" {
		t.Errorf("collectionLevel = %v, want MINIMAL. TABLE_OFF in particular is a trap: "+
			"the export path never sends its parameters, so it collects MORE than this, not less", got)
	}
	targets := metrics["scrapeConfigTargets"].([]any)
	want := []any{"MASTER_EXPORT", "TSERVER_EXPORT", "YSQL_EXPORT"}
	if !reflect.DeepEqual(targets, want) {
		t.Errorf("scrapeConfigTargets = %v, want %v", targets, want)
	}

	exporters, _ := metrics["exporters"].([]any)
	if len(exporters) != 1 {
		t.Fatalf("exporters = %v, want exactly one — an empty list is no collector at all", exporters)
	}
	exp := exporters[0].(map[string]any)
	if got := exp["telemetryProvider"]; got != "byoc-metrics" {
		t.Errorf("telemetryProvider = %v, want the TelemetryProvider entry's name", got)
	}
	for tag, want := range map[string]string{"source": "amp", "environment_id": "acme-prod"} {
		if got := exp["additionalTags"].(map[string]any)[tag]; got != want {
			t.Errorf("additionalTags[%q] = %v, want %q", tag, got, want)
		}
	}
	if _, set := exp["additionalTags"].(map[string]any)["account_id"]; !set {
		t.Error("additionalTags has no account_id; the fleet identity is both ids, not one")
	}
}

// TestTelemetryProviderCarriesNoCredential: the BYOC password reaches YBA as a
// Secret reference, never as a value in the CR.
//
// A TelemetryProvider spec is immutable in every field, so a password written
// here could not even be rotated without deleting the resource — which YBA
// refuses while a universe references it. The Secret itself is created
// out-of-band; Preflight is what checks it exists.
func TestTelemetryProviderCarriesNoCredential(t *testing.T) {
	spec := loadExample(t, "resources.yaml")
	crSpec := renderedSpec(t, spec, "TelemetryProvider")

	if got := crSpec["provider"]; got != "OTLP" {
		t.Errorf("provider = %v, want OTLP", got)
	}
	otlp := crSpec["otlp"].(map[string]any)
	if got := otlp["authType"]; got != "BasicAuth" {
		t.Fatalf("authType = %v, want BasicAuth", got)
	}
	auth := otlp["basicAuth"].(map[string]any)
	if _, leaked := auth["password"]; leaked {
		t.Error("basicAuth carries a literal password; it must be a passwordSecret reference")
	}
	ref, ok := auth["passwordSecret"].(map[string]any)
	if !ok {
		t.Fatalf("passwordSecret = %v, want a {name,key} reference", auth["passwordSecret"])
	}
	if ref["name"] != "byoc-metrics-auth" || ref["key"] != "password" {
		t.Errorf("passwordSecret = %v, want the byoc-metrics-auth/password the install hint creates", ref)
	}
	// The tags that vary per environment belong on the universe's exporter,
	// which is editable; this spec is frozen the moment it is created.
	if _, set := crSpec["tags"]; set {
		t.Error("the provider declares tags, which are immutable — per-environment identity belongs in the universe's additionalTags")
	}
}
