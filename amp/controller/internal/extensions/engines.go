package extensions

import (
	"fmt"
	"sort"
)

// Engine is a named bundle of externalized extensions offered as a product
// flavour ("AI Database", "Oracle"). It is sugar over the extension catalog: an
// engine seeds its Extensions onto the new cluster's default branch, and the
// cluster records which engine it was created as.
//
// The engine is provenance, not a constraint. Extensions stay independently
// extensible through addBranchExtensions, and because that API is add-only
// (there is no removal path) an engine's bundle remains a subset of the branch's
// set for the cluster's whole life — so the recorded engine cannot become a lie.
type Engine struct {
	// Name is the wire value on createCluster and the stored column.
	Name string
	// Label is the human name the UI shows.
	Label string
	// Extensions are the catalog names the engine seeds, in mount order.
	Extensions []string
}

// EnginePostgres is the default: plain Postgres, no extensions seeded.
const EnginePostgres = "postgres"

// builtinEngines is the in-tree engine set. Every name in Extensions must exist
// in `builtin` above — ValidateEngines asserts that, and TestEngineBundles keeps
// it honest, so a typo fails the build rather than the first createCluster.
var builtinEngines = []Engine{
	{Name: EnginePostgres, Label: "PostgreSQL"},
	// Vector search + graph + scheduled jobs: the extensions an "AI Database"
	// workload reaches for.
	{Name: "aidb", Label: "AI Database", Extensions: []string{"pgvector", "age", "pg_cron"}},
	// HexaCluster's Oracle-compatibility layer (DBMS_*/UTL_* packages).
	{Name: "oracle", Label: "Oracle", Extensions: []string{"hexabridge"}},
}

// Engines returns the built-in engines in declaration order.
func Engines() []Engine {
	out := make([]Engine, len(builtinEngines))
	copy(out, builtinEngines)
	return out
}

// EngineNames returns every engine name, in declaration order.
func EngineNames() []string {
	out := make([]string, 0, len(builtinEngines))
	for _, e := range builtinEngines {
		out = append(out, e.Name)
	}
	return out
}

// ResolveEngine returns the extensions an engine seeds. An empty name means the
// default engine. An unknown name is an error naming the valid set, so the
// handler can turn it into a 400 the caller can act on.
func ResolveEngine(name string) ([]string, error) {
	if name == "" {
		name = EnginePostgres
	}
	for _, e := range builtinEngines {
		if e.Name == name {
			out := make([]string, len(e.Extensions))
			copy(out, e.Extensions)
			return out, nil
		}
	}
	return nil, fmt.Errorf("unknown engine %q (known: %v)", name, EngineNames())
}

// NormalizeEngine returns the engine name to store: the default for an empty
// one, else the name unchanged. It does not validate — ResolveEngine does.
func NormalizeEngine(name string) string {
	if name == "" {
		return EnginePostgres
	}
	return name
}

// ValidateEngines checks that every engine's extensions exist in the catalog.
// TestEngineBundles calls it against the built-in catalog, so a bundle naming a
// nonexistent extension fails CI rather than a caller's createCluster.
func (c *Catalog) ValidateEngines() error {
	var bad []string
	for _, e := range builtinEngines {
		for _, name := range e.Extensions {
			if !c.Has(name) {
				bad = append(bad, fmt.Sprintf("%s -> %s", e.Name, name))
			}
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("engine bundles reference unknown extensions: %v", bad)
	}
	return nil
}
