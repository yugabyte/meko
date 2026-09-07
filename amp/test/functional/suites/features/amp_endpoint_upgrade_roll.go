// The fleet roll (https://github.com/yugabyte/meko/issues/809): POST
// /endpoint-upgrades moves endpoints onto a version through one parent task with
// a child per endpoint.
//
// This test runs on the SHARED control plane, so it never rolls the whole fleet:
// every roll names its endpoints. The version has to actually change for the pod
// to move — a roll onto the recorded version re-renders an identical release and
// helm leaves the pod alone (seen on run 346f4971) — so the endpoints are rolled
// onto the previous release's endpoint image and back. The previous release is
// resolved the way amp_upgrade does it: from the report portal, or from
// functional.upgrade.from_endpoint_image.
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
	rollTable = "amp_roll_seed"
	rollRows  = 500
	// rollTaskBudget bounds one roll of two endpoints at max_parallel 1: two
	// pod replacements plus the helm waits around them.
	rollTaskBudget = 15 * time.Minute
	// rollUnknownEndpoint is a well-formed id no endpoint has.
	rollUnknownEndpoint = "00000000-0000-4000-8000-00000000dead"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_upgrade_roll",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Roll two named endpoints through POST /endpoint-upgrades onto the previous release's endpoint image and back, max_parallel 1, and assert each roll's parent task succeeds with one upgraded line and one succeeded upgrade_endpoint subtask per endpoint, both compute pods are replaced and run the target image, each endpoint records the target version and serves its seeded rows intact; a second roll while one is in flight is refused 409, an empty version and an out-of-range max_parallel are refused 400, and a roll naming an unknown endpoint reports it skipped rather than failing.",
		Params:      []string{"functional", "functional.upgrade", "report.portal", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     40 * time.Minute,
		Run:         runAmpEndpointUpgradeRoll,
	})
}

func runAmpEndpointUpgradeRoll(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		r := &rollRun{a: a}
		return []phase{
			{"seed_endpoints", r.seed},
			{"resolve_previous_release", r.resolvePrevious},
			{"roll_refusals", r.refusals},
			{"roll_to_previous_release", func(ctx context.Context) error { return r.roll(ctx, r.previous, true) }},
			{"roll_back_to_current", func(ctx context.Context) error { return r.roll(ctx, r.version, false) }},
			{"roll_unknown_endpoint_is_skipped", r.unknownSkipped},
		}
	})
}

type rollRun struct {
	a  *ampFeatureRun
	cl *controller.Client

	eps      []controller.Endpoint
	pods     map[string]string
	version  string
	previous string
}

// seed puts a read_write endpoint on the default branch and one on a fork, each
// with its own checksummed table, and reads the version they record.
func (r *rollRun) seed(ctx context.Context) error {
	cl, err := r.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	r.cl = cl
	proj := r.a.proj

	base, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create base endpoint: %w", err)
	}
	conn, err := r.a.f.endpointSQL(ctx, base)
	if err != nil {
		return fmt.Errorf("connect base endpoint: %w", err)
	}
	if err := seedRollTable(ctx, conn); err != nil {
		conn.Close()
		return err
	}
	forkLSN, err := currentLSN(ctx, conn)
	conn.Close()
	if err != nil {
		return err
	}
	childConn, err := forkChildAt(ctx, cl, proj, proj.branchID, forkLSN, "roll-fork-"+r.a.f.suffix(), r.a.f.endpointSQL)
	if err != nil {
		return err
	}
	childConn.Close()
	child, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, "roll-fork-"+r.a.f.suffix())
	if err != nil {
		return err
	}
	childEP, ok := lastEndpoint(ctx, cl, proj.poolID, proj.projID, child.BranchID)
	if !ok {
		return fmt.Errorf("the fork %s has no endpoint after forkChildAt", child.BranchID)
	}

	r.eps = []controller.Endpoint{base, childEP}
	for i, ep := range r.eps {
		got, err := cl.GetEndpoint(ctx, proj.poolID, proj.projID, ep.BranchID, ep.ID)
		if err != nil {
			return fmt.Errorf("read endpoint %s: %w", ep.ID, err)
		}
		r.eps[i] = got
	}
	r.version = strings.TrimSpace(r.eps[0].Version)
	if r.version == "" {
		r.a.f.t.Skip("this controller records no endpoint version (predates meko#809 PR 3), so there is nothing to roll to")
		return nil
	}
	for _, ep := range r.eps {
		if strings.TrimSpace(ep.Version) != r.version {
			return fmt.Errorf("endpoints created by the same controller record different versions: %s runs %q, %s runs %q", r.eps[0].ID, r.version, ep.ID, ep.Version)
		}
	}
	r.a.f.logf("endpoints %s and %s record version %q", r.eps[0].ID, r.eps[1].ID, r.version)

	r.pods = map[string]string{}
	if kube := r.a.f.ampKube(); kube != nil {
		roller := &kubeComputeRoller{kube: kube, timeout: restartReadyTimeout}
		for _, ep := range r.eps {
			pod, err := roller.podName(ctx, ep.ID)
			if err != nil {
				return fmt.Errorf("resolve the compute pod of endpoint %s before the roll: %w", ep.ID, err)
			}
			r.pods[ep.ID] = pod
		}
	}
	return nil
}

