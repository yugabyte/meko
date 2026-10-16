package features

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	// helm upgrade --install --wait holds the release in pending-install until
	// the pod is ready, about 20s on a warm node.
	pendingInstallWatch    = 3 * time.Minute
	pendingInstallAttempts = 3
	controllerBackWait     = 10 * time.Minute
	resumedTaskWait        = 15 * time.Minute
)

func init() {
	core.Register(core.Test{
		Name:     "features/amp_endpoint_create_controller_restart",
		Suite:    "features",
		Category: core.Stress,
		// Restarting the controller faults every test on the shared control plane.
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane, FaultsSharedInfra: true},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Start an endpoint create, wait until its helm release is pending-install, and delete the controller pod once. The restarted controller must resume the same task to succeeded, leave exactly one revision of the release in status deployed and none pending, and the endpoint must serve SQL.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     40 * time.Minute,
		Run:         runAmpEndpointCreateControllerRestart,
	})
}

func runAmpEndpointCreateControllerRestart(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"endpoint_create_controller_restart", a.endpointCreateControllerRestart}}
	})
}

// helmReleases lists one release's helm storage secrets as "version=status".
func helmReleases(ctx context.Context, kube *k8s.Kubectl, release string) ([]string, error) {
	out, err := kube.GetSelector(ctx, "secret", "owner=helm,name="+release,
		`{range .items[*]}{.metadata.labels.version}={.metadata.labels.status}{" "}{end}`)
	if err != nil {
		return nil, fmt.Errorf("list helm release secrets of %s: %w", release, err)
	}
	return strings.Fields(out), nil
}

func (a *ampFeatureRun) endpointCreateControllerRestart(ctx context.Context) error {
	kube := a.f.ampKube()
	if kube == nil {
		a.f.t.Skip("no kube handle for the amp namespace: the helm release and the controller pod are only reachable through it")
		return nil
	}
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	var task controller.Task
	var release string
	for attempt := 1; ; attempt++ {
		task, release, err = a.createUntilPendingInstall(ctx, cl, kube)
		if err != nil || release != "" {
			break
		}
		if attempt == pendingInstallAttempts {
			return fmt.Errorf("in %d endpoint creates the test never saw a helm release in pending-install, so the controller was never restarted mid-install", attempt)
		}
	}
	if err != nil {
		return err
	}
	a.f.logf("amp_endpoint_create_controller_restart: release %s is pending-install, deleting the controller pod", release)

	name := ampstack.FromConfig(a.f.t.Options).ControllerName
	pods, err := kube.GetSelector(ctx, "pod", "app="+name, "{.items[*].metadata.name}")
	if err != nil || len(strings.Fields(pods)) == 0 {
		return fmt.Errorf("find the controller pod (selector app=%s): %v", name, err)
	}
	for _, p := range strings.Fields(pods) {
		if err := kube.Delete(ctx, "pod", p); err != nil {
			return fmt.Errorf("delete controller pod %s: %w", p, err)
		}
	}
	if err := kube.RolloutStatus(ctx, "deployment", name, controllerBackWait); err != nil {
		return core.AsEnvFailure(fmt.Errorf("the controller Deployment %s did not come back after its pod was deleted: %w", name, err))
	}

	var final controller.Task
	err = pollUntil(ctx, resumedTaskWait, 5*time.Second, func() (bool, error) {
		cl, err := a.f.controllerClient(ctx)
		if err != nil {
			return false, nil
		}
		t, err := cl.GetTask(ctx, task.ID)
		if err != nil {
			return false, nil
		}
		final = t
		return t.Status != controller.TaskPending && t.Status != controller.TaskRunning, nil
	})
	if err != nil {
		return fmt.Errorf("task %s, interrupted by a controller restart mid helm install, is still %q: the restarted controller did not resume it: %w", task.ID, final.Status, err)
	}
	if final.Status != controller.TaskSucceeded {
		msg := ""
		if final.Error != nil {
			msg = *final.Error
		}
		return fmt.Errorf("the endpoint create interrupted by a controller restart mid helm install ended %s: %s", final.Status, msg)
	}

	revs, err := helmReleases(ctx, kube, release)
	if err != nil {
		return err
	}
	deployed := 0
	for _, r := range revs {
		_, status, _ := strings.Cut(r, "=")
		switch status {
		case "deployed":
			deployed++
		case "superseded", "uninstalled":
		default:
			return fmt.Errorf("after the resumed create, release %s still has a revision in %q (revisions %v)", release, status, revs)
		}
	}
	if deployed != 1 {
		return fmt.Errorf("after the resumed create, release %s has %d revisions in status deployed, want exactly 1 (revisions %v)", release, deployed, revs)
	}

	cl, err = a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	eps, err := cl.ListAllEndpoints(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID)
	if err != nil {
		return err
	}
	if len(eps) != 1 {
		return fmt.Errorf("the branch lists %d endpoints after the resumed create, want 1", len(eps))
	}
	if ampstack.EndpointWorkloadName(eps[0].ID) != release {
		return fmt.Errorf("the branch's endpoint %s does not own release %s, the one that was pending-install", eps[0].ID, release)
	}
	c, err := a.f.endpointSQL(ctx, eps[0])
	if err != nil {
		return fmt.Errorf("connect the endpoint created across a controller restart: %w", err)
	}
	defer c.Close()
	if _, err := c.QueryInt(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("the endpoint created across a controller restart does not serve: %w", err)
	}
	return nil
}

