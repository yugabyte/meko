// The control plane is upgraded in place, then the endpoint fleet is rolled onto
// the new endpoint image through POST /endpoint-upgrades (meko#809), which is the
// order an operator follows.
//
// The release to upgrade FROM needs no configuration. The run's env.ampstack
// images are the release under test; the previous release is the newest one the
// report portal lists that is older than it (framework/releases), and a branch
// build (custom-<sha>), which is ahead of every release, upgrades from the newest
// release there is. functional.upgrade.from_controller_image and
// from_endpoint_image override that lookup; adjacent releases only, since the
// chart and the controller move in lockstep.
//
// This test never touches the run's shared control plane or its backing. It
// provisions its own YugabyteDB and deploys its own control plane on the previous
// release, so the shared environment keeps running the images under test while
// this one is upgraded. That is why it needs an environment that can name its
// backing (deploy mode); attached to a standing control plane it skips, since
// that control plane's backing is not ours to give a second one.
package features

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
	"github.com/yugabyte/amp/test/functional/framework/releases"
)

const (
	// upgradeTable holds the checksummed rows written before the upgrade.
	upgradeTable = "amp_upgrade_seed"
	// upgradePadLen is the length every row's text pad must still have after the
	// upgrade, so a truncated or re-encoded value cannot pass as intact.
	upgradePadLen = 64
	// upgradeVFactor ties v to id, making every row independently checkable.
	upgradeVFactor = 7
	// upgradePodReadTimeout bounds the kubectl reads that resolve an endpoint pod.
	upgradePodReadTimeout = 2 * time.Minute
	// upgradeReleaseFetchTimeout bounds the portal read that names the previous
	// release. The portal is reference data here, not the system under test.
	upgradeReleaseFetchTimeout = 1 * time.Minute
	// upgradeRollBudget bounds the fleet roll of the one seeded endpoint.
	upgradeRollBudget = 10 * time.Minute
	// DefaultUpgradeRows is the functional.upgrade.rows default.
	DefaultUpgradeRows = 2000
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_upgrade",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "upgrade"},
		OwningTeam:  core.TeamControlPlane,
		Description: "In-place control-plane upgrade on its own backing and control plane, apart from the shared environment: resolve the previous release from the report portal (or functional.upgrade.from_*_image), deploy it, seed checksummed data through an endpoint, upgrade the controller image to the release under test, then assert the controller pod runs the new image, the upgraded controller serves and forks pre-upgrade rows, the seeded endpoint still returns its data exactly, a whole-fleet POST /endpoint-upgrades rolls that endpoint onto the new endpoint image with its data intact, and a new endpoint gets the post-upgrade image.",
		Params:      []string{"functional", "functional.upgrade", "report.portal", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     45 * time.Minute,
		Run:         runAmpUpgrade,
	})
}

// upgradeRun carries the state the upgrade phases share.
type upgradeRun struct {
	f    *flow
	step *core.StepHandle

	fromController string
	fromEndpoint   string
	toController   string
	toEndpoint     string
	rows           int
	portalURL      string

	fromVersion string

	proj         ampCluster
	seedBranchID string
	forkBranchID string
	forkLSN      string
	ep           controller.Endpoint
	prePod       string
	preImages    string
}

func (u *upgradeRun) set(key string, val any) {
	if u.step != nil {
		u.step.Set(key, val)
	}
}

// upgradeBackingEnv is the capability that separates an amp environment which
// OWNS its control plane (env/sharedk8s deploy mode: it deployed the backing, so
// it can name it) from one attached to a standing control plane (which reaches
// the controller by API alone and cannot). Both expose the same ampControlPlane
// handle, so the handle itself decides nothing here.
type upgradeBackingEnv interface {
	YBStorage(ctx context.Context) (pg.ConnInfo, error)
}

