package features

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_extension_hypopg",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "hypopg on an opted-in endpoint: it is mounted without being preloaded, EXPLAIN plans a selective query on a hypothetical index without building one, and hypopg_reset drops it from the plan.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     25 * time.Minute,
		Run:         runAmpExtensionHypopg,
	})
}

func runAmpExtensionHypopg(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"extension_hypopg", func(ctx context.Context) error {
			cl, err := a.f.controllerClient(ctx)
			if err != nil {
				return err
			}
			return extensionHypopgSteps(ctx, cl, a.proj, "hypo_probe_"+a.f.suffix(), a.f.endpointSQL, a.f.t.Skip)
		}}}
	})
}

func extensionHypopgSteps(ctx context.Context, cl ampCtl, proj ampCluster, table string, connect endpointConnector, skip func(string)) error {
	c, ok, err := optInExtensionEndpoint(ctx, cl, proj, connect, skip, "hypopg", "hypopg")
	if err != nil || !ok {
		return err
	}
	defer c.Close()

	libs, err := c.QueryString(ctx, "SHOW shared_preload_libraries")
	if err != nil {
		return fmt.Errorf("SHOW shared_preload_libraries: %w", err)
	}
	if containsToken(libs, "hypopg") {
		return fmt.Errorf("shared_preload_libraries is %q: hypopg is not a preload extension", libs)
	}

	for _, stmt := range []string{
		"CREATE TABLE " + table + " (id int, v text)",
		"INSERT INTO " + table + " SELECT g, 'line ' || g FROM generate_series(1, 100000) g",
		"ANALYZE " + table,
	} {
		if _, err := c.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("set up the hypopg table: %w", err)
		}
	}
	query := "SELECT * FROM " + table + " WHERE id = 1"
	if err := assertPlanTop(ctx, c, "", query, "Seq Scan"); err != nil {
		return fmt.Errorf("before any hypothetical index: %w", err)
	}

	// A hypothetical index exists only in the backend that created it, so the
	// rest runs on one pinned session rather than the pool.
	p, ok := c.(pinner)
	if !ok {
		return fmt.Errorf("this connection cannot pin a backend, which hypopg's backend-local indexes need")
	}
	s, err := p.Pin(ctx)
	if err != nil {
		return fmt.Errorf("pin a backend: %w", err)
	}
	defer s.Close()

	// EXPLAIN quotes the name, which starts with "<oid>".
	name, err := s.QueryString(ctx, "SELECT quote_ident(indexname) FROM hypopg_create_index('CREATE INDEX ON "+table+" (id)')")
	if err != nil {
		return fmt.Errorf("hypopg_create_index: %w", err)
	}
	if err := assertPlanTop(ctx, s, "", query, "Index Scan using "+name); err != nil {
		return fmt.Errorf("with hypothetical index %s: %w", name, err)
	}
	if n, err := s.QueryInt(ctx, "SELECT count(*) FROM pg_index WHERE indrelid = $1::regclass", table); err != nil {
		return fmt.Errorf("read pg_index: %w", err)
	} else if n != 0 {
		return fmt.Errorf("%s has %d real indexes after hypopg_create_index, want 0", table, n)
	}

	if _, err := s.Exec(ctx, "SELECT hypopg_reset()"); err != nil {
		return fmt.Errorf("hypopg_reset: %w", err)
	}
	if err := assertPlanTop(ctx, s, "", query, "Seq Scan"); err != nil {
		return fmt.Errorf("after hypopg_reset: %w", err)
	}
	_, err = c.Exec(ctx, "DROP TABLE "+table)
	return err
}

// assertPlanTop checks the top node of query's plan, with hint (if any) leading
// the statement.
func assertPlanTop(ctx context.Context, c sqlConn, hint, query, want string) error {
	stmt := hint + "EXPLAIN (COSTS false) " + query
	top, err := c.QueryString(ctx, stmt)
	if err != nil {
		return fmt.Errorf("%s: %w", stmt, err)
	}
	if !strings.HasPrefix(strings.TrimSpace(top), want) {
		return fmt.Errorf("%s: top plan node is %q, want %s", stmt, top, want)
	}
	return nil
}
