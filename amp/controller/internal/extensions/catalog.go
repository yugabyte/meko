// Package extensions is the controller's catalog of externalized PostgreSQL
// extensions — the ones deploy/extensions builds into per-extension OCI images
// that an endpoint mounts as Kubernetes ImageVolumes (see
// compute/architecture/extensions.md). The catalog maps an extension name to its
// pinned version, load requirements, and default-ness, and resolves a set of
// names to concrete image references the deployer threads into the endpoint spec.
//
// Catalog itself is a pure, code-defined lookup table (no I/O): the built-in set
// is small and versioned in-tree so image tags and controller stay in lockstep.
// Extensions an operator REGISTERS through the API live in the database instead;
// [Service] unions the two and is what callers resolve against. Nothing in this
// package hardcodes a third-party image reference — a third-party image reaches
// amp only by being registered.
package extensions

import "fmt"

// Extension is one catalog entry.
type Extension struct {
	// Name is the extension's canonical name: the image suffix (amp-ext-<Name>),
	// the ImageVolume mount subdir (/extensions/<Name>), and the
	// shared_preload_libraries entry when Preload is set. It need not equal the
	// SQL name used in CREATE EXTENSION (e.g. Name "pgvector" ships "vector").
	Name string
	// Version is the pinned upstream version — the image tag (amp-ext-<Name>:<Version>).
	Version string
	// ImageRevision counts rebuilds of the same Version from a changed recipe
	// (patch, Dockerfile, build flags). Tags are immutable and build.sh skips an
	// existing one, so a recipe change under an unchanged Version never reaches
	// the registry; a revision above 1 makes the tag <Version>-r<N> instead.
	// Mirrors `image_revision:` in deploy/extensions/extensions.yaml —
	// TestCatalogImageTagsMatchManifest keeps the two in step.
	ImageRevision int
	// Preload => the extension must be in shared_preload_libraries, so adding it
	// to a running endpoint needs a postmaster restart (not just a mount).
	Preload bool
	// Default => mounted on every endpoint out of the box (the "batteries
	// included" set), without the branch having to opt in.
	Default bool

	// Description is a one-line human summary, shown by listExtensions.
	Description string
	// Library is the shared-library basename when it differs from Name — the
	// shared_preload_libraries entry for a Preload extension. Empty means Name.
	// Comma-separated when one image ships more than one (documentdb's ships
	// pg_documentdb_core and pg_documentdb); the chart passes it through verbatim.
	Library string

	// ImageRef pins a complete image reference instead of deriving the amp-built
	// <registry>/amp-ext-<Name>:<Version> one. Every REGISTERED extension sets it
	// (that reference is the whole point of registering); a built-in entry leaves
	// it empty and is derived from the extension registry flag.
	ImageRef string
	// Layout describes where the image keeps its pieces. The zero value is the
	// layout deploy/extensions produces, which is what every built-in wants; a
	// registered image declares its own.
	Layout Layout
	// ABIExtraExempt lists the modules in the image (bare file names, no
	// extension) allowed to differ from the server in FMGR_ABI_EXTRA. Needed
	// only for an image built against stock PostgreSQL; every other magic-block
	// field is still enforced by the server. See yb_amp_abi_extra_exempt_libraries in
	// the forked Postgres. Waiving it wrongly can crash the postmaster, which is
	// why registering an extension is an admin-only API.
	ABIExtraExempt []string
	// Requires names other catalog extensions this one's .control file requires.
	// PostgreSQL enforces that at CREATE EXTENSION time: a required extension
	// whose .control is not on the endpoint's extension_control_path fails with
	// `extension "X" is not available`, so mounting this one alone is not enough.
	// Resolve closes over it. Catalog names, not SQL names (pgvector's SQL name is
	// "vector"), and it mirrors `requires:` in deploy/extensions/extensions.yaml —
	// TestCatalogRequiresMatchesManifest keeps the two in step.
	Requires []string
	// Settings are server settings an endpoint mounting this extension starts
	// with — ones it cannot work without, typically startup-only. documentdb's
	// name the database compute_ctl creates it in. The chart merges them into the
	// compute's settings; a branch's own pg_settings still apply on top.
	Settings map[string]string
}

// Layout is where an extension image keeps its pieces, relative to the mount
// root the endpoint gives it (/extensions/<name>). Empty fields take the
// deploy/extensions defaults, so an amp-built entry declares nothing.
type Layout struct {
	// SharePath is the sharedir BASE appended to extension_control_path — PG18
	// appends "/extension" itself. Default "share/postgresql".
	SharePath string
	// LibPath is appended to dynamic_library_path. Default "lib/postgresql".
	LibPath string
	// LibraryPath are dirs holding system shared libraries the extension's .so
	// links but the endpoint image does not carry (PostGIS's geos/proj/gdal).
	// The supervisor appends them to LD_LIBRARY_PATH.
	LibraryPath []string
	// EnvPaths are data dirs the extension reads at runtime, by environment
	// variable name (PROJ_DATA, GDAL_DATA). The chart prefixes the mount root.
	EnvPaths map[string]string
}

