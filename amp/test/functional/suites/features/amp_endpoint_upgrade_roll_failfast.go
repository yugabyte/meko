// The fleet roll's fail-fast contract (ac-endpoint-upgrade.md §5.4), which the
// happy-path roll test never reaches: a child failing stops new children from
// starting while the in-flight ones finish, the endpoints it never reached keep
// their version and keep serving, and a roll back onto the good version repairs
// the fleet. A failed roll keeps its per-endpoint account in the subtasks rather
// than the step result, so that is where this reads it.
//
// The poison is a version tag no registry has (#982: nothing prechecks it), so
// every attempted child fails at the deploy. Production rolls run max_parallel 4
// by default; this one uses 2 over three endpoints so exactly one is never
// attempted.
package features

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	failfastEndpoints = 3
	failfastParallel  = 2
	// failfastRollBudget bounds the poisoned roll: the attempted children wait out
	// the controller's deploy timeout (35m on the QA control plane) in parallel.
	failfastRollBudget = 50 * time.Minute
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_upgrade_roll_failfast",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane, SharedLock: core.LockEndpointFleetUpgrade},
		Tags:        []string{"regression", "gapcoverage"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Roll three seeded endpoints at max_parallel 2 onto a version tag no registry carries: the parent task fails and its error carries the tally and the first failure, nothing is reported upgraded, no more endpoints failed than ran at once so a failing child stopped new ones starting, at least one was left unattempted, every targeted endpoint has a failed upgrade_endpoint subtask and the unattempted ones say they never started, those endpoints still record the original version and serve their rows; a second roll onto the original version then succeeds for all three with every endpoint serving its seeded rows intact.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     150 * time.Minute,
		Run:         runAmpEndpointUpgradeRollFailfast,
	})
}

func runAmpEndpointUpgradeRollFailfast(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		r := &failfastRun{a: a}
		return []phase{
			{"seed_endpoints", r.seed},
			{"roll_to_missing_version", r.poisonedRoll},
			{"roll_back_repairs", r.repair},
		}
	})
}

type failfastRun struct {
	a       *ampFeatureRun
	cl      *controller.Client
	eps     []controller.Endpoint
	version string
	poison  string
}

func (r *failfastRun) seed(ctx context.Context) error {
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
	lsn, err := currentLSN(ctx, conn)
	conn.Close()
	if err != nil {
		return err
	}
	r.eps = []controller.Endpoint{base}
	for i := 1; i < failfastEndpoints; i++ {
		name := fmt.Sprintf("ff-fork-%s-%d", r.a.f.suffix(), i)
		c, err := forkChildAt(ctx, cl, proj, proj.branchID, lsn, name, r.a.f.endpointSQL)
		if err != nil {
			return err
		}
		c.Close()
		child, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, name)
		if err != nil {
			return err
		}
		ep, ok := lastEndpoint(ctx, cl, proj.poolID, proj.projID, child.BranchID)
		if !ok {
			return fmt.Errorf("fork %s has no endpoint", name)
		}
		r.eps = append(r.eps, ep)
	}
	for i, ep := range r.eps {
		got, err := cl.GetEndpoint(ctx, proj.poolID, proj.projID, ep.BranchID, ep.ID)
		if err != nil {
			return fmt.Errorf("read endpoint %s: %w", ep.ID, err)
		}
		r.eps[i] = got
	}
	r.version = strings.TrimSpace(r.eps[0].Version)
	if r.version == "" {
		r.a.f.t.Skip("this controller records no endpoint version, so there is nothing to roll")
		return nil
	}
	r.poison = "0.0.0-missing-" + r.a.f.suffix()
	return nil
}

func (r *failfastRun) ids() []string {
	ids := make([]string, len(r.eps))
	for i, ep := range r.eps {
		ids[i] = ep.ID
	}
	return ids
}

