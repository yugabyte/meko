package extensions

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// memStore is an in-memory extensions.Store for these tests. core.MemBackend has
// the production twin; this keeps the package free of that dependency.
type memStore struct {
	byName map[string]Extension
	users  map[string][]string
}

func newMemStore() *memStore {
	return &memStore{byName: map[string]Extension{}, users: map[string][]string{}}
}

func (m *memStore) ListExtensions(context.Context) ([]Extension, error) {
	out := make([]Extension, 0, len(m.byName))
	for _, e := range m.byName {
		out = append(out, e)
	}
	return out, nil
}

func (m *memStore) GetExtension(_ context.Context, name string) (Extension, bool, error) {
	e, ok := m.byName[name]
	return e, ok, nil
}

func (m *memStore) PutExtension(_ context.Context, e Extension) error {
	m.byName[e.Name] = e
	return nil
}

func (m *memStore) DeleteExtension(_ context.Context, name string) error {
	delete(m.byName, name)
	return nil
}

func (m *memStore) ExtensionUsers(_ context.Context, name string) ([]string, error) {
	return m.users[name], nil
}

// postgis is the worked example the whole feature exists for: a third-party
// image, laid out unlike ours, bundling its own system libraries and built
// against stock PostgreSQL. Nothing in the product hardcodes it — it reaches amp
// only through Register, which is the point.
func postgisEntry() Extension {
	return Extension{
		Name:        "postgis",
		ImageRef:    "ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm",
		Version:     "3.6.2",
		Description: "Spatial types, functions and indexes",
		Layout: Layout{
			SharePath:   "share",
			LibPath:     "lib",
			LibraryPath: []string{"system"},
			EnvPaths:    map[string]string{"PROJ_DATA": "share/proj", "GDAL_DATA": "share/gdal"},
		},
		ABIExtraExempt: []string{"postgis-3", "postgis_raster-3"},
	}
}

func testService() (*Service, *memStore) {
	store := newMemStore()
	return NewService(NewCatalog("quay.io/yugabyte"), store), store
}

