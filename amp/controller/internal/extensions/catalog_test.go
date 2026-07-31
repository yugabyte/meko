package extensions

import (
	"reflect"
	"testing"
)

func TestCatalogNoDefaults(t *testing.T) {
	// pgvector is known but off by default: nothing is auto-mounted; an endpoint
	// opts in explicitly (extensions[] on createEndpoint).
	c := NewCatalog("quay.io/yugabyte")
	if got := c.DefaultNames(); len(got) != 0 {
		t.Fatalf("DefaultNames() = %v, want none (no default-on extensions)", got)
	}
}

func TestCatalogHasKnownAndUnknown(t *testing.T) {
	c := NewCatalog("quay.io/yugabyte")
	if !c.Has("pgvector") {
		t.Fatal("Has(pgvector) = false, want true (a known catalog extension)")
	}
	if c.Has("totally-unknown-ext") {
		t.Fatal("Has(totally-unknown-ext) = true, want false")
	}
}

func TestCatalogResolveBuildsImageRef(t *testing.T) {
	c := NewCatalog("quay.io/yugabyte")
	got := c.Resolve([]string{"pgvector"})
	want := []Resolved{{Name: "pgvector", Image: "quay.io/yugabyte/amp-ext-pgvector:0.8.2", Preload: false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve(pgvector) = %+v, want %+v", got, want)
	}
}

func TestCatalogResolveEmptyRegistryYieldsBareRef(t *testing.T) {
	c := NewCatalog("")
	got := c.Resolve([]string{"pgvector"})
	if len(got) != 1 || got[0].Image != "amp-ext-pgvector:0.8.2" {
		t.Fatalf("Resolve with empty registry = %+v, want bare amp-ext-pgvector:0.8.2", got)
	}
}

func TestCatalogResolveSkipsUnknownAndDeduplicates(t *testing.T) {
	c := NewCatalog("r")
	got := c.Resolve([]string{"pgvector", "does-not-exist", "pgvector"})
	if len(got) != 1 || got[0].Name != "pgvector" {
		t.Fatalf("Resolve dropped/deduped wrong: %+v", got)
	}
}
