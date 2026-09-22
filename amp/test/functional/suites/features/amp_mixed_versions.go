package features

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	DefaultMixedVersionsHold    = 10 * time.Minute
	DefaultMixedVersionsPoll    = 30 * time.Second
	DefaultMixedVersionsHoldGap = 60 * time.Second
	DefaultMixedVersionsRollGap = 15 * time.Minute

	mixedTaskPage  = 20
	mixedRollBudge = 20 * time.Minute
)

var mixedKnownTaskStates = map[string]bool{
	controller.TaskPending:   true,
	controller.TaskRunning:   true,
	controller.TaskSucceeded: true,
	controller.TaskFailed:    true,
}

func init() {
	core.Register(core.Test{
		Name:        "features/amp_mixed_versions",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "The mixed-version window: deploy the control plane at the previous release, upgrade the controller (and its endpoint pin) so the running endpoints stay on the old image, and hold that pair for functional.mixed_versions.hold under a write load, then roll the fleet onto the current image. Writes must not stop for longer than functional.mixed_versions.max_hold_gap during the hold or max_roll_gap across the roll, the new controller must keep serving GET /tasks with every task in a state it knows, the endpoints must still be on the old image throughout the hold, and every acknowledged write must be there afterwards with the bank total unchanged.",
		Params:      []string{"functional", "functional.upgrade", "functional.mixed_versions", "report.portal", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     80 * time.Minute,
		Run:         runAmpMixedVersions,
	})
}

type mixedRun struct {
	u *upgradeRun
	f *flow

	hold     time.Duration
	poll     time.Duration
	holdGap  time.Duration
	rollGap  time.Duration
	proj     ampCluster
	ep       controller.Endpoint
	load     *upgradeLoad
	listed   int
	rollWait time.Duration
}

func runAmpMixedVersions(t *core.T) error {
	acfg := ampstack.FromConfig(t.Options)
	m := &mixedRun{
		hold:    t.Options.GetDuration("functional.mixed_versions.hold", DefaultMixedVersionsHold),
		poll:    t.Options.GetDuration("functional.mixed_versions.poll_interval", DefaultMixedVersionsPoll),
		holdGap: t.Options.GetDuration("functional.mixed_versions.max_hold_gap", DefaultMixedVersionsHoldGap),
		rollGap: t.Options.GetDuration("functional.mixed_versions.max_roll_gap", DefaultMixedVersionsRollGap),
	}
	if m.hold < m.poll || m.poll <= 0 {
		return fmt.Errorf("functional.mixed_versions is hold=%s poll_interval=%s: the hold has to cover at least one poll of the new controller", m.hold, m.poll)
	}
	u := &upgradeRun{
		fromController: strings.TrimSpace(t.Options.GetString("functional.upgrade.from_controller_image", "")),
		fromEndpoint:   strings.TrimSpace(t.Options.GetString("functional.upgrade.from_endpoint_image", "")),
		toController:   acfg.ControllerImage,
		toEndpoint:     acfg.EndpointImage,
		rows:           1,
		portalURL:      upgradePortalURL(t.Options),
	}
	ok, err := u.setup(t)
	if err != nil || !ok {
		return err
	}
	step := u.stepper()
	if err := u.resolveStep(); err != nil {
		return err
	}
	m.u, m.f = u, u.f
	f := u.f
	phases := []phase{
		{"provision_backing", f.provisionBacking},
		{"wait_backing_ready", f.waitBackingReady},
		{"deploy_amp_from", u.deployAmpFrom},
		{"endpoint_on_old_release", m.endpoint},
		{"start_write_load", m.startLoad},
		{"upgrade_controller_only", m.upgradeControllerOnly},
		{"hold_mixed_versions", m.holdMixed},
		{"roll_endpoints_to_current", m.rollFleet},
		{"verify_write_load", m.verifyLoad},
	}
	return runLifecycle(t.Context, step, f.td, phases, f.logf, f.collectDiagnostics, f.onTeardownDone)
}

func (m *mixedRun) endpoint(ctx context.Context) error {
	f := m.f
	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	proj, err := setupAmpCluster(ctx, cl, "mixed-"+f.suffix(), f.cfg.PgVersion, f.register)
	if err != nil {
		return err
	}
	m.proj = proj
	ep, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create the endpoint the previous release deploys: %w", err)
	}
	m.ep = ep
	return nil
}

