// GiST, SP-GiST, GIN and HNSW builds write their pages unlogged and log them in
// one pass at the end. Before meko#1700 another process evicting one of those
// pages dropped it: HNSW and GIN PANICked the compute, GiST lost the page. The
// integration lane covers the core opclasses on a bare compute; this runs the
// pgvector HNSW build, which only exists as a catalog extension, and a parallel
// GIN build on a deployed endpoint.
package features

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	ibeRows      = 40000
	ibeChurnRows = 20000
	ibeSamples   = 20
	// HNSW is approximate; an intact graph finds a row's own vector at
	// ef_search 200 nearly always, a graph with dropped pages does not.
	ibeMinSelfHits   = 18
	ibeBuildBudget   = 5 * time.Minute
	ibeBuildAttempts = 5
)

type ibeIndex struct {
	name, ddl string
	workers   int
}

var ibeIndexes = []ibeIndex{
	{"ibe_hnsw", "CREATE INDEX ibe_hnsw ON ibe_items USING hnsw (v vector_l2_ops)", 0},
	{"ibe_gin_par", "CREATE INDEX ibe_gin_par ON ibe_items USING gin (tags)", 2},
	{"ibe_gist", "CREATE INDEX ibe_gist ON ibe_items USING gist (b)", 0},
}

func init() {
	core.Register(core.Test{
		Name:        "features/pg_index_build_eviction",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "Index builds whose pages are unlogged until the build ends, under eviction by other processes (meko#1700). On an endpoint with a 2MB buffer pool and the LFC off, build a pgvector HNSW index, a GIN index with two parallel workers and a GiST index while a CHECKPOINT loop and random reads on a churn table evict buffers. The compute must not restart, the unlogged-build gauge must return to zero, a cancelled CREATE INDEX CONCURRENTLY must leave its entry until the invalid index is dropped, and each index must answer like a seqscan (HNSW: a row's own vector is its nearest neighbour), both right after the builds and after a compute restart that rebuilds every page from the pageserver.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     50 * time.Minute,
		Run:         runPgIndexBuildEviction,
	})
}

type ibeRun struct {
	a    *ampFeatureRun
	ep   controller.Endpoint
	conn sqlConn
}

func runPgIndexBuildEviction(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		r := &ibeRun{a: a}
		return []phase{
			{"endpoint_with_tiny_buffer_pool", r.endpoint},
			{"build_indexes_under_eviction", r.build},
			{"cancelled_concurrent_build", r.cancelledCIC},
			{"indexes_answer_like_seqscan", func(ctx context.Context) error { return r.check(ctx, r.conn, "after the builds") }},
			{"indexes_after_compute_restart", r.afterRestart},
		}
	})
}

