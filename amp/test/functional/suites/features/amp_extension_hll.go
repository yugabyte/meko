package features

import (
	"context"
	"fmt"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_extension_hll",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "hll on an opted-in endpoint: it is mounted without being preloaded, HyperLogLog sketches stored in a table read back with cardinalities near the true counts, and a union of two overlapping sketches counts their distinct total.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     25 * time.Minute,
		Run:         runAmpExtensionHll,
	})
}

func runAmpExtensionHll(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"extension_hll", func(ctx context.Context) error {
			cl, err := a.f.controllerClient(ctx)
			if err != nil {
				return err
			}
			return extensionHllSteps(ctx, cl, a.proj, "hll_probe_"+a.f.suffix(), a.f.endpointSQL, a.f.t.Skip)
		}}}
	})
}

func extensionHllSteps(ctx context.Context, cl ampCtl, proj ampCluster, table string, connect endpointConnector, skip func(string)) error {
	c, ok, err := optInExtensionEndpoint(ctx, cl, proj, connect, skip, "hll", "hll")
	if err != nil || !ok {
		return err
	}
	defer c.Close()

	libs, err := c.QueryString(ctx, "SHOW shared_preload_libraries")
	if err != nil {
		return fmt.Errorf("SHOW shared_preload_libraries: %w", err)
	}
	if containsToken(libs, "hll") {
		return fmt.Errorf("shared_preload_libraries is %q: hll is not a preload extension", libs)
	}

	// Two sketches of 10,000 distinct ids each, overlapping by half: 15,000 in all.
	// log2m 14 gives a standard error of about 0.8%; the default (11) is about 2.3%
	// and estimates this union 6% low.
	for _, stmt := range []string{
		"CREATE TABLE " + table + " (bucket int PRIMARY KEY, ids hll(14))",
		"INSERT INTO " + table + " SELECT 1, hll_add_agg(hll_hash_integer(g), 14) FROM generate_series(1, 10000) g",
		"INSERT INTO " + table + " SELECT 2, hll_add_agg(hll_hash_integer(g), 14) FROM generate_series(5001, 15000) g",
	} {
		if _, err := c.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("store hll sketches: %w", err)
		}
	}
	for _, tc := range []struct {
		desc, query string
		want        int64
	}{
		{"bucket 1", "SELECT round(hll_cardinality(ids))::bigint FROM " + table + " WHERE bucket = 1", 10000},
		{"bucket 2", "SELECT round(hll_cardinality(ids))::bigint FROM " + table + " WHERE bucket = 2", 10000},
		{"the union", "SELECT round(hll_cardinality(hll_union_agg(ids)))::bigint FROM " + table, 15000},
	} {
		got, err := c.QueryInt(ctx, tc.query)
		if err != nil {
			return fmt.Errorf("hll_cardinality of %s: %w", tc.desc, err)
		}
		if diff := got - tc.want; diff*20 > tc.want || -diff*20 > tc.want {
			return fmt.Errorf("hll_cardinality of %s = %d, want within 5%% of %d", tc.desc, got, tc.want)
		}
	}
	_, err = c.Exec(ctx, "DROP TABLE "+table)
	return err
}
