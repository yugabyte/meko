package features

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/workloadgen"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/releases"
)

const (
	DefaultFormatCompatTrainsBack   = 2
	DefaultFormatCompatRows         = 2000
	DefaultFormatCompatToastRows    = 50
	DefaultFormatCompatChainUpdates = 2000
	DefaultFormatCompatGCTimeout    = 8 * time.Minute

	// About 12 KiB a row, well past the 2 KiB the tuple toaster acts on.
	fcToastChunks = 400
	fcChainRows   = 8
	fcGistRows    = 1000
	fcGinRows     = 1000
	fcGinGroups   = 7
	fcVictimRows  = 2000
	fcSeedTimeout = 3 * time.Minute
	fcRollBudget  = 20 * time.Minute
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_format_compat",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "On-disk format compatibility across two releases: deploy the control plane at the release two trains back (the previous one when the portal has no older, recorded on the result), load a seeded workloadgen dataset, TOAST rows, a GIN and a GiST index, a fork taken at the flushed LSN and a long unflushed delta chain, then upgrade the control plane and roll the endpoints onto the current image. The dataset must verify by checksum, the TOAST bodies must read back byte for byte, both index scans must still be planned and answered from the old indexes, bt_index_check must pass on every btree, the fork must read back what the parent held at the fork point and nothing written after it, the delta chain must materialise after a forced GC, and GC must reclaim a table the old release dropped.",
		Params:      []string{"functional", "functional.upgrade", "functional.format_compat", "report.portal", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     75 * time.Minute,
		Run:         runAmpFormatCompat,
	})
}

type formatCompatRun struct {
	u *upgradeRun
	f *flow

	rows         int
	toastRows    int
	chainUpdates int
	gcTimeout    time.Duration

	spec     workloadgen.Spec
	seed     int64
	proj     ampCluster
	ep       controller.Endpoint
	branchID string

	forkName     string
	forkBranchID string
	forkLSN      string

	victimRel   int64
	victimPages int64
}

func runAmpFormatCompat(t *core.T) error {
	acfg := ampstack.FromConfig(t.Options)
	c := &formatCompatRun{
		rows:         t.Options.GetInt("functional.format_compat.rows", DefaultFormatCompatRows),
		toastRows:    t.Options.GetInt("functional.format_compat.toast_rows", DefaultFormatCompatToastRows),
		chainUpdates: t.Options.GetInt("functional.format_compat.chain_updates", DefaultFormatCompatChainUpdates),
		gcTimeout:    t.Options.GetDuration("functional.format_compat.gc_timeout", DefaultFormatCompatGCTimeout),
		seed:         t.Seed.Fork("format-compat").Value(),
	}
	if c.rows < 1 || c.toastRows < 1 || c.chainUpdates < fcChainRows {
		return fmt.Errorf("functional.format_compat is rows=%d toast_rows=%d chain_updates=%d: each has to write something for the post-upgrade reads to mean anything (chain_updates at least %d)", c.rows, c.toastRows, c.chainUpdates, fcChainRows)
	}
	u := &upgradeRun{
		toController: acfg.ControllerImage,
		toEndpoint:   acfg.EndpointImage,
		rows:         c.rows,
		portalURL:    upgradePortalURL(t.Options),
		trainsBack:   t.Options.GetInt("functional.format_compat.trains_back", DefaultFormatCompatTrainsBack),
	}
	if from := strings.TrimSpace(t.Options.GetString("functional.format_compat.from_image", "")); from != "" {
		tag := imageTag(from)
		u.fromController = releases.ControllerRepo + ":" + tag
		u.fromEndpoint = releases.EndpointRepo + ":" + tag
	}
	ok, err := u.setup(t)
	if err != nil || !ok {
		return err
	}
	step := u.stepper()
	if err := u.resolveStep(); err != nil {
		return err
	}
	c.u, c.f = u, u.f
	f := u.f
	phases := []phase{
		{"provision_backing", f.provisionBacking},
		{"wait_backing_ready", f.waitBackingReady},
		{"deploy_amp_from", u.deployAmpFrom},
		{"load_old_format", c.loadOldFormat},
		{"fork_old_format", c.forkOldFormat},
		{"upgrade_control_plane", u.upgradeControlPlane},
		{"roll_endpoints_to_current", c.rollFleet},
		{"verify_dataset", c.verifyDataset},
		{"verify_indexes", c.verifyIndexes},
		{"verify_fork", c.verifyFork},
		{"materialise_delta_chain", c.verifyChain},
		{"gc_reclaims_dropped_table", c.verifyGC},
	}
	return runLifecycle(t.Context, step, f.td, phases, f.logf, f.collectDiagnostics, f.onTeardownDone)
}