func (r *failfastRun) poisonedRoll(ctx context.Context) error {
	if r.version == "" {
		return nil
	}
	task, loc, err := r.cl.UpgradeEndpoints(ctx, controller.UpgradeEndpointsRequest{Version: r.poison, EndpointIDs: r.ids(), MaxParallel: failfastParallel})
	switch status := apiErrorStatus(err); {
	case err == nil:
	case status == http.StatusBadRequest && strings.Contains(strings.ToLower(err.Error()), "version"):
		r.a.f.t.Skip("this controller prechecks the target version, so a missing tag cannot poison a roll: " + err.Error())
		return nil
	case status == http.StatusNotImplemented || status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		r.a.f.t.Skip("this controller cannot roll endpoints: " + err.Error())
		return nil
	default:
		return fmt.Errorf("plan the roll onto %q: %w", r.poison, err)
	}
	taskID := controller.TaskRef(task, loc)
	wctx, cancel := context.WithTimeout(ctx, failfastRollBudget)
	done, err := r.cl.WaitTask(wctx, taskID)
	cancel()
	if done.Status == "" {
		return fmt.Errorf("the poisoned roll %s did not settle within %s: %v", taskID, failfastRollBudget, err)
	}
	r.a.f.logf("poisoned roll %s settled %s: %v", taskID, done.Status, err)
	if done.Status != controller.TaskFailed {
		return fmt.Errorf("the roll %s onto %q ended %q; no registry carries that tag, and a roll that left every endpoint where it was cannot have converged the fleet", taskID, r.poison, done.Status)
	}
	// A failed roll's per-endpoint account is the subtasks, not the step result:
	// the worker records a failed step's cause rather than its output
	// (ac-endpoint-upgrade.md §"Settled while building §5-6"). The parent's error
	// carries the tally; the children carry the detail.
	if done.Error == nil || strings.TrimSpace(*done.Error) == "" {
		return fmt.Errorf("the failed roll %s recorded no error; on a failed roll it is the whole account the parent gives", taskID)
	}
	parentErr := *done.Error
	r.a.f.logf("parent error: %s", parentErr)
	tally, err := parseRollTally(parentErr)
	if err != nil {
		return fmt.Errorf("roll %s: %w", taskID, err)
	}
	if tally["upgraded"] != 0 {
		return fmt.Errorf("the roll reports %d upgraded for a version no registry carries: %s", tally["upgraded"], parentErr)
	}
	if sum := tally["upgraded"] + tally["skipped"] + tally["failed"] + tally["unattempted"]; sum != failfastEndpoints {
		return fmt.Errorf("the tally accounts for %d endpoints, want %d: %s", sum, failfastEndpoints, parentErr)
	}
	if tally["failed"] == 0 {
		return fmt.Errorf("the roll reports nothing failed although every attempted deploy had to fail: %s", parentErr)
	}
	// Fail-fast: a failing child stops new ones starting, so the roll cannot have
	// deployed more endpoints than it had running at once, and the rest must be
	// left unattempted rather than tried and failed.
	if tally["failed"] > failfastParallel {
		return fmt.Errorf("%d endpoints failed at max_parallel %d, so a failing child did not stop new ones from starting: %s", tally["failed"], failfastParallel, parentErr)
	}
	if tally["unattempted"] == 0 {
		return fmt.Errorf("no endpoint was left unattempted at max_parallel %d over %d endpoints with every deploy failing; that is what fail-fast is for: %s", failfastParallel, failfastEndpoints, parentErr)
	}
	if !strings.Contains(parentErr, "first failure: endpoint ") {
		return fmt.Errorf("the parent error names no first failure, so an operator cannot tell which endpoint broke the roll: %s", parentErr)
	}

	subs, err := r.cl.ListSubtasks(ctx, taskID)
	if err != nil {
		return fmt.Errorf("list subtasks of %s: %w", taskID, err)
	}
	if len(subs.Subtasks) != failfastEndpoints {
		return fmt.Errorf("%d subtasks for %d targeted endpoints; every endpoint the roll named owes an account (%s)", len(subs.Subtasks), failfastEndpoints, describeSubtasks(subs.Subtasks))
	}
	notStarted := map[string]bool{}
	for _, sub := range subs.Subtasks {
		if sub.Kind != "upgrade_endpoint" {
			return fmt.Errorf("subtask %s has kind %q, want upgrade_endpoint", sub.ID, sub.Kind)
		}
		if sub.Status != controller.TaskFailed {
			return fmt.Errorf("subtask %s for endpoint %s is %s under a version no registry carries, want failed", sub.ID, sub.EndpointID(), sub.Status)
		}
		if sub.Error != nil && strings.Contains(*sub.Error, rollNotStarted) {
			notStarted[sub.EndpointID()] = true
		}
	}
	if len(notStarted) != tally["unattempted"] {
		return fmt.Errorf("%d subtasks say %q but the parent tallies %d unattempted (%s)", len(notStarted), rollNotStarted, tally["unattempted"], describeSubtasks(subs.Subtasks))
	}
	for id := range notStarted {
		if err := r.untouched(ctx, id); err != nil {
			return err
		}
	}
	r.a.f.logf("fail-fast: %d failed, %d never started, all %d accounted for", tally["failed"], tally["unattempted"], failfastEndpoints)
	return nil
}

