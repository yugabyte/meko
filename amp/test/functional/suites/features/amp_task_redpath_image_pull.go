package features

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// A tag nobody publishes, on a repository that exists, so the pull fails on
// the tag rather than on auth or DNS.
const redpathMissingImage = "ghcr.io/cloudnative-pg/postgis-extension:0.0.0-ampqa-missing"

func init() {
	core.Register(core.Test{
		Name:        "features/amp_task_redpath_image_pull",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Register an extension whose image tag does not exist and create an endpoint on a cluster opted into it. The create task must fail with an error that names the image and the pull failure, not only a deadline (meko#635), leave no compute behind, and leave the bystander cluster serving.",
		Params:      []string{"functional.redpath", "functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     120 * time.Minute,
		Run:         runAmpTaskRedpathImagePull,
	})
}

func runAmpTaskRedpathImagePull(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"redpath_image_pull", a.redpathImagePull}}
	})
}

func (a *ampFeatureRun) redpathImagePull(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	if _, err := cl.ListExtensions(ctx); err != nil {
		if extensionsUnavailable(err) {
			a.f.t.Skip("this control plane has no extension catalog: " + err.Error())
			return nil
		}
		return fmt.Errorf("list extensions: %w", err)
	}
	admin, err := cl.CreateUser(ctx, controller.CreateUserRequest{IsAdmin: true})
	if err != nil {
		return fmt.Errorf("create the admin user: %w", err)
	}
	name := "qa-missing-" + a.f.suffix()
	if _, err := cl.RegisterExtension(ctx, name, admin.ID, controller.RegisterExtensionRequest{
		Image:       redpathMissingImage,
		Description: "ampqa: an image tag that does not exist",
		Layout:      &controller.ExtensionLayout{SharePath: "share", LibPath: "lib"},
	}); err != nil {
		return fmt.Errorf("register %s with image %s: %w", name, redpathMissingImage, err)
	}
	a.f.registerLast("deregister "+name, func(ctx context.Context) error {
		if err := cl.DeregisterExtension(ctx, name, admin.ID); err != nil && apiErrorStatus(err) != http.StatusNotFound {
			return err
		}
		return nil
	})

	r := &redpathRun{a: a, cfg: redpathFromConfig(a.f.t.Options), cl: cl}
	if r.bystander, err = createEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID); err != nil {
		return fmt.Errorf("create the bystander writer the failure must not disturb: %w", err)
	}
	clusterName := "rp-pull-" + a.f.suffix()
	if _, err := cl.CreateClusterAndWait(ctx, a.proj.poolID, controller.CreateClusterRequest{
		Name: clusterName, PgVersion: a.f.cfg.PgVersion, Extensions: []string{name},
	}); err != nil {
		return fmt.Errorf("create a cluster opted into %s: %w", name, err)
	}
	c, err := findClusterCtl(ctx, cl, a.proj.poolID, clusterName)
	if err != nil {
		return err
	}
	a.f.register("delete "+clusterName, func(ctx context.Context) error {
		_, err := cl.DeleteClusterAndWait(ctx, a.proj.poolID, c.ID)
		return err
	})
	r.target = ampCluster{poolID: a.proj.poolID, projID: c.ID, branchID: c.DefaultBranch.BranchID}

	task, loc, err := cl.CreateEndpoint(ctx, r.target.poolID, r.target.projID, r.target.branchID, controller.CreateEndpointRequest{Role: roleReadWrite})
	if err != nil {
		return fmt.Errorf("plan the create of an endpoint whose extension image cannot be pulled: %w", err)
	}
	what := "create_endpoint with extension image " + redpathMissingImage
	done, err := r.taskFailedWithin(ctx, task, loc, redpathCreateDeadline, what)
	if err != nil {
		return err
	}
	if err := taskErrorNames(done, what, []string{"0.0.0-ampqa-missing"}); err != nil {
		return err
	}
	if err := taskErrorNames(done, what, []string{"pull"}); err != nil {
		return err
	}
	msg := ""
	if done.Error != nil {
		msg = *done.Error
	}
	if !strings.Contains(msg, "yb-ep-") {
		return fmt.Errorf("%s failed with %q, which does not name the endpoint's pod or release: the operator cannot tell which endpoint to look at", what, msg)
	}
	if err := r.noOrphans(ctx, r.target.branchID, r.cfg.OrphanTimeout); err != nil {
		return err
	}
	return r.stillServing(ctx)
}
