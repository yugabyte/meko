package features

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pageserver"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	lfcTable           = "amp_lfc_ws"
	computeCtlHTTPPort = 3080
	lfcOffloadsMetric  = "compute_ctl_lfc_offloads_total"
	lfcOffloadErrors   = "compute_ctl_lfc_offload_errors_total"
	lfcPrewarmErrors   = "compute_ctl_lfc_prewarm_errors_total"
	lfcPrewarmTimeout  = 5 * time.Minute
	// A prewarm reloads what was offloaded; pages evicted or never offloaded
	// are skipped, so most but not all of the working set comes back.
	lfcMinPrewarmedRatio = 0.5
	lfcMinHitRatio       = 0.5
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_lfc_prewarm",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "LFC offload and autoprewarm across a compute restart, which the controller turns on for every Kubernetes endpoint. Read a working set larger than shared_buffers, wait for compute_ctl's periodic offload to save the LFC state to YB, then restart the compute pod. get_prewarm_info must report the saved pages loaded back, the first scan must hit the LFC, and every row must match its checksum. A second endpoint restarted before its first offload must start cold, with no prewarm error.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     40 * time.Minute,
		Run:         runAmpLFCPrewarm,
	})
}

func runAmpLFCPrewarm(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		var ep controller.Endpoint
		var sum string
		var pages int64
		return []phase{
			{"lfc_working_set", func(ctx context.Context) error {
				var err error
				ep, sum, pages, err = a.lfcWorkingSet(ctx)
				return err
			}},
			{"lfc_prewarm_after_restart", func(ctx context.Context) error {
				if ep.ID == "" {
					return nil
				}
				return a.lfcPrewarmAfterRestart(ctx, ep, sum, pages)
			}},
			{"lfc_cold_start_before_offload", a.lfcColdStart},
		}
	})
}

func (a *ampFeatureRun) computeCtl(ctx context.Context, ep controller.Endpoint) (*pageserver.Client, *endpointDaemon, error) {
	d, err := a.f.endpointDaemon(ctx, ep, "compute-ctl")
	if err != nil {
		return nil, nil, err
	}
	return pageserver.New(podPageserverExec(d.kube, d.pod), computeCtlHTTPPort), d, nil
}

func metricValue(ctx context.Context, c *pageserver.Client, name string) (float64, error) {
	s, err := c.Metrics(ctx)
	if err != nil {
		return 0, fmt.Errorf("scrape compute_ctl metrics: %w", err)
	}
	v, ok := s.Value(name, nil)
	if !ok {
		return 0, fmt.Errorf("compute_ctl exports no %s", name)
	}
	return v, nil
}

// lfcOffloadInterval reads the interval the controller rendered into the pod;
// zero means this control plane runs with offload and prewarm off.
func lfcOffloadInterval(ctx context.Context, d *endpointDaemon) (time.Duration, error) {
	res, err := d.exec().Run(ctx, `printf '%s|%s' "$AUTOPREWARM" "$OFFLOAD_LFC_INTERVAL_SECONDS"`)
	if err != nil {
		return 0, fmt.Errorf("read the LFC env in pod %s: %w", d.pod, err)
	}
	auto, secs, _ := strings.Cut(strings.TrimSpace(res.Stdout), "|")
	if auto != "true" || secs == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(secs)
	if err != nil {
		return 0, fmt.Errorf("OFFLOAD_LFC_INTERVAL_SECONDS in pod %s is %q", d.pod, secs)
	}
	return time.Duration(n) * time.Second, nil
}

