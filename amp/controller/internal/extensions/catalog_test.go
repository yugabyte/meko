package extensions

import (
	"encoding/json"
	"os"
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

func TestBuiltinsDeclareNoLayoutOrImageRef(t *testing.T) {
	// Everything built in is built by deploy/extensions, so it uses the amp tree
	// (the zero Layout, which is what the chart defaults to), matches this fork's
	// FMGR_ABI_EXTRA, and derives its image from the registry flag. A built-in
	// carrying an ImageRef would be a third-party image hardcoded in the product,
	// which is exactly what the registration API exists to avoid.
	c := NewCatalog("quay.io/yugabyte")
	for _, name := range c.order {
		if c.byName[name].ImageRef != "" {
			t.Errorf("built-in %s pins a third-party image %q; register it instead", name, c.byName[name].ImageRef)
		}
		got := c.Resolve([]string{name})[0]
		if !reflect.DeepEqual(got.Layout, Layout{}) {
			t.Errorf("Resolve(%s).Layout = %+v, want the zero value", name, got.Layout)
		}
		if len(got.ABIExtraExempt) != 0 {
			t.Errorf("Resolve(%s).ABIExtraExempt = %v; an amp-built image matches FMGR_ABI_EXTRA exactly", name, got.ABIExtraExempt)
		}
	}
}

// documentdb's .control requires pgvector and pg_cron, so resolving it alone
// still has to mount them: without their .control files on the endpoint's
// extension_control_path, CREATE EXTENSION documentdb fails with
// `extension "pg_cron" is not available`. The engine bundle supplies them when
// the caller used the engine, but extensions:["documentdb"] and
// POST /branches/{id}/extensions do not go through an engine.
func TestCatalogResolveClosesOverRequires(t *testing.T) {
	c := NewCatalog("quay.io/yugabyte")
	got := names(c.Resolve([]string{"documentdb"}))
	want := []string{"documentdb", "pgvector", "pg_cron"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve([documentdb]) = %v, want %v", got, want)
	}
}

// Requirements already asked for are not mounted twice, and the caller's order
// wins: the requested names come first, in the order given.
func TestCatalogResolveRequiresDeduplicates(t *testing.T) {
	c := NewCatalog("quay.io/yugabyte")
	got := names(c.Resolve([]string{"pg_cron", "documentdb"}))
	want := []string{"pg_cron", "documentdb", "pgvector"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve([pg_cron documentdb]) = %v, want %v", got, want)
	}
}

// An extension with no Requires resolves to itself — the closure must not change
// what every other extension does.
func TestCatalogResolveWithoutRequiresIsUnchanged(t *testing.T) {
	c := NewCatalog("quay.io/yugabyte")
	for _, n := range []string{"pgvector", "pg_cron", "hexabridge", "age"} {
		if got := names(c.Resolve([]string{n})); !reflect.DeepEqual(got, []string{n}) {
			t.Errorf("Resolve([%s]) = %v, want [%s]", n, got, n)
		}
	}
}

// listExtensions is what a picker renders, so a built-in without a description
// shows a blank card — and the served catalog would then carry strictly LESS
// than the build manifest the UI used to bundle. The manifest's generator
// refuses a row missing one; this is the same guarantee on the served side.
func TestBuiltinsAllCarryADescription(t *testing.T) {
	c := NewCatalog("quay.io/yugabyte")
	for _, n := range c.order {
		if c.byName[n].Description == "" {
			t.Errorf("built-in %s has no description; listExtensions would render a blank card", n)
		}
	}
}

// Resolve drops names the catalog does not carry, so a Requires entry naming a
// non-existent extension would vanish silently and only surface as a failed
// CREATE EXTENSION on a deployed endpoint.
func TestCatalogRequiresNamesKnownExtensions(t *testing.T) {
	c := NewCatalog("")
	for _, e := range builtin {
		for _, req := range e.Requires {
			if _, ok := c.byName[req]; !ok {
				t.Errorf("%s requires %q, which is not in the catalog", e.Name, req)
			}
		}
	}
}

// The catalog's Requires and the build manifest's `requires:` are two hand-kept
// copies of one fact; the harness reads the manifest and the controller reads
// this table, so they diverge silently.
func TestCatalogRequiresMatchesManifest(t *testing.T) {
	const generated = "../../../amp_ui/src/config/extensions.json"
	raw, err := os.ReadFile(generated)
	if err != nil {
		t.Skipf("generated UI catalog not readable (%v) — amp_ui may be absent", err)
	}
	var cfg struct {
		Extensions []struct {
			Name     string   `json:"name"`
			Requires []string `json:"requires"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", generated, err)
	}
	want := map[string][]string{}
	for _, e := range cfg.Extensions {
		if len(e.Requires) > 0 {
			want[e.Name] = e.Requires
		}
	}
	got := map[string][]string{}
	for _, e := range builtin {
		if len(e.Requires) > 0 {
			got[e.Name] = e.Requires
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("catalog requires = %v, manifest requires = %v", got, want)
	}
}

// manifestExtensions reads the build manifest's extension rows through the UI's
// generated catalog, as the other parity tests here do.
func manifestExtensions(t *testing.T) map[string]struct {
	Version       string
	ImageRevision int
	Settings      map[string]string
} {
	t.Helper()
	const generated = "../../../amp_ui/src/config/extensions.json"
	raw, err := os.ReadFile(generated)
	if err != nil {
		t.Skipf("generated UI catalog not readable (%v) — amp_ui may be absent", err)
	}
	var cfg struct {
		Extensions []struct {
			Name          string            `json:"name"`
			Version       string            `json:"version"`
			ImageRevision int               `json:"imageRevision"`
			Settings      map[string]string `json:"settings"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", generated, err)
	}
	out := map[string]struct {
		Version       string
		ImageRevision int
		Settings      map[string]string
	}{}
	for _, e := range cfg.Extensions {
		out[e.Name] = struct {
			Version       string
			ImageRevision int
			Settings      map[string]string
		}{e.Version, e.ImageRevision, e.Settings}
	}
	return out
}

// build.sh tags from the manifest and the controller resolves from this table,
// so a version or revision that differs between them points every endpoint at
// an image that was never built — or, worse, at a stale one that was.
func TestCatalogImageTagsMatchManifest(t *testing.T) {
	manifest := manifestExtensions(t)
	for _, e := range builtin {
		m, ok := manifest[e.Name]
		if !ok {
			t.Errorf("catalog has %s, the manifest does not", e.Name)
			continue
		}
		if got, want := ImageTag(e.Version, e.ImageRevision), ImageTag(m.Version, m.ImageRevision); got != want {
			t.Errorf("%s: catalog tag %q, manifest tag %q", e.Name, got, want)
		}
	}
}

// The settings an endpoint starts with come from this table in the product and
// from the manifest in the local harness; they have to be the same settings.
func TestCatalogSettingsMatchManifest(t *testing.T) {
	manifest := manifestExtensions(t)
	for _, e := range builtin {
		want := manifest[e.Name].Settings
		if len(e.Settings) == 0 && len(want) == 0 {
			continue
		}
		if !reflect.DeepEqual(e.Settings, want) {
			t.Errorf("%s: catalog settings %v, manifest settings %v", e.Name, e.Settings, want)
		}
	}
}

func TestImageTag(t *testing.T) {
	for _, tc := range []struct {
		version  string
		revision int
		want     string
	}{
		{"0.8.2", 0, "0.8.2"},
		{"0.8.2", 1, "0.8.2"},
		{"0.116-0", 3, "0.116-0-r3"},
	} {
		if got := ImageTag(tc.version, tc.revision); got != tc.want {
			t.Errorf("ImageTag(%q, %d) = %q, want %q", tc.version, tc.revision, got, tc.want)
		}
	}
	c := NewCatalog("quay.io/yugabyte")
	if got := c.Resolve([]string{"documentdb"})[0].Image; got != "quay.io/yugabyte/amp-ext-documentdb:0.116-0-r3" {
		t.Errorf("documentdb resolves to %q", got)
	}
}

// names is the resolved mount set's names, in order.
func names(rs []Resolved) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}
