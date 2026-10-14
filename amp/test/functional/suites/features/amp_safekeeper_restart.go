package features

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	skRestartTable = "amp_sk_restart"
	skHotTable     = "amp_sk_hot"
	// Updates to one row on each side of the safekeeper's death, so its page
	// carries deltas from before and after the walreceiver reconnect.
	skHotUpdates = 200
	// When the cancel and the statement_timeout fire into a blocked commit.
	skCancelAfter = 3 * time.Second
	// Long enough that a level which acks without the safekeeper has acked.
	skFrozenHold = 15 * time.Second
	// The blocked commits wait out a safekeeper respawn and walproposer
	// reconnect, never longer.
	skCommitResumeTimeout = 3 * time.Minute
	skSetupTimeout        = 5 * time.Minute
)

// Every level except off promises the commit is on the safekeeper before it
// returns; local included (meko#1564).
var skDurableLevels = []string{"on", "local", "remote_write", "remote_apply"}

// Commits whose sync-rep wait is interrupted: neither a query cancel nor a
// statement_timeout may end the wait and acknowledge a commit the safekeeper
// does not have (meko#473).
var skInterrupted = []skSession{
	{key: "cancelled", setup: []string{"SET synchronous_commit = on"}},
	{key: "statement_timeout", setup: []string{"SET synchronous_commit = on", "SET statement_timeout = '3s'"}},
}

type skSession struct {
	key   string
	setup []string
}

func skSessions() []skSession {
	out := make([]skSession, 0, len(skDurableLevels)+len(skInterrupted))
	for _, l := range skDurableLevels {
		out = append(out, skSession{key: l, setup: []string{"SET synchronous_commit = " + l}})
	}
	return append(out, skInterrupted...)
}

func init() {
	core.Register(core.Test{
		Name:        "features/amp_safekeeper_restart",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "Freeze only the safekeeper process inside an endpoint pod, then issue one commit at each of synchronous_commit on, local, remote_write and remote_apply, plus one whose wait gets pg_cancel_backend and one whose statement_timeout fires during the wait. Each must block, neither acknowledging nor failing, and stay invisible to other sessions while the safekeeper is frozen (meko#1564, meko#473). Kill the frozen safekeeper: the supervisor must respawn it in the same pod and every blocked commit must then complete. After a compute restart every acknowledged row reads back from storage, and a row updated on both sides of the safekeeper's death reads its last value, not a page from before the walreceiver reconnect (meko#1687).",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     30 * time.Minute,
		Run:         runAmpSafekeeperRestart,
	})
}

func runAmpSafekeeperRestart(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"safekeeper_restart", a.safekeeperRestart}}
	})
}

type skCommit struct {
	level    string
	returned time.Time
	err      error
}