// gateUpgradeBacking reports the skip reason when the environment's backing
// YugabyteDB cannot be named. This test never runs against the environment's
// control plane, which already runs the images we upgrade TO; it stands up its
// own on the previous release. So what it needs from an amp environment is a
// nameable backing. A non-amp environment (fake, universe) is the plain
// self-provision path and proceeds.
func gateUpgradeBacking(ctx context.Context, env core.Environment) (string, bool) {
	if _, ok := ampEnvFrom(env); !ok {
		return "", true
	}
	be, ok := env.(upgradeBackingEnv)
	if !ok {
		return fmt.Sprintf("the amp environment %T is missing the YBStorage capability, so an attached standing control plane cannot be told from an owned one; an attached control plane's backing is not ours to name, and the second control plane this test deploys on the previous release's images could not be given storage", env), false
	}
	if _, err := be.YBStorage(ctx); err != nil {
		return "this run is attached to a standing amp control plane whose backing YugabyteDB is not ours to name, so the second control plane this test deploys on the previous release's images cannot be given storage; the missing capability is a nameable backing (YBStorage: " + err.Error() + ")", false
	}
	return "", true
}

func runAmpUpgrade(t *core.T) error {
	cfg := FromConfig(t.Options)
	acfg := ampstack.FromConfig(t.Options)
	u := &upgradeRun{
		fromController: strings.TrimSpace(t.Options.GetString("functional.upgrade.from_controller_image", "")),
		fromEndpoint:   strings.TrimSpace(t.Options.GetString("functional.upgrade.from_endpoint_image", "")),
		toController:   acfg.ControllerImage,
		toEndpoint:     acfg.EndpointImage,
		rows:           t.Options.GetInt("functional.upgrade.rows", DefaultUpgradeRows),
		portalURL:      upgradePortalURL(t.Options),
	}
	if u.rows < 1 {
		return fmt.Errorf("functional.upgrade.rows is %d: the seed verification would pass on an empty table and prove nothing about the pre-upgrade data, so at least 1 row is required", u.rows)
	}
	if reason, ok := gateUpgradeBacking(t.Context, t.Env); !ok {
		t.Skip(reason)
		return nil
	}

	ybaReason, ybaOK := cfg.gateYBAProvisioned(t.Options)
	k8sReason, k8sOK := cfg.gateYBK8s(t.Options)
	flowLogf, rlog := newFlowLog(t.Masker())
	f := &flow{t: t, cfg: cfg, td: &teardownStack{}, logf: flowLogf, rlog: rlog}
	switch {
	case ybaOK:
		f.prov = cfg.provisionYBAProvisioned
	case k8sOK:
		f.logf("YBA-provisioned backing unavailable (%s); using co-located ybk8s backing", ybaReason)
		f.prov = cfg.provisionYBK8s
	default:
		t.Skip(ybaReason + "; ybk8s fallback also unavailable: " + k8sReason)
		return nil
	}
	if reason, ok := gateNoDoubleProvision(t.Env); !ok {
		t.Skip(reason)
		return nil
	}
	f.reg = openRunReapRegistry(f.logf)
	u.f = f

	// Resolved before any infrastructure is stood up: a portal that names no
	// previous release must not cost a universe first.
	var fromErr error
	_ = t.Step("resolve_from_release", func(h *core.StepHandle) error {
		u.step = h
		fromErr = u.resolveFrom(t.Context)
		return fromErr
	})
	if fromErr != nil {
		return fromErr
	}

	step := func(name string, fn func() error) error {
		return t.Step(name, func(h *core.StepHandle) error {
			u.step = h
			return fn()
		})
	}
	phases := []phase{
		{"provision_backing", f.provisionBacking},
		{"wait_backing_ready", f.waitBackingReady},
		{"deploy_amp_from", u.deployAmpFrom},
		{"seed", u.seed},
		{"upgrade_control_plane", u.upgradeControlPlane},
		{"controller_runs_new_image", u.controllerRunsNewImage},
		{"controller_serves_new_version", u.controllerServesNewVersion},
		{"existing_endpoint_survives", u.existingEndpointSurvives},
		{"roll_endpoints_to_new_version", u.rollEndpoints},
		{"new_endpoint_uses_new_image", u.newEndpointUsesNewImage},
	}
	return runLifecycle(t.Context, step, f.td, phases, f.logf, f.collectDiagnostics, f.onTeardownDone)
}

