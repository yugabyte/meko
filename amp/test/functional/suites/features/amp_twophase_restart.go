package features

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// State outside the heap that a compute restart must bring back:
// amp_compute_restart only checks heap rows.
const (
	twoPhaseTable     = "amp_twophase"
	twoPhaseSeedRows  = 100
	twoPhaseCommitGID = "ampqa_commit"
	twoPhaseAbortGID  = "ampqa_rollback"
	twoPhaseLocksGID  = "ampqa_share_lock"
	// Past PGPROC_MAX_CACHED_SUBXIDS (64), so the subxids overflow to pg_subtrans.
	twoPhaseSubxacts = 80
	twoPhaseMaxXacts = "10"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_twophase_restart",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "Check that an UPDATE waits on a share lock held by an open transaction, by two (a multixact), by a prepared one, and by a multixact with a prepared member, as it does in PostgreSQL. Prepare transactions (one with 80 subtransactions, one holding a share lock beside another session's, one to roll back) and commit rows with track_commit_timestamp on, then restart the endpoint's compute pod. The prepared transactions must still be listed and must commit and roll back with the right rows visible, the multixact-locked rows must be lockable and updatable, and commit timestamps must survive (meko#1375) unless the settings API refuses track_commit_timestamp outright.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpTwoPhaseRestart,
	})
}

func runAmpTwoPhaseRestart(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"twophase_restart", func(ctx context.Context) error {
			cl, err := a.f.controllerClient(ctx)
			if err != nil {
				return err
			}
			roller := &kubeComputeRoller{kube: a.f.ampKube(), timeout: restartReadyTimeout}
			return twoPhaseRestartSteps(ctx, cl, a.proj, a.f.endpointSQLAt, roller, a.f.t.Skip)
		}}}
	})
}

func twoPhaseRestartSteps(ctx context.Context, cl ampCtl, proj ampCluster, connect podConnector, roller computeRoller, skip func(string)) error {
	ep, commitTS, err := createTwoPhaseEndpoint(ctx, cl, proj)
	if err != nil {
		return err
	}
	ip, err := roller.runningPodIP(ctx, ep.ID)
	if err != nil {
		return fmt.Errorf("resolve endpoint compute pod IP: %w", err)
	}
	conn, err := connect(ctx, ip, ep)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	pinnable, ok := conn.(pinner)
	if !ok {
		conn.Close()
		skip("this connection cannot pin a backend, and PREPARE TRANSACTION needs one session from BEGIN to PREPARE")
		return nil
	}

	var sessions []pg.Session
	closeSessions := func() {
		for _, s := range sessions {
			s.Close()
		}
		sessions = nil
	}
	// Each error return below closes every session pinned here.
	pin := func() (pg.Session, error) {
		s, err := pinnable.Pin(ctx)
		if err == nil {
			sessions = append(sessions, s)
		}
		return s, err
	}
	sumBefore, lockGaps, err := seedTwoPhase(ctx, conn, pin)
	if err != nil {
		closeSessions()
		conn.Close()
		return err
	}
	var tsBefore string
	if commitTS {
		if tsBefore, err = conn.QueryString(ctx, fmt.Sprintf(
			"SELECT coalesce(pg_xact_commit_timestamp(xmin)::text, 'NULL') FROM %s WHERE id = 1", twoPhaseTable)); err != nil {
			closeSessions()
			conn.Close()
			return fmt.Errorf("read a commit timestamp before the restart: %w", err)
		}
		if tsBefore == "NULL" {
			closeSessions()
			conn.Close()
			return fmt.Errorf("track_commit_timestamp is on but a row committed after the endpoint started has no commit timestamp even before any restart")
		}
	}
	closeSessions()
	conn.Close()

	pod, err := roller.podName(ctx, ep.ID)
	if err != nil {
		return fmt.Errorf("find endpoint compute pod: %w", err)
	}
	if err := roller.deletePod(ctx, pod); err != nil {
		return fmt.Errorf("delete endpoint compute pod %s: %w", pod, err)
	}
	if err := roller.waitReady(ctx, ep.ID); err != nil {
		return fmt.Errorf("endpoint compute did not come back after restart: %w", err)
	}
	ip, err = roller.runningPodIP(ctx, ep.ID)
	if err != nil {
		return fmt.Errorf("resolve rescheduled compute pod IP: %w", err)
	}
	conn, err = connect(ctx, ip, ep)
	if err != nil {
		return fmt.Errorf("reconnect after restart: %w", err)
	}
	defer conn.Close()
	// A leftover prepared transaction holds its locks and pins the xmin horizon.
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		for _, gid := range []string{twoPhaseCommitGID, twoPhaseAbortGID, twoPhaseLocksGID, twoPhaseLockSingleGID} {
			_, _ = conn.Exec(cctx, "ROLLBACK PREPARED '"+gid+"'")
		}
	}()

	after, ok := conn.(pinner)
	if !ok {
		return fmt.Errorf("the connection after the restart cannot pin a backend")
	}
	err = verifyTwoPhaseAfterRestart(ctx, conn, after, sumBefore, commitTS, tsBefore, !slices.Contains(lockGaps, lockCasePreparedMultixact))
	if len(lockGaps) > 0 {
		gapErr := fmt.Errorf("an UPDATE went through a share lock that vanilla PostgreSQL makes it wait for, before any restart, in: %s", strings.Join(lockGaps, "; "))
		return errors.Join(gapErr, err)
	}
	return err
}

