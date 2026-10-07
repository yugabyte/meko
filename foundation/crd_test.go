package foundation

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// crdPath is the CRD itself, and it lives inside its chart rather than beside
// this package because that chart is the only thing that installs it — a copy
// here would be one more file to keep in step. It is plain YAML with no helm
// templating in it, so reading it directly is reading what gets applied.
const crdPath = "deploy/helm/meko-foundation-crd/templates/foundation.yaml"

// crdSpecProperties reads the CRD's spec schema. The file is the shipped
// artifact, so the test reads it rather than a copy.
func crdSpecProperties(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read the CRD: %v", err)
	}
	var doc struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties map[string]any `yaml:"properties"`
							} `yaml:"spec"`
						} `yaml:"properties"`
					} `yaml:"openAPIV3Schema"`
				} `yaml:"schema"`
			} `yaml:"versions"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse the CRD: %v", err)
	}
	if len(doc.Spec.Versions) == 0 {
		t.Fatal("the CRD declares no versions")
	}
	return doc.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties
}

// TestCRDSchemaMatchesTheSpecStruct is the drift guard.
//
// The schema is hand-written — this module has no controller-gen — so nothing
// but this notices when a field is added to Spec and not to the CRD. The
// failure that causes is quiet and bad: the API server strictly validates
// anything NOT under preserve-unknown-fields, so a field the schema does not
// know is REJECTED at apply, and a spec that works from a file stops working as
// a CR with an error that names the field but not the reason.
func TestCRDSchemaMatchesTheSpecStruct(t *testing.T) {
	props := crdSpecProperties(t)

	// `resources` is in the schema but has no yaml tag on Spec: Load fills
	// Spec.Resources from the top-level kinds or the list, never by decoding a
	// field. It is the one legitimate asymmetry.
	const fromLoad = "resources"

	inStruct := map[string]bool{fromLoad: true}
	st := reflect.TypeOf(Spec{})
	for i := 0; i < st.NumField(); i++ {
		tag := st.Field(i).Tag.Get("yaml")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		inStruct[name] = true
		if _, ok := props[name]; !ok {
			t.Errorf("Spec field %q is not in the CRD schema, so the API server will REJECT a "+
				"Foundation that sets it", name)
		}
	}
	for name := range props {
		if !inStruct[name] {
			t.Errorf("the CRD schema declares %q, which Spec has no field for — it would be "+
				"accepted at apply and then silently ignored", name)
		}
	}
}

// The enums in the schema have to list exactly the values Validate accepts.
// Either half drifting is a spec that one of the two refuses for a reason the
// other does not give.
func TestCRDEnumsMatchValidate(t *testing.T) {
	props := crdSpecProperties(t)
	for _, tc := range []struct {
		field string
		want  []string
	}{
		{"chartSource", []string{string(ChartRepo), string(ChartPath)}},
		{"deletionPolicy", []string{string(DeletionRetain), string(DeletionDelete)}},
	} {
		prop, ok := props[tc.field].(map[string]any)
		if !ok {
			t.Errorf("%s is not an object in the schema", tc.field)
			continue
		}
		raw, ok := prop["enum"].([]any)
		if !ok {
			t.Errorf("%s has no enum, so the API server accepts any string for it", tc.field)
			continue
		}
		var got []string
		for _, v := range raw {
			got = append(got, v.(string))
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s enum = %v, want %v", tc.field, got, tc.want)
		}
	}
}

// deletionPolicy defaults to Retain in BOTH places, and they must agree: the
// API server applies the schema default on write, Load applies its own when the
// CLI reads a file, and a disagreement means `kubectl apply` and `foundation up`
// do different things to the same universe.
func TestDeletionPolicyDefaultsToRetainEverywhere(t *testing.T) {
	props := crdSpecProperties(t)
	prop, _ := props["deletionPolicy"].(map[string]any)
	if got := prop["default"]; got != string(DeletionRetain) {
		t.Errorf("the schema defaults deletionPolicy to %v, want %q", got, DeletionRetain)
	}

	spec := loadExample(t, "foundation-cli.yaml")
	if spec.DeletionPolicy != DeletionRetain {
		t.Errorf("Load defaulted deletionPolicy to %q, want %q", spec.DeletionPolicy, DeletionRetain)
	}
}

// Retain is the default because the two mistakes are not symmetric: an orphaned
// universe is recoverable by re-applying the same spec, a dropped database is
// not. This pins that an unset policy never means Delete.
func TestAnUnsetDeletionPolicyIsNeverDelete(t *testing.T) {
	spec, err := Load([]byte(`
