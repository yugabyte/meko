// createCluster takes an engine (meko#1031): a named bundle of catalog
// extensions unioned with the request's own, seeded on the default branch and
// recorded on the cluster. The UI offers AI Database and Oracle; this walks all
// three engines and proves each bundle actually works on an endpoint.
package features

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const engineUnknown = "mongo"

// engineCase is one engine, what it must seed, and a probe that only passes if
// every seeded extension is usable on an endpoint.
type engineCase struct {
	engine string
	extra  []string
	seeds  []string
	probe  func(ctx context.Context, c sqlConn) error
}

var engineCases = []engineCase{
	{engine: "postgres"},
	{engine: "aidb", extra: []string{"hexabridge"}, seeds: []string{"pgvector", "age", "pg_cron", "hexabridge"}, probe: probeAIDB},
	{engine: "oracle", seeds: []string{"hexabridge"}, probe: probeOracle},
}

func init() {
	core.Register(core.Test{
		Name:        "features/amp_cluster_engine",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Create one cluster per engine (postgres, aidb plus an explicit extension, oracle) and assert each records its engine in get and list, seeds exactly its bundle unioned with the request, re-adding a seeded extension is a no-op, and an endpoint on aidb and oracle can create and exercise every seeded extension; an unknown engine is refused with 400.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpClusterEngine,
	})
}

func runAmpClusterEngine(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"cluster_engine", a.clusterEngine}}
	})
}

func (a *ampFeatureRun) clusterEngine(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	base := "eng-" + a.f.suffix()
	_, _, err = cl.CreateCluster(ctx, a.proj.poolID, controller.CreateClusterRequest{Name: base + "-bad", PgVersion: a.f.cfg.PgVersion, Engine: engineUnknown})
	if err == nil {
		return fmt.Errorf("creating a cluster with engine %q was accepted, want 400", engineUnknown)
	}
	if extensionsUnavailable(err) {
		a.f.t.Skip("externalized extensions are not available on this control plane, so no engine but postgres exists: " + err.Error())
		return nil
	}
	if status := apiErrorStatus(err); status != http.StatusBadRequest {
		return fmt.Errorf("creating a cluster with engine %q failed with HTTP %d, want 400: %w", engineUnknown, status, err)
	}

	var unknown []string
	for _, tc := range engineCases {
		err := a.engineCase(ctx, cl, base+"-"+tc.engine, tc)
		if n := unknownExtensionName(err); n != "" {
			a.f.logf("engine %s: this controller's catalog does not know %s, the image under test predates it", tc.engine, n)
			unknown = append(unknown, tc.engine)
			continue
		}
		if err != nil {
			return fmt.Errorf("engine %s: %w", tc.engine, err)
		}
	}
	if len(unknown) == len(engineCases)-1 {
		a.f.t.Skip("no engine but postgres could be exercised: the catalog predates every bundle")
	}
	return nil
}

func (a *ampFeatureRun) engineCase(ctx context.Context, cl *controller.Client, name string, tc engineCase) error {
	if _, err := cl.CreateClusterAndWait(ctx, a.proj.poolID, controller.CreateClusterRequest{
		Name: name, PgVersion: a.f.cfg.PgVersion, Engine: tc.engine, Extensions: tc.extra,
	}); err != nil {
		if unknownExtensionName(err) != "" {
			return err
		}
		return fmt.Errorf("create cluster: %w", err)
	}
	proj, err := findClusterCtl(ctx, cl, a.proj.poolID, name)
	if err != nil {
		return err
	}
	a.f.register("delete "+tc.engine+" cluster", func(ctx context.Context) error {
		_, e := cl.DeleteClusterAndWait(ctx, a.proj.poolID, proj.ID)
		return e
	})
	if proj.Engine != tc.engine {
		return fmt.Errorf("listClusters reports engine %q for %s, want %q", proj.Engine, proj.ID, tc.engine)
	}
	got, err := cl.GetCluster(ctx, a.proj.poolID, proj.ID)
	if err != nil {
		return fmt.Errorf("get cluster %s: %w", proj.ID, err)
	}
	if got.Engine != tc.engine {
		return fmt.Errorf("getCluster reports engine %q, want %q", got.Engine, tc.engine)
	}
	if missing := missingNames(got.Extensions, tc.seeds); len(missing) > 0 {
		return fmt.Errorf("cluster carries extensions %v, missing %v: the engine bundle and the request's extensions must union", got.Extensions, missing)
	}
	if extra := missingNames(tc.seeds, got.Extensions); len(extra) > 0 {
		return fmt.Errorf("cluster carries extensions %v beyond the %s bundle and the request: %v", got.Extensions, tc.engine, extra)
	}
	if len(tc.seeds) == 0 {
		return nil
	}

	// Re-adding a seeded name must be a no-op, not an error or a duplicate.
	if _, err := cl.AddBranchExtensionsAndWait(ctx, a.proj.poolID, proj.ID, proj.DefaultBranch.BranchID, controller.AddBranchExtensionsRequest{Extensions: tc.seeds[:1]}); err != nil {
		return fmt.Errorf("re-add the already seeded %s: %w", tc.seeds[0], err)
	}
	again, err := cl.GetCluster(ctx, a.proj.poolID, proj.ID)
	if err != nil {
		return err
	}
	if len(again.Extensions) != len(got.Extensions) {
		return fmt.Errorf("re-adding %s changed the extension set from %v to %v", tc.seeds[0], got.Extensions, again.Extensions)
	}

	ep, err := createEndpoint(ctx, cl, a.proj.poolID, proj.ID, proj.DefaultBranch.BranchID)
	if err != nil {
		return fmt.Errorf("create endpoint: %w", err)
	}
	c, err := a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	defer c.Close()
	for _, catalog := range tc.seeds {
		sql := catalogSQLName(catalog)
		n, err := c.QueryInt(ctx, "SELECT count(*) FROM pg_available_extensions WHERE name = $1", sql)
		if err != nil {
			return fmt.Errorf("probe pg_available_extensions for %s: %w", sql, err)
		}
		if n == 0 {
			return fmt.Errorf("the endpoint has no %q extension available though the branch carries %s", sql, catalog)
		}
		if _, err := c.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+quoteExtIdent(sql)); err != nil {
			return fmt.Errorf("CREATE EXTENSION %s: %w", sql, err)
		}
	}
	return tc.probe(ctx, c)
}