// upgradePortalURL is the portal the previous release is read from: this test's
// own knob, falling back to the run's portal. The fallback cannot be GetString's
// default: the knob is declared "" in default.yaml, so a present empty string is
// what GetString returns and the default never fires.
func upgradePortalURL(c *core.Config) string {
	if url := strings.TrimSpace(c.GetString("functional.upgrade.portal_url", "")); url != "" {
		return url
	}
	return strings.TrimSpace(c.GetString("report.portal.url", ""))
}

// resolveFrom fills in whichever from-image the config left empty with the
// previous release's, then checks the pair is a real upgrade.
func (u *upgradeRun) resolveFrom(ctx context.Context) error {
	u.set("to_controller_image", u.toController)
	u.set("to_endpoint_image", u.toEndpoint)
	if u.fromController == "" || u.fromEndpoint == "" {
		if u.portalURL == "" {
			return core.AsEnvFailure(fmt.Errorf("no report portal to read the previous release from (report.portal.url and functional.upgrade.portal_url are both empty), and functional.upgrade.from_controller_image / from_endpoint_image do not name it either; the pre-upgrade control plane has no images to deploy"))
		}
		fctx, cancel := context.WithTimeout(ctx, upgradeReleaseFetchTimeout)
		defer cancel()
		list, err := releases.New(u.portalURL).WithLogf(u.f.logf).List(fctx, upgradeReleaseListLimit)
		if err != nil {
			return core.AsEnvFailure(fmt.Errorf("read the release list from the report portal %q: %w. Set functional.upgrade.from_controller_image and from_endpoint_image to name the previous release when the portal cannot be reached", u.portalURL, err))
		}
		prev, err := previousRelease(list, u.toController)
		if err != nil {
			return core.AsEnvFailure(err)
		}
		if u.fromController == "" {
			u.fromController = prev.ControllerImage()
		}
		if u.fromEndpoint == "" {
			u.fromEndpoint = prev.EndpointImage()
		}
		u.set("from_release", prev.Tag)
		u.f.logf("upgrade: previous release %s resolved from %s (upgrading to %s)", prev.Tag, u.portalURL, u.toController)
	}
	u.set("from_controller_image", u.fromController)
	u.set("from_endpoint_image", u.fromEndpoint)
	return u.checkUpgradeIsReal()
}

// upgradeReleaseListLimit leaves room for a run pinned to an older release.
const upgradeReleaseListLimit = 20

// previousRelease picks, from a newest-first release list, the release to
// upgrade FROM when upgrading TO toImage.
//
// A release tag under test takes the newest release strictly older than it. Any
// other tag (custom-<sha>, br-<topic>-<sha>) is a branch build ahead of every
// release, so the previous release is the newest one the portal has.
func previousRelease(list []releases.Release, toImage string) (releases.Release, error) {
	toTag := toImage
	if i := strings.LastIndex(toImage, ":"); i >= 0 {
		toTag = toImage[i+1:]
	}
	toVersion, _, isRelease := releases.ParseTag(toTag)
	for _, r := range list {
		if r.Tag == toTag {
			continue
		}
		if isRelease && releases.Compare(r.Version, toVersion) >= 0 {
			continue
		}
		return r, nil
	}
	if isRelease {
		return releases.Release{}, fmt.Errorf("the report portal lists %d release(s) and none is older than %s, the release under test, so there is no previous release to upgrade from; set functional.upgrade.from_controller_image and from_endpoint_image to name one", len(list), toTag)
	}
	return releases.Release{}, fmt.Errorf("the report portal lists no release other than %s to upgrade from; set functional.upgrade.from_controller_image and from_endpoint_image to name one", toTag)
}

