// meko#1140 asks for a rollback policy: refuse a roll to an earlier version
// across a PG major change, allow it otherwise. Nothing implements it yet, so
// both contracts are asserted and the one taken is recorded as rollback.refused.
package features

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/releases"
)

const (
	rollbackTable  = "amp_rollback_seed"
	rollbackRows   = 300
	rollbackBudget = 15 * time.Minute
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_rollback",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Roll one endpoint back to the previous release's version and assert whichever contract the controller holds to: a refusal that names a reason and leaves the endpoint on its version, its pod and its data untouched, or a performed rollback that actually moves the endpoint onto the earlier image with its data intact (today's behaviour, meko#1140).",
		Params:      []string{"functional", "functional.upgrade", "report.portal", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     40 * time.Minute,
		Run:         runAmpEndpointRollback,
	})
}

type rollbackRun struct {
	a  *ampFeatureRun
	cl *controller.Client

	ep       controller.Endpoint
	pod      string
	version  string
	previous string
}

func runAmpEndpointRollback(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		r := &rollbackRun{a: a}
		return []phase{
			{"seed_endpoint", r.seed},
			{"resolve_previous_release", r.resolvePrevious},
			{"roll_back_to_previous_release", r.rollBack},
		}
	})
}

func (r *rollbackRun) seed(ctx context.Context) error {
	cl, err := r.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	r.cl = cl
	proj := r.a.proj
	ep, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create the endpoint to roll back: %w", err)
	}
	conn, err := r.a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect the endpoint to roll back: %w", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(ctx, "CREATE TABLE "+rollbackTable+" (id int primary key, v bigint)"); err != nil {
		return fmt.Errorf("create %s: %w", rollbackTable, err)
	}
	if _, err := conn.Exec(ctx, "INSERT INTO "+rollbackTable+" SELECT g, g*7 FROM generate_series(1, $1) g", rollbackRows); err != nil {
		return fmt.Errorf("seed %s: %w", rollbackTable, err)
	}
	got, err := cl.GetEndpoint(ctx, proj.poolID, proj.projID, ep.BranchID, ep.ID)
	if err != nil {
		return fmt.Errorf("read endpoint %s back: %w", ep.ID, err)
	}
	r.ep = got
	r.version = strings.TrimSpace(got.Version)
	if r.version == "" {
		r.a.f.t.Skip("this controller records no endpoint version (predates meko#809 PR 3), so there is nothing to roll back from")
		return nil
	}
	if kube := r.a.f.ampKube(); kube != nil {
		pod, err := (&kubeComputeRoller{kube: kube, timeout: restartReadyTimeout}).podName(ctx, got.ID)
		if err != nil {
			return fmt.Errorf("resolve the compute pod of endpoint %s: %w", got.ID, err)
		}
		r.pod = pod
	}
	return nil
}

func (r *rollbackRun) resolvePrevious(ctx context.Context) error {
	if r.version == "" {
		return nil
	}
	opts := r.a.f.t.Options
	if from := strings.TrimSpace(opts.GetString("functional.upgrade.from_endpoint_image", "")); from != "" {
		r.previous = imageTag(from)
	} else {
		portal := upgradePortalURL(opts)
		if portal == "" {
			return core.AsEnvFailure(fmt.Errorf("no report portal to read the previous release from (report.portal.url and functional.upgrade.portal_url are both empty) and functional.upgrade.from_endpoint_image is empty; a rollback needs an earlier version to roll to"))
		}
		fctx, cancel := context.WithTimeout(ctx, upgradeReleaseFetchTimeout)
		defer cancel()
		list, err := releases.New(portal).WithLogf(r.a.f.logf).List(fctx, upgradeReleaseListLimit)
		if err != nil {
			return core.AsEnvFailure(fmt.Errorf("read the release list from %q: %w", portal, err))
		}
		prev, err := previousRelease(list, r.version)
		if err != nil {
			return core.AsEnvFailure(err)
		}
		r.previous = prev.Tag
	}
	if r.previous == r.version {
		return fmt.Errorf("the previous release resolved to %q, the version the endpoint already records: that is not a rollback", r.previous)
	}
	return nil
}