func (a *ampFeatureRun) lfcWorkingSet(ctx context.Context) (controller.Endpoint, string, int64, error) {
	fail := func(err error) (controller.Endpoint, string, int64, error) {
		return controller.Endpoint{}, "", 0, err
	}
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return fail(err)
	}
	ep, err := createEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID)
	if err != nil {
		return fail(err)
	}
	cctl, d, err := a.computeCtl(ctx, ep)
	if err != nil {
		return fail(err)
	}
	interval, err := lfcOffloadInterval(ctx, d)
	if err != nil {
		return fail(err)
	}
	if interval == 0 {
		a.f.t.Skip("this control plane renders endpoints with LFC offload and autoprewarm off (deploy-lfc-offload-interval-seconds=0), so there is nothing to prewarm")
		return fail(nil)
	}

	c, err := a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fail(fmt.Errorf("connect endpoint: %w", err))
	}
	defer c.Close()
	sb, err := c.QueryInt(ctx, "SELECT setting::bigint * 8192 FROM pg_settings WHERE name = 'shared_buffers'")
	if err != nil {
		return fail(err)
	}
	limitMB, err := c.QueryInt(ctx, "SELECT setting::bigint FROM pg_settings WHERE name = 'yb_amp.file_cache_size_limit'")
	if err != nil {
		return fail(fmt.Errorf("read yb_amp.file_cache_size_limit: %w", err))
	}
	target := 2*sb + 32<<20
	if limitMB<<20 < target+target/4 {
		return fail(fmt.Errorf("the endpoint's LFC limit is %d MB, which cannot hold a working set of twice shared_buffers (%d MB): autoprewarm is on, so the LFC has to be big enough to be worth prewarming", limitMB, target>>20))
	}
	// About seven 1 KB rows fit a page.
	rows := target / 1170
	a.f.logf("amp_lfc_prewarm: shared_buffers %d MB, LFC limit %d MB, seeding %d rows (~%d MB), offload every %s", sb>>20, limitMB, rows, target>>20, interval)
	for _, stmt := range []string{
		"CREATE TABLE " + lfcTable + " (id bigint PRIMARY KEY, pad text NOT NULL)",
		fmt.Sprintf("INSERT INTO %s SELECT g, repeat(md5(g::text), 31) FROM generate_series(1, %d) g", lfcTable, rows),
		"VACUUM ANALYZE " + lfcTable,
	} {
		if _, err := c.Exec(ctx, stmt); err != nil {
			return fail(fmt.Errorf("seed the working set: %w", err))
		}
	}
	sum, err := c.QueryString(ctx, lfcChecksum)
	if err != nil {
		return fail(err)
	}
	pages, err := c.QueryInt(ctx, "SELECT relpages::bigint FROM pg_class WHERE relname = $1", lfcTable)
	if err != nil {
		return fail(err)
	}

	before, err := metricValue(ctx, cctl, lfcOffloadsMetric)
	if err != nil {
		return fail(err)
	}
	var now float64
	err = pollUntil(ctx, interval+3*time.Minute, 10*time.Second, func() (bool, error) {
		v, err := metricValue(ctx, cctl, lfcOffloadsMetric)
		if err != nil {
			return false, err
		}
		now = v
		return v > before, nil
	})
	if err != nil {
		return fail(fmt.Errorf("%s stayed at %v for more than one offload interval (%s) after the working set was read: compute_ctl is not saving the LFC state: %w", lfcOffloadsMetric, now, interval, err))
	}
	if errs, err := metricValue(ctx, cctl, lfcOffloadErrors); err != nil {
		return fail(err)
	} else if errs > 0 {
		return fail(fmt.Errorf("compute_ctl counted %v LFC offload errors before the restart", errs))
	}
	return ep, sum, pages, nil
}

var lfcChecksum = "SELECT md5(string_agg(md5(id::text || pad), '' ORDER BY id)) FROM " + lfcTable

func lfcStat(ctx context.Context, c sqlReader, key string) (int64, error) {
	return c.QueryInt(ctx, "SELECT coalesce(max(v), 0) FROM yb_amp.yb_amp_get_lfc_stats() AS t(k text, v bigint) WHERE k = $1", key)
}