// A 400 naming track_commit_timestamp is an honest answer to meko#1375, so the
// test goes on without it.
func createTwoPhaseEndpoint(ctx context.Context, cl ampCtl, proj ampCluster) (controller.Endpoint, bool, error) {
	settings := map[string]string{"max_prepared_transactions": twoPhaseMaxXacts, "track_commit_timestamp": "on"}
	ep, err := createEndpointReq(ctx, cl, proj.poolID, proj.projID, proj.branchID,
		controller.CreateEndpointRequest{Role: roleReadWrite, PgSettings: settings})
	if err == nil {
		return ep, true, nil
	}
	var apiErr *controller.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest || !strings.Contains(err.Error(), "track_commit_timestamp") {
		return controller.Endpoint{}, false, fmt.Errorf("create an endpoint with max_prepared_transactions=%s and track_commit_timestamp=on: %w", twoPhaseMaxXacts, err)
	}
	delete(settings, "track_commit_timestamp")
	ep, err = createEndpointReq(ctx, cl, proj.poolID, proj.projID, proj.branchID,
		controller.CreateEndpointRequest{Role: roleReadWrite, PgSettings: settings})
	if err != nil {
		return controller.Endpoint{}, false, fmt.Errorf("create an endpoint with max_prepared_transactions=%s: %w", twoPhaseMaxXacts, err)
	}
	return ep, false, nil
}

