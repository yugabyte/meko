package features

import (
	"context"
	"fmt"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_extension_age",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Apache AGE on an opted-in endpoint: LOAD the library, create a graph, run a cypher CREATE and MATCH round trip, and drop the graph. Skips (visibly) while the controller's catalog predates the age entry; the skip disappears with the image that ships it.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     25 * time.Minute,
		Run:         runAmpExtensionAge,
	})
}

func runAmpExtensionAge(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"extension_age", func(ctx context.Context) error {
			cl, err := a.f.controllerClient(ctx)
			if err != nil {
				return err
			}
			return extensionAgeSteps(ctx, cl, a.proj, "aggraph_"+a.f.suffix(), a.f.endpointSQL, a.f.t.Skip)
		}}}
	})
}

func extensionAgeSteps(ctx context.Context, cl ampCtl, proj ampCluster, graph string, connect endpointConnector, skip func(string)) error {
	c, ok, err := optInExtensionEndpoint(ctx, cl, proj, connect, skip, "age", "age")
	if err != nil || !ok {
		return err
	}
	defer c.Close()

	// AGE loads per session rather than through shared_preload_libraries
	// (deploy/extensions/extensions.yaml), so the library must be loadable in
	// THIS backend before any of its functions run.
	if _, err := c.Exec(ctx, "LOAD 'age'"); err != nil {
		return fmt.Errorf("LOAD 'age' on an endpoint where CREATE EXTENSION age succeeded — the ImageVolume carries the SQL but not a loadable library: %w", err)
	}
	// AGE's own setup: its objects live in ag_catalog, and create_graph does
	// internal DDL that names graphid_ops unqualified — without the schema on the
	// search_path that resolves to "operator class graphid_ops does not exist".
	if _, err := c.Exec(ctx, `SET search_path = ag_catalog, "$user", public`); err != nil {
		return fmt.Errorf("put ag_catalog on the search_path: %w", err)
	}
	if _, err := c.Exec(ctx, "SELECT ag_catalog.create_graph('"+graph+"')"); err != nil {
		return fmt.Errorf("ag_catalog.create_graph: %w", err)
	}
	// Vertices go through cypher into AGE's own tables (backed by amp storage),
	// and come back out through a MATCH — the round trip is the feature.
	for i := 0; i < 3; i++ {
		if _, err := c.Exec(ctx,
			"SELECT * FROM ag_catalog.cypher('"+graph+"', $$CREATE (:amp_node {seq: "+fmt.Sprint(i)+"})$$) AS (v ag_catalog.agtype)"); err != nil {
			return fmt.Errorf("cypher CREATE vertex %d: %w", i, err)
		}
	}
	n, err := c.QueryInt(ctx,
		"SELECT count(*) FROM ag_catalog.cypher('"+graph+"', $$MATCH (v:amp_node) RETURN v$$) AS (v ag_catalog.agtype)")
	if err != nil {
		return fmt.Errorf("cypher MATCH over the created vertices: %w", err)
	}
	if n != 3 {
		return fmt.Errorf("cypher MATCH returned %d amp_node vertices, want the 3 just created — AGE's graph storage over amp is losing vertices", n)
	}
	if _, err := c.Exec(ctx, "SELECT ag_catalog.drop_graph('"+graph+"', true)"); err != nil {
		return fmt.Errorf("ag_catalog.drop_graph: %w", err)
	}
	return nil
}
