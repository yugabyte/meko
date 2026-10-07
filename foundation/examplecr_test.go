package foundation

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The CR example is applied to a cluster by CI with `kubectl apply -n $NS`,
// which FAILS against a manifest that declares a different namespace. So it must
// declare none, and spec.namespace must be absent too — the loop then defaults
// it from the object's own namespace.
//
// Without this the lane breaks only on a run whose namespace is not the one
// written in the file, which is every run.
func TestTheCRExampleTakesItsNamespaceFromKubectl(t *testing.T) {
	data, err := os.ReadFile("examples/foundation-cr.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Metadata struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
		Spec map[string]any `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Metadata.Namespace != "" {
		t.Errorf("metadata.namespace is %q; `kubectl apply -n <other>` would be refused, "+
			"so a per-run namespace cannot use this file", doc.Metadata.Namespace)
	}
	if ns, ok := doc.Spec["namespace"]; ok {
		t.Errorf("spec.namespace is %v; leaving it out is what makes the object's own "+
			"namespace the default", ns)
	}
	// The chart watches a fixed name, defaulted to this one.
	if doc.Metadata.Name != "foundation" {
		t.Errorf("metadata.name = %q, want the name the chart's foundation.name defaults to",
			doc.Metadata.Name)
	}
}

// The spec inside the CR has to load and validate against whichever namespace
// kubectl put the object in — the same path the reconcile loop takes.
func TestTheCRExampleLoadsInAnyNamespace(t *testing.T) {
	data, err := os.ReadFile("examples/foundation-cr.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Spec map[string]any `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(doc.Spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"amp", "amp-pr-1725", "foundation-smoke"} {
		cr := &FoundationCR{Spec: raw}
		cr.Metadata.Namespace = ns
		spec, err := loadCRSpec(cr)
		if err != nil {
			t.Fatalf("namespace %q: %v", ns, err)
		}
		if spec.Namespace != ns {
			t.Errorf("namespace %q: spec.Namespace = %q", ns, spec.Namespace)
		}
		// The universe-facing lookups run in the provider zones' kubeNamespace,
		// which defaults from the above — so a per-run namespace moves the DB
		// pods with it rather than stranding them in a namespace the file named.
		if got := spec.UniverseNamespace(); got != ns {
			t.Errorf("namespace %q: UniverseNamespace = %q, want it to follow", ns, got)
		}
	}
}

// CI asserts a verify-full session and a certificate SAN naming the endpoint
// Service. An example that stopped being encrypted would make both vacuous
// rather than failing, so the lane's premise is pinned here.
func TestTheCRExampleIsEncryptedEndToEnd(t *testing.T) {
	cr := &FoundationCR{Spec: crSpecJSON(t)}
	cr.Metadata.Namespace = "amp"
	spec, err := loadCRSpec(cr)
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.SSLMode(); got != "verify-full" {
		t.Errorf("sslMode = %q, want verify-full: the lane reads this off the status and "+
			"refuses anything else", got)
	}
	overrides, _ := spec.universe()["kubernetesOverrides"].(map[string]any)
	tls, _ := overrides["tls"].(map[string]any)
	sans, _ := tls["extraSANs"].(map[string]any)
	names, _ := sans["dnsNames"].([]any)
	if len(names) == 0 {
		t.Fatal("no tls.extraSANs.dnsNames: the endpoint Service is in none of the SANs the " +
			"chart computes itself, so verify-full cannot connect")
	}
	if s, _ := names[0].(string); !strings.Contains(s, TServerEndpointName) {
		t.Errorf("extraSANs does not name %s: %v", TServerEndpointName, names[0])
	}
}