// Resolved is a catalog entry paired with its concrete image reference, ready
// for the endpoint spec / Helm values. Preload is carried through so the chart
// can extend shared_preload_libraries, and the layout/ABI fields so it can
// render a third-party image's paths.
type Resolved struct {
	Name           string
	Image          string
	Preload        bool
	Library        string
	Layout         Layout
	ABIExtraExempt []string
	Settings       map[string]string
}

// Catalog is the fixed set of known extensions plus the registry their images
// live under. It is read-only after construction and safe for concurrent use.
type Catalog struct {
	registry string
	byName   map[string]Extension
	order    []string // insertion order, for stable DefaultNames/Resolve output
}

// DocumentDBGatewayPort is the port the documentdb gateway listens on for the
// MongoDB wire protocol. It is the gateway's own default, NOT MongoDB's
// conventional 27017, so a client must be pointed at it. Must match
// documentdbGateway.port in the yb-amp-endpoint chart, which is what actually
// opens it — TestDocumentDBGatewayPortMatchesChart asserts they agree.
const DocumentDBGatewayPort = 10260

// builtin is the in-tree extension set. Keep versions AND descriptions in
// lockstep with deploy/extensions/extensions.yaml (the build manifest).
//
// The description is not decoration: listExtensions is the catalog a picker
// renders, so an entry without one shows a blank card. The manifest's own
// generator refuses a row that lacks it for exactly that reason.
var builtin = []Extension{
	// pgvector: known/available but NOT a default — opting in is per branch
	// (seeded on the default branch by extensions[] on createCluster, extended by
	// addBranchExtensions), persisted on amp_registry.branches.extensions (a fork
	// inherits it) so every endpoint on the branch mounts it. No preload needed.
	// The image ships vector.control / vector*.sql (the SQL name is "vector").
	{Name: "pgvector", Version: "0.8.2", Default: false, Preload: false,
		Description: "Vector similarity search"},
	// pg_cron: a background-worker job scheduler. Opt-in per branch like
	// pgvector, but Preload: true — its library must be in
	// shared_preload_libraries (the chart appends it), so an endpoint that opts
	// in needs a postmaster restart. Its scheduler binds to one database via the
	// cron.database_name GUC, default "postgres", which every endpoint serves. On
	// a branch that mounts documentdb it is "documentdb" (documentdb's Settings
	// below), and pg_cron cannot be created in postgres there. Elsewhere a
	// non-default database is set through the endpoint's pg_settings.
	{Name: "pg_cron", Version: "1.6.7", Default: false, Preload: true,
		Description: "Cron-style job scheduler"},
	// hexabridge: HexaCluster's Oracle-compatibility layer. Opt-in per branch
	// like the others and NOT a default. Preload: true — its _PG_init registers a
	// dbms_scheduler background worker and dbms_alert/dbms_pipe shared memory, so
	// its library must be in shared_preload_libraries and opting in on a live
	// endpoint needs a postmaster restart (like pg_cron). The SQL name equals the
	// catalog name (CREATE EXTENSION hexabridge); its source is fetched from a
	// private repo at build time (see deploy/extensions).
	{Name: "hexabridge", Version: "0.1", Default: false, Preload: true,
		Description: "Oracle compatibility (DBMS_*/UTL_* packages)"},
	// hexabridge_mssql: hexabridge's SQL Server-compatibility sibling, from the
	// same vendor and the same private-source build. Preload: false — its
	// _PG_init only defines a GUC, so opting in on a live endpoint is a mount, not
	// a restart. CREATE EXTENSION hexabridge_mssql installs into schema `sys`.
	{Name: "hexabridge_mssql", Version: "0.1", Default: false, Preload: false,
		Description: "SQL Server (T-SQL) compatibility functions"},
	// age: Apache AGE, the openCypher graph extension. Opt-in per branch like the
	// others and NOT a default. Preload: false — AGE loads per session via
	// `LOAD 'age'` and does not require shared_preload_libraries (like pgvector),
	// so opting in on a live endpoint is a mount, not a restart. The SQL name
	// equals the catalog name (CREATE EXTENSION age; schema ag_catalog). v1.8.0
	// is AGE's PG18 release line.
	{Name: "age", Version: "1.8.0", Default: false, Preload: false,
		Description: "Apache AGE — openCypher graph"},
	// documentdb: the MongoDB-compatible document store, and the bundle behind the
	// documentdb engine. One image ships three extensions (documentdb_core,
	// documentdb, documentdb_extended_rum). `CREATE EXTENSION documentdb CASCADE`
	// installs documentdb_core with it, plus pg_cron, tsm_system_rows and vector;
	// without CASCADE it fails on the first missing one. documentdb_extended_rum
	// is not a requirement and is created separately, if at all. Preload: true — pg_documentdb_core and pg_documentdb must be in
	// shared_preload_libraries, so opting in on a live endpoint is a restart. Its
	// .control ships `superuser = true`, so it is not trusted. Requires pgvector
	// and pg_cron, which its .control names; tsm_system_rows is also required but
	// ships inside the compute's Postgres, so it is not a catalog entry. postgis
	// is required too and amp does not ship it — dropping it needs both a
	// `requires` edit AND gating the install SQL's GIST operator classes, which
	// is documentdb-no-postgis.patch's job (see compute/architecture/extensions.md).
	//
	// compute_ctl creates it, in a `documentdb` database of its own, whenever
	// pg_documentdb is preloaded: the gateway serves nothing until it exists.
	// Both settings name that database — pg_cron refuses CREATE EXTENSION
	// anywhere but cron.database_name, and documentdb's background index builds
	// run in documentdb.bg_worker_database_name. Both are startup-only. Revision
	// 1 was published from an early recipe that still created the postgis
	// operator classes; revision 3 is the recipe after the download-checksum
	// change moved extension.Dockerfile onto safe_curl.sh.
	{Name: "documentdb", Version: "0.116-0", ImageRevision: 3, Default: false, Preload: true,
		Description: "DocumentDB — MongoDB-compatible document store",
		Library:     "pg_documentdb_core,pg_documentdb",
		Requires:    []string{"pgvector", "pg_cron"},
		Settings: map[string]string{
			"cron.database_name":                 "documentdb",
			"documentdb.bg_worker_database_name": "documentdb",
		}},
}