func (r *ibeRun) endpoint(ctx context.Context) error {
	cl, err := r.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	proj := r.a.proj
	name := "ibe-" + r.a.f.suffix()
	_, err = cl.CreateClusterAndWait(ctx, proj.poolID, controller.CreateClusterRequest{Name: name, PgVersion: r.a.f.cfg.PgVersion, Extensions: []string{"pgvector"}})
	if extensionsUnavailable(err) || unknownExtensionName(err) != "" {
		r.a.f.t.Skip("this control plane cannot opt a branch into pgvector: " + err.Error())
		return nil
	}
	if err != nil {
		return fmt.Errorf("create a pgvector cluster: %w", err)
	}
	c, err := findClusterCtl(ctx, cl, proj.poolID, name)
	if err != nil {
		return err
	}
	r.a.f.register("delete pgvector cluster", func(ctx context.Context) error {
		_, e := cl.DeleteClusterAndWait(ctx, proj.poolID, c.ID)
		return e
	})
	r.ep, err = createEndpointReq(ctx, cl, proj.poolID, c.ID, c.DefaultBranch.BranchID, controller.CreateEndpointRequest{
		Role:       roleReadWrite,
		PgSettings: map[string]string{"shared_buffers": "2MB", "yb_amp.file_cache_size_limit": "0"},
	})
	if err != nil {
		return fmt.Errorf("create the endpoint with a 2MB buffer pool: %w", err)
	}
	if r.conn, err = r.a.f.endpointSQLWith(ctx, r.ep, func(ci *pg.ConnInfo) { ci.MaxConns = 8 }); err != nil {
		return err
	}
	r.a.f.register("close the index-build connection", func(context.Context) error { r.conn.Close(); return nil })
	sb, err := r.conn.QueryString(ctx, "SHOW shared_buffers")
	if err != nil {
		return err
	}
	if sb != "2MB" {
		return fmt.Errorf("the endpoint created with pg_settings shared_buffers=2MB runs with %s: the setting was stored but not applied, and index pages may never be evicted", sb)
	}
	for _, stmt := range []string{
		"CREATE EXTENSION IF NOT EXISTS vector",
		"CREATE EXTENSION IF NOT EXISTS yb_amp",
		"CREATE TABLE ibe_items (id int PRIMARY KEY, v vector(32), tags int[], b box) WITH (autovacuum_enabled = off, parallel_workers = 2)",
		fmt.Sprintf(`INSERT INTO ibe_items
			SELECT g, (SELECT array_agg(sin(g * 7 + d) * 100)::vector(32) FROM generate_series(1, 32) d),
			       ARRAY[g %% 997, g %% 991, g %% 983], box(point(g %% 1000, g / 1000), point(g %% 1000 + 3, g / 1000 + 3))
			FROM generate_series(1, %d) g`, ibeRows),
		"CREATE TABLE ibe_churn (id int PRIMARY KEY, pad text) WITH (autovacuum_enabled = off)",
		fmt.Sprintf("INSERT INTO ibe_churn SELECT g, repeat('x', 900) FROM generate_series(1, %d) g", ibeChurnRows),
	} {
		if _, err := r.conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("seed: %s: %w", truncateSQL(stmt), err)
		}
	}
	return nil
}

func (r *ibeRun) postmasterStart(ctx context.Context) (string, error) {
	return r.conn.QueryString(ctx, "SELECT pg_postmaster_start_time()::text")
}

func (r *ibeRun) unloggedBuilds(ctx context.Context) (int64, error) {
	return r.conn.QueryInt(ctx, "SELECT value::bigint FROM yb_amp_perf_counters WHERE metric = 'unlogged_builds'")
}

// underEviction runs sql on its own session while a CHECKPOINT loop and random
// churn reads evict buffers from other backends. It returns how many
// checkpoints completed during it.
func (r *ibeRun) underEviction(ctx context.Context, setup []string, sql string) (int64, error) {
	p, ok := r.conn.(pinner)
	if !ok {
		return 0, fmt.Errorf("the endpoint connection cannot pin a backend")
	}
	builder, err := p.Pin(ctx)
	if err != nil {
		return 0, err
	}
	defer builder.Close()
	for _, s := range setup {
		if _, err := builder.Exec(ctx, s); err != nil {
			return 0, fmt.Errorf("%s: %w", s, err)
		}
	}
	bctx, stop := context.WithCancel(ctx)
	defer stop()
	var (
		checkpoints atomic.Int64
		wg          sync.WaitGroup
		mu          sync.Mutex
		churnErr    error
	)
	loop := func(stmt string, pause time.Duration, count bool) {
		defer wg.Done()
		s, err := p.Pin(bctx)
		if err != nil {
			if bctx.Err() != nil {
				return
			}
			mu.Lock()
			churnErr = errors.Join(churnErr, err)
			mu.Unlock()
			return
		}
		defer s.Close()
		for bctx.Err() == nil {
			if _, err := s.Exec(bctx, stmt); err != nil {
				if bctx.Err() == nil {
					mu.Lock()
					churnErr = errors.Join(churnErr, fmt.Errorf("%s: %w", truncateSQL(stmt), err))
					mu.Unlock()
				}
				return
			}
			if count {
				checkpoints.Add(1)
			}
			sleepFor(bctx, pause)
		}
	}
	wg.Add(2)
	go loop("CHECKPOINT", 100*time.Millisecond, true)
	go loop(fmt.Sprintf("DO $$ DECLARE k int; BEGIN FOR i IN 1..200 LOOP k := 1 + (random() * %d)::int; PERFORM pad FROM ibe_churn WHERE id = k; END LOOP; END $$", ibeChurnRows-1), 0, false)
	_, buildErr := builder.Exec(ctx, sql)
	stop()
	wg.Wait()
	if buildErr != nil {
		return checkpoints.Load(), buildErr
	}
	if churnErr != nil {
		return checkpoints.Load(), fmt.Errorf("an evicting session failed during the build: %w", churnErr)
	}
	return checkpoints.Load(), nil
}

