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
	if !c.Has("pg_cron") {
		t.Fatal("Has(pg_cron) = false, want true (a known catalog extension)")
	}
	if !c.Has("hexabridge") {
		t.Fatal("Has(hexabridge) = false, want true (a known catalog extension)")
	}
	if !c.Has("age") {
		t.Fatal("Has(age) = false, want true (a known catalog extension)")
	}
	if c.Has("totally-unknown-ext") {
		t.Fatal("Has(totally-unknown-ext) = true, want false")
	}
}

func TestCatalogResolvePreloadExtension(t *testing.T) {
	// pg_cron carries Preload through Resolve so the deployer/chart can extend
	// shared_preload_libraries.
	c := NewCatalog("quay.io/yugabyte")
	got := c.Resolve([]string{"pg_cron"})
	want := []Resolved{{Name: "pg_cron", Image: "quay.io/yugabyte/amp-ext-pg_cron:1.6.7", Preload: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve(pg_cron) = %+v, want %+v", got, want)
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

func TestCatalogResolveHexabridge(t *testing.T) {
	// hexabridge is an opt-in extension (off the default set) that preloads: its
	// _PG_init registers a dbms_scheduler background worker and dbms_alert/
	// dbms_pipe shared memory, so Resolve carries Preload:true through to the
	// chart's shared_preload_libraries.
	c := NewCatalog("quay.io/yugabyte")
	got := c.Resolve([]string{"hexabridge"})
	want := []Resolved{{Name: "hexabridge", Image: "quay.io/yugabyte/amp-ext-hexabridge:0.1", Preload: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve(hexabridge) = %+v, want %+v", got, want)
	}
}

func TestCatalogResolveAge(t *testing.T) {
	// age is an opt-in, non-preload extension (loads per session via LOAD 'age',
	// no shared_preload_libraries), so Resolve carries Preload:false — like
	// pgvector, not pg_cron/hexabridge.
	c := NewCatalog("quay.io/yugabyte")
	got := c.Resolve([]string{"age"})
	want := []Resolved{{Name: "age", Image: "quay.io/yugabyte/amp-ext-age:1.8.0", Preload: false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve(age) = %+v, want %+v", got, want)
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
