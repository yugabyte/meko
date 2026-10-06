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
	// Long enough that a level which acks without the safekeeper has acked.
	skFrozenHold = 15 * time.Second
	// The blocked commits wait out a safekeeper respawn and walproposer
	// reconnect, never longer.
	skCommitResumeTimeout = 3 * time.Minute
)

// Every level except off promises the commit is on the safekeeper before it
// returns; local included (meko#1564).
var skDurableLevels = []string{"on", "local", "remote_write", "remote_apply"}

func init() {
	core.Register(core.Test{
		Name:        "features/amp_safekeeper_restart",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "Freeze only the safekeeper process inside an endpoint pod, then issue one commit at each of synchronous_commit on, local, remote_write and remote_apply. Each must block, neither acknowledging nor failing, while the safekeeper is frozen (meko#1564). Kill the frozen safekeeper: the supervisor must respawn it in the same pod and every blocked commit must then complete. After a compute restart every acknowledged row reads back from storage.",
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
	conn, err := a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	defer conn.Close()
	p, ok := conn.(pinner)
	if !ok {
		return fmt.Errorf("the endpoint connection cannot pin a backend, and each synchronous_commit level needs its own session")
	}
	for _, stmt := range []string{
		"CREATE TABLE " + skRestartTable + " (level text PRIMARY KEY, phase text NOT NULL)",
		"INSERT INTO " + skRestartTable + " VALUES ('seed', 'before')",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
	}

	sessions := make([]pg.Session, 0, len(skDurableLevels))
	defer func() {
		for _, s := range sessions {
			s.Close()
		}
	}()
	for _, level := range skDurableLevels {
		s, err := p.Pin(ctx)
		if err != nil {
			return err
		}
		sessions = append(sessions, s)
		if _, err := s.Exec(ctx, "SET synchronous_commit = "+level); err != nil {
			return fmt.Errorf("SET synchronous_commit = %s: %w", level, err)
		}
		// Warm the session's catalog and the table's pages, so the frozen
		// commit waits on the safekeeper and nothing else.
		if _, err := s.QueryInt(ctx, "SELECT count(*) FROM "+skRestartTable); err != nil {
			return err
		}
	}

	sk, err := a.f.endpointDaemon(ctx, ep, "safekeeper")
	if err != nil {
		return err
	}
	pids, err := sk.running(ctx)
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
	for i, level := range skDurableLevels {
		wg.Add(1)
		go func(s pg.Session, level string) {
			defer wg.Done()
			_, err := s.Exec(wctx, fmt.Sprintf("INSERT INTO %s VALUES ('%s', 'frozen')", skRestartTable, level))
			mu.Lock()
			results = append(results, skCommit{level: level, returned: time.Now(), err: err})
			mu.Unlock()
		}(sessions[i], level)
	}

	sleepFor(ctx, skFrozenHold)
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
	a.f.logf("amp_safekeeper_restart: all %d commits still blocked after %s with the safekeeper frozen", len(skDurableLevels), skFrozenHold)

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
			len(failed), len(skDurableLevels), strings.Join(failed, "; "))
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
	got, err := cold.QueryString(ctx, fmt.Sprintf("SELECT string_agg(level, ',' ORDER BY level) FROM %s WHERE phase = 'frozen'", skRestartTable))
	if err != nil {
		return fmt.Errorf("read the acknowledged rows after a compute restart: %w", err)
	}
	want := strings.Join(slices.Sorted(slices.Values(skDurableLevels)), ",")
	if got != want {
		return fmt.Errorf("after a compute restart the rows acknowledged across the safekeeper's death read %q, want %q", got, want)
	}
	return nil
}