func (a *ampFeatureRun) lfcPrewarmAfterRestart(ctx context.Context, ep controller.Endpoint, sum string, pages int64) error {
	c, ok, err := a.restartEndpointCompute(ctx, ep)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	defer c.Close()

	var total, prewarmed, skipped, active int64
	err = pollUntil(ctx, lfcPrewarmTimeout, 3*time.Second, func() (bool, error) {
		row, err := c.QueryString(ctx, "SELECT concat_ws(',', total_pages, prewarmed_pages, skipped_pages, active_workers) FROM yb_amp.get_prewarm_info()")
		if err != nil {
			return false, fmt.Errorf("get_prewarm_info: %w", err)
		}
		if _, err := fmt.Sscanf(strings.ReplaceAll(row, ",", " "), "%d %d %d %d", &total, &prewarmed, &skipped, &active); err != nil {
			return false, fmt.Errorf("parse get_prewarm_info %q: %w", row, err)
		}
		return total > 0 && active == 0 && prewarmed+skipped >= total, nil
	})
	a.f.logf("amp_lfc_prewarm: prewarm total=%d prewarmed=%d skipped=%d active=%d", total, prewarmed, skipped, active)
	if err != nil {
		return fmt.Errorf("after the restart get_prewarm_info reads total=%d prewarmed=%d skipped=%d active_workers=%d: autoprewarm did not load the offloaded LFC state: %w",
			total, prewarmed, skipped, active, err)
	}
	if float64(prewarmed) < lfcMinPrewarmedRatio*float64(total) {
		return fmt.Errorf("autoprewarm loaded %d of the %d offloaded pages (skipped %d), under %.0f%%", prewarmed, total, skipped, 100*lfcMinPrewarmedRatio)
	}
	a.f.t.Metric("lfc_prewarmed_pages", float64(prewarmed), "pages")

	cctl, _, err := a.computeCtl(ctx, ep)
	if err != nil {
		return err
	}
	if errs, err := metricValue(ctx, cctl, lfcPrewarmErrors); err != nil {
		return err
	} else if errs > 0 {
		return fmt.Errorf("compute_ctl counted %v LFC prewarm errors after the restart", errs)
	}

	hits0, err := lfcStat(ctx, c, "file_cache_hits")
	if err != nil {
		return fmt.Errorf("read LFC hits: %w", err)
	}
	got, err := c.QueryString(ctx, lfcChecksum)
	if err != nil {
		return err
	}
	if got != sum {
		return fmt.Errorf("the working set read back after a prewarmed restart with a different checksum")
	}
	hits1, err := lfcStat(ctx, c, "file_cache_hits")
	if err != nil {
		return err
	}
	hits := hits1 - hits0
	a.f.t.Metric("lfc_first_scan_hits", float64(hits), "pages")
	if float64(hits) < lfcMinHitRatio*float64(pages) {
		return fmt.Errorf("the first scan of the %d-page working set after the prewarmed restart hit the LFC %d times: the prewarmed pages are not being served", pages, hits)
	}
	return nil
}

func (a *ampFeatureRun) lfcColdStart(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	name := "lfc-cold-" + a.f.suffix()
	if _, err := cl.CreateBranchAndWait(ctx, a.proj.poolID, a.proj.projID, controller.CreateBranchRequest{Name: name, ParentBranchID: a.proj.branchID}); err != nil {
		return fmt.Errorf("create branch %s: %w", name, err)
	}
	br, err := findBranchCtl(ctx, cl, a.proj.poolID, a.proj.projID, name)
	if err != nil {
		return err
	}
	ep, err := createEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, br.BranchID)
	if err != nil {
		return err
	}
	c, ok, err := a.restartEndpointCompute(ctx, ep)
	if err != nil {
		return fmt.Errorf("an endpoint restarted before its first LFC offload must start cold, not fail: %w", err)
	}
	if !ok {
		return nil
	}
	defer c.Close()
	if _, err := c.QueryInt(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("an endpoint restarted before its first LFC offload does not serve: %w", err)
	}
	cctl, _, err := a.computeCtl(ctx, ep)
	if err != nil {
		return err
	}
	if errs, err := metricValue(ctx, cctl, lfcPrewarmErrors); err != nil {
		return err
	} else if errs > 0 {
		return fmt.Errorf("an endpoint restarted with no LFC state saved yet counted %v prewarm errors: a missing state is a cold start, not an error", errs)
	}
	return nil
}
