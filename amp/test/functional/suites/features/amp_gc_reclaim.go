package features

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pageserver"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	gcReclaimVictimRows = 2000
	// gcReclaimSeedTimeout bounds waiting for the seeded pages to land in
	// amp_storage (the pageserver flushes on its own cadence; the forced flush
	// below is best-effort).
	gcReclaimSeedTimeout = 3 * time.Minute
	// gcReclaimTimeout bounds the drain: with gc_period=1s, a 2s LSN lease and a
	// churned WAL frontier the integration suite reclaims in well under a minute,
	// so this is generous for a deployed stack.
	gcReclaimTimeout = 5 * time.Minute
)

// gcReclaimPageserverSettings turns GC on for this endpoint and tightens its
// retention, through the settings API. `enable_gc` is redundant on a current
// image and deliberately kept: against a released image whose compiled default
// is still off, relying on the default would fail this test at its reclaim
// deadline with a message pointing at the drain, sending whoever triages the
// nightly after a bug that is not there. The rest is the integration suite's GC
// fragment (test/integration/compute/cluster.rs) as create-time overrides.
var gcReclaimPageserverSettings = map[string]any{
	"tenant_config.enable_gc":                     true,
	"tenant_config.gc_candidate_every_n_bases":    1,
	"tenant_config.gc_period":                     "1s",
	"tenant_config.gc_queue_floor_reset_interval": "5s",
	"tenant_config.pitr_interval":                 "0s",
	"tenant_config.gc_horizon":                    1024,
	// The cutoff is min(retention, unexpired LSN lease), and a lease lasts ten
	// minutes by default — longer than any sane test window, so without this the
	// drain queues candidates and deletes nothing.
	"tenant_config.lsn_lease_length": "2s",
}

const (
	gcRowsDeletedMetric        = "pageserver_yb_gc_rows_deleted_total"
	gcCandidatesDequeuedMetric = "pageserver_yb_gc_candidates_dequeued_total"
	gcCandidatesEmittedMetric  = "pageserver_yb_gc_candidates_emitted_total"
)

// gcEmitPaths are the producer paths the emitted counter seeds at startup.
var gcEmitPaths = []string{"ingest", "materialiser", "drop", "shrink"}

// gcCountersPresent requires the GC counters before GC has run: they are
// registered at startup so an idle drain reads zero, and an absent series cannot
// be read as flat. https://github.com/yugabyte/meko/issues/1142
func gcCountersPresent(s pageserver.Samples) error {
	for _, name := range []string{gcCandidatesDequeuedMetric, gcRowsDeletedMetric} {
		if _, ok := s.Value(name, nil); !ok {
			return fmt.Errorf("the pageserver does not export %s before any GC work: an operator cannot tell a GC with nothing to do from one that is switched off, which is why it is registered at startup", name)
		}
	}
	for _, path := range gcEmitPaths {
		if _, ok := s.Value(gcCandidatesEmittedMetric, map[string]string{"path": path}); !ok {
			return fmt.Errorf("the pageserver exports no %s{path=%q} before any GC work: every producer path is seeded off the enum's variants so a childless vec cannot hide one, and an unseeded path reads as \"nothing is emitting\"", gcCandidatesEmittedMetric, path)
		}
	}
	return nil
}

func init() {
	core.Register(core.Test{
		Name:        "features/amp_gc_reclaim",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "DROP TABLE reclamation end to end: on an endpoint whose GC retention is tightened via pageserver_settings, the dropped relation's amp_storage.pages and pending_gc rows drain to zero while a live table's data and stored pages survive. The pageserver exports its three GC counters, every emitted producer path included, before any GC has run, and its rows-deleted counter moves with the drain.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     25 * time.Minute,
		Run:         runAmpGCReclaim,
	})
}

func runAmpGCReclaim(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"gc_reclaim", func(ctx context.Context) error {
			cl, err := a.f.controllerClient(ctx)
			if err != nil {
				return err
			}
			// The verdict is read out of amp_storage on the backing, which the
			// flow reaches in both modes. No route at all means the capability is
			// absent (skip); a route that will not answer is the cluster failing,
			// never the product.
			storage, err := a.f.ampStorageSQL(ctx)
			if err != nil {
				if errors.Is(err, errNoStorageRoute) {
					a.f.t.Skip(err.Error())
					return nil
				}
				return core.AsEnvFailure(fmt.Errorf("reach the backing amp storage database for the reclaim verdict: %w", err))
			}
			defer storage.Close()
			return gcReclaimSteps(ctx, cl, a.proj, a.f.endpointSQL, a.f.endpointPageserver, storage, a.f.t.Metric, a.f.logf, a.f.t.Skip)
		}}}
	})
}