func seedTwoPhase(ctx context.Context, conn sqlConn, pin func() (pg.Session, error)) (string, []string, error) {
	if got, err := conn.QueryString(ctx, "SHOW max_prepared_transactions"); err != nil || got != twoPhaseMaxXacts {
		return "", nil, fmt.Errorf("max_prepared_transactions is %q (err %v), want %s from the endpoint's pg_settings", got, err, twoPhaseMaxXacts)
	}
	for _, stmt := range []string{
		fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v text NOT NULL)", twoPhaseTable),
		fmt.Sprintf("INSERT INTO %s SELECT g, md5(g::text) FROM generate_series(1, %d) g", twoPhaseTable, twoPhaseSeedRows),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return "", nil, fmt.Errorf("seed: %w", err)
		}
	}

	commit, err := pin()
	if err != nil {
		return "", nil, err
	}
	if err := execAll(ctx, commit, append(append([]string{"BEGIN",
		fmt.Sprintf("INSERT INTO %s VALUES (1001, 'prepared-commit')", twoPhaseTable)},
		subxactStatements(twoPhaseSubxacts)...),
		fmt.Sprintf("UPDATE %s SET v = 'prepared-commit' WHERE id = 3", twoPhaseTable),
		"PREPARE TRANSACTION '"+twoPhaseCommitGID+"'")); err != nil {
		return "", nil, fmt.Errorf("prepare %s: %w", twoPhaseCommitGID, err)
	}
	commit.Close()

	abort, err := pin()
	if err != nil {
		return "", nil, err
	}
	if err := execAll(ctx, abort, []string{"BEGIN",
		fmt.Sprintf("INSERT INTO %s VALUES (2001, 'prepared-rollback')", twoPhaseTable),
		fmt.Sprintf("UPDATE %s SET v = 'prepared-rollback' WHERE id = 4", twoPhaseTable),
		"PREPARE TRANSACTION '" + twoPhaseAbortGID + "'"}); err != nil {
		return "", nil, fmt.Errorf("prepare %s: %w", twoPhaseAbortGID, err)
	}
	abort.Close()

	a, err := pin()
	if err != nil {
		return "", nil, err
	}
	b, err := pin()
	if err != nil {
		return "", nil, err
	}
	lock := fmt.Sprintf("SELECT v FROM %s WHERE id = 1 FOR SHARE", twoPhaseTable)
	if err := execAll(ctx, a, []string{"BEGIN", lock}); err != nil {
		return "", nil, fmt.Errorf("first share lock: %w", err)
	}
	if err := execAll(ctx, b, []string{"BEGIN", lock}); err != nil {
		return "", nil, fmt.Errorf("second share lock: %w", err)
	}
	if err := execAll(ctx, a, []string{"PREPARE TRANSACTION '" + twoPhaseLocksGID + "'"}); err != nil {
		return "", nil, fmt.Errorf("prepare %s: %w", twoPhaseLocksGID, err)
	}
	if err := execAll(ctx, b, []string{"COMMIT"}); err != nil {
		return "", nil, fmt.Errorf("commit the second share locker: %w", err)
	}
	a.Close()
	b.Close()

	gaps, err := lockMatrix(ctx, conn, pin)
	if err != nil {
		return "", nil, err
	}
	n, err := conn.QueryInt(ctx, "SELECT count(*) FROM pg_prepared_xacts WHERE gid LIKE 'ampqa_%'")
	if err != nil {
		return "", nil, err
	}
	if n != 3 {
		return "", nil, fmt.Errorf("pg_prepared_xacts lists %d of the 3 transactions just prepared, before any restart", n)
	}
	sum, err := conn.QueryString(ctx, twoPhaseChecksum)
	return sum, gaps, err
}

var twoPhaseChecksum = fmt.Sprintf("SELECT md5(string_agg(id || ':' || v, ',' ORDER BY id)) FROM %s WHERE id <= %d", twoPhaseTable, twoPhaseSeedRows)

