// There is no endpoint-upgrade route on the controller today, so this covers the
// control plane only: the endpoint side is asserted through what the API does
// promise, that the controller deploys the endpoint image tag it was given.
//
// STATUS: written, UNVALIDATED end to end. No run has reached the upgrade itself,
// so every assertion past the seed is unproven. Progress so far: the env gate and
// functional.enabled gate are both cleared, and the previous release's control
// plane does deploy (provision_backing, wait_backing_ready and deploy_amp_from all
// passed on 2026-08-13). The seed then failed on a fork point, since fixed by
// seeding on the cluster's own branch. Next run should reach the upgrade.
//
// To RUN rather than skip, all of these must hold, and each skip names the one
// that failed:
//   - functional.upgrade.from_controller_image and from_endpoint_image are set
//     (the previous release; adjacent releases only, chart and controller move in
//     lockstep) and differ from the images the run upgrades TO
//   - functional.enabled is true, a separate opt-in from the env shape, because
//     this test provisions its own control plane instead of using the env's
//   - the env can name its backing YugabyteDB, which only deploy mode can do; an
//     attached standing control plane's backing is not ours to give a second
//     control plane
package features

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/amp/test/functional/framework/core"
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
	// DefaultUpgradeRows is the functional.upgrade.rows default.
	DefaultUpgradeRows = 2000
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_upgrade",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "In-place control-plane upgrade: deploy the previous release, seed checksummed data through an endpoint, upgrade the controller image, then assert the controller pod runs the new image, the upgraded controller serves and forks pre-upgrade rows, the seeded endpoint still returns its data exactly, and a new endpoint gets the post-upgrade image.",
		Params:      []string{"functional", "functional.upgrade", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
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
	}
	// The chart and the controller move in lockstep, so only adjacent releases are
	// a supported from-image pair.
	if u.fromController == "" || u.fromEndpoint == "" {
		t.Skip("in-place upgrade needs both functional.upgrade.from_controller_image and functional.upgrade.from_endpoint_image (the previous release's images) to deploy the pre-upgrade control plane; one or both are empty")
		return nil
	}
	if u.rows < 1 {
		return fmt.Errorf("functional.upgrade.rows is %d: the seed verification would pass on an empty table and prove nothing about the pre-upgrade data, so at least 1 row is required", u.rows)
	}
	if err := u.checkUpgradeIsReal(); err != nil {
		return err
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
		{"new_endpoint_uses_new_image", u.newEndpointUsesNewImage},
	}
	return runLifecycle(t.Context, step, f.td, phases, f.logf, f.collectDiagnostics, f.onTeardownDone)
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
// rolled is recorded, not asserted: an endpoint upgrade is not implemented today,
// and a rolled pod must stay legal once it is.
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
	kube := u.f.ampKube()
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