func (m *mixedRun) dial(ctx context.Context) (sqlConn, error) {
	ep := m.ep
	if cl, err := m.f.controllerClient(ctx); err == nil {
		if got, gerr := cl.GetEndpoint(ctx, m.proj.poolID, m.proj.projID, m.proj.branchID, m.ep.ID); gerr == nil {
			ep = got
		}
	}
	return m.f.endpointSQL(ctx, ep)
}

func (m *mixedRun) startLoad(ctx context.Context) error {
	f := m.f
	m.load = newUpgradeLoad(loadTablePrefix("mixed", f.suffix()), upgradeLoadConfigFrom(f.t.Options),
		f.t.Seed.Fork("mixed-load").Value(), m.dial, f.logf)
	if err := m.load.Setup(ctx); err != nil {
		return err
	}
	f.register("stop the write load", func(context.Context) error { m.load.Stop(); return nil })
	m.load.Start(ctx)
	return nil
}

// The controller's endpoint pin moves with it: a controller that pins the roll
// target to its own release rolls onto nothing else. The running endpoints must
// still stay where they are until the roll.
func (m *mixedRun) upgradeControllerOnly(ctx context.Context) error {
	if err := m.f.ampApp.UpgradeImages(ctx, m.u.toController, m.u.toEndpoint); err != nil {
		return fmt.Errorf("upgrade the controller from %s to %s: %w", m.u.fromController, m.u.toController, err)
	}
	if err := m.u.controllerRunsNewImage(ctx); err != nil {
		return err
	}
	return m.endpointsStillOld(ctx)
}

func (m *mixedRun) endpointsStillOld(ctx context.Context) error {
	kube := m.f.ampKube()
	if kube == nil {
		return nil
	}
	pod, images, err := endpointPodImages(ctx, kube, m.ep.ID)
	if err != nil {
		return fmt.Errorf("read the endpoint's pod during the mixed-version window: %w", err)
	}
	m.u.set("endpoint_image_mixed", images)
	if !imageMatches(images, m.u.fromEndpoint) {
		return fmt.Errorf("endpoint %s runs %q on pod %s while the controller is on %s, want the previous release's endpoint image %q: the control-plane upgrade moved the compute on its own, so there is no mixed-version window to test",
			m.ep.ID, images, pod, m.u.toController, m.u.fromEndpoint)
	}
	return nil
}

func (m *mixedRun) holdMixed(ctx context.Context) error {
	f := m.f
	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	m.load.ResetGap()
	deadline := time.Now().Add(m.hold)
	polls := 0
	for time.Now().Before(deadline) {
		if err := waitFor(ctx, m.poll); err != nil {
			return err
		}
		list, err := cl.ListTasks(ctx, controller.ListTasksOptions{Limit: mixedTaskPage})
		if err != nil {
			return fmt.Errorf("the upgraded controller could not list the tasks the previous release wrote after %s of the mixed-version window: %w", time.Since(deadline.Add(-m.hold)).Round(time.Second), err)
		}
		for _, task := range list.Tasks {
			if !mixedKnownTaskStates[task.Status] {
				return fmt.Errorf("the upgraded controller reports task %s (kind %s) in state %q, which is not one of pending/running/succeeded/failed: a row written by the previous release is no longer parseable", task.ID, task.Kind, task.Status)
			}
		}
		m.listed = list.Total
		polls++
		if err := m.endpointsStillOld(ctx); err != nil {
			return err
		}
	}
	gap := m.load.MaxGap()
	m.u.set("hold_polls", polls)
	m.u.set("tasks_listed", m.listed)
	m.u.set("hold_write_gap_seconds", gap.Seconds())
	if gap > m.holdGap {
		return fmt.Errorf("writes to the endpoint stopped for %s during the %s mixed-version hold (limit %s, functional.mixed_versions.max_hold_gap): nothing was being upgraded, so a new controller over old endpoints must not interrupt them. Last writer error: %s",
			gap.Round(time.Second), m.hold, m.holdGap, m.load.LastWriterError())
	}
	if polls == 0 {
		return fmt.Errorf("the mixed-version hold polled the controller no times, so neither the task feed nor the endpoint image was checked")
	}
	return nil
}