func (c *formatCompatRun) set(key string, val any) { c.u.set(key, val) }

func (c *formatCompatRun) baseConn(ctx context.Context) (sqlConn, *pgxpool.Pool, error) {
	ep := c.ep
	if cl, err := c.f.controllerClient(ctx); err == nil {
		if got, gerr := cl.GetEndpoint(ctx, c.proj.poolID, c.proj.projID, c.proj.branchID, c.ep.ID); gerr == nil {
			ep = got
		}
	}
	conn, err := c.f.endpointSQL(ctx, ep)
	if err != nil {
		return nil, nil, fmt.Errorf("connect the endpoint holding the old-format data: %w", err)
	}
	pool := poolOf(conn)
	if pool == nil {
		conn.Close()
		return nil, nil, fmt.Errorf("the connection to endpoint %s carries no pgx pool, and the format checks read rows", c.ep.ID)
	}
	return conn, pool, nil
}

func (c *formatCompatRun) loadOldFormat(ctx context.Context) error {
	f := c.f
	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	proj, err := setupAmpCluster(ctx, cl, "fc-"+f.suffix(), f.cfg.PgVersion, f.register)
	if err != nil {
		return err
	}
	c.proj = proj
	ep, err := createEndpointReq(ctx, cl, proj.poolID, proj.projID, proj.branchID, controller.CreateEndpointRequest{
		Role:               roleReadWrite,
		PageserverSettings: map[string]any{"tenant_config.enable_gc": true},
	})
	if err != nil {
		return fmt.Errorf("create the endpoint the old release writes through: %w", err)
	}
	c.ep = ep
	conn, pool, err := c.baseConn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	c.spec = workloadgen.Spec{TablePrefix: "fc_wg", NumTables: 2, RowsPerTable: c.rows}
	if err := workloadgen.Schema(ctx, pool, c.spec); err != nil {
		return fmt.Errorf("create the workloadgen tables on the old release: %w", err)
	}
	if err := workloadgen.Load(ctx, pool, c.spec, c.seed, nil); err != nil {
		return fmt.Errorf("load the workloadgen dataset on the old release: %w", err)
	}

	stmts := []string{
		"CREATE TABLE fc_toast (id int primary key, body text)",
		// EXTERNAL, or pglz packs the repeated payload back under the threshold.
		"ALTER TABLE fc_toast ALTER COLUMN body SET STORAGE EXTERNAL",
		"CREATE TABLE fc_gin (id int primary key, doc jsonb)",
		"CREATE TABLE fc_gist (id int primary key, p point)",
		"CREATE TABLE fc_chain (id int primary key, n bigint)",
		"CREATE TABLE fc_fork (id int primary key, tag text)",
		"CREATE TABLE fc_victim (id int primary key, payload text)",
	}
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			return fmt.Errorf("%s on the old release: %w", s, err)
		}
	}
	if _, err := conn.Exec(ctx, "INSERT INTO fc_toast SELECT g, repeat(md5(g::text), $1) FROM generate_series(1, $2) g", fcToastChunks, c.toastRows); err != nil {
		return fmt.Errorf("write the TOAST rows on the old release: %w", err)
	}
	toastRel, err := conn.QueryString(ctx, "SELECT CASE WHEN reltoastrelid = 0 THEN '' ELSE reltoastrelid::regclass::text END FROM pg_class WHERE oid = 'fc_toast'::regclass")
	if err != nil || strings.TrimSpace(toastRel) == "" {
		return fmt.Errorf("resolve fc_toast's TOAST relation (got %q): %v", toastRel, err)
	}
	chunks, err := conn.QueryInt(ctx, "SELECT count(*) FROM "+toastRel)
	if err != nil {
		return fmt.Errorf("count the TOAST chunks of fc_toast: %w", err)
	}
	if chunks == 0 {
		return fmt.Errorf("fc_toast holds %d rows of about %d KiB and its TOAST relation %s is empty, so the post-upgrade read would cross no TOAST pointer", c.toastRows, fcToastChunks*32/1024, toastRel)
	}
	c.set("toast_chunks", chunks)
	if _, err := conn.Exec(ctx, "INSERT INTO fc_gin SELECT g, jsonb_build_object('k', g, 'grp', g % $1) FROM generate_series(1, $2) g", fcGinGroups, fcGinRows); err != nil {
		return fmt.Errorf("write the GIN rows on the old release: %w", err)
	}
	if _, err := conn.Exec(ctx, "CREATE INDEX fc_gin_idx ON fc_gin USING gin (doc jsonb_path_ops)"); err != nil {
		return fmt.Errorf("build the GIN index on the old release: %w", err)
	}
	if _, err := conn.Exec(ctx, "INSERT INTO fc_gist SELECT g, point(g % 100, g / 100) FROM generate_series(1, $1) g", fcGistRows); err != nil {
		return fmt.Errorf("write the GiST rows on the old release: %w", err)
	}
	if _, err := conn.Exec(ctx, "CREATE INDEX fc_gist_idx ON fc_gist USING gist (p)"); err != nil {
		return fmt.Errorf("build the GiST index on the old release: %w", err)
	}

	if _, err := conn.Exec(ctx, "INSERT INTO fc_chain SELECT g, 0 FROM generate_series(1, $1) g", fcChainRows); err != nil {
		return fmt.Errorf("seed fc_chain on the old release: %w", err)
	}
	for i := 0; i < c.chainUpdates; i++ {
		if _, err := conn.Exec(ctx, "UPDATE fc_chain SET n = n + 1 WHERE id = $1", i%fcChainRows+1); err != nil {
			return fmt.Errorf("update %d of the delta chain on the old release: %w", i, err)
		}
	}
	c.set("chain_updates", c.chainUpdates)

	c.branchID, err = conn.QueryString(ctx, "SELECT current_setting('yb_amp.branch_id')")
	if err != nil {
		return fmt.Errorf("read the endpoint's branch id: %w", err)
	}
	return c.dropVictim(ctx, conn)
}

