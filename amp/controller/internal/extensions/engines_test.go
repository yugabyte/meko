package extensions

import (
	"encoding/json"
	"os"
	"testing"
)

// A bundle naming an extension the catalog does not carry would surface as a
// 400 on createCluster (the resolved names are validated like any other), which
// is a confusing way to learn about a typo in an in-tree table.
func TestEngineBundles(t *testing.T) {
	c := NewCatalog("quay.io/yugabyte")
	if err := c.ValidateEngines(); err != nil {
		t.Fatal(err)
	}
}

func TestResolveEngine(t *testing.T) {
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"", nil},
		{EnginePostgres, nil},
		{"aidb", []string{"pgvector", "age", "pg_cron"}},
		{"oracle", []string{"hexabridge"}},
		{"documentdb", []string{"documentdb", "pgvector", "pg_cron"}},
	} {
		got, err := ResolveEngine(tc.name)
		if err != nil {
			t.Fatalf("ResolveEngine(%q): %v", tc.name, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("ResolveEngine(%q) = %v, want %v", tc.name, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("ResolveEngine(%q) = %v, want %v", tc.name, got, tc.want)
			}
		}
	}

	if _, err := ResolveEngine("mysql"); err == nil {
		t.Fatal("ResolveEngine(mysql): want an error naming the known engines")
	}
}

// The seeded bundle must survive as a subset of the branch's set: the engine is
// recorded as provenance and add_branch_extensions can only union, so nothing
// can remove what the engine seeded.
func TestEngineExtensionsAreCatalogDefaultsFree(t *testing.T) {
	c := NewCatalog("")
	for _, e := range Engines() {
		for _, name := range e.Extensions {
			ext, ok := c.byName[name]
			if !ok {
				t.Fatalf("engine %s seeds unknown extension %s", e.Name, name)
			}
			if ext.Default {
				t.Errorf("engine %s seeds %s, which is already a catalog default", e.Name, name)
			}
		}
	}
}

// The engine table exists in three places: this package (runtime), the build
// manifest deploy/extensions/extensions.yaml (source of truth), and the UI's
// generated catalog. The UI's own test asserts its JSON matches the manifest, so
// asserting Go against that same JSON closes the loop — an engine added to the
// manifest but not here (or vice versa) fails, and no YAML parser is needed in
// Go to do it.
func TestEngineTableMatchesManifest(t *testing.T) {
	const generated = "../../../amp_ui/src/config/extensions.json"
	raw, err := os.ReadFile(generated)
	if err != nil {
		t.Skipf("generated UI catalog not readable (%v) — amp_ui may be absent", err)
	}
	var cfg struct {
		Engines []struct {
			Name       string   `json:"name"`
			Extensions []string `json:"extensions"`
		} `json:"engines"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", generated, err)
	}

	want := map[string][]string{}
	for _, e := range cfg.Engines {
		want[e.Name] = e.Extensions
	}
	got := map[string][]string{}
	for _, e := range Engines() {
		got[e.Name] = e.Extensions
	}

	for name, exts := range want {
		mine, ok := got[name]
		if !ok {
			t.Errorf("manifest has engine %q, engines.go does not", name)
			continue
		}
		if len(mine) != len(exts) {
			t.Errorf("engine %q: manifest %v, engines.go %v", name, exts, mine)
			continue
		}
		for i := range exts {
			if mine[i] != exts[i] {
				t.Errorf("engine %q: manifest %v, engines.go %v", name, exts, mine)
				break
			}
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("engines.go has engine %q, the manifest does not", name)
		}
	}
}