// pendingGCKeyHex renders a relnode as the 4 big-endian hex bytes pending_gc
// keys carry at bytes 10-13 (1-indexed) of the encoded Key.
func pendingGCKeyHex(relnode int64) string {
	return fmt.Sprintf("%08x", uint32(relnode))
}

func gcReclaimSteps(ctx context.Context, cl ampCtl, proj ampCluster, connect endpointConnector, findPS pageserverFinder,
	storage *pg.Client, metric func(string, float64, string), logf func(string, ...any), skip func(string)) error {
	ep, err := createEndpointReq(ctx, cl, proj.poolID, proj.projID, proj.branchID, controller.CreateEndpointRequest{
		Role:               roleReadWrite,
		PageserverSettings: gcReclaimPageserverSettings,
	})
	if err != nil {
		return fmt.Errorf("create endpoint with GC pageserver_settings: %w", err)
	}
	// A controller predating the settings API ignores the unknown JSON fields,
	// leaving an endpoint whose GC is off — reclamation would then time out
	// against a healthy product. The read-back is the capability check.
	got, err := cl.GetEndpoint(ctx, proj.poolID, proj.projID, proj.branchID, ep.ID)
	if err != nil {
		return fmt.Errorf("read endpoint %s back: %w", ep.ID, err)
	}
	if len(got.PageserverSettings) == 0 {
		skip("this controller does not persist pageserver_settings, so GC cannot be turned on or tightened on the endpoint")
		return nil
	}

	c, err := connect(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	defer c.Close()

	// The storage rows are keyed by the branch id the ENDPOINT reports, which is
	// the only id amp_storage is guaranteed to agree with.
	branchID, err := c.QueryString(ctx, "SELECT current_setting('yb_amp.branch_id')")
	if err != nil {
		return fmt.Errorf("read the endpoint's branch id: %w", err)
	}

	victim, keeper := "gc_reclaim_victim", "gc_reclaim_keeper"
	for _, table := range []string{victim, keeper} {
		if _, err := c.Exec(ctx, "CREATE TABLE "+table+" (id int primary key, payload text)"); err != nil {
			return fmt.Errorf("create %s: %w", table, err)
		}
	}
	if _, err := c.Exec(ctx, "INSERT INTO "+victim+" SELECT g, md5(g::text) FROM generate_series(1, $1) g", gcReclaimVictimRows); err != nil {
		return fmt.Errorf("seed %s: %w", victim, err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO "+keeper+" SELECT g, md5(g::text) FROM generate_series(1, 200) g"); err != nil {
		return fmt.Errorf("seed %s: %w", keeper, err)
	}
	victimRel, err := c.QueryInt(ctx, "SELECT pg_relation_filenode($1)::bigint", victim)
	if err != nil {
		return fmt.Errorf("read %s relfilenode: %w", victim, err)
	}
	keeperRel, err := c.QueryInt(ctx, "SELECT pg_relation_filenode($1)::bigint", keeper)
	if err != nil {
		return fmt.Errorf("read %s relfilenode: %w", keeper, err)
	}
	if _, err := c.Exec(ctx, "CHECKPOINT"); err != nil {
		return fmt.Errorf("checkpoint after seed: %w", err)
	}

	pagesCount := func(relnode int64) (int64, error) {
		return storage.QueryInt(ctx, "SELECT count(*) FROM amp_storage.pages WHERE branch_id = $1 AND relnode = $2", branchID, relnode)
	}
	queueCount := func(relnode int64) (int64, error) {
		return storage.QueryInt(ctx,
			"SELECT count(*) FROM amp_storage.pending_gc WHERE branch_id = $1 AND substring(key FROM 10 FOR 4) = decode($2, 'hex')",
			branchID, pendingGCKeyHex(relnode))
	}
	// No kube context is structural; anything else is a flake that would leave
	// this test green while asserting nothing it exists to assert.
	ps, psErr := findPS(ctx, ep)
	psOK := psErr == nil
	switch {
	case psOK:
	case errors.Is(psErr, errNoKubeContext):
		logf("amp_gc_reclaim: no kube context, so the forced flush and the GC counter contract are not asserted: %v", psErr)
	default:
		return core.AsEnvFailure(fmt.Errorf("reach the pageserver serving endpoint %s, which this test needs to read the GC counters: %w", ep.ID, psErr))
	}
	// A determinism aid: the poll below covers the pageserver's flush cadence.
	forceFlush := func() {
		if !psOK {
			return
		}
		lsn, err := c.QueryString(ctx, "SELECT pg_current_wal_flush_lsn()::text")
		if err != nil {
			return
		}
		if err := ps.FlushBranch(ctx, branchID, lsn); err != nil {
			logf("amp_gc_reclaim: forced flush failed (best-effort): %v", err)
		}
	}

	// Before this endpoint's GC has reclaimed anything.
	var deletedBefore float64
	if psOK {
		idle, err := ps.Metrics(ctx)
		if err != nil {
			return fmt.Errorf("scrape the pageserver's metrics before the drain: %w", err)
		}
		if err := gcCountersPresent(idle); err != nil {
			return err
		}
		deletedBefore, _ = idle.Value(gcRowsDeletedMetric, nil)
	}

	forceFlush()
	seedDeadline := time.Now().Add(gcReclaimSeedTimeout)
	var stored int64
	for {
		if stored, err = pagesCount(victimRel); err != nil {
			return fmt.Errorf("count %s pages in amp_storage: %w", victim, err)
		}
		if stored > 0 {
			break
		}
		if time.Now().After(seedDeadline) {
			return fmt.Errorf("amp_storage.pages holds no rows for %s (branch %s relnode %d) within %s of the seed — the writes never reached storage", victim, branchID, victimRel, gcReclaimSeedTimeout)
		}
		if _, err := c.Exec(ctx, "CHECKPOINT"); err != nil {
			return fmt.Errorf("checkpoint while waiting for the seed flush: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	if _, err := c.Exec(ctx, "DROP TABLE "+victim); err != nil {
		return fmt.Errorf("drop %s: %w", victim, err)
	}
	dropAt := time.Now()

	// Churn the keeper so the WAL frontier — and with pitr=0s the GC cutoff —
	// keeps moving past the drop.
	pages, queue := int64(-1), int64(-1)
	reclaimDeadline := time.Now().Add(gcReclaimTimeout)
	for i := 201; ; i++ {
		if _, err := c.Exec(ctx, "INSERT INTO "+keeper+" VALUES ($1, md5($2))", i, strconv.Itoa(i)); err != nil {
			return fmt.Errorf("churn %s: %w", keeper, err)
		}
		if _, err := c.Exec(ctx, "CHECKPOINT"); err != nil {
			return fmt.Errorf("checkpoint during churn: %w", err)
		}
		if pages, err = pagesCount(victimRel); err != nil {
			return fmt.Errorf("count dropped %s pages: %w", victim, err)
		}
		if queue, err = queueCount(victimRel); err != nil {
			return fmt.Errorf("count dropped %s pending_gc rows: %w", victim, err)
		}
		if pages == 0 && queue == 0 {
			break
		}
		if time.Now().After(reclaimDeadline) {
			return fmt.Errorf("dropped table %s not reclaimed within %s: amp_storage still holds pages=%d pending_gc=%d for branch %s relnode %d (seeded %d page rows) — GC is enabled and tightened on this endpoint via pageserver_settings, so the drain should have reached the drop", victim, gcReclaimTimeout, pages, queue, branchID, victimRel, stored)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	metric("gc_reclaim.drop_to_zero_seconds", time.Since(dropAt).Seconds(), "s")
	metric("gc_reclaim.victim_pages_reclaimed", float64(stored), "rows")

	// Charged per committed statement, so a drain that emptied the relation has
	// to move it; it used to be charged once, on the success path only.
	if psOK {
		drained, err := ps.Metrics(ctx)
		if err != nil {
			return fmt.Errorf("scrape the pageserver's metrics after the drain: %w", err)
		}
		deletedAfter, ok := drained.Value(gcRowsDeletedMetric, nil)
		if !ok {
			return fmt.Errorf("%s was exported before the drain and is absent after it", gcRowsDeletedMetric)
		}
		if deletedAfter <= deletedBefore {
			return fmt.Errorf("%s reads %.0f after the drain, unchanged from %.0f before it, but amp_storage went from %d page row(s) for the dropped relation to none: the deletes are charged as each statement commits, so a monitor reading this counter sees a GC that reclaimed nothing",
				gcRowsDeletedMetric, deletedAfter, deletedBefore, stored)
		}
		metric("gc_reclaim.rows_deleted_reported", deletedAfter-deletedBefore, "rows")
	}

	// GC must not have overreached: the keeper's data reads back intact through
	// the endpoint, and its stored pages are still there.
	final, err := c.QueryInt(ctx, "SELECT count(*) FROM "+keeper)
	if err != nil {
		return fmt.Errorf("re-read %s after the reclaim: %w", keeper, err)
	}
	if final < 200 {
		return fmt.Errorf("%s has %d rows after the victim's reclaim, want at least the 200 seeded — GC touched a live relation", keeper, final)
	}
	keeperPages, err := pagesCount(keeperRel)
	if err != nil {
		return fmt.Errorf("count %s pages in amp_storage: %w", keeper, err)
	}
	if keeperPages == 0 {
		return fmt.Errorf("amp_storage.pages holds no rows for the live table %s (relnode %d) after the victim's reclaim — GC deleted a live relation's storage", keeper, keeperRel)
	}
	return nil
}
