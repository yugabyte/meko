package foundation

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	spec := loadExample(t, "amp.yaml")
	manifests, err := spec.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(manifests) == 0 {
		t.Fatal("Render produced nothing")
	}
	for _, m := range manifests {
		golden := filepath.Join("testdata", "amp", m.Name)
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
	spec.Universe["gFlags"] = map[string]any{"tserverGFlags": map[string]any{
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
	crSpec := renderedSpec(t, spec, "ybuniverse.json")

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

// renderedSpec is the spec body of one rendered manifest.
func renderedSpec(t *testing.T, spec *Spec, name string) map[string]any {
	t.Helper()
	manifests, err := spec.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, m := range manifests {
		if m.Name != name {
			continue
		}
		var cr map[string]any
		if err := json.Unmarshal(m.JSON, &cr); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		body, _ := cr["spec"].(map[string]any)
		return body
	}
	t.Fatalf("Render produced no %s", name)
	return nil
}

// TestPassthroughReachesTheCR: the point of the design. A field this package
// does not model, set in the spec file, must arrive in the CR unchanged — that
// is what makes a new operator release need no code here.
func TestPassthroughReachesTheCR(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	// Real CRD fields this package has no knowledge of.
	spec.Universe["paused"] = false
	spec.Universe["useYbdbInbuiltYbc"] = true
	spec.Universe["masterResourceSpec"] = map[string]any{"cpu": 2, "memory": 8}

	body := renderedSpec(t, spec, "ybuniverse.json")
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
	original, _ := spec.Provider["regions"].([]any)
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
	regions, _ := spec.Provider["regions"].([]any)
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
	regions, _ := spec.Provider["regions"].([]any)
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