func (r *rollbackRun) rollBack(ctx context.Context) error {
	if r.version == "" {
		return nil
	}
	proj := r.a.proj
	task, loc, err := r.cl.UpgradeEndpoints(ctx, controller.UpgradeEndpointsRequest{
		Version: r.previous, EndpointIDs: []string{r.ep.ID},
	})
	if status := apiErrorStatus(err); status >= 400 && status < 500 {
		if status == http.StatusNotImplemented || status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
			r.a.f.t.Skip("this controller cannot roll endpoints at all: " + err.Error())
			return nil
		}
		return r.refused(ctx, fmt.Sprintf("HTTP %d: %v", status, err))
	}
	if err != nil {
		return fmt.Errorf("plan the rollback of endpoint %s from %q to %q: %w", r.ep.ID, r.version, r.previous, err)
	}
	taskID := controller.TaskRef(task, loc)
	wctx, cancel := context.WithTimeout(ctx, rollbackBudget)
	done, waitErr := r.cl.WaitTask(wctx, taskID)
	cancel()
	if waitErr != nil {
		subs, _ := r.cl.ListSubtasks(ctx, taskID)
		return r.refused(ctx, fmt.Sprintf("the roll task %s did not succeed: %v (subtasks: %s)", taskID, waitErr, describeSubtasks(subs.Subtasks)))
	}
	report, ok := done.RollReport()
	if !ok {
		return fmt.Errorf("the succeeded rollback %s recorded no report (%s)", taskID, string(done.Result))
	}
	line, ok := rollLineFor(report, r.ep.ID)
	if !ok {
		return fmt.Errorf("the rollback %s naming endpoint %s enumerated no line for it (report: %+v)", taskID, r.ep.ID, report.Endpoints)
	}
	if line.Outcome != controller.RollUpgraded {
		return r.refused(ctx, fmt.Sprintf("the roll reported the endpoint %q: %s", line.Outcome, line.Reason))
	}

	r.a.f.logf("rollback to %q was performed, not refused (meko#1140 is not implemented)", r.previous)
	r.a.f.t.Metric("rollback.refused", 0, "bool")
	got, err := r.cl.GetEndpoint(ctx, proj.poolID, proj.projID, r.ep.BranchID, r.ep.ID)
	if err != nil {
		return fmt.Errorf("read endpoint %s after the rollback: %w", r.ep.ID, err)
	}
	if strings.TrimSpace(got.Version) != r.previous {
		return fmt.Errorf("the rollback of endpoint %s reported it upgraded to %q, but the endpoint records %q: an endpoint the controller says it moved has to be where it says", r.ep.ID, r.previous, got.Version)
	}
	if r.pod != "" {
		roller := &kubeComputeRoller{kube: r.a.f.ampKube(), timeout: restartReadyTimeout}
		if err := roller.waitReady(ctx, r.ep.ID); err != nil {
			return fmt.Errorf("endpoint %s compute is not ready after the rollback: %w", r.ep.ID, err)
		}
		after, err := roller.podName(ctx, r.ep.ID)
		if err != nil {
			return fmt.Errorf("resolve the compute pod of endpoint %s after the rollback: %w", r.ep.ID, err)
		}
		if after == r.pod {
			return fmt.Errorf("endpoint %s still runs pod %s after a rollback to %q reported it upgraded; a version change redeploys the compute", r.ep.ID, r.pod, r.previous)
		}
		images, err := r.a.f.ampKube().Get(ctx, "pod", after, "{.spec.containers[*].image}")
		if err != nil {
			return fmt.Errorf("read the images of pod %s: %w", after, err)
		}
		if !strings.Contains(images, ":"+r.previous) {
			return fmt.Errorf("endpoint %s records %q after the rollback but its pod %s runs %q", r.ep.ID, r.previous, after, images)
		}
		r.pod = after
	}
	r.ep = got
	return r.serves(ctx, "the endpoint rolled back to "+r.previous)
}

func (r *rollbackRun) refused(ctx context.Context, reason string) error {
	r.a.f.logf("rollback to %q was refused: %s", r.previous, reason)
	r.a.f.t.Metric("rollback.refused", 1, "bool")
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("the rollback of endpoint %s from %q to %q was refused with no reason at all; an operator reaching for a rollback has to be told why it was denied (meko#1140)", r.ep.ID, r.version, r.previous)
	}
	if !strings.Contains(reason, r.previous) && !strings.Contains(strings.ToLower(reason), "version") && !strings.Contains(strings.ToLower(reason), "downgrade") {
		return fmt.Errorf("the rollback of endpoint %s from %q to %q was refused with %q, which names neither the target version nor the rule it broke (meko#1140)", r.ep.ID, r.version, r.previous, reason)
	}
	got, err := r.cl.GetEndpoint(ctx, r.a.proj.poolID, r.a.proj.projID, r.ep.BranchID, r.ep.ID)
	if err != nil {
		return fmt.Errorf("read endpoint %s after the refused rollback: %w", r.ep.ID, err)
	}
	if strings.TrimSpace(got.Version) != r.version {
		return fmt.Errorf("the rollback was refused (%s) but endpoint %s now records version %q instead of %q: a refused roll must leave the endpoint where it was", reason, r.ep.ID, got.Version, r.version)
	}
	if r.pod != "" {
		after, err := (&kubeComputeRoller{kube: r.a.f.ampKube(), timeout: restartReadyTimeout}).podName(ctx, r.ep.ID)
		if err != nil {
			return fmt.Errorf("resolve the compute pod of endpoint %s after the refused rollback: %w", r.ep.ID, err)
		}
		if after != r.pod {
			return fmt.Errorf("the rollback was refused (%s) but endpoint %s moved from pod %s to %s: a refused roll must not replace the compute", reason, r.ep.ID, r.pod, after)
		}
	}
	return r.serves(ctx, "the endpoint whose rollback was refused")
}

func (r *rollbackRun) serves(ctx context.Context, where string) error {
	conn, err := r.a.f.endpointSQL(ctx, r.ep)
	if err != nil {
		return fmt.Errorf("connect %s: %w", where, err)
	}
	defer conn.Close()
	n, err := conn.QueryInt(ctx, "SELECT count(*) FROM "+rollbackTable)
	if err != nil {
		return fmt.Errorf("count %s on %s: %w", rollbackTable, where, err)
	}
	if n != rollbackRows {
		return fmt.Errorf("%s serves %d rows of %s, want %d", where, n, rollbackTable, rollbackRows)
	}
	bad, err := conn.QueryInt(ctx, "SELECT count(*) FROM "+rollbackTable+" WHERE v <> id::bigint * 7")
	if err != nil {
		return fmt.Errorf("checksum %s on %s: %w", rollbackTable, where, err)
	}
	if bad != 0 {
		return fmt.Errorf("%s serves %d rows of %s whose value no longer matches id*7", where, bad, rollbackTable)
	}
	return nil
}

func rollLineFor(report controller.RollReport, endpointID string) (controller.RollLine, bool) {
	for i := range report.Endpoints {
		if report.Endpoints[i].EndpointID == endpointID {
			return report.Endpoints[i], true
		}
	}
	return controller.RollLine{}, false
}
