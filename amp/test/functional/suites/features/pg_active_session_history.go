package features

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	ashWorkers       = 8
	ashScanners      = 6
	ashHammer        = 75 * time.Second
	ashParsedBatch   = 20000
	ashMinWorkerRows = 100
	ashSetupTimeout  = 5 * time.Minute
)

func init() {
	core.Register(core.Test{
		Name:        "features/pg_active_session_history",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "pgsentinel, preloaded on every endpoint, must never return a row mixed from two samples (meko#1875). Eight sessions with their own application_name keep busy while six sessions scan pg_active_session_history and one calls get_parsedinfo() on a backend alternating a SELECT and an UPDATE. Every sampled row of a worker pairs its pid with its own application_name and its usesysid with its usename, every get_parsedinfo() row pairs a queryid with that statement's cmdtype, enough worker samples were taken to make that meaningful, and the compute never restarts.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     30 * time.Minute,
		Run:         runPgActiveSessionHistory,
	})
}

func runPgActiveSessionHistory(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"ash_rows_from_one_sample", a.activeSessionHistory}}
	})
}

type ashCounts struct {
	tornASH, mixedUser, workerRows, tornParsed, parsedReads atomic.Int64
}

func (a *ampFeatureRun) activeSessionHistory(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, err := createEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID)
	if err != nil {
		return err
	}
	conn, err := a.f.endpointSQLWith(ctx, ep, func(ci *pg.ConnInfo) { ci.MaxConns, ci.ConnectTimeout = ashWorkers+ashScanners+6, 20*time.Second })
	if err != nil {
		return err
	}
	defer conn.Close()
	p, ok := conn.(pinner)
	if !ok {
		return fmt.Errorf("the endpoint connection cannot pin a backend")
	}
	sctx, setupDone := context.WithTimeout(ctx, ashSetupTimeout)
	defer setupDone()
	for _, stmt := range []string{
		"CREATE EXTENSION IF NOT EXISTS pgsentinel",
		"CREATE TABLE ash_t (id int PRIMARY KEY, v bigint)",
		"INSERT INTO ash_t VALUES (1, 0)",
	} {
		if _, err := conn.Exec(sctx, stmt); err != nil {
			return fmt.Errorf("%s: %w (pgsentinel is preloaded on every endpoint since meko#1446)", stmt, err)
		}
	}
	started, err := conn.QueryString(sctx, "SELECT pg_postmaster_start_time()::text")
	if err != nil {
		return err
	}
	since, err := conn.QueryString(sctx, "SELECT now()::text")
	if err != nil {
		return err
	}

	hctx, stop := context.WithTimeout(ctx, ashHammer)
	defer stop()
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		loopErr error
		counts  ashCounts
	)
	fail := func(err error) {
		if hctx.Err() != nil {
			return
		}
		mu.Lock()
		loopErr = errors.Join(loopErr, err)
		mu.Unlock()
	}
	pin := func(setup ...string) (pg.Session, int64, error) {
		s, err := p.Pin(sctx)
		if err != nil {
			return nil, 0, err
		}
		for _, q := range setup {
			if _, err := s.Exec(sctx, q); err != nil {
				s.Close()
				return nil, 0, fmt.Errorf("%s: %w", q, err)
			}
		}
		pid, err := s.QueryInt(sctx, "SELECT pg_backend_pid()")
		if err != nil {
			s.Close()
			return nil, 0, err
		}
		return s, pid, nil
	}

	var apps []string
	var pids []int64
	var sessions []pg.Session
	defer func() {
		for _, s := range sessions {
			s.Close()
		}
	}()
	for i := range ashWorkers {
		app := fmt.Sprintf("ampqa-ash-%d", i)
		s, pid, err := pin("SET application_name = '" + app + "'")
		if err != nil {
			return err
		}
		sessions = append(sessions, s)
		apps, pids = append(apps, app), append(pids, pid)
	}
	alt, altPID, err := pin()
	if err != nil {
		return err
	}
	sessions = append(sessions, alt)
	reader, _, err := pin()
	if err != nil {
		return err
	}
	sessions = append(sessions, reader)

	selectQ, updateQ := "SELECT v FROM ash_t WHERE id = 1", "UPDATE ash_t SET v = v + 1 WHERE id = 1"
	parsed := func(stmt string) (string, error) {
		if _, err := alt.Exec(sctx, stmt); err != nil {
			return "", err
		}
		return reader.QueryString(sctx, "SELECT queryid::text || '/' || cmdtype FROM get_parsedinfo($1::int)", altPID)
	}
	selectInfo, err := parsed(selectQ)
	if err != nil {
		return fmt.Errorf("read get_parsedinfo() for the SELECT: %w", err)
	}
	updateInfo, err := parsed(updateQ)
	if err != nil {
		return fmt.Errorf("read get_parsedinfo() for the UPDATE: %w", err)
	}
	if selectInfo == updateInfo {
		return fmt.Errorf("get_parsedinfo() reports %q for both a SELECT and an UPDATE, so a mixed row cannot be told apart", selectInfo)
	}
	a.f.logf("get_parsedinfo(): SELECT is %s, UPDATE is %s", selectInfo, updateInfo)

	for i := range ashWorkers {
		wg.Add(1)
		go func(s pg.Session) {
			defer wg.Done()
			for hctx.Err() == nil {
				if _, err := s.Exec(hctx, "SELECT pg_sleep(0.02)"); err != nil {
					fail(fmt.Errorf("worker: %w", err))
					return
				}
			}
		}(sessions[i])
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		// A fresh comment per call forces a parse, which is what writes the
		// entry; the queryid ignores comments.
		for n := 0; hctx.Err() == nil; n++ {
			q := selectQ
			if n%2 == 1 {
				q = updateQ
			}
			if _, err := alt.Exec(hctx, fmt.Sprintf("%s /* %d */", q, n)); err != nil {
				fail(fmt.Errorf("alternator: %w", err))
				return
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for hctx.Err() == nil {
			bad, err := reader.QueryInt(hctx, fmt.Sprintf(`SELECT count(*) FILTER (WHERE queryid::text || '/' || cmdtype NOT IN ($2, $3))
				FROM generate_series(1, %d) g, LATERAL get_parsedinfo($1::int + g * 0)`, ashParsedBatch), altPID, selectInfo, updateInfo)
			if err != nil {
				fail(fmt.Errorf("get_parsedinfo() reader: %w", err))
				return
			}
			counts.tornParsed.Add(bad)
			counts.parsedReads.Add(ashParsedBatch)
		}
	}()
	tornSQL := `WITH m(app, pid) AS (SELECT * FROM unnest($1::text[], $2::int[]))
		SELECT count(*) FILTER (WHERE NOT EXISTS (SELECT 1 FROM m WHERE m.app = h.application_name AND m.pid = h.pid)),
		       count(*) FILTER (WHERE h.usesysid <> 0 AND h.usename IS DISTINCT FROM (SELECT rolname FROM pg_roles WHERE oid = h.usesysid)),
		       count(*) FILTER (WHERE EXISTS (SELECT 1 FROM m WHERE m.app = h.application_name AND m.pid = h.pid))
		FROM pg_active_session_history h
		WHERE h.ash_time >= $3::timestamptz AND (h.application_name IN (SELECT app FROM m) OR h.pid IN (SELECT pid FROM m))`
	for range ashScanners {
		s, _, err := pin()
		if err != nil {
			stop()
			wg.Wait()
			return err
		}
		sessions = append(sessions, s)
		wg.Add(1)
		go func(s pg.Session) {
			defer wg.Done()
			for hctx.Err() == nil {
				torn, mixed, good, err := scanASH(hctx, s, tornSQL, apps, pids, since)
				if err != nil {
					fail(fmt.Errorf("pg_active_session_history scanner: %w", err))
					return
				}
				counts.tornASH.Add(torn)
				counts.mixedUser.Add(mixed)
				if good > counts.workerRows.Load() {
					counts.workerRows.Store(good)
				}
			}
		}(s)
	}
	wg.Wait()
	if loopErr != nil {
		return loopErr
	}
	if now, err := conn.QueryString(ctx, "SELECT pg_postmaster_start_time()::text"); err != nil || now != started {
		return fmt.Errorf("the compute restarted while pgsentinel was being read (postmaster start %q, now %q, %v)", started, now, err)
	}
	a.f.t.Metric("ash.worker_rows", float64(counts.workerRows.Load()), "rows")
	a.f.t.Metric("ash.parsedinfo_reads", float64(counts.parsedReads.Load()), "reads")
	var bad []string
	if n := counts.tornASH.Load(); n > 0 {
		bad = append(bad, fmt.Sprintf("%d pg_active_session_history rows paired a worker's pid with another session's application_name", n))
	}
	if n := counts.mixedUser.Load(); n > 0 {
		bad = append(bad, fmt.Sprintf("%d pg_active_session_history rows carried a usename that is not the role of their usesysid", n))
	}
	if n := counts.tornParsed.Load(); n > 0 {
		bad = append(bad, fmt.Sprintf("%d of %d get_parsedinfo() rows matched neither %s nor %s", n, counts.parsedReads.Load(), selectInfo, updateInfo))
	}
	if len(bad) > 0 {
		return fmt.Errorf("pgsentinel returned rows mixed from two samples (meko#1875): %v", bad)
	}
	if n := counts.workerRows.Load(); n < ashMinWorkerRows {
		return fmt.Errorf("pg_active_session_history sampled the %d busy workers only %d times in %s, want at least %d: the sampler is not running, so nothing was checked", ashWorkers, n, ashHammer, ashMinWorkerRows)
	}
	return nil
}

func scanASH(ctx context.Context, s pg.Session, q string, apps []string, pids []int64, since string) (torn, mixed, good int64, err error) {
	row, err := s.QueryString(ctx, "SELECT torn || ',' || mixed || ',' || good FROM ("+q+") AS r(torn, mixed, good)", apps, pids, since)
	if err != nil {
		return 0, 0, 0, err
	}
	_, err = fmt.Sscanf(row, "%d,%d,%d", &torn, &mixed, &good)
	return torn, mixed, good, err
}
