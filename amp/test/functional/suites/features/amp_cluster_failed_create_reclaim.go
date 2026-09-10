// The cluster half of the failed-create story (ac-tasks.md §Failed-create GC).
// amp_failed_create_reclaim covers a failed endpoint; a create_cluster with
// with_writer that fails mid-provisioning leaves a cluster row instead. It must
// be hidden from the default list, visible under include_failed=true so an
// operator can delete it, deletable, and swept by the collector after gc-grace
// when nobody does.
package features

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_cluster_failed_create_reclaim",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "gapcoverage"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Two clusters created with a writer no node can schedule: both create tasks fail; the orphans are absent from the default cluster list and present, with a consistent total, under include_failed=true; deleting one through the API succeeds and removes it; the other is swept by the failed-create collector within the sweep budget, and a fresh cluster under the same name can be created afterwards.",
		Params:      []string{"functional", "functional.failed_create_reclaim", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		// Each failure waits out the controller's deploy timeout (35m on the QA
		// control plane); both run in parallel, then the sweep grace (30m).
		Timeout: 150 * time.Minute,
		Run:     runAmpClusterFailedCreateReclaim,
	})
}

func runAmpClusterFailedCreateReclaim(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"cluster_failed_create_reclaim", a.clusterFailedCreateReclaim}}
	})
}

func (a *ampFeatureRun) clusterFailedCreateReclaim(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	pool := a.proj.poolID
	sweep := a.f.t.Options.GetDuration("functional.failed_create_reclaim.sweep_timeout", 45*time.Minute)
	names := []string{"orphan-del-" + a.f.suffix(), "orphan-gc-" + a.f.suffix()}
	tasks := make([]string, len(names))
	for i, name := range names {
		task, loc, err := cl.CreateCluster(ctx, pool, controller.CreateClusterRequest{
			Name: name, PgVersion: a.f.cfg.PgVersion, WithWriter: true,
			Resources: &controller.EndpointResources{CPU: unschedulableCPU, Memory: unschedulableMemory},
		})
		if err != nil {
			if apiErrorStatus(err) == http.StatusBadRequest {
				a.f.t.Skip("the controller refuses a " + unschedulableMemory + " writer synchronously, so no failed cluster create can be produced this way: " + err.Error())
				return nil
			}
			return fmt.Errorf("plan the unschedulable cluster create %s: %w", name, err)
		}
		tasks[i] = controller.TaskRef(task, loc)
	}
	start := time.Now()
	for i, id := range tasks {
		done, err := cl.WaitTask(ctx, id)
		if err != nil && done.Status == "" {
			return fmt.Errorf("wait for create_cluster %s: %w", id, err)
		}
		if done.Status != controller.TaskFailed {
			return fmt.Errorf("create_cluster %s for %s settled %q, want failed: no node can hold a %s writer", id, names[i], done.Status, unschedulableMemory)
		}
	}
	a.f.t.Metric("create_failed_after_seconds", time.Since(start).Seconds(), "s")

	visible, err := clusterNames(ctx, cl, pool, false)
	if err != nil {
		return err
	}
	failed, err := clusterNames(ctx, cl, pool, true)
	if err != nil {
		return err
	}
	for _, name := range names {
		if visible[name] != "" {
			return fmt.Errorf("the failed-create cluster %s is in the default listing; pickers and counts must not see half-provisioned clusters", name)
		}
		if failed[name] == "" {
			return fmt.Errorf("the failed-create cluster %s is missing from include_failed=true; an operator has no way to see or delete it", name)
		}
	}
	if err := includeFailedTotalConsistent(ctx, cl, pool); err != nil {
		return err
	}

	delTask, err := cl.DeleteClusterAndWait(ctx, pool, failed[names[0]])
	if err != nil {
		return fmt.Errorf("delete the failed-create cluster %s (%s) through the API: %w (task %s)", names[0], failed[names[0]], err, delTask.ID)
	}
	after, err := clusterNames(ctx, cl, pool, true)
	if err != nil {
		return err
	}
	if after[names[0]] != "" {
		return fmt.Errorf("cluster %s is still listed under include_failed=true after its delete task succeeded", names[0])
	}

	sweepStart := time.Now()
	err = a.f.t.WaitFor("the failed-create cluster to be swept", sweep, 30*time.Second, func() (bool, error) {
		got, err := clusterNames(ctx, cl, pool, true)
		if err != nil {
			return false, nil
		}
		return got[names[1]] == "", nil
	})
	if err != nil {
		return fmt.Errorf("cluster %s (%s) is still listed under include_failed=true %s after its create failed; the collector must sweep a failed-create cluster after gc-grace or orphans accumulate invisibly: %w", names[1], failed[names[1]], sweep, err)
	}
	a.f.t.Metric("reclaim_seconds", time.Since(sweepStart).Seconds(), "s")

	if _, err := cl.CreateClusterAndWait(ctx, pool, controller.CreateClusterRequest{Name: names[1], PgVersion: a.f.cfg.PgVersion}); err != nil {
		return fmt.Errorf("create a cluster under the swept name %s: %w", names[1], err)
	}
	fresh, err := findClusterCtl(ctx, cl, pool, names[1])
	if err != nil {
		return err
	}
	a.f.register("delete reclaimed-name cluster", func(ctx context.Context) error {
		_, e := cl.DeleteClusterAndWait(ctx, pool, fresh.ID)
		if apiErrorStatus(e) == http.StatusNotFound {
			return nil
		}
		return e
	})
	return nil
}

// clusterNames maps name to id over the whole pool listing, failed included or
// not.
func clusterNames(ctx context.Context, cl *controller.Client, pool string, includeFailed bool) (map[string]string, error) {
	out := map[string]string{}
	const page = 100
	for offset := 0; ; offset += page {
		var list controller.ClusterList
		var err error
		if includeFailed {
			list, err = cl.ListClustersIncludingFailed(ctx, pool, page, offset)
		} else {
			list, err = cl.ListClusters(ctx, pool, page, offset)
		}
		if err != nil {
			return nil, fmt.Errorf("list clusters (include_failed=%v): %w", includeFailed, err)
		}
		for _, c := range list.Clusters {
			out[c.Name] = c.ID
		}
		if list.Count < page || offset+len(list.Clusters) >= list.Total {
			return out, nil
		}
	}
}

// includeFailedTotalConsistent: the flag changes the total together with the
// page, so paging stays consistent.
func includeFailedTotalConsistent(ctx context.Context, cl *controller.Client, pool string) error {
	def, err := cl.ListClusters(ctx, pool, 1000, 0)
	if err != nil {
		return err
	}
	all, err := cl.ListClustersIncludingFailed(ctx, pool, 1000, 0)
	if err != nil {
		return err
	}
	if def.Total != len(def.Clusters) || all.Total != len(all.Clusters) {
		return fmt.Errorf("total disagrees with the page: default total=%d rows=%d, include_failed total=%d rows=%d", def.Total, len(def.Clusters), all.Total, len(all.Clusters))
	}
	if all.Total < def.Total+2 {
		return fmt.Errorf("include_failed=true total is %d against a default total of %d with two failed clusters present; the flag must widen the total with the page", all.Total, def.Total)
	}
	return nil
}