// rollNotStarted is how a child the roll never ran records that fact.
const rollNotStarted = "not started"

// parseRollTally reads the counts out of a failed roll's parent error, which is
// where they live once the step result is gone.
func parseRollTally(msg string) (map[string]int, error) {
	out := map[string]int{}
	for _, kind := range []string{"upgraded", "skipped", "failed", "unattempted"} {
		m := regexp.MustCompile(`(\d+) ` + kind).FindStringSubmatch(msg)
		if m == nil {
			return nil, fmt.Errorf("the parent error carries no %q count, so the roll's outcome cannot be read from it: %s", kind, msg)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("unreadable %q count in %q", kind, msg)
		}
		out[kind] = n
	}
	return out, nil
}

// untouched: an unattempted endpoint records its old version and still serves.
func (r *failfastRun) untouched(ctx context.Context, id string) error {
	for _, ep := range r.eps {
		if ep.ID != id {
			continue
		}
		got, err := r.cl.GetEndpoint(ctx, r.a.proj.poolID, r.a.proj.projID, ep.BranchID, ep.ID)
		if err != nil {
			return fmt.Errorf("read unattempted endpoint %s: %w", id, err)
		}
		if strings.TrimSpace(got.Version) != r.version {
			return fmt.Errorf("unattempted endpoint %s records version %q, want the untouched %q", id, got.Version, r.version)
		}
		conn, err := r.a.f.endpointSQL(ctx, got)
		if err != nil {
			return fmt.Errorf("connect unattempted endpoint %s: %w", id, err)
		}
		defer conn.Close()
		return verifyRollTable(ctx, conn, "unattempted endpoint "+id)
	}
	return fmt.Errorf("the report names endpoint %s the roll never included", id)
}

// repair rolls everything back onto the version they started on; the attempted
// endpoints have a pod stuck on a missing image until this lands.
func (r *failfastRun) repair(ctx context.Context) error {
	if r.version == "" {
		return nil
	}
	task, loc, err := r.cl.UpgradeEndpoints(ctx, controller.UpgradeEndpointsRequest{Version: r.version, EndpointIDs: r.ids(), MaxParallel: failfastParallel})
	if err != nil {
		return fmt.Errorf("plan the repair roll onto %q: %w", r.version, err)
	}
	taskID := controller.TaskRef(task, loc)
	wctx, cancel := context.WithTimeout(ctx, rollTaskBudget*2)
	done, err := r.cl.WaitTask(wctx, taskID)
	cancel()
	if err != nil {
		subs, _ := r.cl.ListSubtasks(ctx, taskID)
		return fmt.Errorf("the repair roll %s onto %q did not succeed: %w (subtasks: %s)", taskID, r.version, err, describeSubtasks(subs.Subtasks))
	}
	report, ok := done.RollReport()
	if !ok {
		return fmt.Errorf("the repair roll %s recorded no report", taskID)
	}
	for _, line := range report.Endpoints {
		if line.Outcome != controller.RollUpgraded && line.Outcome != controller.RollSkipped {
			return fmt.Errorf("repair: endpoint %s ended %q (%s)", line.EndpointID, line.Outcome, line.Reason)
		}
	}
	for _, ep := range r.eps {
		got, err := r.cl.GetEndpoint(ctx, r.a.proj.poolID, r.a.proj.projID, ep.BranchID, ep.ID)
		if err != nil {
			return fmt.Errorf("read endpoint %s after the repair: %w", ep.ID, err)
		}
		if strings.TrimSpace(got.Version) != r.version {
			return fmt.Errorf("endpoint %s records %q after the repair roll onto %q", ep.ID, got.Version, r.version)
		}
		conn, err := reconnect(ctx, r.a.f.endpointSQL, got)
		if err != nil {
			return fmt.Errorf("endpoint %s after the repair: %w", ep.ID, err)
		}
		err = verifyRollTable(ctx, conn, "endpoint "+ep.ID+" after the repair roll")
		conn.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
