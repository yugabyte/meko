// Package extensions is the controller's catalog of externalized PostgreSQL
// extensions — the ones deploy/extensions builds into per-extension OCI images
// that an endpoint mounts as Kubernetes ImageVolumes (see
// compute/architecture/extensions.md). The catalog maps an extension name to its
// pinned version, load requirements, and default-ness, and resolves a set of
// names to concrete image references the deployer threads into the endpoint spec.
//
// It is a pure, code-defined lookup table (no I/O): the built-in set is small
// and versioned in-tree so image tags and controller stay in lockstep. A future
// iteration can source it from config or a table without changing callers.
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
	// Preload => the extension must be in shared_preload_libraries, so adding it
	// to a running endpoint needs a postmaster restart (not just a mount).
	Preload bool
	// Default => mounted on every endpoint out of the box (the "batteries
	// included" set), without the branch having to opt in.
	Default bool
}

// Resolved is a catalog entry paired with its concrete image reference, ready
// for the endpoint spec / Helm values. Preload is carried through so the chart
// can extend shared_preload_libraries.
type Resolved struct {
	Name    string
	Image   string
	Preload bool
}

// Catalog is the fixed set of known extensions plus the registry their images
// live under. It is read-only after construction and safe for concurrent use.
type Catalog struct {
	registry string
	byName   map[string]Extension
	order    []string // insertion order, for stable DefaultNames/Resolve output
}

// builtin is the in-tree extension set. Keep versions in lockstep with
// deploy/extensions/extensions.yaml (the build manifest).
var builtin = []Extension{
	// pgvector: known/available but NOT a default — opting in is a branch
	// property (extensions[] on createEndpoint), persisted onto
	// amp_registry.branches.extensions so forks and read replicas inherit it.
	// No preload needed. The image ships vector.control / vector*.sql (the SQL
	// name is "vector").
	{Name: "pgvector", Version: "0.8.2", Default: false, Preload: false},
	// pg_cron: a background-worker job scheduler. Opt-in per branch like
	// pgvector, but Preload: true — its library must be in
	// shared_preload_libraries (the chart appends it), so an endpoint that opts
	// in needs a postmaster restart. Its scheduler binds to one database via the
	// cron.database_name GUC (default "postgres", which every endpoint serves);
	// a non-default database is set through the endpoint's pg_settings.
	{Name: "pg_cron", Version: "1.6.7", Default: false, Preload: true},
	// hexabridge: HexaCluster's Oracle-compatibility layer. Opt-in per branch
	// like the others and NOT a default. No preload (plain function library, no
	// background worker), so opting in on a live endpoint needs only a mount. The
	// SQL name equals the catalog name (CREATE EXTENSION hexabridge); its source
	// is fetched from a private repo at build time (see deploy/extensions).
	{Name: "hexabridge", Version: "0.1", Default: false, Preload: false},
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

// image builds an entry's full image reference: <registry>/amp-ext-<name>:<version>,
// or "amp-ext-<name>:<version>" when the registry is empty.
func (c *Catalog) image(e Extension) string {
	ref := fmt.Sprintf("amp-ext-%s:%s", e.Name, e.Version)
	if c.registry == "" {
		return ref
	}
	return c.registry + "/" + ref
}

// Has reports whether name is a known catalog extension — the createEndpoint
// handler's validation gate (an unknown opt-in is rejected with 400 rather than
// silently dropped at resolve time).
func (c *Catalog) Has(name string) bool {
	_, ok := c.byName[name]
	return ok
}

// DefaultNames returns the names of the always-mounted extensions, in catalog
// order. These are unioned with a branch's opted-in set at endpoint create.
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
	for _, n := range names {
		e, ok := c.byName[n]
		if !ok {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, Resolved{Name: e.Name, Image: c.image(e), Preload: e.Preload})
	}
	return out
}
