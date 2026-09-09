// GC's per-key trim of a LIVE block's version chain, the half of the drain
// `features/amp_gc_reclaim` does not cover. Scale comes from configuration, not
// data. https://github.com/yugabyte/meko/issues/1209
package features

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	// deepTrimBatches x deepTrimPerBatch versions, checkpointed per batch.
	deepTrimBatches  = 10
	deepTrimPerBatch = 300
	// deepTrimRowsPerStatement caps one GC delete, low enough that the chain
	// below needs several statements and the re-issue loop is exercised.
	deepTrimRowsPerStatement = 500
	// deepTrimMinDepth is the depth below which the setup, not GC, is at fault.
	deepTrimMinDepth = 3 * deepTrimRowsPerStatement
	// deepTrimDrainTimeout bounds many forced runs, not one gc_period.
	deepTrimDrainTimeout = 15 * time.Minute
	// deepTrimSettle is how long the count must hold still to count as settled.
	deepTrimSettle = 20 * time.Second
	// deepTrimSeedTimeout bounds waiting for the chain to reach storage.
	deepTrimSeedTimeout = 5 * time.Minute
	// deepTrimKeeperRows sizes the untouched table that catches over-reclamation.
	deepTrimKeeperRows = 500
	// deepTrimMinSLRUDepth is the clog depth below which the SLRU half is moot.
	deepTrimMinSLRUDepth = 200
	// deepTrimWatermarkDefault is the pageserver's default, set so the two
	// phases differ in exactly one value.
	deepTrimWatermarkDefault = 100000
)

// gcDeepTrimSettings turn GC on, tighten retention and cap the delete statement.
// pitr_interval 0s keeps find_gc_cutoffs from overwriting the space horizon, and
// lsn_lease_length doubles as GC's startup grace.
func gcDeepTrimSettings(watermarkCapacity int) map[string]any {
	return map[string]any{
		"tenant_config.enable_gc":                     true,
		"tenant_config.gc_candidate_every_n_bases":    1,
		"tenant_config.gc_queue_floor_reset_interval": "5s",
		// Pushed out of the way, not off: at a 1s period the drain would trim
		// the chain while it is still being written. Zero reads as GC disabled.
		"tenant_config.gc_period":                        "3600s",
		"tenant_config.pitr_interval":                    "0s",
		"tenant_config.gc_horizon":                       1024,
		"tenant_config.lsn_lease_length":                 "2s",
		"tenant_config.gc_rows_per_statement":            deepTrimRowsPerStatement,
		"tenant_config.gc_trim_watermark_cache_capacity": watermarkCapacity,
	}
}

func init() {
	core.Register(core.Test{
		Name:       "features/amp_gc_deep_trim",
		Suite:      "features",
		Category:   core.Stress,
		Env:        core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:       []string{"regression"},
		OwningTeam: core.TeamCompute,
		Description: "GC trims a live block whose version chain is far deeper than one delete statement may name, both with the trim watermarks on and with them off (the state a restart or an eviction leaves a key in). " +
			"Each version is its own committed transaction, so the clog chain deepens with the heap one and amp_storage.slru_pages, trimmed by a statement of its own, is held to reclaiming too. " +
			"The stored versions drain instead of standing still, the drain reports the rows it deleted and the queue it shrank as it commits them, and afterwards bt_index_check and a forced heap scan hold the trimmed relation to reconstructing the page that was written, with an untouched table alongside keeping every row and page.",
		Params:  []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout: 50 * time.Minute,
		Run:     runAmpGCDeepTrim,
	})
}

func runAmpGCDeepTrim(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{
			{"deep_trim_watermarked", a.deepTrim(1, "deep-wm", deepTrimWatermarkDefault)},
			// Capacity 0: every key trims from Lsn(0) in its own statement, the
			// state a restart or an eviction leaves a key in.
			{"deep_trim_watermarkless", a.deepTrim(2, "deep-nowm", 0)},
		}
	})
}