func verifyTwoPhaseAfterRestart(ctx context.Context, conn sqlConn, pinnable pinner, sumBefore string, commitTS bool, tsBefore string, lockHeldBefore bool) error {
	gids, err := conn.QueryString(ctx, "SELECT coalesce(string_agg(gid, ',' ORDER BY gid), '') FROM pg_prepared_xacts WHERE gid LIKE 'ampqa_%'")
	if err != nil {
		return fmt.Errorf("list prepared transactions after restart: %w", err)
	}
	want := strings.Join([]string{twoPhaseCommitGID, twoPhaseAbortGID, twoPhaseLocksGID}, ",")
	if gids != want {
		return fmt.Errorf("after a compute restart pg_prepared_xacts lists %q, want %q: PREPARE TRANSACTION returned, so each of these was promised durable and must survive a restart", gids, want)
	}
	sumAfter, err := conn.QueryString(ctx, twoPhaseChecksum)
	if err != nil {
		return err
	}
	if sumAfter != sumBefore {
		return fmt.Errorf("the committed baseline changed across the restart while prepared transactions over it were still pending")
	}

	// Only asked when it blocked before the restart, or a pass says nothing.
	if lockHeldBefore {
		blocked, err := updateWithLockTimeout(ctx, pinnable, 1, "blocked", "2s")
		if err != nil {
			return err
		}
		if !blocked {
			return fmt.Errorf("row 1 is share-locked by a transaction that is still prepared, and an UPDATE of it blocked before the compute restart, but went through after it: the restarted compute no longer sees the prepared member of the row's multixact as in progress")
		}
	}

	for _, stmt := range []string{
		"COMMIT PREPARED '" + twoPhaseCommitGID + "'",
		"ROLLBACK PREPARED '" + twoPhaseAbortGID + "'",
		"COMMIT PREPARED '" + twoPhaseLocksGID + "'",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s after the restart: %w", stmt, err)
		}
	}

	checks := []struct {
		sql, want, what string
	}{
		{fmt.Sprintf("SELECT coalesce(v, '') FROM %s WHERE id = 1001", twoPhaseTable), "prepared-commit", "the committed prepared transaction's insert"},
		{fmt.Sprintf("SELECT v FROM %s WHERE id = 3", twoPhaseTable), "prepared-commit", "the committed prepared transaction's update"},
		{fmt.Sprintf("SELECT count(*)::text FROM %s WHERE id BETWEEN 5001 AND %d", twoPhaseTable, 5000+twoPhaseSubxacts), fmt.Sprint(twoPhaseSubxacts), "the rows its subtransactions inserted"},
		{fmt.Sprintf("SELECT count(*)::text FROM %s WHERE id = 2001", twoPhaseTable), "0", "the rolled-back prepared transaction's insert (must be absent)"},
		{fmt.Sprintf("SELECT v FROM %s WHERE id = 4", twoPhaseTable), "a87ff679a2f3e71d9181a67b7542122c", "row 4, whose update was rolled back (must be the seeded md5)"},
	}
	for _, c := range checks {
		got, err := conn.QueryString(ctx, c.sql)
		if err != nil {
			return fmt.Errorf("read %s: %w", c.what, err)
		}
		if got != c.want {
			return fmt.Errorf("%s reads %q after the restart and 2PC finish, want %q", c.what, got, c.want)
		}
	}

	blocked, err := updateWithLockTimeout(ctx, pinnable, 1, "after-multixact", "10s")
	if err != nil {
		return err
	}
	if blocked {
		return fmt.Errorf("row 1 cannot be updated after both of its share lockers finished: its multixact did not resolve after the restart")
	}

	if commitTS {
		tsAfter, err := conn.QueryString(ctx, fmt.Sprintf(
			"SELECT coalesce(pg_xact_commit_timestamp(xmin)::text, 'NULL') FROM %s WHERE id = 2", twoPhaseTable))
		if err != nil {
			return fmt.Errorf("read a commit timestamp after the restart: %w", err)
		}
		if tsAfter == "NULL" {
			return fmt.Errorf("track_commit_timestamp=on was accepted, and a baseline row had a commit timestamp (%s) before the restart, but has none after it: amp does not persist pg_commit_ts (meko#1375)", tsBefore)
		}
	}
	return nil
}

func subxactStatements(n int) []string {
	out := make([]string, 0, 2*n)
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("SAVEPOINT s%d", i),
			fmt.Sprintf("INSERT INTO %s VALUES (%d, 'subxact')", twoPhaseTable, 5000+i))
	}
	return out
}