// NewCatalog returns the built-in catalog with images under registry (e.g.
// "quay.io/yugabyte"). An empty registry yields bare "amp-ext-<name>:<ver>"
// references (useful for a locally-built image / tests).
func NewCatalog(registry string) *Catalog {
	c := &Catalog{registry: registry, byName: make(map[string]Extension, len(builtin))}
	for _, e := range builtin {
		if _, dup := c.byName[e.Name]; dup {
			continue
		}
		c.byName[e.Name] = e
		c.order = append(c.order, e.Name)
	}
	return c
}

// ImageTag is an amp-built image's tag: its Version, suffixed -r<N> from the
// second ImageRevision on. deploy/extensions/build.sh tags with the same rule.
func ImageTag(version string, revision int) string {
	if revision > 1 {
		return fmt.Sprintf("%s-r%d", version, revision)
	}
	return version
}

// image builds an entry's full image reference: <registry>/amp-ext-<name>:<tag>,
// or "amp-ext-<name>:<tag>" when the registry is empty. An entry carrying an
// ImageRef (every registered one) is used verbatim — the amp registry flag says
// where WE publish and nothing about where someone else does.
func image(e Extension, registry string) string {
	if e.ImageRef != "" {
		return e.ImageRef
	}
	ref := fmt.Sprintf("amp-ext-%s:%s", e.Name, ImageTag(e.Version, e.ImageRevision))
	if registry == "" {
		return ref
	}
	return registry + "/" + ref
}

// resolve pairs an entry with its concrete image reference. Shared by the
// built-in catalog and [Service], so a registered extension reaches the deployer
// through exactly the same shape as a built-in one.
func resolve(e Extension, registry string) Resolved {
	return Resolved{
		Name:           e.Name,
		Image:          image(e, registry),
		Preload:        e.Preload,
		Library:        e.Library,
		Layout:         e.Layout,
		ABIExtraExempt: e.ABIExtraExempt,
		Settings:       e.Settings,
	}
}

// Has reports whether name is a known catalog extension — the
// createCluster/addBranchExtensions handler's validation gate (an unknown
// opt-in is rejected with 400 rather than silently dropped at resolve time).
func (c *Catalog) Has(name string) bool {
	_, ok := c.byName[name]
	return ok
}

// DefaultNames returns the names of the always-mounted extensions, in catalog
// order. These are unioned with the branch's opted-in set at endpoint create.
func (c *Catalog) DefaultNames() []string {
	var out []string
	for _, n := range c.order {
		if c.byName[n].Default {
			out = append(out, n)
		}
	}
	return out
}

// Resolve maps names to their resolved image references, skipping unknown names
// and de-duplicating, preserving the order names are given. Unknown names are
// dropped (not an error): the catalog is the source of truth for what can be
// mounted, so a stale branch entry can't wedge a deploy.
func (c *Catalog) Resolve(names []string) []Resolved {
	seen := make(map[string]struct{}, len(names))
	var out []Resolved
	// A queue, not a range: an entry's Requires are appended as they are found, so
	// a required extension pulls in its own requirements too. Requested names keep
	// their order and come first; requirements follow in the order they are named.
	// Unknown names are skipped here as they always were — a stale name must not
	// wedge the deploy — which also means a Requires entry naming something the
	// catalog does not carry is silently absent rather than fatal at deploy time.
	// TestCatalogRequiresNamesKnownExtensions is what keeps that from happening.
	queue := append([]string(nil), names...)
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		e, ok := c.byName[n]
		if !ok {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, resolve(e, c.registry))
		queue = append(queue, e.Requires...)
	}
	return out
}