func (r *ibeRun) build(ctx context.Context) error {
	if r.conn == nil {
		return nil
	}
	started, err := r.postmasterStart(ctx)
	if err != nil {
		return err
	}
	for _, ix := range ibeIndexes {
		if err := r.buildOne(ctx, ix, started); err != nil {
			return err
		}
	}
	n, err := r.unloggedBuilds(ctx)
	if err != nil {
		return fmt.Errorf("read the unlogged_builds gauge: %w", err)
	}
	if n != 0 {
		return fmt.Errorf("%d unlogged builds are still registered after every build ended", n)
	}
	return nil
}

func (r *ibeRun) buildOne(ctx context.Context, ix ibeIndex, started string) error {
	bctx, cancel := context.WithTimeout(ctx, ibeBuildBudget)
	defer cancel()
	setup := []string{fmt.Sprintf("SET max_parallel_maintenance_workers = %d", ix.workers), "SET maintenance_work_mem = '64MB'"}
	var n int64
	var err error
	// The parallel GIN build can finish inside one CHECKPOINT round trip.
	for attempt := 1; ; attempt++ {
		n, err = r.underEviction(bctx, setup, ix.ddl)
		if err != nil {
			if now, perr := r.postmasterStart(ctx); perr != nil || now != started {
				return fmt.Errorf("building %s under eviction: %w; the compute restarted (postmaster start %q, now %q, %v): an evicted unlogged-build page crashed it (meko#1700)", ix.name, err, started, now, perr)
			}
			return fmt.Errorf("building %s under eviction: %w", ix.name, err)
		}
		r.a.f.logf("%s built with %d checkpoints running meanwhile (attempt %d)", ix.name, n, attempt)
		if n > 0 || attempt == ibeBuildAttempts {
			break
		}
		if _, err := r.conn.Exec(ctx, "DROP INDEX "+ix.name); err != nil {
			return fmt.Errorf("drop %s to rebuild it under a checkpoint: %w", ix.name, err)
		}
	}
	if n == 0 {
		return fmt.Errorf("no CHECKPOINT completed during any of %d builds of %s, so nothing evicted its pages and the build proves nothing", ibeBuildAttempts, ix.name)
	}
	if now, err := r.postmasterStart(ctx); err != nil || now != started {
		return fmt.Errorf("the compute restarted during the %s build (postmaster start %q, now %q, %v)", ix.name, started, now, err)
	}
	return nil
}