func (c *formatCompatRun) dropVictim(ctx context.Context, conn sqlConn) error {
	if _, err := conn.Exec(ctx, "INSERT INTO fc_victim SELECT g, md5(g::text) FROM generate_series(1, $1) g", fcVictimRows); err != nil {
		return fmt.Errorf("seed fc_victim on the old release: %w", err)
	}
	rel, err := conn.QueryInt(ctx, "SELECT pg_relation_filenode('fc_victim')::bigint")
	if err != nil {
		return fmt.Errorf("read fc_victim's relfilenode: %w", err)
	}
	c.victimRel = rel
	storage, err := c.f.ampStorageSQL(ctx)
	if err != nil {
		if errors.Is(err, errNoStorageRoute) {
			c.f.logf("amp_format_compat: no route to the backing amp storage database, so the GC of the dropped table is not asserted: %v", err)
			c.set("gc_leg", "no storage route: "+err.Error())
			return nil
		}
		return core.AsEnvFailure(err)
	}
	defer storage.Close()
	deadline := time.Now().Add(fcSeedTimeout)
	for {
		if _, err := conn.Exec(ctx, "CHECKPOINT"); err != nil {
			return fmt.Errorf("checkpoint after seeding fc_victim: %w", err)
		}
		pages, err := c.victimPageCount(ctx, storage)
		if err != nil {
			return err
		}
		if pages > 0 {
			c.victimPages = pages
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("amp_storage holds no page for fc_victim (branch %s relnode %d) within %s of writing it, so a later reclaim would prove nothing", c.branchID, c.victimRel, fcSeedTimeout)
		}
		if err := waitFor(ctx, 2*time.Second); err != nil {
			return err
		}
	}
	c.set("victim_pages_before_drop", c.victimPages)
	if _, err := conn.Exec(ctx, "DROP TABLE fc_victim"); err != nil {
		return fmt.Errorf("drop fc_victim on the old release: %w", err)
	}
	return nil
}