func execAll(ctx context.Context, s pg.Session, stmts []string) error {
	for _, stmt := range stmts {
		if _, err := s.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

const (
	lockCaseOpenSingle        = "one open share locker (row 90)"
	lockCaseOpenMultixact     = "two open share lockers, a multixact (row 91)"
	lockCasePreparedSingle    = "one prepared share locker (row 92)"
	lockCasePreparedMultixact = "a multixact whose member is prepared (row 1)"
	twoPhaseLockSingleGID     = "ampqa_lock_single"
)

// lockMatrix returns the lock shapes an UPDATE did not wait on, so a failure
// names the shape that breaks. Row 1's multixact is left for the restart.
func lockMatrix(ctx context.Context, conn sqlConn, pin func() (pg.Session, error)) ([]string, error) {
	p, ok := conn.(pinner)
	if !ok {
		return nil, fmt.Errorf("this connection cannot pin a backend for the lock checks")
	}
	lock := func(id int) string {
		return fmt.Sprintf("SELECT v FROM %s WHERE id = %d FOR SHARE", twoPhaseTable, id)
	}
	var gaps []string
	check := func(id int, what string) error {
		blocked, err := updateWithLockTimeout(ctx, p, id, "blocked", "2s")
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if !blocked {
			gaps = append(gaps, what)
		}
		return nil
	}

	s1, err := pin()
	if err != nil {
		return nil, err
	}
	if err := execAll(ctx, s1, []string{"BEGIN", lock(90)}); err != nil {
		return nil, err
	}
	if err := check(90, lockCaseOpenSingle); err != nil {
		return nil, err
	}
	_, _ = s1.Exec(ctx, "ROLLBACK")
	s1.Close()

	s2, err := pin()
	if err != nil {
		return nil, err
	}
	s3, err := pin()
	if err != nil {
		return nil, err
	}
	if err := execAll(ctx, s2, []string{"BEGIN", lock(91)}); err != nil {
		return nil, err
	}
	if err := execAll(ctx, s3, []string{"BEGIN", lock(91)}); err != nil {
		return nil, err
	}
	if err := check(91, lockCaseOpenMultixact); err != nil {
		return nil, err
	}
	_, _ = s2.Exec(ctx, "ROLLBACK")
	_, _ = s3.Exec(ctx, "ROLLBACK")
	s2.Close()
	s3.Close()

	s4, err := pin()
	if err != nil {
		return nil, err
	}
	if err := execAll(ctx, s4, []string{"BEGIN", lock(92), "PREPARE TRANSACTION '" + twoPhaseLockSingleGID + "'"}); err != nil {
		return nil, fmt.Errorf("prepare %s: %w", twoPhaseLockSingleGID, err)
	}
	s4.Close()
	if err := check(92, lockCasePreparedSingle); err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, "ROLLBACK PREPARED '"+twoPhaseLockSingleGID+"'"); err != nil {
		return nil, fmt.Errorf("roll back %s: %w", twoPhaseLockSingleGID, err)
	}

	if err := check(1, lockCasePreparedMultixact); err != nil {
		return nil, err
	}
	return gaps, nil
}

func updateWithLockTimeout(ctx context.Context, pinnable pinner, id int, v, timeout string) (bool, error) {
	s, err := pinnable.Pin(ctx)
	if err != nil {
		return false, err
	}
	defer s.Close()
	if err := execAll(ctx, s, []string{"BEGIN", "SET LOCAL lock_timeout = '" + timeout + "'"}); err != nil {
		return false, err
	}
	_, err = s.Exec(ctx, fmt.Sprintf("UPDATE %s SET v = '%s' WHERE id = %d", twoPhaseTable, v, id))
	if err != nil {
		_, _ = s.Exec(ctx, "ROLLBACK")
		if strings.Contains(err.Error(), "lock timeout") || strings.Contains(err.Error(), "55P03") {
			return true, nil
		}
		return false, fmt.Errorf("update row %d: %w", id, err)
	}
	if _, err := s.Exec(ctx, "COMMIT"); err != nil {
		return false, fmt.Errorf("commit the update of row %d: %w", id, err)
	}
	return false, nil
}