// checkUpgradeIsReal rejects an identical from/to image pair: re-applying the
// same manifest returns from the rollout at once and every later assertion would
// pass against a stack that never upgraded.
func (u *upgradeRun) checkUpgradeIsReal() error {
	if strings.TrimSpace(u.fromController) == strings.TrimSpace(u.toController) {
		return fmt.Errorf("functional.upgrade.from_controller_image and env.ampstack controller_image are both %q: this test cannot prove an upgrade when both refs are identical, because the rollout re-applies the manifest it already ran; point the from-image at the previous release",
			u.toController)
	}
	if strings.TrimSpace(u.fromEndpoint) == strings.TrimSpace(u.toEndpoint) {
		return fmt.Errorf("functional.upgrade.from_endpoint_image and env.ampstack endpoint_image are both %q: this test cannot prove the upgraded controller deploys the new endpoint image when both refs are identical; point the from-image at the previous release",
			u.toEndpoint)
	}
	return nil
}

// controllerRunsNewImage proves the controller was replaced without depending on
// any version string: the running controller pod must carry the target image.
func (u *upgradeRun) controllerRunsNewImage(ctx context.Context) error {
	kube := u.f.ampKube()
	if kube == nil {
		return fmt.Errorf("no kubectl is available for the amp namespace, so the controller pod's image cannot be read after the upgrade, and nothing else proves the controller was replaced")
	}
	name := ampstack.FromConfig(u.f.t.Options).ControllerName
	pctx, cancel := context.WithTimeout(ctx, upgradePodReadTimeout)
	defer cancel()
	images, err := kube.GetSelector(pctx, "pod", "app="+name, "{.items[*].spec.containers[*].image}")
	if err != nil {
		return fmt.Errorf("read the container images of the controller pods (selector app=%s) after the upgrade: %w", name, err)
	}
	images = strings.TrimSpace(images)
	u.set("controller_pod_images_after", images)
	if images == "" {
		return fmt.Errorf("no pod matches app=%s after the in-place upgrade, so the upgraded controller is not running", name)
	}
	if !imageMatches(images, u.toController) {
		return fmt.Errorf("the controller pods run %q after the in-place upgrade, want the post-upgrade controller image %q (env.ampstack controller_image): the rollout did not replace the running controller",
			images, u.toController)
	}
	return nil
}

// deployAmpFrom stands the control plane up on the PREVIOUS release's images.
func (u *upgradeRun) deployAmpFrom(ctx context.Context) error {
	f := u.f
	acfg := ampstack.FromConfig(f.t.Options)
	if acfg.ControllerManifest != "" {
		return fmt.Errorf("this test upgrades the control plane by re-rendering the controller chart, but ampstack is in raw-manifest mode (env.ampstack controller manifest set), where the image fields are ignored: run it without a raw controller manifest")
	}
	acfg.Namespace = f.namespace()
	acfg.Backing = ybbacking.ExternalBacking(f.store)
	acfg.ExtraNamespaceLabels = map[string]string{"ampqa-foundation": f.suffix()}
	acfg.ControllerImage = u.fromController
	acfg.EndpointImage = u.fromEndpoint

	app := ampstack.New(acfg)
	f.ampApp = app
	info, derr := app.Deploy(ctx)
	f.register("close amp port-forward", func(ctx context.Context) error { return app.Close(ctx) })
	f.register("teardown amp namespace", func(ctx context.Context) error { return app.Teardown(ctx) })
	if derr != nil {
		return core.AsEnvFailure(fmt.Errorf("deploy the pre-upgrade control plane (controller %s, endpoint %s): %w. Standing the BASELINE release up is environment work; check that the from-images exist in the configured registry", u.fromController, u.fromEndpoint, derr))
	}
	u.set("from_controller_image", info.Images["controller"])
	u.set("from_endpoint_image", info.Images["endpoint"])

	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	v, err := cl.Version(ctx)
	if err != nil {
		u.set("from_version", "unknown: "+err.Error())
		return nil
	}
	u.fromVersion = v.Version
	u.set("from_version", v.Version)
	u.set("from_git_commit", v.GitCommit)
	return nil
}