func (c *formatCompatRun) victimPageCount(ctx context.Context, storage *pg.Client) (int64, error) {
	n, err := storage.QueryInt(ctx, "SELECT count(*) FROM amp_storage.pages WHERE branch_id = $1 AND relnode = $2", c.branchID, c.victimRel)
	if err != nil {
		return 0, fmt.Errorf("count fc_victim's pages in amp_storage: %w", err)
	}
	return n, nil
}

// The fork point is the parent's DURABLE position, so the flush is forced first.
func (c *formatCompatRun) forkOldFormat(ctx context.Context) error {
	f := c.f
	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	conn, _, err := c.baseConn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.Exec(ctx, "INSERT INTO fc_fork VALUES (1, 'pre-fork')"); err != nil {
		return fmt.Errorf("write the pre-fork marker: %w", err)
	}
	lsn, err := conn.QueryString(ctx, "SELECT pg_current_wal_flush_lsn()::text")
	if err != nil {
		return fmt.Errorf("read the flushed WAL position to fork at: %w", err)
	}
	ps, err := f.endpointPageserver(ctx, c.ep)
	if err != nil {
		if skipUnreachablePageserver(err, f.t.Skip) {
			return nil
		}
		return err
	}
	if err := ps.FlushBranch(ctx, c.branchID, lsn); err != nil {
		return core.AsEnvFailure(fmt.Errorf("force the pageserver to flush branch %s to %s before the fork: %w", c.branchID, lsn, err))
	}
	c.forkLSN = lsn
	c.forkName = "fc-fork-" + f.suffix()
	if _, err := cl.CreateBranchAndWait(ctx, c.proj.poolID, c.proj.projID, controller.CreateBranchRequest{
		Name:           c.forkName,
		ParentBranchID: c.proj.branchID,
		ForkPointLSN:   lsn,
	}); err != nil {
		return fmt.Errorf("fork %s off the old-format branch at %s: %w", c.forkName, lsn, err)
	}
	b, err := findBranchCtl(ctx, cl, c.proj.poolID, c.proj.projID, c.forkName)
	if err != nil {
		return err
	}
	c.forkBranchID = b.BranchID
	c.set("fork_branch_id", c.forkBranchID)
	c.set("fork_lsn", lsn)
	if _, err := conn.Exec(ctx, "INSERT INTO fc_fork VALUES (2, 'post-fork')"); err != nil {
		return fmt.Errorf("write the post-fork marker: %w", err)
	}
	return nil
}