// The pending-install window is about 10s and a controller call can stall 30s on
// the exec relay, so the watch reads only helm secrets.
func (a *ampFeatureRun) createUntilPendingInstall(ctx context.Context, cl *controller.Client, kube *k8s.Kubectl) (controller.Task, string, error) {
	before, err := endpointReleases(ctx, kube)
	if err != nil {
		return controller.Task{}, "", err
	}
	task, _, err := cl.CreateEndpoint(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, controller.CreateEndpointRequest{Role: roleReadWrite})
	if err != nil {
		return task, "", fmt.Errorf("start the endpoint create: %w", err)
	}
	var release, missed string
	err = pollUntil(ctx, pendingInstallWatch, 300*time.Millisecond, func() (bool, error) {
		now, err := endpointReleases(ctx, kube)
		if err != nil {
			return false, nil
		}
		for n, status := range now {
			if _, ok := before[n]; ok {
				continue
			}
			switch status {
			case "pending-install":
				release = n
				return true, nil
			case "deployed":
				missed = n
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t, terr := cl.GetTask(ctx, task.ID)
		return task, "", fmt.Errorf("watch for the endpoint's helm release in pending-install (task %s is %q, read error %v): %w", task.ID, t.Status, terr, err)
	}
	if release != "" {
		return task, release, nil
	}
	a.f.logf("amp_endpoint_create_controller_restart: release %s reached deployed before the watch saw it pending-install, deleting it and creating again", missed)
	if _, err := cl.WaitTask(ctx, task.ID); err != nil {
		return task, "", fmt.Errorf("wait for the endpoint create %s whose install was missed: %w", task.ID, err)
	}
	id := strings.TrimPrefix(missed, ampstack.EndpointWorkloadName(""))
	if _, err := cl.DeleteEndpointAndWait(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, id); err != nil {
		return task, "", fmt.Errorf("delete endpoint %s before creating again: %w", id, err)
	}
	return task, "", nil
}

func endpointReleases(ctx context.Context, kube *k8s.Kubectl) (map[string]string, error) {
	out, err := kube.GetSelector(ctx, "secret", "owner=helm",
		`{range .items[*]}{.metadata.labels.name}={.metadata.labels.status}{" "}{end}`)
	if err != nil {
		return nil, fmt.Errorf("list helm release secrets: %w", err)
	}
	m := map[string]string{}
	for _, f := range strings.Fields(out) {
		n, status, _ := strings.Cut(f, "=")
		if !strings.HasPrefix(n, ampstack.EndpointWorkloadName("")) {
			continue
		}
		if m[n] == "" || status == "pending-install" || status == "deployed" {
			m[n] = status
		}
	}
	return m, nil
}
