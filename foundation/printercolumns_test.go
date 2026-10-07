package foundation

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type printerColumn struct {
	Name     string `yaml:"name"`
	Type     string `yaml:"type"`
	JSONPath string `yaml:"jsonPath"`
}

func printerColumns(t *testing.T) []printerColumn {
	t.Helper()
	data, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Spec struct {
			Versions []struct {
				Columns []printerColumn `yaml:"additionalPrinterColumns"`
			} `yaml:"versions"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Spec.Versions[0].Columns
}

// TestPrinterColumnsResolveAgainstARealStatus.
//
// A printer column whose jsonPath names a field nothing writes is an error
// NOWHERE: `kubectl get foundation` just prints an empty cell. Renaming a
// status field therefore blanks a column silently, and the first person to
// notice is someone reading the output and believing the blank.
//
// Each path is resolved against the JSON a real Status marshals to, which is
// what the API server evaluates them against.
func TestPrinterColumnsResolveAgainstARealStatus(t *testing.T) {
	// A healthy foundation: YBA up, three CRs ready, an address. Every column
	// should have something to show.
	st := &Status{
		Namespace: "amp",
		Phase:     PhaseReady,
		YBA:       YBAStatus{Release: "amp-yugaware", Installed: true},
		Resources: []ResourceStatus{
			{Kind: "YBCertificate", Name: "amp-ca", State: ResourceReady},
			{Kind: "YBProvider", Name: "foundation", State: ResourceReady},
			{Kind: "YBUniverse", Name: "foundation", State: ResourceReady},
		},
		Address: &Info{YSQLHostPorts: "svc:5433"},
	}
	st.ResourceStates = resourceStates(st.Resources)

	obj := statusObject(t, st, []Condition{
		{Type: ConditionReady, Status: CondTrue, LastTransitionTime: "2026-10-06T00:00:00Z"},
		{Type: ConditionConverged, Status: CondTrue, LastTransitionTime: "2026-10-06T00:00:00Z"},
	})

	cols := printerColumns(t)
	if len(cols) == 0 {
		t.Fatal("the CRD declares no printer columns")
	}
	for _, c := range cols {
		got, err := columnValue(obj, c.JSONPath)
		if err != nil {
			t.Errorf("column %q (%s): %v", c.Name, c.JSONPath, err)
			continue
		}
		if got == "" {
			t.Errorf("column %q (%s) is blank on a healthy foundation, so "+
				"`kubectl get foundation` would print an empty cell", c.Name, c.JSONPath)
		}
	}
}

// The two columns a customer asks about. Neither is reachable without the
// kind-keyed projection, because JSONPath cannot filter a list by one field and
// project another — so a change that dropped resourceStates would blank both.
func TestTheColumnsNameTheUniverseAndTheProvider(t *testing.T) {
	want := map[string]string{
		"Universe": ".status.resourceStates.YBUniverse",
		"Provider": ".status.resourceStates.YBProvider",
	}
	got := map[string]string{}
	for _, c := range printerColumns(t) {
		got[c.Name] = c.JSONPath
	}
	for name, path := range want {
		if got[name] != path {
			t.Errorf("column %q has jsonPath %q, want %q", name, got[name], path)
		}
	}
}

// A foundation that has gone wrong still shows WHICH part did: the phase and
// the failing kind's own state. A column set that only reads on a healthy
// object would be the less useful half.
func TestPrinterColumnsShowAFailure(t *testing.T) {
	st := &Status{
		Namespace: "amp",
		Phase:     PhaseFailed,
		YBA:       YBAStatus{Release: "amp-yugaware", Installed: true},
		Resources: []ResourceStatus{
			{Kind: "YBProvider", Name: "foundation", State: ResourceFailed, Raw: "ERROR"},
			{Kind: "YBUniverse", Name: "foundation", State: ResourcePending},
		},
	}
	st.ResourceStates = resourceStates(st.Resources)
	obj := statusObject(t, st, []Condition{{Type: ConditionReady, Status: CondFalse,
		LastTransitionTime: "2026-10-06T00:00:00Z"}})

	for path, want := range map[string]string{
		".status.phase":                                 string(PhaseFailed),
		".status.resourceStates.YBProvider":             string(ResourceFailed),
		".status.resourceStates.YBUniverse":             string(ResourcePending),
		`.status.conditions[?(@.type=="Ready")].status`: CondFalse,
	} {
		got, err := columnValue(obj, path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

// statusObject builds the object the API server would hold.
func statusObject(t *testing.T, st *Status, conds []Condition) map[string]any {
	t.Helper()
	body := map[string]any{
		"namespace":      st.Namespace,
		"phase":          st.Phase,
		"yba":            st.YBA,
		"resources":      st.Resources,
		"resourceStates": st.ResourceStates,
		"conditions":     conds,
	}
	if st.Address != nil {
		body["address"] = st.Address
	}
	raw, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"creationTimestamp": "2026-10-06T00:00:00Z"},
		"status":   body,
	})
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

// columnValue evaluates the two path shapes the printer columns use: a dotted
// key chain, and one filtered list selector (`conditions[?(@.type=="X")].y`).
//
// Deliberately not a general JSONPath implementation — it answers "does this
// path hit something", and a general one would be a second engine to keep
// correct. Anything outside those two shapes is reported rather than guessed
// at, so a column written in a form this cannot check fails loudly.
func columnValue(obj map[string]any, path string) (string, error) {
	rest := strings.TrimPrefix(path, ".")
	cur := any(obj)
	for rest != "" {
		var seg string
		if i := strings.IndexAny(rest, ".["); i < 0 {
			seg, rest = rest, ""
		} else if rest[i] == '.' {
			seg, rest = rest[:i], rest[i+1:]
		} else {
			seg, rest = rest[:i], rest[i:]
		}

		if seg != "" {
			m, ok := cur.(map[string]any)
			if !ok {
				return "", fmt.Errorf("looked for %q inside something that is not an object", seg)
			}
			cur, ok = m[seg]
			if !ok {
				return "", fmt.Errorf("no such field: %q", seg)
			}
		}
		if !strings.HasPrefix(rest, "[") {
			continue
		}
		// list[?(@.key=="value")].field
		close := strings.Index(rest, "]")
		if close < 0 || !strings.HasPrefix(rest, "[?(@.") {
			return "", fmt.Errorf("unsupported selector: %q", rest)
		}
		expr := rest[len("[?(@.") : close-1] // drop the trailing ')'
		key, value, ok := strings.Cut(expr, "==")
		if !ok {
			return "", fmt.Errorf("unsupported filter: %q", expr)
		}
		value = strings.Trim(value, `"`)
		list, ok := cur.([]any)
		if !ok {
			return "", fmt.Errorf("filtered something that is not a list")
		}
		cur = nil
		for _, item := range list {
			if e, ok := item.(map[string]any); ok && e[key] == value {
				cur = e
				break
			}
		}
		if cur == nil {
			return "", nil // no match: the API server prints a blank
		}
		rest = strings.TrimPrefix(rest[close+1:], ".")
	}
	if cur == nil {
		return "", nil
	}
	return fmt.Sprint(cur), nil
}