func (m *mixedRun) rollFleet(ctx context.Context) error {
	f := m.f
	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	target := imageTag(m.u.toEndpoint)
	m.load.ResetGap()
	started := time.Now()
	task, loc, err := cl.UpgradeEndpoints(ctx, controller.UpgradeEndpointsRequest{Version: target})
	if status := apiErrorStatus(err); status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented {
		return fmt.Errorf("the upgraded controller has no usable POST /endpoint-upgrades (%v), so the mixed-version window cannot be closed", err)
	}
	if err != nil {
		return fmt.Errorf("plan the fleet roll onto %q: %w", target, err)
	}
	taskID := controller.TaskRef(task, loc)
	rctx, cancel := context.WithTimeout(ctx, mixedRollBudge)
	done, err := cl.WaitTask(rctx, taskID)
	cancel()
	if err != nil {
		subs, _ := cl.ListSubtasks(ctx, taskID)
		return fmt.Errorf("the fleet roll %s onto %q failed: %w (subtasks: %s)", taskID, target, err, describeSubtasks(subs.Subtasks))
	}
	report, ok := done.RollReport()
	if !ok {
		return fmt.Errorf("the succeeded roll %s recorded no report (%s)", taskID, string(done.Result))
	}
	for _, line := range report.Endpoints {
		if line.EndpointID == m.ep.ID && line.Outcome != controller.RollUpgraded {
			return fmt.Errorf("the endpoint held at the previous release ended the roll %q (%s), want upgraded", line.Outcome, line.Reason)
		}
	}
	if kube := f.ampKube(); kube != nil {
		if err := ampstack.WaitEndpointWorkloadReady(ctx, kube, m.ep.ID, upgradePodReadTimeout); err != nil {
			return fmt.Errorf("endpoint %s compute is not ready after the roll: %w", m.ep.ID, err)
		}
		_, images, err := endpointPodImages(ctx, kube, m.ep.ID)
		if err != nil {
			return fmt.Errorf("read the rolled endpoint's pod: %w", err)
		}
		if !imageMatches(images, m.u.toEndpoint) {
			return fmt.Errorf("endpoint %s runs %q after the roll, want the current endpoint image %q", m.ep.ID, images, m.u.toEndpoint)
		}
	}
	m.rollWait = time.Since(started)
	return nil
}

func (m *mixedRun) verifyLoad(ctx context.Context) error {
	gap := m.load.MaxGap()
	m.load.Stop()
	m.load.record(m.u.set, gap)
	m.u.set("roll_seconds", m.rollWait.Seconds())
	m.f.t.Datapoint(core.Datapoint{Name: "upgrade.write_gap_seconds", Value: gap.Seconds(), Unit: "s", Direction: core.LowerIsBetter})
	if gap > m.rollGap {
		return fmt.Errorf("writes to the endpoint stopped for %s across a fleet roll that took %s (limit %s, functional.mixed_versions.max_roll_gap): a client that reconnects must be back inside the roll's own window. Last writer error: %s",
			gap.Round(time.Second), m.rollWait.Round(time.Second), m.rollGap, m.load.LastWriterError())
	}
	conn, err := m.dial(ctx)
	if err != nil {
		return fmt.Errorf("connect the rolled endpoint to verify the write load: %w", err)
	}
	defer conn.Close()
	return m.load.Verify(ctx, conn)
}