// deepTrim runs one phase on its own cluster: the watermark capacity is read
// once at activation, and a branch takes one writer.
func (a *ampFeatureRun) deepTrim(round int, clusterName string, watermarkCapacity int) func(context.Context) error {
	return func(ctx context.Context) error {
		cl, err := a.f.controllerClient(ctx)
		if err != nil {
			return err
		}
		storage, err := a.f.ampStorageSQL(ctx)
		if err != nil {
			if errors.Is(err, errNoStorageRoute) {
				a.f.t.Skip(err.Error())
				return nil
			}
			return core.AsEnvFailure(fmt.Errorf("reach the backing amp storage database for the trim verdict: %w", err))
		}
		defer storage.Close()

		proj, err := createClusterOnPool(ctx, cl, a.proj.poolID, clusterName+"-"+a.f.suffix(), a.f.cfg.PgVersion, a.f.register)
		if err != nil {
			return fmt.Errorf("create the cluster for the %s phase: %w", clusterName, err)
		}
		return deepTrimSteps(ctx, cl, proj, round, watermarkCapacity, a.f.endpointSQL, a.f.endpointPageserver,
			storage, a.f.t.Metric, a.f.logf, a.f.t.Skip)
	}
}

func deepTrimSteps(ctx context.Context, cl ampCtl, proj ampCluster, round, watermarkCapacity int,
	connect endpointConnector, findPS pageserverFinder, storage *pg.Client,
	metric func(string, float64, string), logf func(string, ...any), skip func(string)) error {
	ep, err := createEndpointReq(ctx, cl, proj.poolID, proj.projID, proj.branchID, controller.CreateEndpointRequest{
		Role:               roleReadWrite,
		PageserverSettings: gcDeepTrimSettings(watermarkCapacity),
	})
	if err != nil {
		return fmt.Errorf("create the endpoint with the deep-trim GC settings: %w", err)
	}
	got, err := cl.GetEndpoint(ctx, proj.poolID, proj.projID, proj.branchID, ep.ID)
	if err != nil {
		return fmt.Errorf("read endpoint %s back: %w", ep.ID, err)
	}
	// A controller predating the settings API drops the fields and leaves GC off.
	if len(got.PageserverSettings) == 0 {
		skip("this controller does not persist pageserver_settings, so GC cannot be turned on or its statement cap lowered")
		return nil
	}
	c, err := connect(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect the deep-trim endpoint: %w", err)
	}
	defer c.Close()
	ps, err := findPS(ctx, ep)
	if err != nil {
		if skipUnreachablePageserver(err, skip) {
			return nil
		}
		return err
	}
	branchID, err := c.QueryString(ctx, "SELECT current_setting('yb_amp.branch_id')")
	if err != nil {
		return fmt.Errorf("read the endpoint's branch id: %w", err)
	}

	deep := fmt.Sprintf("gc_deep_%d", round)
	keeper := fmt.Sprintf("gc_deep_keeper_%d", round)
	if _, err := c.Exec(ctx, "CREATE TABLE "+keeper+" (id int primary key, payload text)"); err != nil {
		return fmt.Errorf("create %s: %w", keeper, err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO "+keeper+" SELECT g, md5(g::text) FROM generate_series(1, $1) g", deepTrimKeeperRows); err != nil {
		return fmt.Errorf("seed %s: %w", keeper, err)
	}
	keeperSum, err := c.QueryString(ctx, "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM "+keeper)
	if err != nil {
		return fmt.Errorf("checksum %s: %w", keeper, err)
	}
	// One row, so every update rewrites the same block.
	if _, err := c.Exec(ctx, "CREATE TABLE "+deep+" (id int primary key, payload text)"); err != nil {
		return fmt.Errorf("create %s: %w", deep, err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO "+deep+" (id, payload) VALUES (1, 'seed')"); err != nil {
		return fmt.Errorf("seed %s: %w", deep, err)
	}
	deepRel, err := c.QueryInt(ctx, "SELECT pg_relation_filenode($1)::bigint", deep)
	if err != nil {
		return fmt.Errorf("read %s relfilenode: %w", deep, err)
	}
	keeperRel, err := c.QueryInt(ctx, "SELECT pg_relation_filenode($1)::bigint", keeper)
	if err != nil {
		return fmt.Errorf("read %s relfilenode: %w", keeper, err)
	}

	// COMMIT inside the loop, so each version gets its own xid and the clog
	// chain deepens with the heap one; under one transaction slru_pages stays
	// shallow and its trim is never exercised.
	churn := deep + "_churn"
	if _, err := c.Exec(ctx, fmt.Sprintf(
		`CREATE PROCEDURE %s(rounds int, tag text) LANGUAGE plpgsql AS $proc$
		 BEGIN
		   FOR i IN 1..rounds LOOP
		     UPDATE %s SET payload = tag || '-' || i::text WHERE id = 1;
		     COMMIT;
		   END LOOP;
		 END $proc$`, churn, deep)); err != nil {
		return fmt.Errorf("create the churn procedure: %w", err)
	}
	var lastPayload string
	for batch := range deepTrimBatches {
		lastPayload = fmt.Sprintf("v%d-%d", round, batch)
		if _, err := c.Exec(ctx, fmt.Sprintf("CALL %s(%d, '%s')", churn, deepTrimPerBatch, lastPayload)); err != nil {
			return fmt.Errorf("deepen %s, batch %d: %w", deep, batch, err)
		}
		if _, err := c.Exec(ctx, "CHECKPOINT"); err != nil {
			return fmt.Errorf("checkpoint after batch %d: %w", batch, err)
		}
	}
	lastPayload = fmt.Sprintf("%s-%d", lastPayload, deepTrimPerBatch)
	if err := flushToFrontier(ctx, c, ps, branchID); err != nil {
		if skipUnreachablePageserver(err, skip) {
			return nil
		}
		return err
	}

	pagesOf := func(relnode int64) (int64, error) {
		return storage.QueryInt(ctx, "SELECT count(*) FROM amp_storage.pages WHERE branch_id = $1 AND relnode = $2", branchID, relnode)
	}
	// Keyed by branch alone: every SLRU segment here is history the churn wrote.
	slruRows := func() (int64, error) {
		return storage.QueryInt(ctx, "SELECT count(*) FROM amp_storage.slru_pages WHERE branch_id = $1", branchID)
	}
	depth, err := waitForDepth(ctx, pagesOf, deepRel, deepTrimMinDepth, deepTrimSeedTimeout)
	if err != nil {
		return core.AsEnvFailure(fmt.Errorf("%d update(s) of one row on branch %s did not leave a deep enough version chain in amp_storage to exercise the trim: %w",
			deepTrimBatches*deepTrimPerBatch, branchID, err))
	}
	slruBefore, err := slruRows()
	if err != nil {
		return fmt.Errorf("count the branch's stored SLRU versions: %w", err)
	}
	metric(fmt.Sprintf("gc_deep_trim.round_%d.versions_stored", round), float64(depth), "rows")
	metric(fmt.Sprintf("gc_deep_trim.round_%d.slru_versions_stored", round), float64(slruBefore), "rows")
	logf("amp_gc_deep_trim: branch %s holds %d stored version(s) of one block after %d update(s), watermark capacity %d, %d rows per delete statement",
		branchID, depth, deepTrimBatches*deepTrimPerBatch, watermarkCapacity, deepTrimRowsPerStatement)

	before, err := gcRowsDeleted(ctx, ps)
	if err != nil {
		if skipUnreachablePageserver(err, skip) {
			return nil
		}
		return err
	}
	dequeuedBefore, err := gcCounter(ctx, ps, gcCandidatesDequeuedMetric)
	if err != nil {
		return err
	}
	drainedAt := time.Now()
	remaining, forced, settled, err := drainDeepBlock(ctx, ps, branchID, pagesOf, deepRel, depth, logf)
	if err != nil {
		if skipUnreachablePageserver(err, skip) {
			return nil
		}
		return err
	}
	drainTook := time.Since(drainedAt).Round(time.Second)
	after, err := gcRowsDeleted(ctx, ps)
	if err != nil {
		return err
	}
	dequeuedAfter, err := gcCounter(ctx, ps, gcCandidatesDequeuedMetric)
	if err != nil {
		return err
	}
	slruAfter, err := slruRows()
	if err != nil {
		return fmt.Errorf("re-count the branch's stored SLRU versions: %w", err)
	}
	metric(fmt.Sprintf("gc_deep_trim.round_%d.versions_remaining", round), float64(remaining), "rows")
	metric(fmt.Sprintf("gc_deep_trim.round_%d.forced_runs", round), float64(forced), "runs")

	// Half, not a tight tail: the trim stops at the anchor image, and the bug
	// guarded against reclaims nothing at all. The exact tail goes to a metric.
	if remaining*2 > depth {
		return fmt.Errorf("branch %s's deep block still holds %d of its %d stored version(s) after %s of forced GC (%d runs, %.0f row(s) reported deleted, settled=%t, watermark capacity %d, %d rows per delete statement): the drain is not reclaiming this block",
			branchID, remaining, depth, drainTook, forced, after-before, settled, watermarkCapacity, deepTrimRowsPerStatement)
	}
	if !settled {
		logf("amp_gc_deep_trim: branch %s was still reclaiming at the %s budget (%d of %d version(s) left after %d forced run(s)); it made progress, so this is a budget note rather than a failure",
			branchID, deepTrimDrainTimeout, remaining, depth, forced)
	}

	// The clog half, trimmed by its own statement. Gated, because a clog block
	// with no image has no anchor to trim to.
	metric(fmt.Sprintf("gc_deep_trim.round_%d.slru_versions_remaining", round), float64(slruAfter), "rows")
	switch {
	case slruBefore < deepTrimMinSLRUDepth:
		logf("amp_gc_deep_trim: branch %s stored only %d SLRU version(s) for %d committed transactions, below the %d this phase needs to judge the slru_pages trim, so that half is not asserted",
			branchID, slruBefore, deepTrimBatches*deepTrimPerBatch, deepTrimMinSLRUDepth)
	case slruAfter >= slruBefore:
		return fmt.Errorf("branch %s's stored SLRU versions went from %d to %d while its heap block drained from %d to %d: the commit history the same %d transactions wrote is not being reclaimed. amp_storage.slru_pages is trimmed by a statement of its own, so it fails on its own",
			branchID, slruBefore, slruAfter, depth, remaining, deepTrimBatches*deepTrimPerBatch)
	default:
		logf("amp_gc_deep_trim: branch %s reclaimed %d of %d stored SLRU version(s)", branchID, slruBefore-slruAfter, slruBefore)
	}

	if dequeuedAfter <= dequeuedBefore {
		return fmt.Errorf("%s reads %.0f after a drain that deleted %.0f row(s): the dequeue is what shrinks pending_gc, so a queue that never shrinks grows without bound however much the trim deletes",
			gcCandidatesDequeuedMetric, dequeuedAfter, after-before)
	}
	if after <= before {
		return fmt.Errorf("%s reads %.0f after a drain that took branch %s's deep block from %d stored version(s) to %d: the deletes are charged as each statement commits, so a drain that reclaimed this much cannot leave the counter where it was",
			gcRowsDeletedMetric, after, branchID, depth, remaining)
	}
	metric(fmt.Sprintf("gc_deep_trim.round_%d.rows_deleted_reported", round), after-before, "rows")

	// Over a fresh connection, so no cached page can answer the read.
	fresh, err := connect(ctx, ep)
	if err != nil {
		return fmt.Errorf("reconnect after the drain: %w", err)
	}
	defer fresh.Close()
	payload, err := fresh.QueryString(ctx, "SELECT payload FROM "+deep+" WHERE id = 1")
	if err != nil {
		return fmt.Errorf("re-read the deep block's row after its trim: %w", err)
	}
	if payload != lastPayload {
		return fmt.Errorf("the deep block's row reads back %q after its version chain was trimmed, want %q: the trim removed a version the block's current value is reconstructed from", payload, lastPayload)
	}
	sum, err := fresh.QueryString(ctx, "SELECT md5(string_agg(payload, ',' ORDER BY id)) FROM "+keeper)
	if err != nil {
		return fmt.Errorf("re-checksum %s after the drain: %w", keeper, err)
	}
	if sum != keeperSum {
		return fmt.Errorf("the untouched table %s checksums %s after the deep block's trim, want %s: the trim reached a block it was not bounded to", keeper, sum, keeperSum)
	}
	keeperPages, err := pagesOf(keeperRel)
	if err != nil {
		return fmt.Errorf("count %s pages in amp_storage: %w", keeper, err)
	}
	if keeperPages == 0 {
		return fmt.Errorf("amp_storage holds no pages for the untouched table %s (relnode %d) after the deep block's trim: the trim deleted a live relation's storage", keeper, keeperRel)
	}
	return storageIntactAfterTrim(ctx, fresh, deep, keeper)
}

// storageIntactAfterTrim holds the trimmed relations to reconstructing what was
// written, which a row count cannot check.
func storageIntactAfterTrim(ctx context.Context, c sqlConn, deep, keeper string) error {
	if _, err := c.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS amcheck"); err != nil {
		return fmt.Errorf("create amcheck to verify the trimmed relations: %w", err)
	}
	for _, table := range []string{deep, keeper} {
		if _, err := c.Exec(ctx, "SELECT bt_index_check(($1 || '_pkey')::regclass, true)", table); err != nil {
			return fmt.Errorf("amcheck bt_index_check over %s after its storage was trimmed: the index and the heap disagree, so a page reconstructed from the surviving versions is not the page that was written: %w", table, err)
		}
	}
	// Qualified on payload, which carries no index, so the pkey cannot answer it
	// and the heap is read. A session GUC would not do this: Exec is pooled.
	rows, err := c.QueryInt(ctx, "SELECT count(*) FROM "+deep+" WHERE payload IS NOT NULL")
	if err != nil {
		return fmt.Errorf("heap read of %s after its storage was trimmed: %w", deep, err)
	}
	if rows != 1 {
		return fmt.Errorf("a heap read of %s returns %d row(s) after the trim, want the 1 that was written: the pages do not reconstruct to what the index says is there", deep, rows)
	}
	return nil
}

// drainDeepBlock forces GC until the version count stops falling.
func drainDeepBlock(ctx context.Context, ps pageserverAdmin, branchID string, pagesOf func(int64) (int64, error),
	relnode, depth int64, logf func(string, ...any)) (remaining int64, forced int, settled bool, err error) {
	deadline := time.Now().Add(deepTrimDrainTimeout)
	remaining = depth
	lastFall := time.Now()
	for {
		if _, err := ps.ForceGC(ctx, branchID, cutoffForceGCHorizonBytes); err != nil {
			return remaining, forced, false, err
		}
		forced++
		n, err := pagesOf(relnode)
		if err != nil {
			return remaining, forced, false, fmt.Errorf("count the deep block's stored versions: %w", err)
		}
		if n < remaining {
			lastFall = time.Now()
			remaining = n
		}
		// Nothing off for a while, so this is where the trim leaves the block.
		if time.Since(lastFall) > deepTrimSettle {
			logf("amp_gc_deep_trim: branch %s settled at %d stored version(s) after %d forced GC run(s)", branchID, remaining, forced)
			return remaining, forced, true, nil
		}
		if time.Now().After(deadline) {
			return remaining, forced, false, nil
		}
		select {
		case <-ctx.Done():
			return remaining, forced, false, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// waitForDepth polls until the chain is deep enough to be worth trimming.
func waitForDepth(ctx context.Context, pagesOf func(int64) (int64, error), relnode, want int64, timeout time.Duration) (int64, error) {
	deadline := time.Now().Add(timeout)
	var n int64
	for {
		var err error
		if n, err = pagesOf(relnode); err != nil {
			return 0, fmt.Errorf("count the deep block's stored versions: %w", err)
		}
		if n >= want {
			return n, nil
		}
		if time.Now().After(deadline) {
			return n, fmt.Errorf("amp_storage holds %d version(s) for relnode %d after %s, want at least %d", n, relnode, timeout, want)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// flushToFrontier makes ingest reach the frontier, which GC cannot reclaim past.
func flushToFrontier(ctx context.Context, c sqlConn, ps pageserverAdmin, branchID string) error {
	lsn, err := currentLSN(ctx, c)
	if err != nil {
		return err
	}
	return ps.FlushBranch(ctx, branchID, lsn)
}

func gcRowsDeleted(ctx context.Context, ps pageserverAdmin) (float64, error) {
	return gcCounter(ctx, ps, gcRowsDeletedMetric)
}

func gcCounter(ctx context.Context, ps pageserverAdmin, name string) (float64, error) {
	s, err := ps.Metrics(ctx)
	if err != nil {
		return 0, fmt.Errorf("scrape the pageserver's metrics: %w", err)
	}
	v, ok := s.Value(name, nil)
	if !ok {
		return 0, fmt.Errorf("the pageserver does not export %s", name)
	}
	return v, nil
}
