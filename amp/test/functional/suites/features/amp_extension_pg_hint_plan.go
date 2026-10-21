package features

import (
	"context"
	"fmt"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_extension_pg_hint_plan",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "pg_hint_plan on an opted-in endpoint: the library is preloaded, a SeqScan hint and an IndexScan hint each override the plan the planner picks on its own, and the hints still apply after a compute restart.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     25 * time.Minute,
		Run:         runAmpExtensionPgHintPlan,
	})
}

func runAmpExtensionPgHintPlan(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		table := "hint_probe_" + a.f.suffix()
		return []phase{
			{"extension_pg_hint_plan", func(ctx context.Context) error {
				cl, err := a.f.controllerClient(ctx)
				if err != nil {
					return err
				}
				return extensionPgHintPlanSteps(ctx, cl, a.proj, table, a.f.endpointSQL, a.f.t.Skip)
			}},
			{"hints_survive_restart", func(ctx context.Context) error { return a.hintPlanSurvivesRestart(ctx, table) }},
		}
	})
}

func extensionPgHintPlanSteps(ctx context.Context, cl ampCtl, proj ampCluster, table string, connect endpointConnector, skip func(string)) error {
	c, ok, err := optInExtensionEndpoint(ctx, cl, proj, connect, skip, "pg_hint_plan", "pg_hint_plan")
	if err != nil || !ok {
		return err
	}
	defer c.Close()
	if err := assertPreloaded(ctx, c, "pg_hint_plan"); err != nil {
		return err
	}
	for _, stmt := range []string{
		"CREATE TABLE " + table + " (id int PRIMARY KEY, v int)",
		"INSERT INTO " + table + " SELECT g, g FROM generate_series(1, 10000) g",
		"ANALYZE " + table,
	} {
		if _, err := c.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("set up the hint table: %w", err)
		}
	}

	// Each hint must flip the plan the planner picks unhinted, or the test
	// cannot tell a working hint from a coincidence.
	point := "SELECT * FROM " + table + " t WHERE id = 42"
	scan := "SELECT * FROM " + table + " t WHERE id > 0"
	for _, tc := range []struct {
		hint, query, want string
	}{
		{"", point, "Index Scan"},
		{"/*+ SeqScan(t) */ ", point, "Seq Scan"},
		{"", scan, "Seq Scan"},
		{"/*+ IndexScan(t) */ ", scan, "Index Scan"},
	} {
		if err := assertPlanTop(ctx, c, tc.hint, tc.query, tc.want); err != nil {
			return err
		}
	}
	return nil
}

// hintPlanSurvivesRestart rolls the compute: the preload must come back with it,
// or hints silently stop applying.
func (a *ampFeatureRun) hintPlanSurvivesRestart(ctx context.Context, table string) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, ok := lastEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID)
	if !ok {
		return fmt.Errorf("the branch has no endpoint to restart")
	}
	c, ok, err := a.restartEndpointCompute(ctx, ep)
	if err != nil || !ok {
		return err
	}
	defer c.Close()
	if err := assertPreloaded(ctx, c, "pg_hint_plan"); err != nil {
		return fmt.Errorf("after a compute restart: %w", err)
	}
	return assertPlanTop(ctx, c, "/*+ SeqScan(t) */ ", "SELECT * FROM "+table+" t WHERE id = 42", "Seq Scan")
}