// seed creates the cluster, a branch and a read-write endpoint on the
// pre-upgrade control plane, and writes the checksummed table the post-upgrade
// phases read back.
func (u *upgradeRun) seed(ctx context.Context) error {
	f := u.f
	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	name := "upg-" + f.suffix()
	proj, err := setupAmpCluster(ctx, cl, name, f.cfg.PgVersion, f.register)
	if err != nil {
		return err
	}
	u.proj = proj

	// Seed on the cluster's own branch. A child branch cannot be forked here: the
	// parent carries no endpoint yet, so the controller has no fork point. The
	// post-upgrade fork below is taken after this branch has one.
	u.seedBranchID = proj.branchID

	ep, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, u.seedBranchID)
	if err != nil {
		return fmt.Errorf("create the pre-upgrade endpoint: %w", err)
	}
	u.ep = ep

	conn, err := f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect the pre-upgrade endpoint %s: %w", ep.Address, err)
	}
	defer conn.Close()

	if _, err := conn.Exec(ctx, "CREATE TABLE "+upgradeTable+" (id int primary key, v bigint, pad text)"); err != nil {
		return fmt.Errorf("create the seed table on the pre-upgrade endpoint: %w", err)
	}
	if _, err := conn.Exec(ctx,
		"INSERT INTO "+upgradeTable+" (id, v, pad) SELECT g, g*$1, repeat('x', $2) FROM generate_series(1, $3) g",
		upgradeVFactor, upgradePadLen, u.rows); err != nil {
		return fmt.Errorf("seed %d rows on the pre-upgrade endpoint: %w", u.rows, err)
	}
	if err := verifyUpgradeSeed(ctx, conn, u.rows, "the pre-upgrade endpoint before the upgrade"); err != nil {
		return err
	}
	u.forkLSN, err = currentLSN(ctx, conn)
	if err != nil {
		return err
	}
	u.set("seeded_rows", u.rows)
	u.set("fork_lsn", u.forkLSN)

	pod, images, err := u.endpointPod(ctx, ep.ID)
	if err != nil {
		u.set("endpoint_pod_before", "unresolved: "+err.Error())
		return nil
	}
	u.prePod, u.preImages = pod, images
	u.set("endpoint_pod_before", pod)
	u.set("endpoint_image_before", images)
	return nil
}

// upgradeControlPlane re-renders the controller chart with this run's configured
// images. A controller that cannot come up on the new image (a dirty migration,
// a crash loop) fails here.
func (u *upgradeRun) upgradeControlPlane(ctx context.Context) error {
	u.set("to_controller_image", u.toController)
	u.set("to_endpoint_image", u.toEndpoint)
	if err := u.f.ampApp.UpgradeImages(ctx, u.toController, u.toEndpoint); err != nil {
		return fmt.Errorf("in-place upgrade of the control plane from controller %s to %s never completed: %w. The new controller image must roll out over the running one with the pre-upgrade database in place",
			u.fromController, u.toController, err)
	}
	return nil
}