// cancelledCIC cancels a CREATE INDEX CONCURRENTLY mid-build. Its invalid index
// keeps half-built pages in shared buffers, so its entry must outlive the abort
// and go only with DROP INDEX.
func (r *ibeRun) cancelledCIC(ctx context.Context) error {
	if r.conn == nil {
		return nil
	}
	p := r.conn.(pinner)
	s, err := p.Pin(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	pid, err := s.QueryInt(ctx, "SELECT pg_backend_pid()")
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Exec(ctx, "CREATE INDEX CONCURRENTLY ibe_cic ON ibe_items USING gist (b)")
		done <- err
	}()
	var cancelled bool
	err = pollUntil(ctx, 3*time.Minute, 50*time.Millisecond, func() (bool, error) {
		select {
		case err := <-done:
			return false, fmt.Errorf("the concurrent build ended (%v) before it could be cancelled mid-build", err)
		default:
		}
		n, err := r.conn.QueryInt(ctx, "SELECT count(*) FROM pg_stat_progress_create_index WHERE pid = $1 AND phase LIKE 'building index%' AND blocks_done > 0", pid)
		if err != nil || n == 0 {
			return false, err
		}
		_, err = r.conn.QueryString(ctx, "SELECT pg_cancel_backend($1::int)::text", pid)
		cancelled = err == nil
		return cancelled, err
	})
	if err != nil {
		return fmt.Errorf("cancel the concurrent GiST build: %w", err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "57014") && !strings.Contains(err.Error(), "canceling") {
		return fmt.Errorf("the cancelled concurrent build ended with %v, want a query-cancel error", err)
	}
	if n, err := r.unloggedBuilds(ctx); err != nil || n != 1 {
		return fmt.Errorf("after cancelling the concurrent build the unlogged_builds gauge reads %d (%v), want 1 for the invalid index still holding unlogged pages", n, err)
	}
	if _, err := r.conn.Exec(ctx, "CHECKPOINT"); err != nil {
		return fmt.Errorf("checkpoint with the invalid index's pages in shared buffers: %w", err)
	}
	if _, err := r.conn.Exec(ctx, "DROP INDEX ibe_cic"); err != nil {
		return err
	}
	if n, err := r.unloggedBuilds(ctx); err != nil || n != 0 {
		return fmt.Errorf("after DROP INDEX of the invalid index the unlogged_builds gauge reads %d (%v), want 0", n, err)
	}
	return nil
}

func (r *ibeRun) check(ctx context.Context, c sqlConn, when string) error {
	if c == nil {
		return nil
	}
	p, ok := c.(pinner)
	if !ok {
		return fmt.Errorf("the endpoint connection cannot pin a backend")
	}
	s, err := p.Pin(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	for _, pred := range []string{"tags @> ARRAY[5]", "b && box '((100,10),(300,30))'"} {
		var counts [2]int64
		for i, scan := range []string{"on", "off"} {
			for _, set := range []string{"SET enable_seqscan = " + scan, "SET enable_bitmapscan = " + flip(scan), "SET enable_indexscan = " + flip(scan)} {
				if _, err := s.Exec(ctx, set); err != nil {
					return err
				}
			}
			if counts[i], err = s.QueryInt(ctx, "SELECT count(*) FROM ibe_items WHERE "+pred); err != nil {
				return fmt.Errorf("%s: count %s with seqscan %s: %w", when, pred, scan, err)
			}
		}
		if counts[0] != counts[1] || counts[0] == 0 {
			return fmt.Errorf("%s: %s counts %d by seqscan and %d by index", when, pred, counts[0], counts[1])
		}
	}
	for _, set := range []string{"SET enable_seqscan = off", "SET enable_indexscan = on", "SET hnsw.ef_search = 200"} {
		if _, err := s.Exec(ctx, set); err != nil {
			return err
		}
	}
	hits := 0
	var misses []string
	for i := range ibeSamples {
		id := 1 + i*(ibeRows/ibeSamples)
		got, err := s.QueryInt(ctx, "SELECT id FROM ibe_items ORDER BY v <-> (SELECT v FROM ibe_items WHERE id = $1) LIMIT 1", id)
		if err != nil {
			return fmt.Errorf("%s: nearest neighbour of row %d through the HNSW index: %w", when, id, err)
		}
		if got == int64(id) {
			hits++
		} else {
			misses = append(misses, fmt.Sprintf("%d->%d", id, got))
		}
	}
	if hits < ibeMinSelfHits {
		return fmt.Errorf("%s: the HNSW index found a row's own vector as its nearest neighbour for %d of %d rows, want at least %d (misses %s): the graph is missing pages", when, hits, ibeSamples, ibeMinSelfHits, strings.Join(misses, ", "))
	}
	if amcheck, _ := s.QueryInt(ctx, "SELECT count(*) FROM pg_available_extensions WHERE name = 'amcheck'"); amcheck > 0 {
		if _, err := s.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS amcheck"); err != nil {
			return fmt.Errorf("%s: CREATE EXTENSION amcheck: %w", when, err)
		}
		if has, _ := s.QueryInt(ctx, "SELECT count(*) FROM pg_proc WHERE proname = 'gin_index_check'"); has > 0 {
			if _, err := s.QueryString(ctx, "SELECT gin_index_check('ibe_gin_par')::text"); err != nil {
				return fmt.Errorf("%s: amcheck finds the parallel-built GIN index corrupt: %w", when, err)
			}
		}
	}
	return nil
}

func flip(onOff string) string {
	if onOff == "on" {
		return "off"
	}
	return "on"
}

func (r *ibeRun) afterRestart(ctx context.Context) error {
	if r.conn == nil {
		return nil
	}
	r.conn.Close()
	cold, ok, err := r.a.restartEndpointCompute(ctx, r.ep)
	if err != nil || !ok {
		return err
	}
	defer cold.Close()
	return r.check(ctx, cold, "after a compute restart")
}
