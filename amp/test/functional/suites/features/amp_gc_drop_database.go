package features

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	dropDBName = "amp_gc_d1"
	dropDBRows = 5000
	// GC has to have looked at the drop before the fork's cold read means anything.
	dropDBGCWait = 3 * time.Minute
)

// gcDropDatabaseUnderFork drops a database on a parent while a fork taken
// before the drop still reads it. The parent's GC must keep every page version
// the fork needs, and reclaim them once the fork is gone.
func (a *ampFeatureRun) gcDropDatabaseUnderFork(ctx context.Context) error {
	storage, err := a.f.ampStorageSQL(ctx)
	if err != nil {
		if errors.Is(err, errNoStorageRoute) {
			a.f.t.Skip(err.Error())
			return nil
		}
		return core.AsEnvFailure(fmt.Errorf("reach the backing amp storage database: %w", err))
	}
	defer storage.Close()
	kube := a.f.ampKube()
	if kube == nil {
		a.f.t.Skip("no kube handle: the dropped database is read with psql from inside an endpoint pod")
		return nil
	}
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	proj := a.proj
	ep, ok := lastEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if !ok {
		return fmt.Errorf("the default branch has no endpoint with GC tightened from the previous phase")
	}
	c, err := a.f.endpointSQL(ctx, ep)
	if err != nil {
		return err
	}
	defer c.Close()
	parentID, err := c.QueryString(ctx, "SELECT current_setting('yb_amp.branch_id')")
	if err != nil {
		return err
	}

	psqlD1 := func(on controller.Endpoint, ip, sql string) (string, error) {
		host, p, ok := strings.Cut(on.Address, ":")
		port, _ := strconv.Atoi(p)
		if !ok || port == 0 {
			return "", fmt.Errorf("endpoint %s has address %q, want host:port", on.ID, on.Address)
		}
		if ip != "" {
			host = ip
		}
		return ampstack.EndpointPsql(ctx, kube, on.ID, ampstack.EndpointAuth{
			Host: host, Port: port, User: a.f.cfg.EndpointUser, Password: controller.DefaultEndpointPassword, Database: dropDBName,
		}, sql)
	}
	if _, err := c.Exec(ctx, "CREATE DATABASE "+dropDBName); err != nil {
		return err
	}
	sum := "SELECT md5(string_agg(id || v, ',' ORDER BY id)) FROM t"
	if _, err := psqlD1(ep, "", fmt.Sprintf("CREATE TABLE t AS SELECT g AS id, md5(g::text) AS v FROM generate_series(1, %d) g; CHECKPOINT", dropDBRows)); err != nil {
		return fmt.Errorf("seed database %s: %w", dropDBName, err)
	}
	want, err := psqlD1(ep, "", sum)
	if err != nil {
		return err
	}
	dbOID, err := c.QueryInt(ctx, "SELECT oid::bigint FROM pg_database WHERE datname = $1", dropDBName)
	if err != nil {
		return err
	}
	dbPages := func() (int64, error) {
		return storage.QueryInt(ctx, "SELECT count(*) FROM amp_storage.pages WHERE branch_id = $1::uuid AND dbnode = $2", parentID, dbOID)
	}
	var stored int64
	err = pollUntil(ctx, 3*time.Minute, 3*time.Second, func() (bool, error) {
		if _, err := c.Exec(ctx, "CHECKPOINT"); err != nil {
			return false, err
		}
		stored, err = dbPages()
		return stored > 0, err
	})
	if err != nil {
		return fmt.Errorf("database %s (oid %d) never reached amp_storage.pages on branch %s: %w", dropDBName, dbOID, parentID, err)
	}

	lsn, err := currentLSN(ctx, c)
	if err != nil {
		return err
	}
	forkName := "dropdb-fork-" + a.f.suffix()
	fc, err := forkChildAt(ctx, cl, proj, proj.branchID, lsn, forkName, a.f.endpointSQL)
	if err != nil {
		return err
	}
	forkID, err := fc.QueryString(ctx, "SELECT current_setting('yb_amp.branch_id')")
	fc.Close()
	if err != nil {
		return err
	}
	fork, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, forkName)
	if err != nil {
		return err
	}
	forkEP, ok := lastEndpoint(ctx, cl, proj.poolID, proj.projID, fork.BranchID)
	if !ok {
		return fmt.Errorf("the fork %s has no endpoint", forkName)
	}

	var dequeuedBefore float64
	ps, psErr := a.f.endpointPageserver(ctx, ep)
	if psErr == nil {
		if s, err := ps.Metrics(ctx); err == nil {
			dequeuedBefore, _ = s.Value(gcCandidatesDequeuedMetric, nil)
		}
	}
	if _, err := c.Exec(ctx, "DROP DATABASE "+dropDBName); err != nil {
		return fmt.Errorf("drop database %s on the parent: %w", dropDBName, err)
	}
	// Churn so the WAL frontier, and with it the GC cutoff, moves past the drop.
	churn := func() error {
		if _, err := c.Exec(ctx, "CREATE TABLE IF NOT EXISTS amp_gc_churn (id serial PRIMARY KEY, v text)"); err != nil {
			return err
		}
		if _, err := c.Exec(ctx, "INSERT INTO amp_gc_churn (v) SELECT md5(g::text) FROM generate_series(1, 200) g"); err != nil {
			return err
		}
		_, err := c.Exec(ctx, "CHECKPOINT")
		return err
	}
	gcLooked := false
	err = pollUntil(ctx, dropDBGCWait, 5*time.Second, func() (bool, error) {
		if err := churn(); err != nil {
			return false, err
		}
		if psErr != nil {
			return false, nil
		}
		s, err := ps.Metrics(ctx)
		if err != nil {
			return false, nil
		}
		v, _ := s.Value(gcCandidatesDequeuedMetric, nil)
		gcLooked = v > dequeuedBefore
		return gcLooked, nil
	})
	left, err := dbPages()
	if err != nil {
		return err
	}
	a.f.logf("amp_gc_reclaim: after DROP DATABASE the parent keeps %d of %d page rows for oid %d while the fork lives (GC dequeue counter moved: %v)", left, stored, dbOID, gcLooked)
	if !gcLooked && left >= stored {
		return core.AsEnvFailure(fmt.Errorf("the parent's GC neither dequeued a candidate (pageserver reachable: %v) nor reclaimed a page row of dropped database %s within %s, so a cold read on the fork would prove nothing", psErr == nil, dropDBName, dropDBGCWait))
	}

	restarted, ok, err := a.restartEndpointCompute(ctx, forkEP)
	if err != nil {
		return fmt.Errorf("restart the fork's compute for a cold read: %w", err)
	}
	forkIP := ""
	if ok {
		restarted.Close()
		// The Service can still point at the deleted pod right after a restart.
		roller := &kubeComputeRoller{kube: kube, timeout: restartReadyTimeout}
		if forkIP, err = roller.runningPodIP(ctx, forkEP.ID); err != nil {
			return err
		}
	}
	got, err := psqlD1(forkEP, forkIP, sum)
	if err != nil {
		return fmt.Errorf("cold-read database %s on the fork after the parent dropped it (pages kept %d of %d): %w", dropDBName, left, stored, err)
	}
	if got != want {
		return fmt.Errorf("database %s reads back on the fork with a different checksum after the parent dropped it and its GC ran (pages kept %d of %d): the parent's GC reclaimed page versions a live fork still reads", dropDBName, left, stored)
	}

	if _, err := cl.DeleteEndpointAndWait(ctx, proj.poolID, proj.projID, fork.BranchID, forkEP.ID); err != nil {
		return err
	}
	if _, err := cl.DeleteBranchAndWait(ctx, proj.poolID, proj.projID, fork.BranchID); err != nil {
		return err
	}
	// A dropped fork pins the parent's cutoff until the reaper removes its
	// branch_state row, 24h after the drop by default.
	moved, err := storage.Exec(ctx, `UPDATE amp_registry.branches SET reap_after = now()
		 WHERE branch_id = $1::uuid AND dropped_at IS NOT NULL AND gc_completed_at IS NULL`, forkID)
	if err != nil {
		return err
	}
	if moved != 1 {
		return fmt.Errorf("amp_registry.branches has no tombstone for the dropped fork %s (updated %d rows): deleteBranch succeeded but the drop did not stamp dropped_at", forkID, moved)
	}
	err = pollUntil(ctx, gcReclaimTimeout+branchReclaimTimeout, 5*time.Second, func() (bool, error) {
		if err := churn(); err != nil {
			return false, err
		}
		left, err = dbPages()
		return left == 0, err
	})
	if err != nil {
		return fmt.Errorf("the parent still holds %d page rows of dropped database %s (oid %d) after its fork was dropped and reclaimed: %w", left, dropDBName, dbOID, err)
	}
	return nil
}