// controllerServesNewVersion asserts the upgraded controller is ready, reports a
// changed version where the builds are stamped, and can still drive a real
// control-plane task against rows created before the upgrade.
func (u *upgradeRun) controllerServesNewVersion(ctx context.Context) error {
	f := u.f
	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	if err := cl.Readyz(ctx); err != nil {
		return fmt.Errorf("the upgraded controller (%s) does not answer /readyz after the in-place upgrade: %w", u.toController, err)
	}
	v, err := cl.Version(ctx)
	if err != nil {
		u.set("to_version", "unknown: "+err.Error())
	} else {
		u.set("to_version", v.Version)
		u.set("to_git_commit", v.GitCommit)
		if stampedVersion(u.fromVersion) && stampedVersion(v.Version) && u.fromVersion == v.Version {
			return fmt.Errorf("the controller still reports version %q after the in-place upgrade from image %s to %s, so the rollout did not replace the running controller",
				v.Version, u.fromController, u.toController)
		}
	}

	forkName := "upg-fork-" + f.suffix()
	if _, err := cl.CreateBranchAndWait(ctx, u.proj.poolID, u.proj.projID, controller.CreateBranchRequest{
		Name:           forkName,
		ParentBranchID: u.seedBranchID,
		ForkPointLSN:   u.forkLSN,
	}); err != nil {
		return fmt.Errorf("the upgraded controller could not fork the pre-upgrade branch %s at LSN %s: %w. Migrations and the task pipeline must work against registry rows written by the previous release",
			u.seedBranchID, u.forkLSN, err)
	}
	fork, err := findBranchCtl(ctx, cl, u.proj.poolID, u.proj.projID, forkName)
	if err != nil {
		return err
	}
	u.forkBranchID = fork.BranchID
	u.set("fork_branch_id", u.forkBranchID)
	return nil
}

// existingEndpointSurvives asserts the pre-upgrade endpoint still serves SQL and
// its seeded rows verify exactly, on a fresh connection. Whether its pod was
// rolled is recorded, not asserted: the control-plane upgrade alone must not move
// an endpoint, and the roll that does is the next phase.
func (u *upgradeRun) existingEndpointSurvives(ctx context.Context) error {
	f := u.f
	conn, err := f.endpointSQL(ctx, u.ep)
	if err != nil {
		return fmt.Errorf("the endpoint %s created before the upgrade no longer accepts connections at %s: %w", u.ep.ID, u.ep.Address, err)
	}
	defer conn.Close()

	if err := verifyUpgradeSeed(ctx, conn, u.rows, "the endpoint created before the upgrade"); err != nil {
		return err
	}

	pod, images, err := u.endpointPod(ctx, u.ep.ID)
	if err != nil {
		u.set("endpoint_pod_after", "unresolved: "+err.Error())
		return nil
	}
	u.set("endpoint_pod_after", pod)
	u.set("endpoint_image_after", images)
	u.set("endpoint_pod_rolled", u.prePod != "" && pod != u.prePod)
	u.set("endpoint_image_changed", u.preImages != "" && images != u.preImages)
	return nil
}