namespace: amp
chartVersion: "2026.1.3.0-b21"
yba: {values: {yugaware: {defaultUser: {password: hunter2}}}}
resources:
  - kind: YBUniverse
    name: foundation
    spec:
      ybSoftwareVersion: 2026.1.3.0-b21
      enableLoadBalancer: true
      kubernetesOverrides:
        serviceEndpoints:
          - {name: yb-tserver-conn, type: ClusterIP, scope: Namespaced, app: yb-tserver, ports: {tcp-ysql-port: "5433", tcp-rpc-port: "9100"}}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if spec.DeletionPolicy == DeletionDelete {
		t.Fatal("an unset deletionPolicy resolved to Delete; deleting the object would drop the database")
	}
}

// crdStatusProperties reads the CRD's status schema.
func crdStatusProperties(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read the CRD: %v", err)
	}
	var doc struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Status map[string]any `yaml:"status"`
						} `yaml:"properties"`
					} `yaml:"openAPIV3Schema"`
				} `yaml:"schema"`
			} `yaml:"versions"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse the CRD: %v", err)
	}
	props, _ := doc.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Status["properties"].(map[string]any)
	return props
}

// compareStruct walks a Go type against a schema node's properties, both ways.
// `extra` names schema properties with no Go field, which at the top level are
// the ones the reconcile loop writes rather than Status.
func compareStruct(t *testing.T, path string, gt reflect.Type, props map[string]any, extra map[string]bool) {
	t.Helper()
	for gt.Kind() == reflect.Ptr || gt.Kind() == reflect.Slice {
		gt = gt.Elem()
	}
	if gt.Kind() != reflect.Struct {
		return
	}
	seen := map[string]bool{}
	for i := 0; i < gt.NumField(); i++ {
		f := gt.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" || !f.IsExported() {
			continue
		}
		seen[name] = true
		prop, ok := props[name].(map[string]any)
		if !ok {
			t.Errorf("%s.%s is written by Go but absent from the CRD schema, so the API "+
				"server drops it from status", path, name)
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Ptr || ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		if ft.Kind() != reflect.Struct {
			continue
		}
		nested, _ := prop["properties"].(map[string]any)
		if nested == nil {
			items, _ := prop["items"].(map[string]any)
			nested, _ = items["properties"].(map[string]any)
		}
		compareStruct(t, path+"."+name, ft, nested, nil)
	}
	for name := range props {
		if !seen[name] && !extra[name] {
			t.Errorf("the CRD declares %s.%s, which nothing writes", path, name)
		}
	}
}

// TestCRDStatusMatchesTheStatusStruct.
//
// Status is strictly validated like the spec, so a field Go writes and the
// schema does not declare is SILENTLY DROPPED by the API server rather than
// rejected — the write succeeds and the field is simply not there afterwards.
// That is the failure this guards, and it is invisible without it.
func TestCRDStatusMatchesTheStatusStruct(t *testing.T) {
	// Written by the reconcile loop, which owns the object; Status is the
	// reading of the cluster and knows nothing about generations or time.
	loopOwned := map[string]bool{
		"observedGeneration": true,
		"conditions":         true,
		"heartbeat":          true,
		"lastSuccess":        true,
	}
	compareStruct(t, "status", reflect.TypeOf(Status{}), crdStatusProperties(t), loopOwned)
}