func (c *formatCompatRun) rollFleet(ctx context.Context) error {
	f := c.f
	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	target := imageTag(c.u.toEndpoint)
	task, loc, err := cl.UpgradeEndpoints(ctx, controller.UpgradeEndpointsRequest{Version: target})
	if status := apiErrorStatus(err); status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented {
		return fmt.Errorf("the upgraded controller has no usable POST /endpoint-upgrades (%v), so the endpoints stay on the old image and the format checks would read the old release's own data", err)
	}
	if err != nil {
		return fmt.Errorf("plan the fleet roll onto endpoint version %q: %w", target, err)
	}
	taskID := controller.TaskRef(task, loc)
	rctx, cancel := context.WithTimeout(ctx, fcRollBudget)
	done, err := cl.WaitTask(rctx, taskID)
	cancel()
	if err != nil {
		subs, _ := cl.ListSubtasks(ctx, taskID)
		return fmt.Errorf("the fleet roll %s onto %q failed: %w (subtasks: %s)", taskID, target, err, describeSubtasks(subs.Subtasks))
	}
	report, ok := done.RollReport()
	if !ok {
		return fmt.Errorf("the succeeded roll %s recorded no report (%s)", taskID, string(done.Result))
	}
	c.set("roll_report", fmt.Sprintf("upgraded=%d skipped=%d failed=%d unattempted=%d", report.Upgraded, report.Skipped, report.Failed, report.Unattempted))
	for _, line := range report.Endpoints {
		if line.EndpointID == c.ep.ID && line.Outcome != controller.RollUpgraded {
			return fmt.Errorf("the endpoint holding the old-format data ended the roll %q (%s), want upgraded", line.Outcome, line.Reason)
		}
	}
	if kube := f.ampKube(); kube != nil {
		if err := ampstack.WaitEndpointWorkloadReady(ctx, kube, c.ep.ID, upgradePodReadTimeout); err != nil {
			return fmt.Errorf("endpoint %s compute is not ready after the roll: %w", c.ep.ID, err)
		}
		_, images, err := endpointPodImages(ctx, kube, c.ep.ID)
		if err != nil {
			return fmt.Errorf("read the rolled endpoint's pod: %w", err)
		}
		c.set("endpoint_image_after_roll", images)
		if !imageMatches(images, c.u.toEndpoint) {
			return fmt.Errorf("endpoint %s runs %q after the roll, want the current endpoint image %q", c.ep.ID, images, c.u.toEndpoint)
		}
	}
	return nil
}

func (c *formatCompatRun) verifyDataset(ctx context.Context) error {
	conn, pool, err := c.baseConn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := verifyFormatDataset(ctx, pool, c.spec, c.seed, "the endpoint rolled onto the current image"); err != nil {
		return err
	}
	bad, err := conn.QueryInt(ctx, "SELECT count(*) FROM fc_toast WHERE body <> repeat(md5(id::text), $1)", fcToastChunks)
	if err != nil {
		return fmt.Errorf("read the TOAST rows back: %w", err)
	}
	if bad != 0 {
		return fmt.Errorf("%d of %d fc_toast bodies written by the old release no longer read back byte for byte on the current one, so a TOAST chunk did not survive the upgrade", bad, c.toastRows)
	}
	n, err := conn.QueryInt(ctx, "SELECT count(*) FROM fc_toast")
	if err != nil {
		return fmt.Errorf("count fc_toast: %w", err)
	}
	if n != int64(c.toastRows) {
		return fmt.Errorf("fc_toast serves %d rows after the upgrade, want the %d the old release wrote", n, c.toastRows)
	}
	return nil
}

func verifyFormatDataset(ctx context.Context, pool *pgxpool.Pool, spec workloadgen.Spec, seed int64, where string) error {
	report, err := workloadgen.Verify(ctx, pool, spec, seed)
	if err != nil {
		return fmt.Errorf("verify the workloadgen dataset on %s: %w", where, err)
	}
	if report.OK {
		return nil
	}
	for _, tr := range report.Tables {
		if !tr.OK {
			return fmt.Errorf("%s serves %s with count=%d checksum=%s, want count=%d checksum=%s: the dataset the older release wrote does not read back intact",
				where, tr.Table, tr.ActualCount, tr.ActualChecksum, tr.ExpectedCount, tr.ExpectedChecksum)
		}
	}
	return fmt.Errorf("the workloadgen report on %s is not OK but names no failing table", where)
}