// rollEndpoints is the operator's second step after the control-plane upgrade:
// one whole-fleet POST /endpoint-upgrades onto the new endpoint image tag. This
// control plane is the test's own, so the fleet is exactly the seeded endpoint.
// The upgraded controller must roll an endpoint the PREVIOUS release created,
// the pod must come back on the new image, and the seeded rows must survive it.
func (u *upgradeRun) rollEndpoints(ctx context.Context) error {
	f := u.f
	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	target := imageTag(u.toEndpoint)
	u.set("roll_target_version", target)
	task, loc, err := cl.UpgradeEndpoints(ctx, controller.UpgradeEndpointsRequest{Version: target})
	if status := apiErrorStatus(err); status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented {
		u.set("roll", "unsupported: "+err.Error())
		f.logf("upgrade: the upgraded controller has no usable POST /endpoint-upgrades (%v); the endpoint roll is not asserted", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("plan the fleet roll onto endpoint version %q on the upgraded controller: %w", target, err)
	}
	taskID := controller.TaskRef(task, loc)
	rctx, cancel := context.WithTimeout(ctx, upgradeRollBudget)
	done, err := cl.WaitTask(rctx, taskID)
	cancel()
	if err != nil {
		subs, _ := cl.ListSubtasks(ctx, taskID)
		return fmt.Errorf("the fleet roll %s onto %q failed on the upgraded controller: %w (subtasks: %s). An endpoint created by release %s must be rollable by the release that follows it",
			taskID, target, err, describeSubtasks(subs.Subtasks), u.fromController)
	}
	report, ok := done.RollReport()
	if !ok {
		return fmt.Errorf("the succeeded roll %s recorded no report (%s)", taskID, string(done.Result))
	}
	u.set("roll_report", fmt.Sprintf("upgraded=%d skipped=%d failed=%d unattempted=%d", report.Upgraded, report.Skipped, report.Failed, report.Unattempted))
	var line *controller.RollLine
	for i := range report.Endpoints {
		if report.Endpoints[i].EndpointID == u.ep.ID {
			line = &report.Endpoints[i]
		}
	}
	if line == nil {
		return fmt.Errorf("the whole-fleet roll %s did not enumerate the pre-upgrade endpoint %s at all (report: %+v); every live endpoint with an address is part of the fleet", taskID, u.ep.ID, report.Endpoints)
	}
	if line.Outcome != controller.RollUpgraded {
		return fmt.Errorf("the pre-upgrade endpoint %s ended the roll %q (%s), want upgraded: it was recorded at the previous release's version (or none), so a roll onto %q must move it", u.ep.ID, line.Outcome, line.Reason, target)
	}

	got, err := cl.GetEndpoint(ctx, u.proj.poolID, u.proj.projID, u.seedBranchID, u.ep.ID)
	if err != nil {
		return fmt.Errorf("read the rolled endpoint %s: %w", u.ep.ID, err)
	}
	u.set("endpoint_version_after_roll", got.Version)
	if strings.TrimSpace(got.Version) != target {
		return fmt.Errorf("endpoint %s records version %q after the roll onto %q", u.ep.ID, got.Version, target)
	}
	if kube := f.ampKube(); kube != nil {
		if err := ampstack.WaitEndpointWorkloadReady(ctx, kube, u.ep.ID, upgradePodReadTimeout); err != nil {
			return fmt.Errorf("endpoint %s compute is not ready after the roll: %w", u.ep.ID, err)
		}
		pod, images, err := u.endpointPod(ctx, u.ep.ID)
		if err != nil {
			return fmt.Errorf("read the rolled endpoint's pod: %w", err)
		}
		u.set("endpoint_pod_after_roll", pod)
		u.set("endpoint_image_after_roll", images)
		if !imageMatches(images, u.toEndpoint) {
			return fmt.Errorf("endpoint %s reports upgraded but pod %s runs %q, want the post-upgrade endpoint image %q", u.ep.ID, pod, images, u.toEndpoint)
		}
		if u.prePod != "" && pod == u.prePod {
			return fmt.Errorf("endpoint %s reports upgraded but still runs its pre-upgrade pod %s", u.ep.ID, pod)
		}
	}
	conn, err := f.endpointSQL(ctx, got)
	if err != nil {
		return fmt.Errorf("the rolled endpoint %s no longer accepts connections: %w", u.ep.ID, err)
	}
	defer conn.Close()
	return verifyUpgradeSeed(ctx, conn, u.rows, "the endpoint rolled onto the new version")
}

// imageTag is the tag of an image ref (the whole ref when it carries none).
func imageTag(image string) string {
	image = strings.TrimSpace(image)
	slash := strings.LastIndexByte(image, '/')
	if i := strings.LastIndexByte(image, ':'); i > slash {
		return image[i+1:]
	}
	return image
}

// newEndpointUsesNewImage asserts the upgraded controller deploys endpoints from
// the image tag it was upgraded with.
func (u *upgradeRun) newEndpointUsesNewImage(ctx context.Context) error {
	f := u.f
	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, err := createEndpoint(ctx, cl, u.proj.poolID, u.proj.projID, u.forkBranchID)
	if err != nil {
		return fmt.Errorf("create an endpoint on the post-upgrade fork %s: %w", u.forkBranchID, err)
	}
	pod, images, err := u.endpointPod(ctx, ep.ID)
	if err != nil {
		return fmt.Errorf("read the pod of the endpoint created after the upgrade: %w", err)
	}
	u.set("new_endpoint_pod", pod)
	u.set("new_endpoint_image", images)
	if !imageMatches(images, u.toEndpoint) {
		return fmt.Errorf("the upgraded controller is still deploying the old endpoint image: pod %s runs %q, want the post-upgrade endpoint image %q (env.ampstack endpoint_image); the controller must deploy the endpoint image tag it was given",
			pod, images, u.toEndpoint)
	}
	return nil
}

// endpointPod returns the endpoint's compute pod name and its container image
// refs (space separated when the pod has more than one container).
func (u *upgradeRun) endpointPod(ctx context.Context, endpointID string) (string, string, error) {
	return endpointPodImages(ctx, u.f.ampKube(), endpointID)
}

// endpointPodImages resolves an endpoint's running compute pod and the image
// refs its containers carry.
func endpointPodImages(ctx context.Context, kube *k8s.Kubectl, endpointID string) (string, string, error) {
	if kube == nil {
		return "", "", fmt.Errorf("no kubectl is available for the amp namespace, so endpoint pod %s* cannot be read", ampstack.EndpointWorkloadName(endpointID))
	}
	pctx, cancel := context.WithTimeout(ctx, upgradePodReadTimeout)
	defer cancel()
	roller := &kubeComputeRoller{kube: kube, timeout: upgradePodReadTimeout}
	pod, _, err := roller.runningPod(pctx, endpointID)
	if err != nil {
		return "", "", err
	}
	images, err := kube.Get(pctx, "pod", pod, "{.spec.containers[*].image}")
	if err != nil {
		return pod, "", fmt.Errorf("read container images of pod %s: %w", pod, err)
	}
	return pod, images, nil
}

// verifyUpgradeSeed asserts the seeded table is exactly as written: the full row
// count, and not one row whose v or pad drifted.
func verifyUpgradeSeed(ctx context.Context, c sqlConn, rows int, where string) error {
	got, err := c.QueryInt(ctx, "SELECT count(*) FROM "+upgradeTable)
	if err != nil {
		return fmt.Errorf("count the seeded rows on %s: %w", where, err)
	}
	if got != int64(rows) {
		return fmt.Errorf("%s serves %d rows of %s, want the %d rows seeded before the upgrade", where, got, upgradeTable, rows)
	}
	bad, err := c.QueryInt(ctx, fmt.Sprintf(
		"SELECT count(*) FROM %s WHERE v <> id::bigint * %d OR length(pad) <> %d", upgradeTable, upgradeVFactor, upgradePadLen))
	if err != nil {
		return fmt.Errorf("checksum the seeded rows on %s: %w", where, err)
	}
	if bad != 0 {
		return fmt.Errorf("%s serves %d of %d rows of %s whose value no longer satisfies v = id*%d with a %d-byte pad, so the pre-upgrade data did not survive intact",
			where, bad, rows, upgradeTable, upgradeVFactor, upgradePadLen)
	}
	return nil
}

// stampedVersion reports whether a controller version string carries a real
// build identity; an unstamped build reports "dev", which makes any comparison
// vacuous.
func stampedVersion(v string) bool {
	v = strings.TrimSpace(v)
	return v != "" && v != "dev"
}

// imageMatches reports whether any of a pod's image refs is the wanted image,
// comparing on the repository:tag suffix so a registry-qualified pod ref matches
// the configured short ref.
func imageMatches(podImages, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	tail := want
	if i := strings.LastIndexByte(want, '/'); i >= 0 {
		tail = want[i+1:]
	}
	for _, got := range strings.Fields(podImages) {
		if got == want || strings.HasSuffix(got, "/"+tail) {
			return true
		}
	}
	return false
}