// catalogSQLName maps a catalog name to what CREATE EXTENSION uses.
func catalogSQLName(catalog string) string {
	if catalog == "pgvector" {
		return "vector"
	}
	return catalog
}

// probeAIDB runs one real call per aidb extension plus the unioned hexabridge.
func probeAIDB(ctx context.Context, c sqlConn) error {
	dist, err := c.QueryString(ctx, "SELECT ('[1,2,3]'::vector <-> '[1,2,4]'::vector)::text")
	if err != nil {
		return fmt.Errorf("vector distance: %w", err)
	}
	if dist != "1" {
		return fmt.Errorf("'[1,2,3]' <-> '[1,2,4]' = %s, want 1", dist)
	}
	if _, err := c.Exec(ctx, "LOAD 'age'"); err != nil {
		return fmt.Errorf("LOAD 'age': %w", err)
	}
	if _, err := c.Exec(ctx, `SET search_path = ag_catalog, "$user", public`); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, "SELECT ag_catalog.create_graph('aidb_probe')"); err != nil {
		return fmt.Errorf("age create_graph: %w", err)
	}
	if _, err := c.Exec(ctx, "SELECT * FROM ag_catalog.cypher('aidb_probe', $$CREATE (:n {k: 1})$$) AS (v ag_catalog.agtype)"); err != nil {
		return fmt.Errorf("age cypher CREATE: %w", err)
	}
	if n, err := c.QueryInt(ctx, "SELECT count(*) FROM ag_catalog.cypher('aidb_probe', $$MATCH (v:n) RETURN v$$) AS (v ag_catalog.agtype)"); err != nil {
		return fmt.Errorf("age cypher MATCH: %w", err)
	} else if n != 1 {
		return fmt.Errorf("age MATCH returned %d vertices, want 1", n)
	}
	if err := assertPreloaded(ctx, c, "pg_cron"); err != nil {
		return err
	}
	if n, err := c.QueryInt(ctx, "SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'pg_cron launcher'"); err != nil {
		return fmt.Errorf("look for the pg_cron launcher: %w", err)
	} else if n == 0 {
		return fmt.Errorf("pg_cron is seeded by aidb but no launcher backend is running")
	}
	return probeOracle(ctx, c)
}

// probeOracle is hexabridge's Oracle number formatting.
func probeOracle(ctx context.Context, c sqlConn) error {
	got, err := c.QueryString(ctx, "SELECT hexabridge.to_char(123.45, '999.99')")
	if err != nil {
		return fmt.Errorf("hexabridge.to_char: %w", err)
	}
	if strings.TrimSpace(got) != "123.45" {
		return fmt.Errorf("hexabridge.to_char(123.45, '999.99') = %q, want 123.45", got)
	}
	return nil
}

func missingNames(have, want []string) []string {
	set := map[string]bool{}
	for _, h := range have {
		set[strings.ToLower(h)] = true
	}
	var missing []string
	for _, w := range want {
		if !set[strings.ToLower(w)] {
			missing = append(missing, w)
		}
	}
	return missing
}