func (c *formatCompatRun) verifyIndexes(ctx context.Context) error {
	conn, pool, err := c.baseConn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	var wantGin, wantGist int64
	for g := 1; g <= fcGinRows; g++ {
		if g%fcGinGroups == 3 {
			wantGin++
		}
	}
	for g := 1; g <= fcGistRows; g++ {
		if x, y := g%100, g/100; x <= 9 && y <= 1 {
			wantGist++
		}
	}
	if err := indexedCount(ctx, pool, "fc_gin_idx",
		"SELECT count(*) FROM fc_gin WHERE doc @> '{\"grp\": 3}'::jsonb", wantGin); err != nil {
		return err
	}
	if err := indexedCount(ctx, pool, "fc_gist_idx",
		"SELECT count(*) FROM fc_gist WHERE p <@ box '((0,-1),(9,1))'", wantGist); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS amcheck"); err != nil {
		return fmt.Errorf("install amcheck on the upgraded endpoint: %w. It is one of the baked extensions (functional.extensions.baked), so a btree written by the older release cannot be checked without it", err)
	}
	checked, err := conn.QueryInt(ctx, `SELECT count(*) FROM (
		SELECT bt_index_check(i.indexrelid)
		FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_am a ON a.oid = c.relam
		WHERE a.amname = 'btree' AND c.relname LIKE 'fc\_%') s`)
	if err != nil {
		return fmt.Errorf("bt_index_check over the btrees the older release built: %w", err)
	}
	if checked == 0 {
		return fmt.Errorf("bt_index_check ran over no index at all, so the btrees the older release built were not checked")
	}
	c.set("btrees_checked", checked)
	return nil
}

func indexedCount(ctx context.Context, pool *pgxpool.Pool, index, query string, want int64) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire a connection to read through %s: %w", index, err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		return fmt.Errorf("disable sequential scans before reading through %s: %w", index, err)
	}
	var plan strings.Builder
	rows, err := conn.Query(ctx, "EXPLAIN (COSTS OFF) "+query)
	if err != nil {
		return fmt.Errorf("explain the read through %s: %w", index, err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			return fmt.Errorf("read the plan of the query through %s: %w", index, err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("explain the read through %s: %w", index, err)
	}
	if !strings.Contains(plan.String(), index) {
		return fmt.Errorf("the query %q is not planned through %s after the upgrade (plan: %s), so the index the older release built is unusable to the current one", query, index, strings.TrimSpace(plan.String()))
	}
	var got int64
	if err := conn.QueryRow(ctx, query).Scan(&got); err != nil {
		return fmt.Errorf("run the query through %s: %w", index, err)
	}
	if got != want {
		return fmt.Errorf("the query %q answers %d through %s, want %d: the index the older release built no longer returns the rows it covers", query, got, index, want)
	}
	return nil
}

func (c *formatCompatRun) verifyFork(ctx context.Context) error {
	if c.forkBranchID == "" {
		return nil
	}
	f := c.f
	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, err := createEndpoint(ctx, cl, c.proj.poolID, c.proj.projID, c.forkBranchID)
	if err != nil {
		return fmt.Errorf("create an endpoint on the fork %s with the upgraded controller: %w", c.forkName, err)
	}
	conn, err := f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect the endpoint on the fork %s: %w", c.forkName, err)
	}
	defer conn.Close()
	pool := poolOf(conn)
	if pool == nil {
		return fmt.Errorf("the connection to the fork's endpoint carries no pgx pool")
	}
	if err := verifyFormatDataset(ctx, pool, c.spec, c.seed, "the fork of the old-format branch"); err != nil {
		return err
	}
	pre, err := conn.QueryInt(ctx, "SELECT count(*) FROM fc_fork WHERE tag = 'pre-fork'")
	if err != nil {
		return fmt.Errorf("read the pre-fork marker on the fork: %w", err)
	}
	if pre != 1 {
		return fmt.Errorf("the fork taken at %s serves %d pre-fork marker rows, want 1: the fork point is the parent's flushed position and that write was flushed before the fork", c.forkLSN, pre)
	}
	post, err := conn.QueryInt(ctx, "SELECT count(*) FROM fc_fork WHERE tag = 'post-fork'")
	if err != nil {
		return fmt.Errorf("read the post-fork marker on the fork: %w", err)
	}
	if post != 0 {
		return fmt.Errorf("the fork taken at %s serves the parent's post-fork write, so the fork point did not hold", c.forkLSN)
	}
	return nil
}

