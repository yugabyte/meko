package api

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/extensions"
)

// core deliberately does not import the catalog (its `extensions []string`
// params would shadow the package), so it carries its own default-engine
// constant. This is what keeps the two from drifting apart.
func TestDefaultEngineMatchesCatalog(t *testing.T) {
	if core.DefaultEngine != extensions.EnginePostgres {
		t.Fatalf("core.DefaultEngine = %q, extensions.EnginePostgres = %q",
			core.DefaultEngine, extensions.EnginePostgres)
	}
}

// The spec's enum and the in-tree engine table are written separately; a name in
// one and not the other is a 400 for a value the schema advertises as valid, or
// a schema that rejects an engine the controller implements.
//
// Both directions are checked against real artifacts rather than a hand-written
// list: catalog->spec through the generated Valid(), and spec->catalog by reading
// the enum out of openapi_spec.yaml itself. A hand-retyped constant list would
// keep passing when someone adds a fourth engine to the spec.
func TestEngineEnumMatchesCatalog(t *testing.T) {
	// catalog -> spec: every engine the controller implements must be a value the
	// generated enum accepts, or createCluster 400s on its own engine.
	for _, name := range extensions.EngineNames() {
		if !CreateClusterRequestEngine(name).Valid() {
			t.Errorf("catalog engine %q is not a member of the openapi enum", name)
		}
	}

	// spec -> catalog: read the enum straight out of the spec.
	for _, name := range specEngineEnum(t) {
		if _, err := extensions.ResolveEngine(name); err != nil {
			t.Errorf("openapi enum advertises engine %q the catalog cannot resolve: %v", name, err)
		}
	}
}

// specEngineEnum extracts CreateClusterRequest.engine's enum from the spec. It
// fails rather than returning empty if the shape it keys off ever moves, so a
// restructured spec surfaces here instead of silently disarming the check.
func specEngineEnum(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("openapi_spec.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	m := regexp.MustCompile(`(?m)^\s+engine:\n\s+type: string\n\s+enum: \[([^\]]+)\]`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("could not find CreateClusterRequest.engine's enum in openapi_spec.yaml — " +
			"the spec shape changed; update this check rather than deleting it")
	}
	var out []string
	for _, part := range strings.Split(string(m[1]), ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		t.Fatal("engine enum parsed empty")
	}
	return out
}

func TestUnionExtensions(t *testing.T) {
	got := unionExtensions([]string{"pgvector"}, []string{"age", "pgvector", "pg_cron"})
	want := []string{"pgvector", "age", "pg_cron"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
