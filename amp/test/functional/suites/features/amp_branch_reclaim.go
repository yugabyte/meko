package features

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// The reaper ticks every 30s and backs off to 8x that while nothing is due.
const branchReclaimTimeout = 15 * time.Minute

// The amp_storage tables the controller's branch-drop reaper deletes from
// (controller/internal/branchgc specs).
var reaperTables = []string{"branch_state", "pages", "wals", "pending_gc", "slru_pages", "meta", "blobs"}

func init() {
	core.Register(core.Test{
		Name:        "features/amp_branch_reclaim",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Branch-drop reclamation on a deployed stack: fork a branch, write on it, delete its endpoint and the branch, then move only that tombstone's reap_after to now (the retention is 24h). The controller's reaper must mark it gc_completed_at, leave no row for it in branch_state, pages, wals, pending_gc, slru_pages, meta or blobs, and leave the parent's data intact.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     40 * time.Minute,
		Run:         runAmpBranchReclaim,
	})
}

func runAmpBranchReclaim(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"branch_reclaim", a.branchReclaim}}
	})
}

func reaperRowCounts(ctx context.Context, storage *pg.Client, branchID string) (map[string]int64, error) {
	out := make(map[string]int64, len(reaperTables))
	for _, table := range reaperTables {
		n, err := storage.QueryInt(ctx, "SELECT count(*) FROM amp_storage."+table+" WHERE branch_id = $1::uuid", branchID)
		if err != nil {
			return nil, fmt.Errorf("count amp_storage.%s rows of branch %s: %w", table, branchID, err)
		}
		out[table] = n
	}
	return out, nil
}

func (a *ampFeatureRun) branchReclaim(ctx context.Context) error {
	storage, err := a.f.ampStorageSQL(ctx)
	if err != nil {
		if errors.Is(err, errNoStorageRoute) {
			a.f.t.Skip(err.Error())
			return nil
		}
		return core.AsEnvFailure(fmt.Errorf("reach the backing amp storage database: %w", err))
	}
	defer storage.Close()
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	proj := a.proj
	parent, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return err
	}
	pc, err := a.f.endpointSQL(ctx, parent)
	if err != nil {
		return err
	}
	defer pc.Close()
	for _, stmt := range []string{
		"CREATE TABLE amp_reclaim (id int PRIMARY KEY, v text NOT NULL)",
		"INSERT INTO amp_reclaim SELECT g, md5(g::text) FROM generate_series(1, 2000) g",
	} {
		if _, err := pc.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	sum := "SELECT md5(string_agg(id || v, ',' ORDER BY id)) FROM amp_reclaim"
	parentSum, err := pc.QueryString(ctx, sum)
	if err != nil {
		return err
	}
	lsn, err := currentLSN(ctx, pc)
	if err != nil {
		return err
	}

	name := "reclaim-" + a.f.suffix()
	child, err := forkChildAt(ctx, cl, proj, proj.branchID, lsn, name, a.f.endpointSQL)
	if err != nil {
		return err
	}
	childID, err := child.QueryString(ctx, "SELECT current_setting('yb_amp.branch_id')")
	if err != nil {
		child.Close()
		return err
	}
	for _, stmt := range []string{
		"INSERT INTO amp_reclaim SELECT g, 'child' FROM generate_series(2001, 6000) g",
		"CREATE TABLE amp_reclaim_child AS SELECT g AS id, repeat('x', 500) AS pad FROM generate_series(1, 5000) g",
		"CHECKPOINT",
	} {
		if _, err := child.Exec(ctx, stmt); err != nil {
			child.Close()
			return fmt.Errorf("write on the fork: %w", err)
		}
	}
	child.Close()

	var before map[string]int64
	err = pollUntil(ctx, 3*time.Minute, 5*time.Second, func() (bool, error) {
		var err error
		before, err = reaperRowCounts(ctx, storage, childID)
		return err == nil && before["pages"] > 0 && before["wals"] > 0, err
	})
	if err != nil {
		return fmt.Errorf("the fork %s never had pages and wals rows in amp_storage to reclaim (counts %v): %w", childID, before, err)
	}
	a.f.logf("amp_branch_reclaim: fork %s holds %v before the drop", childID, before)

	b, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, name)
	if err != nil {
		return err
	}
	eps, err := cl.ListAllEndpoints(ctx, proj.poolID, proj.projID, b.BranchID)
	if err != nil {
		return err
	}
	for _, e := range eps {
		if _, err := cl.DeleteEndpointAndWait(ctx, proj.poolID, proj.projID, b.BranchID, e.ID); err != nil {
			return fmt.Errorf("delete the fork's endpoint: %w", err)
		}
	}
	if _, err := cl.DeleteBranchAndWait(ctx, proj.poolID, proj.projID, b.BranchID); err != nil {
		return fmt.Errorf("drop the fork: %w", err)
	}

	moved, err := storage.Exec(ctx, `UPDATE amp_registry.branches SET reap_after = now()
		 WHERE branch_id = $1::uuid AND dropped_at IS NOT NULL AND gc_completed_at IS NULL`, childID)
	if err != nil {
		return fmt.Errorf("move the dropped fork's reap_after to now: %w", err)
	}
	if moved != 1 {
		return fmt.Errorf("amp_registry.branches has no tombstone for the dropped fork %s (updated %d rows): deleteBranch succeeded but the drop did not stamp dropped_at", childID, moved)
	}

	start := time.Now()
	err = pollUntil(ctx, branchReclaimTimeout, 10*time.Second, func() (bool, error) {
		n, err := storage.QueryInt(ctx, "SELECT count(*) FROM amp_registry.branches WHERE branch_id = $1::uuid AND gc_completed_at IS NOT NULL", childID)
		return n == 1, err
	})
	if err != nil {
		left, _ := reaperRowCounts(ctx, storage, childID)
		return fmt.Errorf("the reaper did not mark the dropped fork %s reclaimed within %s of its reap_after (rows left %v): %w", childID, branchReclaimTimeout, left, err)
	}
	a.f.t.Metric("reclaim_seconds", time.Since(start).Seconds(), "s")

	after, err := reaperRowCounts(ctx, storage, childID)
	if err != nil {
		return err
	}
	var left []string
	for _, table := range reaperTables {
		if after[table] > 0 {
			left = append(left, fmt.Sprintf("%s=%d", table, after[table]))
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("the reaper marked the dropped fork %s gc_completed_at but left rows behind: %s (before the drop %v)", childID, strings.Join(left, ", "), before)
	}

	got, err := pc.QueryString(ctx, sum)
	if err != nil {
		return fmt.Errorf("read the parent after its fork was reclaimed: %w", err)
	}
	if got != parentSum {
		return fmt.Errorf("the parent's rows changed after its fork was reclaimed")
	}
	return nil
}