func TestServiceResolvesRegisteredAlongsideBuiltin(t *testing.T) {
	svc, _ := testService()
	ctx := context.Background()
	if _, err := svc.Register(ctx, postgisEntry()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := svc.Resolve(ctx, []string{"pgvector", "postgis"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Resolve = %+v, want 2 entries", got)
	}
	byName := map[string]Resolved{got[0].Name: got[0], got[1].Name: got[1]}
	// The built-in still derives its reference from the registry flag...
	if byName["pgvector"].Image != "quay.io/yugabyte/amp-ext-pgvector:0.8.2" {
		t.Errorf("pgvector image = %q", byName["pgvector"].Image)
	}
	// ...and the registered one is used verbatim: our registry says nothing about
	// where someone else publishes.
	if byName["postgis"].Image != "ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm" {
		t.Errorf("postgis image = %q", byName["postgis"].Image)
	}
	want := Layout{
		SharePath:   "share",
		LibPath:     "lib",
		LibraryPath: []string{"system"},
		EnvPaths:    map[string]string{"PROJ_DATA": "share/proj", "GDAL_DATA": "share/gdal"},
	}
	if !reflect.DeepEqual(byName["postgis"].Layout, want) {
		t.Errorf("postgis layout = %+v, want %+v", byName["postgis"].Layout, want)
	}
	if len(byName["postgis"].ABIExtraExempt) != 2 {
		t.Errorf("postgis ABIExtraExempt = %v", byName["postgis"].ABIExtraExempt)
	}
}

func TestServiceResolveDropsDeregisteredName(t *testing.T) {
	// A branch that opted into an extension since deregistered must still deploy —
	// dropping the unknown name is what keeps a stale opt-in from wedging every
	// endpoint on that branch.
	svc, store := testService()
	ctx := context.Background()
	if _, err := svc.Register(ctx, postgisEntry()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	delete(store.byName, "postgis")
	got, err := svc.Resolve(ctx, []string{"pgvector", "postgis"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 || got[0].Name != "pgvector" {
		t.Fatalf("Resolve = %+v, want just pgvector", got)
	}
}

func TestServiceHasCoversBothHalves(t *testing.T) {
	svc, _ := testService()
	ctx := context.Background()
	if _, err := svc.Register(ctx, postgisEntry()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for name, want := range map[string]bool{"pgvector": true, "postgis": true, "nope": false} {
		got, err := svc.Has(ctx, name)
		if err != nil {
			t.Fatalf("Has(%s): %v", name, err)
		}
		if got != want {
			t.Errorf("Has(%s) = %v, want %v", name, got, want)
		}
	}
}

func TestServiceRegisterRefusesBuiltinName(t *testing.T) {
	// Shadowing is refused rather than resolved by precedence: an operator
	// registering "pgvector" means a different pgvector, and either outcome of a
	// silent contest is wrong.
	svc, _ := testService()
	e := postgisEntry()
	e.Name = "pgvector"
	_, err := svc.Register(context.Background(), e)
	if !errors.Is(err, ErrBuiltinName) {
		t.Fatalf("Register(pgvector) error = %v, want ErrBuiltinName", err)
	}
}

func TestServiceDeregisterRefusesWhileInUse(t *testing.T) {
	svc, store := testService()
	ctx := context.Background()
	if _, err := svc.Register(ctx, postgisEntry()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	store.users["postgis"] = []string{"11111111-1111-1111-1111-111111111111"}
	err := svc.Deregister(ctx, "postgis")
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("Deregister while in use = %v, want ErrInUse", err)
	}
	// The branch id is in the message so an operator knows who to go fix.
	if !strings.Contains(err.Error(), "11111111") {
		t.Errorf("error does not name the branch: %v", err)
	}
	// Once nothing references it, it goes.
	store.users["postgis"] = nil
	if err := svc.Deregister(ctx, "postgis"); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if _, ok := store.byName["postgis"]; ok {
		t.Error("postgis still registered after Deregister")
	}
}

func TestServiceDeregisterRefusesBuiltin(t *testing.T) {
	svc, _ := testService()
	if err := svc.Deregister(context.Background(), "pgvector"); !errors.Is(err, ErrBuiltinName) {
		t.Fatalf("Deregister(pgvector) = %v, want ErrBuiltinName", err)
	}
}

func TestServiceListMarksProvenance(t *testing.T) {
	svc, _ := testService()
	ctx := context.Background()
	if _, err := svc.Register(ctx, postgisEntry()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	entries, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var sawBuiltin, sawRegistered bool
	for _, e := range entries {
		switch e.Name {
		case "pgvector":
			sawBuiltin = e.Builtin
		case "postgis":
			sawRegistered = !e.Builtin
		}
	}
	if !sawBuiltin || !sawRegistered {
		t.Fatalf("List provenance wrong: %+v", entries)
	}
}

func TestServiceRegisteredIsNeverADefault(t *testing.T) {
	// Registering makes an image AVAILABLE. Mounting it on every endpoint in the
	// deployment is a different decision, and not one a registration may make.
	svc, _ := testService()
	e := postgisEntry()
	e.Default = true
	if _, err := svc.Register(context.Background(), e); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, n := range svc.DefaultNames() {
		if n == "postgis" {
			t.Fatal("a registered extension became a default")
		}
	}
}

func TestValidateRejectsUnsafeMetadata(t *testing.T) {
	// Every one of these values is interpolated into a container mount path, the
	// loader path, or the pod's environment, so the refusals matter more than the
	// acceptances.
	base := postgisEntry()
	cases := []struct {
		name string
		mut  func(*Extension)
		want string
	}{
		{"empty image", func(e *Extension) { e.ImageRef = "" }, "image is required"},
		{"untagged image", func(e *Extension) { e.ImageRef = "ghcr.io/foo/bar" }, "pinned to a tag"},
		{"latest tag", func(e *Extension) { e.ImageRef = "ghcr.io/foo/bar:latest" }, ":latest is refused"},
		{"registry port not a tag", func(e *Extension) { e.ImageRef = "localhost:5000/foo" }, "pinned to a tag"},
		{"bad name", func(e *Extension) { e.Name = "../etc" }, "must match"},
		{"upper name", func(e *Extension) { e.Name = "PostGIS" }, "must match"},
		{"absolute share path", func(e *Extension) { e.Layout.SharePath = "/share" }, "must be relative"},
		{"traversing lib path", func(e *Extension) { e.Layout.LibPath = "../../usr/lib" }, "relative path segments"},
		{"traversing library path", func(e *Extension) { e.Layout.LibraryPath = []string{"a/../.."} }, "relative path segments"},
		{"reserved env", func(e *Extension) { e.Layout.EnvPaths = map[string]string{"LD_LIBRARY_PATH": "system"} }, "cannot be overridden"},
		{"bad env name", func(e *Extension) { e.Layout.EnvPaths = map[string]string{"proj data": "share/proj"} }, "not a valid environment variable"},
		{"absolute env path", func(e *Extension) { e.Layout.EnvPaths = map[string]string{"PROJ_DATA": "/usr/share/proj"} }, "must be relative"},
		{"abi entry with suffix", func(e *Extension) { e.ABIExtraExempt = []string{"postgis-3.so"} }, "drop the .so"},
		{"abi entry with a path", func(e *Extension) { e.ABIExtraExempt = []string{"../postgis-3"} }, "bare module name"},
		{"library with a path", func(e *Extension) { e.Library = "dir/lib" }, "bare shared-library name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := base
			// Copy the maps/slices the mutation may replace wholesale.
			e.Layout.EnvPaths = map[string]string{"PROJ_DATA": "share/proj"}
			e.Layout.LibraryPath = []string{"system"}
			e.ABIExtraExempt = []string{"postgis-3"}
			tc.mut(&e)
			err := Validate(&e)
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate(%s) = %q, want it to mention %q", tc.name, err, tc.want)
			}
		})
	}
}

func TestValidateAcceptsADigestPinnedImage(t *testing.T) {
	e := postgisEntry()
	e.ImageRef = "ghcr.io/cloudnative-pg/postgis-extension@sha256:" + strings.Repeat("a", 64)
	if err := Validate(&e); err != nil {
		t.Fatalf("Validate(digest) = %v, want nil", err)
	}
}

func TestServiceWithoutStoreServesBuiltinsAndRefusesRegistration(t *testing.T) {
	// The Docker driver and any deployment without the feature: the built-ins
	// still resolve, and registering says so plainly instead of panicking.
	svc := NewService(NewCatalog("quay.io/yugabyte"), nil)
	ctx := context.Background()
	got, err := svc.Resolve(ctx, []string{"pgvector"})
	if err != nil || len(got) != 1 {
		t.Fatalf("Resolve = %+v, %v", got, err)
	}
	if _, err := svc.Register(ctx, postgisEntry()); err == nil {
		t.Fatal("Register with no store succeeded")
	}
}

// A URL path can carry a name with surrounding whitespace, and Validate is what
// trims it. Checking the built-in table before that ran let " pgvector" through:
// no 409, then a row stored as "pgvector" that every read hides behind the
// built-in table and Deregister refuses to delete.
func TestServiceRegisterChecksTheNormalisedNameAgainstBuiltins(t *testing.T) {
	svc, store := testService()
	e := postgisEntry()
	e.Name = " pgvector"
	_, err := svc.Register(context.Background(), e)
	if !errors.Is(err, ErrBuiltinName) {
		t.Fatalf("Register(%q) error = %v, want ErrBuiltinName", e.Name, err)
	}
	if _, ok := store.byName["pgvector"]; ok {
		t.Fatal("a row was stored under a built-in's name")
	}
}

// Register returns what it stored, so a caller answers with the normalised name
// rather than re-reading under the one it was handed (which would miss).
func TestServiceRegisterReturnsTheStoredEntry(t *testing.T) {
	svc, _ := testService()
	e := postgisEntry()
	e.Name = "  postgis  "
	got, err := svc.Register(context.Background(), e)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got.Name != "postgis" {
		t.Errorf("stored name = %q, want the trimmed %q", got.Name, "postgis")
	}
	if got.Builtin {
		t.Error("a registered entry reported builtin = true")
	}
	if got.Image != e.ImageRef {
		t.Errorf("image = %q, want %q", got.Image, e.ImageRef)
	}
}

// Only a rejection of the body is ErrInvalid. A store failure must not be,
// or the API reports an outage as "fix your request".
func TestServiceRegisterDistinguishesBadBodyFromStoreFailure(t *testing.T) {
	svc, _ := testService()
	bad := postgisEntry()
	bad.ImageRef = "ghcr.io/x/y:latest"
	if _, err := svc.Register(context.Background(), bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad body error = %v, want ErrInvalid", err)
	}
	// No store configured is an operational failure, not a bad body.
	noStore := NewService(NewCatalog("quay.io/yugabyte"), nil)
	if _, err := noStore.Register(context.Background(), postgisEntry()); err == nil {
		t.Fatal("Register with no store succeeded")
	} else if errors.Is(err, ErrInvalid) {
		t.Fatalf("missing store reported as ErrInvalid: %v", err)
	}
}

// The endpoint's own environment must stay the endpoint's. The chart renders
// envPaths AFTER envFrom, so a registration's key wins over the ConfigMap —
// REQUIRE_WRITE_TOKEN set to a path fails the supervisor's "true"|"1" match and
// brings the endpoint up unfenced.
func TestValidateRefusesEveryVariableTheEndpointOwns(t *testing.T) {
	for _, name := range []string{
		"REQUIRE_WRITE_TOKEN", "WRITE_TOKEN_TTL_MS", "WRITE_TOKEN_CLIENT_ID",
		"YB_HOST_PORTS", "YB_USER", "YB_DBNAME", "YB_SSLMODE", "YB_PASSWORD_ENV",
		"COMPUTE_EXTRA_SETTINGS_JSON", "COMPUTE_PORT", "ENDPOINT_ID", "BRANCH_ID",
		"PAGESERVER_PG_PORT", "SAFEKEEPER_HTTP_PORT", "SUPERVISOR_HTTP_PORT",
		"CONTROLLER_URL", "PG_DISTRIB_DIR", "LD_LIBRARY_PATH", "EXTENSION_LIBRARY_PATH",
		"PATH", "RUN_DIR", "BIN_DIR", "SHARED_PRELOAD_LIBRARIES", "EXTRA_SHARED_PRELOAD_LIBRARIES", "BASE_SHARED_PRELOAD_LIBRARIES",
	} {
		e := postgisEntry()
		e.Layout.EnvPaths = map[string]string{name: "share/proj"}
		if err := Validate(&e); err == nil {
			t.Errorf("Validate accepted a registration setting %s", name)
		}
	}
	// A genuine extension data dir is still fine.
	e := postgisEntry()
	e.Layout.EnvPaths = map[string]string{"PROJ_DATA": "share/proj", "GDAL_DATA": "share/gdal"}
	if err := Validate(&e); err != nil {
		t.Fatalf("Validate rejected a legitimate data dir: %v", err)
	}
}

// The chart sanitizes a catalog name to an RFC-1123 label for the volume, so
// "_" and "-" collapse to the same one. Two names that differ only there would
// render two ImageVolumes called ext-pg-cron, which the API server rejects —
// breaking every deploy on a branch that opted into both, long after the
// registration returned 200.
func TestServiceRegisterRefusesASanitisedNameCollision(t *testing.T) {
	svc, store := testService()
	ctx := context.Background()

	// The dashed spelling of a built-in: the obvious next move for an operator
	// whose pg_cron was refused.
	e := postgisEntry()
	e.Name = "pg-cron"
	_, err := svc.Register(ctx, e)
	if !errors.Is(err, ErrBuiltinName) {
		t.Fatalf("Register(pg-cron) = %v, want ErrBuiltinName", err)
	}
	if _, ok := store.byName["pg-cron"]; ok {
		t.Fatal("pg-cron was stored despite colliding with the built-in pg_cron")
	}

	// And between two registered ones.
	first := postgisEntry()
	first.Name = "my_ext"
	if _, err := svc.Register(ctx, first); err != nil {
		t.Fatalf("Register(my_ext): %v", err)
	}
	second := postgisEntry()
	second.Name = "my-ext"
	if _, err := svc.Register(ctx, second); !errors.Is(err, ErrNameCollision) {
		t.Fatalf("Register(my-ext) = %v, want ErrNameCollision", err)
	}

	// Re-registering the SAME name is an upsert, not a collision with itself.
	if _, err := svc.Register(ctx, first); err != nil {
		t.Fatalf("re-register my_ext: %v", err)
	}
}

func TestVolumeNameMatchesTheChartHelper(t *testing.T) {
	// Kept in step with templates/_helpers.tpl:
	//   printf "ext-%s" (regexReplaceAll "[^a-z0-9]+" (.name | lower) "-" | trimAll "-")
	for in, want := range map[string]string{
		"pg_cron": "pg-cron", "pg-cron": "pg-cron", "pgvector": "pgvector",
		"postgis": "postgis", "a__b": "a-b", "_x_": "x",
	} {
		if got := volumeName(in); got != want {
			t.Errorf("volumeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Both spec copies publish "Built-ins come first, each group name-sorted".
// builtin.order is insertion order, which DefaultNames wants and List does not.
func TestServiceListSortsBothGroups(t *testing.T) {
	svc, _ := testService()
	ctx := context.Background()
	for _, n := range []string{"zeta_ext", "alpha_ext"} {
		e := postgisEntry()
		e.Name = n
		if _, err := svc.Register(ctx, e); err != nil {
			t.Fatalf("Register(%s): %v", n, err)
		}
	}
	entries, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var builtins, registered []string
	for _, e := range entries {
		if e.Builtin {
			if len(registered) > 0 {
				t.Fatal("a built-in came after a registered entry; built-ins must come first")
			}
			builtins = append(builtins, e.Name)
		} else {
			registered = append(registered, e.Name)
		}
	}
	if !sort.StringsAreSorted(builtins) {
		t.Errorf("built-in group not name-sorted: %v", builtins)
	}
	if !sort.StringsAreSorted(registered) {
		t.Errorf("registered group not name-sorted: %v", registered)
	}
}
