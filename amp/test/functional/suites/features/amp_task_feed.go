// listTasks?updated_since is the change feed an incremental reader (the UI, the
// YCP reconciler) walks with a watermark. The contract that makes a watermark
// safe: ordered updated_at ascending, the bound inclusive, `ordered_by` saying
// so, and a task created long ago that only now went terminal showing up above
// the watermark, where the default created_at ordering would bury it.
package features

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	taskFeedOrderedByFeed    = "updated_at_asc"
	taskFeedOrderedByDefault = "created_at_desc"
	taskFeedFastTasks        = 3
	taskFeedAttempts         = 5
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_task_feed",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "gapcoverage"},
		OwningTeam:  core.TeamControlPlane,
		Description: "The listTasks change feed: with updated_since the page says ordered_by=updated_at_asc and is sorted so, the bound is inclusive, an updated_since after every task is an empty page, the default listing says created_at_desc or omits ordered_by; and a long create_endpoint task planned before three quick branch forks reappears above a watermark taken after the forks once it finishes, which the default ordering would hide below them.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     45 * time.Minute,
		Run:         runAmpTaskFeed,
	})
}

func runAmpTaskFeed(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"task_feed", a.taskFeed}}
	})
}

func (a *ampFeatureRun) taskFeed(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	proj := a.proj
	feed := func(since time.Time) (controller.TaskList, error) {
		return cl.ListTasks(ctx, controller.ListTasksOptions{ClusterID: proj.projID, UpdatedSince: since, Limit: 1000})
	}

	// A writer on the default branch first: it gives every fork below a fork
	// point (a parent with no compute has none), and its LSN keeps them
	// deterministic.
	writer, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create the writer the fork point comes from: %w", err)
	}
	conn, err := a.f.endpointSQL(ctx, writer)
	if err != nil {
		return fmt.Errorf("connect the writer: %w", err)
	}
	lsn, err := currentLSN(ctx, conn)
	conn.Close()
	if err != nil {
		return err
	}

	// The long task: an endpoint deploy on a branch of its own, planned before the
	// quick forks and left running through them. Its row is created first and goes
	// terminal last, which is exactly the shape the change feed exists for.
	var (
		epTaskID string
		forkIDs  []string
	)
	for attempt := 1; ; attempt++ {
		slowBranch := fmt.Sprintf("feed-slow-%s-%d", a.f.suffix(), attempt)
		if err := createBranchRetrying(ctx, cl, proj.poolID, proj.projID, controller.CreateBranchRequest{
			Name: slowBranch, ParentBranchID: proj.branchID, ForkPointLSN: lsn,
		}); err != nil {
			return fmt.Errorf("fork %s: %w", slowBranch, err)
		}
		slow, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, slowBranch)
		if err != nil {
			return err
		}
		epTask, epLoc, err := cl.CreateEndpoint(ctx, proj.poolID, proj.projID, slow.BranchID, controller.CreateEndpointRequest{Role: roleReadWrite})
		if err != nil {
			return fmt.Errorf("plan the endpoint create: %w", err)
		}
		epTaskID = controller.TaskRef(epTask, epLoc)

		// Serial forks add up to more than the ~15s endpoint deploy, so they go out
		// together. A single call stalled ~30s on the exec relay still loses the
		// attempt, which taskFeedAttempts absorbs.
		forkIDs = make([]string, taskFeedFastTasks)
		dones := make([]controller.Task, taskFeedFastTasks)
		errs := make([]error, taskFeedFastTasks)
		var wg sync.WaitGroup
		for i := range taskFeedFastTasks {
			wg.Add(1)
			go func() {
				defer wg.Done()
				name := fmt.Sprintf("feed-%s-%d-%d", a.f.suffix(), attempt, i)
				task, loc, err := cl.CreateBranch(ctx, proj.poolID, proj.projID, controller.CreateBranchRequest{
					Name: name, ParentBranchID: proj.branchID, ForkPointLSN: lsn,
				})
				if err != nil {
					errs[i] = fmt.Errorf("fork %s: %w", name, err)
					return
				}
				forkIDs[i] = controller.TaskRef(task, loc)
				if dones[i], err = cl.WaitTask(ctx, forkIDs[i]); err != nil {
					errs[i] = fmt.Errorf("fork %s task %s: %w", name, forkIDs[i], err)
				}
			}()
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return err
		}
		sort.Slice(dones, func(i, j int) bool { return dones[i].UpdatedAt.Before(dones[j].UpdatedAt) })
		for i, d := range dones {
			forkIDs[i] = d.ID
		}
		lastFork := dones[len(dones)-1].UpdatedAt

		// A deploy that lands before the last fork leaves nothing above the
		// watermark to look for; that is timing, not a feed defect.
		ep, err := cl.GetTask(ctx, epTaskID)
		if err != nil {
			return fmt.Errorf("read the endpoint create task %s: %w", epTaskID, err)
		}
		if (ep.Status != controller.TaskSucceeded && ep.Status != controller.TaskFailed) || ep.UpdatedAt.After(lastFork) {
			break
		}
		if attempt == taskFeedAttempts {
			return fmt.Errorf("in %d attempts the endpoint create went terminal before the last of %d forks (last: %s at %s, fork done %s), so the scenario never formed", attempt, taskFeedFastTasks, epTaskID, ep.UpdatedAt, lastFork)
		}
		a.f.logf("attempt %d: endpoint create %s went %s at %s, before the last fork at %s; planning another", attempt, epTaskID, ep.Status, ep.UpdatedAt, lastFork)
	}

	def, err := cl.ListTasks(ctx, controller.ListTasksOptions{ClusterID: proj.projID, Limit: 1000})
	if err != nil {
		return fmt.Errorf("default listing: %w", err)
	}
	if def.OrderedBy != "" && def.OrderedBy != taskFeedOrderedByDefault {
		return fmt.Errorf("the default listing reports ordered_by=%q, want %q or absent", def.OrderedBy, taskFeedOrderedByDefault)
	}
	for i := 1; i < len(def.Tasks); i++ {
		if def.Tasks[i].CreatedAt.After(def.Tasks[i-1].CreatedAt) {
			return fmt.Errorf("the default listing is not created_at descending: %s (%s) follows %s (%s)", def.Tasks[i].ID, def.Tasks[i].CreatedAt, def.Tasks[i-1].ID, def.Tasks[i-1].CreatedAt)
		}
	}

	if len(def.Tasks) == 0 {
		return fmt.Errorf("listTasks?cluster_id=%s returned nothing although this test planned a create_endpoint and %d forks against it", proj.projID, taskFeedFastTasks)
	}
	forks := map[string]controller.Task{}
	for _, t := range def.Tasks {
		forks[t.ID] = t
	}
	oldest := def.Tasks[len(def.Tasks)-1].UpdatedAt
	all, err := feed(oldest.Add(-time.Minute))
	if err != nil {
		return fmt.Errorf("feed since before every task: %w", err)
	}
	if all.OrderedBy != taskFeedOrderedByFeed {
		if all.OrderedBy == "" {
			a.f.t.Skip("this controller ignores updated_since (no ordered_by in the reply), so it predates the change feed")
			return nil
		}
		return fmt.Errorf("a feed page reports ordered_by=%q, want %q", all.OrderedBy, taskFeedOrderedByFeed)
	}
	if err := ascendingByUpdatedAt(all.Tasks); err != nil {
		return err
	}
	for _, id := range forkIDs {
		if !hasTask(all.Tasks, id) {
			return fmt.Errorf("the feed since before every task omits fork task %s", id)
		}
	}

	// Inclusive bound: since = the middle fork's updated_at must return it.
	mid, err := cl.GetTask(ctx, forkIDs[1])
	if err != nil {
		return fmt.Errorf("read fork task %s: %w", forkIDs[1], err)
	}
	fromMid, err := feed(mid.UpdatedAt)
	if err != nil {
		return fmt.Errorf("feed since %s: %w", mid.UpdatedAt, err)
	}
	if !hasTask(fromMid.Tasks, mid.ID) {
		return fmt.Errorf("updated_since=%s (the exact updated_at of task %s) omits that task: the bound must be inclusive or a same-instant twin is skipped by every watermark reader", mid.UpdatedAt.Format(time.RFC3339Nano), mid.ID)
	}
	if hasTask(fromMid.Tasks, forkIDs[0]) && forks[forkIDs[0]].UpdatedAt.Before(mid.UpdatedAt) {
		return fmt.Errorf("updated_since=%s returned task %s updated earlier at %s", mid.UpdatedAt.Format(time.RFC3339Nano), forkIDs[0], forks[forkIDs[0]].UpdatedAt)
	}

	// The watermark an incremental reader would hold after consuming the forks.
	// Not the endpoint create's own: it may already be terminal in this page.
	watermark := newestUpdatedAt(all.Tasks, epTaskID)
	empty, err := feed(watermark.Add(time.Second))
	if err != nil {
		return fmt.Errorf("feed past the watermark: %w", err)
	}
	if n := countNotID(empty.Tasks, epTaskID); n != 0 {
		return fmt.Errorf("a feed from after the newest updated_at still returns %d task(s) other than the in-flight endpoint create", n)
	}

	// The long task lands after the watermark. The feed must surface it; the
	// default order puts it below the forks (created first), which is the bug the
	// feed exists to avoid.
	epDone, err := cl.WaitTask(ctx, epTaskID)
	if err != nil {
		return fmt.Errorf("endpoint create task %s: %w", epTaskID, err)
	}
	if !epDone.UpdatedAt.After(watermark) {
		return fmt.Errorf("the endpoint task went terminal at %s, not after the watermark %s; it did not outlive the forks so the scenario did not form", epDone.UpdatedAt, watermark)
	}
	after, err := feed(watermark)
	if err != nil {
		return fmt.Errorf("feed since the watermark: %w", err)
	}
	if !hasTask(after.Tasks, epTaskID) {
		return fmt.Errorf("the create_endpoint task %s (created before the forks, finished after them) is missing from the feed since %s: a watermark reader would never see it finish", epTaskID, watermark.Format(time.RFC3339Nano))
	}
	def2, err := cl.ListTasks(ctx, controller.ListTasksOptions{ClusterID: proj.projID, Limit: 1000})
	if err != nil {
		return fmt.Errorf("default listing after the endpoint create: %w", err)
	}
	pos := indexOf(def2.Tasks, epTaskID)
	for _, id := range forkIDs {
		if fp := indexOf(def2.Tasks, id); fp >= 0 && pos >= 0 && fp > pos {
			return fmt.Errorf("in the default listing fork task %s sits below the endpoint task created before it; the default order is not created_at descending", id)
		}
	}
	a.f.logf("feed surfaced %s above the watermark; default listing puts it at position %d behind %d forks", epTaskID, pos, len(forkIDs))
	return nil
}