func (a *ampFeatureRun) safekeeperRestart(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, err := createEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID)
	if err != nil {
		return err
	}
	specs := skSessions()
	conn, err := a.f.endpointSQLWith(ctx, ep, func(ci *pg.ConnInfo) { ci.MaxConns, ci.ConnectTimeout = len(specs)+4, 20*time.Second })
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	defer conn.Close()
	p, ok := conn.(pinner)
	if !ok {
		return fmt.Errorf("the endpoint connection cannot pin a backend, and each synchronous_commit level needs its own session")
	}
	sctx, setupDone := context.WithTimeout(ctx, skSetupTimeout)
	defer setupDone()
	for _, stmt := range []string{
		"CREATE TABLE " + skRestartTable + " (level text PRIMARY KEY, phase text NOT NULL)",
		"INSERT INTO " + skRestartTable + " VALUES ('seed', 'before')",
		"CREATE TABLE " + skHotTable + " (id int PRIMARY KEY, n bigint NOT NULL)",
		"INSERT INTO " + skHotTable + " VALUES (1, 0)",
	} {
		if _, err := conn.Exec(sctx, stmt); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
	}

	if err := skBumpHot(sctx, conn); err != nil {
		return err
	}
	sessions := make([]pg.Session, 0, len(specs))
	backendPIDs := make(map[string]int64, len(specs))
	defer func() {
		for _, s := range sessions {
			s.Close()
		}
	}()
	for _, spec := range specs {
		s, err := p.Pin(sctx)
		if err != nil {
			return err
		}
		sessions = append(sessions, s)
		for _, q := range spec.setup {
			if _, err := s.Exec(sctx, q); err != nil {
				return fmt.Errorf("%s: %s: %w", spec.key, q, err)
			}
		}
		if backendPIDs[spec.key], err = s.QueryInt(sctx, "SELECT pg_backend_pid()"); err != nil {
			return err
		}
		// Warm the session's catalog and the table's pages, so the frozen
		// commit waits on the safekeeper and nothing else.
		if _, err := s.QueryInt(sctx, "SELECT count(*) FROM "+skRestartTable); err != nil {
			return err
		}
	}

	// Pinned before the freeze: the pool has no idle connection left, and the
	// cancel and the visibility check must not wait on a new backend's start.
	ctrl, err := p.Pin(sctx)
	if err != nil {
		return err
	}
	sessions = append(sessions, ctrl)

	sk, err := a.f.endpointDaemon(sctx, ep, "safekeeper")
	if err != nil {
		return err
	}
	pids, err := sk.running(sctx)
	if err != nil {
		return err
	}
	if err := sk.signal(ctx, "STOP", pids); err != nil {
		return err
	}
	thawed := false
	defer func() {
		if !thawed {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			_ = sk.signal(cctx, "CONT", pids)
			cancel()
		}
	}()
	frozenAt := time.Now()

	var (
		mu      sync.Mutex
		results []skCommit
		wg      sync.WaitGroup
	)
	wctx, cancelWrites := context.WithTimeout(ctx, skFrozenHold+daemonRespawnTimeout+skCommitResumeTimeout)
	defer cancelWrites()
	for i, spec := range specs {
		wg.Add(1)
		go func(s pg.Session, level string) {
			defer wg.Done()
			_, err := s.Exec(wctx, fmt.Sprintf("INSERT INTO %s VALUES ('%s', 'frozen')", skRestartTable, level))
			mu.Lock()
			results = append(results, skCommit{level: level, returned: time.Now(), err: err})
			mu.Unlock()
		}(sessions[i], spec.key)
	}

	sleepFor(ctx, skCancelAfter)
	cctx, ccancel := context.WithTimeout(ctx, 30*time.Second)
	answer, err := ctrl.QueryString(cctx, "SELECT pg_cancel_backend($1::int)::text", backendPIDs["cancelled"])
	ccancel()
	if err != nil || answer != "true" {
		thawed = true
		_ = sk.signal(ctx, "CONT", pids)
		wg.Wait()
		return fmt.Errorf("pg_cancel_backend on the session blocked in its commit answered %q (%v), want true", answer, err)
	}
	sleepFor(ctx, skFrozenHold-skCancelAfter)
	if err := ctx.Err(); err != nil {
		return err
	}
	mu.Lock()
	early := append([]skCommit(nil), results...)
	mu.Unlock()
	if len(early) > 0 {
		var lines []string
		for _, r := range early {
			outcome := "acknowledged"
			if r.err != nil {
				outcome = "failed: " + r.err.Error()
			}
			lines = append(lines, fmt.Sprintf("synchronous_commit=%s %s after %s", r.level, outcome, r.returned.Sub(frozenAt).Round(time.Millisecond)))
		}
		thawed = true
		_ = sk.signal(ctx, "CONT", pids)
		wg.Wait()
		return fmt.Errorf("with the safekeeper frozen, a commit must wait for it, neither acknowledging nor failing (meko#1564): %s", strings.Join(lines, "; "))
	}
	a.f.logf("amp_safekeeper_restart: all %d commits still blocked after %s with the safekeeper frozen", len(specs), skFrozenHold)
	vctx, vcancel := context.WithTimeout(ctx, 30*time.Second)
	visible, verr := ctrl.QueryInt(vctx, "SELECT count(*) FROM "+skRestartTable+" WHERE phase = 'frozen'")
	vcancel()
	nctx, ncancel := context.WithTimeout(ctx, 10*time.Second)
	if _, nerr := conn.QueryInt(nctx, "SELECT 1"); nerr != nil {
		a.f.logf("amp_safekeeper_restart: a query needing a new connection failed with the safekeeper frozen: %v", nerr)
	}
	ncancel()
	if verr != nil || visible != 0 {
		thawed = true
		_ = sk.signal(ctx, "CONT", pids)
		wg.Wait()
		return fmt.Errorf("with every commit still waiting on the frozen safekeeper, another session counts %d of their rows (%v), want 0: a commit the safekeeper does not have was made visible (meko#473)", visible, verr)
	}

	// SIGKILL reaches a stopped process; the supervisor sees a dead child.
	killedAt := time.Now()
	if err := sk.kill(ctx, pids); err != nil {
		return err
	}
	thawed = true
	fresh, err := sk.respawned(ctx, pids)
	if err != nil {
		return err
	}
	a.f.logf("amp_safekeeper_restart: safekeeper respawned as pid(s) %v", fresh)

	wg.Wait()
	var failed []string
	for _, r := range results {
		if r.err != nil {
			failed = append(failed, fmt.Sprintf("synchronous_commit=%s: %v", r.level, r.err))
			continue
		}
		a.f.t.Metric("commit_after_kill_seconds_"+r.level, r.returned.Sub(killedAt).Seconds(), "s")
	}
	if len(failed) > 0 {
		return fmt.Errorf("commits blocked on a frozen safekeeper must complete once it is respawned, but %d of %d did not: %s",
			len(failed), len(specs), strings.Join(failed, "; "))
	}
	if err := skBumpHot(ctx, conn); err != nil {
		return fmt.Errorf("after the safekeeper respawn: %w", err)
	}
	for _, s := range sessions {
		s.Close()
	}
	sessions = nil
	conn.Close()

	cold, ok, err := a.restartEndpointCompute(ctx, ep)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	defer cold.Close()
	got, err := cold.QueryString(ctx, fmt.Sprintf("SELECT string_agg(level, ',' ORDER BY level COLLATE \"C\") FROM %s WHERE phase = 'frozen'", skRestartTable))
	if err != nil {
		return fmt.Errorf("read the acknowledged rows after a compute restart: %w", err)
	}
	keys := make([]string, 0, len(specs))
	for _, spec := range specs {
		keys = append(keys, spec.key)
	}
	want := strings.Join(slices.Sorted(slices.Values(keys)), ",")
	if got != want {
		return fmt.Errorf("after a compute restart the rows acknowledged across the safekeeper's death read %q, want %q", got, want)
	}
	n, err := cold.QueryInt(ctx, "SELECT n FROM "+skHotTable+" WHERE id = 1")
	if err != nil {
		return fmt.Errorf("read the hot row after a compute restart: %w", err)
	}
	if n != 2*skHotUpdates {
		return fmt.Errorf("after a compute restart the row updated %d times on each side of the safekeeper's death reads %d, want %d: the pageserver served a page older than its last acknowledged change (meko#1687)", skHotUpdates, n, 2*skHotUpdates)
	}
	return nil
}

func skBumpHot(ctx context.Context, conn sqlConn) error {
	for i := range skHotUpdates {
		if _, err := conn.Exec(ctx, "UPDATE "+skHotTable+" SET n = n + 1 WHERE id = 1"); err != nil {
			return fmt.Errorf("update %d of the hot row: %w", i, err)
		}
	}
	return nil
}
