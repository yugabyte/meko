package features

import (
	"context"
	"fmt"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pageserver"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	psRestartTable    = "amp_ps_restart"
	psRestartSeedRows = 20000
	psRestartOutage   = 20
	psCatchUpTimeout  = 3 * time.Minute
	// One commit waits out a pageserver respawn at most, never a hang.
	psOutageWriteTimeout = 2 * time.Minute
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_pageserver_restart",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "Kill only the pageserver process inside an endpoint pod while a session stays open. The supervisor must respawn it in the same pod, commits issued on the open session right after the kill must succeed, the respawned pageserver must catch up to the compute's flush LSN, and the same session must read every seeded row and every row written during the outage. A compute restart afterwards reads them all back cold.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     30 * time.Minute,
		Run:         runAmpPageserverRestart,
	})
}

func runAmpPageserverRestart(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"pageserver_restart", a.pageserverRestart}}
	})
}

func (a *ampFeatureRun) pageserverRestart(ctx context.Context) error {
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
		return fmt.Errorf("the endpoint connection cannot pin a backend, and this test needs one session open across the pageserver's death")
	}
	s, err := p.Pin(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	branchID, err := s.QueryString(ctx, "SELECT current_setting('yb_amp.branch_id')")
	if err != nil {
		return fmt.Errorf("read the endpoint's branch id: %w", err)
	}
	for _, stmt := range []string{
		"CREATE TABLE " + psRestartTable + " (id int PRIMARY KEY, v text NOT NULL)",
		fmt.Sprintf("INSERT INTO %s SELECT g, md5(g::text) FROM generate_series(1, %d) g", psRestartTable, psRestartSeedRows),
	} {
		if _, err := s.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
	}
	seedSum, err := s.QueryString(ctx, psRestartChecksum)
	if err != nil {
		return err
	}

	ps, err := a.f.endpointDaemon(ctx, ep, "pageserver")
	if err != nil {
		return err
	}
	old, err := ps.running(ctx)
	if err != nil {
		return err
	}
	if err := ps.kill(ctx, old); err != nil {
		return err
	}
	a.f.logf("amp_pageserver_restart: killed pageserver pid(s) %v in pod %s", old, ps.pod)

	// Commits go to the safekeeper and do not need the pageserver; any that
	// read a page wait for it to come back, and none may fail.
	start := time.Now()
	for i := 1; i <= psRestartOutage; i++ {
		wctx, cancel := context.WithTimeout(ctx, psOutageWriteTimeout)
		_, err := s.Exec(wctx, fmt.Sprintf("INSERT INTO %s VALUES (%d, 'outage')", psRestartTable, psRestartSeedRows+i))
		cancel()
		if err != nil {
			return fmt.Errorf("commit %d of %d on the open session, %s after the pageserver was killed, failed: a pageserver death must not fail a commit, which only the safekeeper has to accept: %w",
				i, psRestartOutage, time.Since(start).Round(time.Millisecond), err)
		}
	}
	a.f.t.Metric("outage_commits_seconds", time.Since(start).Seconds(), "s")

	fresh, err := ps.respawned(ctx, old)
	if err != nil {
		return err
	}
	a.f.logf("amp_pageserver_restart: pageserver respawned as pid(s) %v", fresh)

	flush, err := s.QueryString(ctx, "SELECT pg_current_wal_flush_lsn()::text")
	if err != nil {
		return fmt.Errorf("read the compute's flush LSN: %w", err)
	}
	if err := waitPageserverCaughtUp(ctx, a.f.endpointPageserver, ep, branchID, flush); err != nil {
		return err
	}

	if err := checkPSRestartRows(ctx, s, seedSum, "on the session that stayed open across the pageserver's death"); err != nil {
		return err
	}
	s.Close()
	conn.Close()

	cold, ok, err := a.restartEndpointCompute(ctx, ep)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	defer cold.Close()
	return checkPSRestartRows(ctx, cold, seedSum, "after a compute restart, read cold through the respawned pageserver's storage")
}

var psRestartChecksum = fmt.Sprintf("SELECT md5(string_agg(id || ':' || v, ',' ORDER BY id)) FROM %s WHERE id <= %d", psRestartTable, psRestartSeedRows)

type sqlReader interface {
	QueryInt(ctx context.Context, sql string, args ...any) (int64, error)
	QueryString(ctx context.Context, sql string, args ...any) (string, error)
}

func checkPSRestartRows(ctx context.Context, c sqlReader, seedSum, where string) error {
	sum, err := c.QueryString(ctx, psRestartChecksum)
	if err != nil {
		return fmt.Errorf("checksum the seeded rows %s: %w", where, err)
	}
	if sum != seedSum {
		return fmt.Errorf("the seeded rows read back with a different checksum %s", where)
	}
	n, err := c.QueryInt(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE id > %d AND v = 'outage'", psRestartTable, psRestartSeedRows))
	if err != nil {
		return fmt.Errorf("count the outage rows %s: %w", where, err)
	}
	if n != psRestartOutage {
		return fmt.Errorf("%d of the %d rows committed while the pageserver was down are visible %s", n, psRestartOutage, where)
	}
	return nil
}

// waitPageserverCaughtUp waits for the branch's last_record_lsn to reach lsn.
// The management port refuses until the respawned pageserver listens.
func waitPageserverCaughtUp(ctx context.Context, find pageserverFinder, ep controller.Endpoint, branchID, lsn string) error {
	want, err := pageserver.ParseLSN(lsn)
	if err != nil {
		return err
	}
	var last string
	var lastErr error
	err = pollUntil(ctx, psCatchUpTimeout, 2*time.Second, func() (bool, error) {
		ps, err := find(ctx, ep)
		if err != nil {
			lastErr = err
			return false, nil
		}
		info, err := ps.BranchInfo(ctx, branchID)
		if err != nil {
			lastErr = err
			return false, nil
		}
		last = info.LastRecordLSN
		got, err := pageserver.ParseLSN(info.LastRecordLSN)
		if err != nil {
			return false, err
		}
		return got >= want, nil
	})
	if err != nil {
		return fmt.Errorf("the respawned pageserver's last_record_lsn for branch %s is %q, short of the compute's flush LSN %s (last error %v): it did not catch up from the safekeeper: %w",
			branchID, last, lsn, lastErr, err)
	}
	return nil
}