func (c *formatCompatRun) verifyChain(ctx context.Context) error {
	conn, pool, err := c.baseConn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	ps, err := c.f.endpointPageserver(ctx, c.ep)
	if err != nil {
		if skipUnreachablePageserver(err, c.f.t.Skip) {
			return nil
		}
		return err
	}
	res, err := ps.ForceGC(ctx, c.branchID, fcGCHorizonBytes)
	if err != nil {
		return core.AsEnvFailure(fmt.Errorf("force GC on branch %s to materialise the delta chain: %w", c.branchID, err))
	}
	c.set("chain_gc_rows_deleted", res.RowsDeleted)

	want := make([]int64, fcChainRows)
	for i := 0; i < c.chainUpdates; i++ {
		want[i%fcChainRows]++
	}
	rows, err := pool.Query(ctx, "SELECT id, n FROM fc_chain ORDER BY id")
	if err != nil {
		return fmt.Errorf("read the delta chain back after the upgrade: %w", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			return fmt.Errorf("scan a delta-chain row: %w", err)
		}
		if id < 1 || id > int64(fcChainRows) {
			return fmt.Errorf("fc_chain holds an unexpected id %d", id)
		}
		if n != want[id-1] {
			return fmt.Errorf("fc_chain row %d reads %d after the upgrade, want %d: the chain of %d small updates the older release wrote did not materialise", id, n, want[id-1], c.chainUpdates)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the delta chain back after the upgrade: %w", err)
	}
	if seen != fcChainRows {
		return fmt.Errorf("fc_chain serves %d rows after the upgrade, want %d", seen, fcChainRows)
	}
	return nil
}

const fcGCHorizonBytes = 1024

// Retention is tightened here, not at endpoint create, which would have collected
// the fork point out from under the phase above.
func (c *formatCompatRun) verifyGC(ctx context.Context) error {
	if c.victimRel == 0 {
		return nil
	}
	f := c.f
	storage, err := f.ampStorageSQL(ctx)
	if err != nil {
		if errors.Is(err, errNoStorageRoute) {
			f.logf("amp_format_compat: no route to the backing amp storage database, so the reclaim of the dropped table is not asserted: %v", err)
			return nil
		}
		return core.AsEnvFailure(err)
	}
	defer storage.Close()
	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	settings := map[string]any{}
	for k, v := range gcReclaimPageserverSettings {
		settings[k] = v
	}
	if _, err := cl.UpdateEndpointSettings(ctx, c.proj.poolID, c.proj.projID, c.proj.branchID, c.ep.ID,
		controller.UpdateSettingsRequest{PageserverSettings: &settings}); err != nil {
		return fmt.Errorf("tighten retention on endpoint %s through the upgraded controller: %w", c.ep.ID, err)
	}
	conn, _, err := c.baseConn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	deadline := time.Now().Add(c.gcTimeout)
	pages := int64(-1)
	for i := 1; ; i++ {
		if _, err := conn.Exec(ctx, "INSERT INTO fc_chain VALUES ($1, 0) ON CONFLICT (id) DO UPDATE SET n = fc_chain.n", fcChainRows+i); err != nil {
			return fmt.Errorf("churn the WAL frontier past the drop: %w", err)
		}
		if _, err := conn.Exec(ctx, "CHECKPOINT"); err != nil {
			return fmt.Errorf("checkpoint while waiting for the reclaim: %w", err)
		}
		if pages, err = c.victimPageCount(ctx, storage); err != nil {
			return err
		}
		if pages == 0 {
			c.set("victim_pages_after_gc", pages)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the table the %s release dropped is still in storage %s after the upgrade: amp_storage holds %d pages for branch %s relnode %d (it held %d when it was dropped), so the current release's GC does not reclaim what an older one wrote",
				c.u.fromEndpoint, c.gcTimeout, pages, c.branchID, c.victimRel, c.victimPages)
		}
		if err := waitFor(ctx, 3*time.Second); err != nil {
			return err
		}
	}
}

func waitFor(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