func seedRollTable(ctx context.Context, c sqlConn) error {
	if _, err := c.Exec(ctx, "CREATE TABLE "+rollTable+" (id int primary key, v bigint)"); err != nil {
		return fmt.Errorf("create %s: %w", rollTable, err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO "+rollTable+" SELECT g, g*7 FROM generate_series(1, $1) g", rollRows); err != nil {
		return fmt.Errorf("seed %s: %w", rollTable, err)
	}
	return verifyRollTable(ctx, c, "the freshly seeded endpoint")
}

func verifyRollTable(ctx context.Context, c sqlConn, where string) error {
	n, err := c.QueryInt(ctx, "SELECT count(*) FROM "+rollTable)
	if err != nil {
		return fmt.Errorf("count %s on %s: %w", rollTable, where, err)
	}
	if n != rollRows {
		return fmt.Errorf("%s serves %d rows of %s, want %d", where, n, rollTable, rollRows)
	}
	bad, err := c.QueryInt(ctx, "SELECT count(*) FROM "+rollTable+" WHERE v <> id::bigint * 7")
	if err != nil {
		return fmt.Errorf("checksum %s on %s: %w", rollTable, where, err)
	}
	if bad != 0 {
		return fmt.Errorf("%s serves %d rows of %s whose value no longer matches id*7", where, bad, rollTable)
	}
	return nil
}

// resolvePrevious names the endpoint image tag the endpoints are rolled onto
// first: functional.upgrade.from_endpoint_image, else the previous release the
// report portal lists relative to the version the endpoints record.
func (r *rollRun) resolvePrevious(ctx context.Context) error {
	if r.version == "" {
		return nil
	}
	opts := r.a.f.t.Options
	if from := strings.TrimSpace(opts.GetString("functional.upgrade.from_endpoint_image", "")); from != "" {
		r.previous = imageTag(from)
	} else {
		portal := upgradePortalURL(opts)
		if portal == "" {
			return core.AsEnvFailure(fmt.Errorf("no report portal to read the previous release from (report.portal.url and functional.upgrade.portal_url are both empty) and functional.upgrade.from_endpoint_image is empty; the roll needs a second endpoint version to move to"))
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
		return fmt.Errorf("the previous endpoint version resolved to %q, the version the endpoints already record; a roll onto the same version leaves the pod in place and proves nothing", r.previous)
	}
	r.a.f.logf("rolling %s -> %s -> %s", r.version, r.previous, r.version)
	return nil
}

// refusals: the request-level 400s. A 501 means no endpoint deployer, which is
// the one configuration this test cannot run against.
func (r *rollRun) refusals(ctx context.Context) error {
	if r.version == "" {
		return nil
	}
	_, _, err := r.cl.UpgradeEndpoints(ctx, controller.UpgradeEndpointsRequest{Version: "", EndpointIDs: []string{r.eps[0].ID}})
	switch status := apiErrorStatus(err); status {
	case http.StatusBadRequest:
	case http.StatusNotImplemented:
		r.a.f.t.Skip("this controller has no endpoint deployer, so it cannot roll endpoints: " + err.Error())
		return nil
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		r.a.f.t.Skip("this controller has no POST /endpoint-upgrades route: " + err.Error())
		return nil
	default:
		return fmt.Errorf("a roll with an empty version returned HTTP %d (%v), want 400", status, err)
	}
	_, _, err = r.cl.UpgradeEndpoints(ctx, controller.UpgradeEndpointsRequest{Version: r.version, EndpointIDs: []string{r.eps[0].ID}, MaxParallel: 51})
	if status := apiErrorStatus(err); status != http.StatusBadRequest {
		return fmt.Errorf("a roll with max_parallel 51 returned HTTP %d (%v), want 400", status, err)
	}
	return nil
}

// roll names both endpoints at max_parallel 1 and checks the report, the
// children, the replaced pods, the recorded version and the data. The first roll
// also proves a concurrent roll is refused.
func (r *rollRun) roll(ctx context.Context, target string, checkConcurrent bool) error {
	if r.version == "" {
		return nil
	}
	ids := []string{r.eps[0].ID, r.eps[1].ID}
	task, loc, err := r.cl.UpgradeEndpoints(ctx, controller.UpgradeEndpointsRequest{Version: target, EndpointIDs: ids, MaxParallel: 1})
	if err != nil {
		return fmt.Errorf("plan the roll of %v to %q: %w", ids, target, err)
	}
	taskID := controller.TaskRef(task, loc)

	// One roll at a time: the second is refused while the first is non-terminal.
	// Only probed once, since a probe that were admitted would be a real roll
	// nothing joins.
	if checkConcurrent {
		second, secondLoc, err := r.cl.UpgradeEndpoints(ctx, controller.UpgradeEndpointsRequest{Version: target, EndpointIDs: ids[:1]})
		if status := apiErrorStatus(err); status != http.StatusConflict {
			if err == nil {
				_, _ = r.cl.WaitTask(ctx, controller.TaskRef(second, secondLoc))
			}
			first, gerr := r.cl.GetTask(ctx, taskID)
			if gerr == nil && (first.Status == controller.TaskSucceeded || first.Status == controller.TaskFailed) {
				return fmt.Errorf("the first roll %s was already %s when the second was planned, so the one-roll-at-a-time refusal went untested; two pod replacements cannot finish that fast, check the roll actually did anything", taskID, first.Status)
			}
			return fmt.Errorf("a second roll planned while roll %s was in flight returned HTTP %d (%v), want 409: two rolls naming different versions would flip the fleet twice", taskID, status, err)
		}
	}

	wctx, cancel := context.WithTimeout(ctx, rollTaskBudget)
	done, err := r.cl.WaitTask(wctx, taskID)
	cancel()
	if err != nil {
		subs, _ := r.cl.ListSubtasks(ctx, taskID)
		return fmt.Errorf("roll %s of %v to %q did not succeed: %w (subtasks: %s)", taskID, ids, target, err, describeSubtasks(subs.Subtasks))
	}
	report, ok := done.RollReport()
	if !ok {
		return fmt.Errorf("the succeeded roll %s recorded no report in its result (%s)", taskID, string(done.Result))
	}
	if err := checkRollReport(report, target, ids, controller.RollUpgraded); err != nil {
		return fmt.Errorf("roll %s: %w", taskID, err)
	}

	subs, err := r.cl.ListSubtasks(ctx, taskID)
	if err != nil {
		return fmt.Errorf("list the subtasks of roll %s: %w", taskID, err)
	}
	if err := checkRollSubtasks(subs.Subtasks, ids); err != nil {
		return fmt.Errorf("roll %s: %w", taskID, err)
	}
	if n, err := r.cl.ListTasks(ctx, controller.ListTasksOptions{Kind: "upgrade_endpoint", Limit: 5}); err == nil && n.Total > 0 {
		return fmt.Errorf("%d upgrade_endpoint rows appear in GET /tasks: a roll's children are subtasks and must not be listed as tasks", n.Total)
	}

	for _, ep := range r.eps {
		got, err := r.cl.GetEndpoint(ctx, r.a.proj.poolID, r.a.proj.projID, ep.BranchID, ep.ID)
		if err != nil {
			return fmt.Errorf("read endpoint %s after the roll: %w", ep.ID, err)
		}
		if strings.TrimSpace(got.Version) != target {
			return fmt.Errorf("endpoint %s records version %q after a roll to %q", ep.ID, got.Version, target)
		}
		if before := r.pods[ep.ID]; before != "" {
			roller := &kubeComputeRoller{kube: r.a.f.ampKube(), timeout: restartReadyTimeout}
			if err := roller.waitReady(ctx, ep.ID); err != nil {
				return fmt.Errorf("endpoint %s compute is not ready after the roll: %w", ep.ID, err)
			}
			after, err := roller.podName(ctx, ep.ID)
			if err != nil {
				return fmt.Errorf("resolve the compute pod of endpoint %s after the roll: %w", ep.ID, err)
			}
			if after == before {
				return fmt.Errorf("endpoint %s still runs pod %s after the roll to %q reported it upgraded; a version change redeploys the compute, so its pod must have been replaced", ep.ID, before, target)
			}
			images, err := r.a.f.ampKube().Get(ctx, "pod", after, "{.spec.containers[*].image}")
			if err != nil {
				return fmt.Errorf("read the images of pod %s: %w", after, err)
			}
			if !strings.Contains(images, ":"+target) {
				return fmt.Errorf("endpoint %s records version %q but its pod %s runs %q", ep.ID, target, after, images)
			}
			r.a.f.logf("endpoint %s: pod %s -> %s (%s)", ep.ID, before, after, images)
			r.pods[ep.ID] = after
		}
		conn, err := r.a.f.endpointSQL(ctx, got)
		if err != nil {
			return fmt.Errorf("connect endpoint %s after the roll: %w", ep.ID, err)
		}
		err = verifyRollTable(ctx, conn, "endpoint "+ep.ID+" after the roll")
		conn.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// unknownSkipped: an id that no longer exists is a skipped line, not a refusal
// and not a failure, and it fans out to no child.
func (r *rollRun) unknownSkipped(ctx context.Context) error {
	if r.version == "" {
		return nil
	}
	task, loc, err := r.cl.UpgradeEndpoints(ctx, controller.UpgradeEndpointsRequest{Version: r.version, EndpointIDs: []string{rollUnknownEndpoint}})
	if err != nil {
		return fmt.Errorf("a roll naming only the unknown endpoint %s was refused (%v), want 202 with a skipped line", rollUnknownEndpoint, err)
	}
	taskID := controller.TaskRef(task, loc)
	wctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	done, err := r.cl.WaitTask(wctx, taskID)
	cancel()
	if err != nil {
		return fmt.Errorf("the roll %s naming only an unknown endpoint failed: %w; a roll is never refused because one endpoint cannot move", taskID, err)
	}
	report, ok := done.RollReport()
	if !ok {
		return fmt.Errorf("roll %s recorded no report (%s)", taskID, string(done.Result))
	}
	if err := checkRollReport(report, r.version, []string{rollUnknownEndpoint}, controller.RollSkipped); err != nil {
		return fmt.Errorf("roll %s: %w", taskID, err)
	}
	subs, err := r.cl.ListSubtasks(ctx, taskID)
	if err != nil {
		return fmt.Errorf("list subtasks of %s: %w", taskID, err)
	}
	if len(subs.Subtasks) != 0 {
		return fmt.Errorf("roll %s of an unknown endpoint created %d subtask(s), want none: a skipped endpoint needs no child", taskID, len(subs.Subtasks))
	}
	return nil
}

// checkRollReport asserts the report names exactly ids, every one with outcome.
func checkRollReport(report controller.RollReport, version string, ids []string, outcome string) error {
	if report.Version != version {
		return fmt.Errorf("report names version %q, want %q", report.Version, version)
	}
	if len(report.Endpoints) != len(ids) {
		return fmt.Errorf("report has %d endpoint lines, want %d: %+v", len(report.Endpoints), len(ids), report.Endpoints)
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	for _, line := range report.Endpoints {
		if !want[line.EndpointID] {
			return fmt.Errorf("report carries a line for endpoint %s the roll never named: %+v", line.EndpointID, report.Endpoints)
		}
		if line.Outcome != outcome {
			return fmt.Errorf("endpoint %s ended %q (%s), want %q", line.EndpointID, line.Outcome, line.Reason, outcome)
		}
	}
	counts := map[string]int{
		controller.RollUpgraded:    report.Upgraded,
		controller.RollSkipped:     report.Skipped,
		controller.RollFailed:      report.Failed,
		controller.RollUnattempted: report.Unattempted,
	}
	for k, n := range counts {
		wantN := 0
		if k == outcome {
			wantN = len(ids)
		}
		if n != wantN {
			return fmt.Errorf("report tallies %s=%d, want %d (report: upgraded=%d skipped=%d failed=%d unattempted=%d)", k, n, wantN, report.Upgraded, report.Skipped, report.Failed, report.Unattempted)
		}
	}
	return nil
}

// checkRollSubtasks asserts one succeeded upgrade_endpoint child per named id.
func checkRollSubtasks(subs []controller.Subtask, ids []string) error {
	if len(subs) != len(ids) {
		return fmt.Errorf("%d subtask(s) for %d named endpoints: %s", len(subs), len(ids), describeSubtasks(subs))
	}
	seen := map[string]bool{}
	for _, s := range subs {
		if s.Kind != "upgrade_endpoint" {
			return fmt.Errorf("subtask %s has kind %q, want upgrade_endpoint", s.ID, s.Kind)
		}
		if s.Status != controller.TaskSucceeded {
			msg := ""
			if s.Error != nil {
				msg = *s.Error
			}
			return fmt.Errorf("subtask %s for endpoint %s is %s: %s", s.ID, s.EndpointID(), s.Status, msg)
		}
		seen[s.EndpointID()] = true
	}
	for _, id := range ids {
		if !seen[id] {
			return fmt.Errorf("no subtask rolled endpoint %s: %s", id, describeSubtasks(subs))
		}
	}
	return nil
}

func describeSubtasks(subs []controller.Subtask) string {
	if len(subs) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(subs))
	for _, s := range subs {
		line := s.Kind + " " + s.EndpointID() + "=" + s.Status
		if s.Error != nil && *s.Error != "" {
			line += " (" + *s.Error + ")"
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "; ")
}