func ascendingByUpdatedAt(ts []controller.Task) error {
	if sort.SliceIsSorted(ts, func(i, j int) bool { return ts[i].UpdatedAt.Before(ts[j].UpdatedAt) }) {
		return nil
	}
	for i := 1; i < len(ts); i++ {
		if ts[i].UpdatedAt.Before(ts[i-1].UpdatedAt) {
			return fmt.Errorf("the feed is not updated_at ascending: %s (%s) follows %s (%s)", ts[i].ID, ts[i].UpdatedAt, ts[i-1].ID, ts[i-1].UpdatedAt)
		}
	}
	return nil
}

func hasTask(ts []controller.Task, id string) bool { return indexOf(ts, id) >= 0 }

func indexOf(ts []controller.Task, id string) int {
	for i, t := range ts {
		if t.ID == id {
			return i
		}
	}
	return -1
}

func countNotID(ts []controller.Task, id string) int {
	n := 0
	for _, t := range ts {
		if t.ID != id {
			n++
		}
	}
	return n
}

func newestUpdatedAt(ts []controller.Task, skipID string) time.Time {
	var newest time.Time
	for _, t := range ts {
		if t.ID != skipID && t.UpdatedAt.After(newest) {
			newest = t.UpdatedAt
		}
	}
	return newest
}
