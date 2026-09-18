// amp on a multi-region YugabyteDB: the backing is a YBA universe spread over
// several GCE regions with tablet leaders preferred beside the control plane,
// stood up for this test alone (core.EnvSpec.BackingConfig). It asserts the
// universe took that shape, that amp writes and reads through it, and records
// the commit latency a cross-region quorum costs.
package features

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/multiregion"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/yba"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/universe"
)

const multiRegionCommitSamples = 200

func init() {
	core.Register(core.Test{
		Name:     "features/multiregion_backing",
		Suite:    "features",
		Category: core.Functional,
		Env: core.EnvSpec{
			Sharing:       core.Exclusive,
			Shape:         core.DedicatedStack,
			BackingConfig: multiregion.BackingConfig,
		},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Stand amp on a YBA universe spread over several GCE regions (leaders preferred beside the control plane), assert the universe took the declared shape, round-trip writes through an amp endpoint, and record cross-region commit latency.",
		Params:      []string{"multiregion", "apps.yba.gcp", "apps.ybastack", "env.ampstack"},
		Preflight:   multiregion.Preflight,
		Timeout:     90 * time.Minute,
		Run:         runMultiRegionBacking,
	})
}

// multiRegionEnv is what the dedicated stack exposes: the endpoint's Postgres,
// the YBA behind the backing, and a route to the storage database on it.
type multiRegionEnv interface {
	pgEnv
	YBAUniverse() (universe.YBAMaintainer, string)
	YBStorage(ctx context.Context) (pg.ConnInfo, error)
}

func runMultiRegionBacking(t *core.T) error {
	env, ok := t.Env.(multiRegionEnv)
	if !ok {
		t.Skip("requires a dedicated stack exposing its backing universe (sharedk8s in deploy mode)")
		return nil
	}
	yb, universeID := env.YBAUniverse()
	if yb == nil {
		t.Skip("the backing is not a YBA universe: the run moved this test off its own stack (env.stack=shared or env.yb_deploy overrides)")
		return nil
	}
	shape := multiregion.FromConfig(t.Options)

	var u yba.Universe
	if err := t.Step("universe_shape", func(s *core.StepHandle) error {
		var err error
		u, err = yb.GetUniverse(t.Context, universeID)
		if err != nil {
			return core.EnvFail("read universe %s: %w", universeID, err)
		}
		s.Set("universe", u.Name)
		s.Set("topology", u.DescribeTopology())
		s.Set("preferred_region", shape.PreferredRegion)
		s.Set("affinitized_regions", strings.Join(u.AffinitizedRegions(), ","))
		return shape.Check(u)
	}); err != nil {
		return err
	}

	if err := t.Step("backing_sees_every_region", func(s *core.StepHandle) error {
		ci, err := env.YBStorage(t.Context)
		if err != nil {
			return core.EnvFail("route to the storage database: %w", err)
		}
		client, err := pg.Connect(t.Context, ci)
		if err != nil {
			return core.EnvFail("connect to the storage database at %s: %w", ci.Addr(), err)
		}
		defer client.Close()
		regions, err := regionsFromYBServers(t.Context, client)
		if err != nil {
			return err
		}
		s.Set("yb_servers_regions", describeCounts(regions))
		for _, r := range shape.Regions {
			if regions[r.Code] != r.Nodes {
				return fmt.Errorf("yb_servers() lists %d tserver(s) in %s, the universe placed %d there: the database does not see the placement YBA reports", regions[r.Code], r.Code, r.Nodes)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	client, err := connect(t, env)
	if err != nil {
		return err
	}
	defer client.Close()
	table := "mr_" + suffix(t)
	defer func() { _, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table) }()

	if err := t.Step("roundtrip_through_amp", func(s *core.StepHandle) error {
		rows, wantSum := genRows(t, 500)
		if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v bigint)", table)); err != nil {
			return err
		}
		batch := make([]string, 0, len(rows))
		for id, v := range rows {
			batch = append(batch, fmt.Sprintf("(%d,%d)", id, v))
		}
		if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES %s", table, strings.Join(batch, ","))); err != nil {
			return err
		}
		count, err := client.QueryInt(t, "SELECT count(*) FROM "+table)
		if err != nil {
			return err
		}
		if int(count) != len(rows) {
			return fmt.Errorf("%s holds %d rows after inserting %d through amp on the multi-region backing", table, count, len(rows))
		}
		gotSum, err := client.QueryInt(t, "SELECT coalesce(sum(v),0) FROM "+table)
		if err != nil {
			return err
		}
		if gotSum != wantSum {
			return fmt.Errorf("sum(v) over %s = %d, want %d", table, gotSum, wantSum)
		}
		s.Set("rows", len(rows))
		return nil
	}); err != nil {
		return err
	}

	return t.Step("commit_latency", func(s *core.StepHandle) error {
		lat := make([]time.Duration, 0, multiRegionCommitSamples)
		for i := 0; i < multiRegionCommitSamples; i++ {
			start := time.Now()
			if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), 1_000_000+i, int64(i)); err != nil {
				return fmt.Errorf("single-row commit %d: %w", i, err)
			}
			lat = append(lat, time.Since(start))
		}
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		p50, p95, max := lat[len(lat)/2], lat[len(lat)*95/100], lat[len(lat)-1]
		s.Set("samples", len(lat))
		s.Set("p50", p50.String())
		s.Set("p95", p95.String())
		s.Set("max", max.String())
		t.Metric("multiregion_commit_p50_ms", float64(p50.Microseconds())/1000, "ms")
		t.Metric("multiregion_commit_p95_ms", float64(p95.Microseconds())/1000, "ms")
		t.Metric("multiregion_commit_max_ms", float64(max.Microseconds())/1000, "ms")
		return nil
	})
}

// regionsFromYBServers counts the tservers the database itself reports per
// region (yb_servers() is served by the tserver the connection landed on).
func regionsFromYBServers(ctx context.Context, client *pg.Client) (map[string]int, error) {
	out, err := client.QueryString(ctx, "SELECT string_agg(region || '=' || n, ',' ORDER BY region) FROM (SELECT region, count(*) AS n FROM yb_servers() GROUP BY region) r")
	if err != nil {
		return nil, fmt.Errorf("yb_servers(): %w", err)
	}
	regions := map[string]int{}
	for _, part := range strings.Split(strings.TrimSpace(out), ",") {
		region, count, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(count)
		if err != nil {
			return nil, fmt.Errorf("yb_servers(): unexpected count %q for region %q", count, region)
		}
		regions[region] = n
	}
	if len(regions) == 0 {
		return nil, fmt.Errorf("yb_servers() reported no region for any tserver (%q)", out)
	}
	return regions, nil
}

func describeCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, ",")
}
