// A ROLLING chain of in-place upgrades across REAL releases.
//
// features/amp_upgrade covers one hop, N-1 to N. This covers the shape a
// standing deployment actually lives through: installed some releases ago and
// upgraded again and again, never reinstalled. Bugs that only appear on the
// second or third migration over the same registry database, or on data written
// by one release and read by three later ones, are invisible to a single hop.
//
// The releases are the ones amp-build really cut. They come from the report
// portal (`GET /releases`, framework/releases), never from a hand-written list
// and never from a `custom-<sha>` branch build: a chain of builds nobody ships
// proves nothing about the upgrades a customer takes.
//
// A hop that fails does NOT end the run. It is recorded, the control plane is
// reinstalled from scratch at that release, the data is re-seeded, and the chain
// continues — so one broken hop costs one hop of coverage instead of four. Every
// failed hop is a red step in the portal, and the test fails at the end naming
// all of them.
//
// SCOPE, and what this test does NOT prove:
//   - The CONTROL PLANE is what upgrades. Endpoints created before a hop keep
//     running their old image: nothing here calls the endpoint-upgrade route
//     (`POST /endpoint-upgrades`, meko#809), so a hop moves the control plane and
//     leaves the fleet where it was. That is why their data surviving is the
//     interesting assertion and their pod image is recorded, not asserted.
//     Rolling the fleet per hop is a different test, and since `0011` recorded
//     each endpoint's version it is a writable one.
//   - The Helm CHART is this checkout's, at every hop. Only the images move. So
//     this covers image rollout and registry-database migration across releases,
//     not chart evolution across them. A chart change that a released controller
//     could not run would show up here as a hop failure, which is a true failure
//     but one whose cause is the test's own chart pinning — read the hop's
//     controller diagnostics before filing it as a product bug.
//
// To RUN rather than skip it needs a backing YugabyteDB it
// can name (deploy mode, not an attached standing control plane), and a portal
// that lists at least two releases. Each skip names the one that failed.
package features

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
	"github.com/yugabyte/amp/test/functional/framework/releases"
)

const (
	// chainTypesTable holds one row per id with a column per storage-relevant
	// type, each value derived from the id so any row can be checked alone.
	chainTypesTable = "amp_chain_types"
	// chainWideTable holds rows too wide for a heap page, so the upgrade is
	// exercised over out-of-line (TOASTed) values as well as inline ones.
	chainWideTable = "amp_chain_wide"
	// chainHopsTable records a batch of rows written AFTER each hop: data a
	// later release must still serve, written by an earlier one.
	chainHopsTable = "amp_chain_hops"

	// chainVFactor and chainPadLen tie v and pad to the id.
	chainVFactor = 7
	chainPadLen  = 64
	// chainWideLen is comfortably over the ~2KB TOAST threshold.
	chainWideLen = 16000
	// chainHopRows is how many rows each hop appends to chainHopsTable.
	chainHopRows = 100

	// DefaultChainHops is the functional.upgrade_chain.hops default: five
	// upgrades in a row.
	DefaultChainHops = 5
	// DefaultChainRows and DefaultChainWideRows are the seed sizes.
	DefaultChainRows     = 2000
	DefaultChainWideRows = 50

	// chainPodReadTimeout bounds the kubectl reads that resolve an endpoint pod.
	chainPodReadTimeout = 2 * time.Minute
	// chainReleaseFetchTimeout bounds the portal read. The portal is reference
	// data here, not the system under test.
	chainReleaseFetchTimeout = 1 * time.Minute
	// failedInstallCollectTimeout bounds the log collection that runs before a
	// broken install is deleted. It is evidence gathering, not the test: it must
	// not eat the budget the remaining hops need.
	failedInstallCollectTimeout = 2 * time.Minute
	// controllerServeTimeout and controllerServePoll bound the wait for the
	// upgraded controller to answer after its rollout reports complete.
	controllerServeTimeout = 5 * time.Minute
	controllerServePoll    = 5 * time.Second
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_upgrade_chain",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "upgrade"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Rolling in-place upgrades across real releases: install the control plane some releases back, seed data of every storage-relevant type through an endpoint, then upgrade release by release, verifying after each hop that the controller runs the new image and serves, that every seeded row is still exactly as written, that rows written after earlier hops survive, and that new writes and forks still work. A failed hop is recorded, reinstalled at that release, and the chain continues.",
		Params:      []string{"functional", "functional.upgrade_chain", "report.portal", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     150 * time.Minute,
		Run:         runAmpUpgradeChain,
	})
}

// chainConfig is every knob this test reads, resolved once at the top.
type chainConfig struct {
	hops      int
	rows      int
	wideRows  int
	versions  []string
	portalURL string
	reinstall bool
}

func chainConfigFrom(c *core.Config) chainConfig {
	return chainConfig{
		hops:      c.GetInt("functional.upgrade_chain.hops", DefaultChainHops),
		rows:      c.GetInt("functional.upgrade_chain.rows", DefaultChainRows),
		wideRows:  c.GetInt("functional.upgrade_chain.wide_rows", DefaultChainWideRows),
		versions:  c.GetStringSlice("functional.upgrade_chain.versions", nil),
		portalURL: chainPortalURL(c),
		reinstall: c.GetBool("functional.upgrade_chain.reinstall_on_failure", true),
	}
}

// chainPortalURL resolves the portal the release list is read from: this test's
// own knob, falling back to the run's portal.
//
// The fallback cannot be GetString's default argument. The knob is DECLARED as
// "" in default.yaml, so it is present, and a present empty string is what
// GetString returns — the default never fires and the run's portal is never
// consulted.
func chainPortalURL(c *core.Config) string {
	if url := strings.TrimSpace(c.GetString("functional.upgrade_chain.portal_url", "")); url != "" {
		return url
	}
	return strings.TrimSpace(c.GetString("report.portal.url", ""))
}

// hopOutcome is what one hop did, kept for the summary the test fails with.
type hopOutcome struct {
	From        string
	To          string
	Err         string
	Reinstalled bool
}

// upgradeChainRun carries the state the phases share. Everything below the
// chain is per-INSTALL: a reinstall replaces the control plane, so the cluster,
// the endpoint and the seeded data are all new.
type upgradeChainRun struct {
	f    *flow
	cfg  chainConfig
	step *core.StepHandle

	chain []releases.Release
	// installs counts control planes stood up: 1 normally, more when a failed
	// hop forced a reinstall. It also namespaces each install's resources.
	installs int
	outcomes []hopOutcome

	proj         ampCluster
	seedBranchID string
	seedLSN      string
	ep           controller.Endpoint
	// baselines are the per-table checksums captured at seed time. A table whose
	// rows never change again must still hash to exactly this after every hop.
	baselines map[string]string
	// hopRows is how many rows chainHopsTable holds, which grows by design.
	hopRows int
}

func (u *upgradeChainRun) set(key string, val any) {
	if u.step != nil {
		u.step.Set(key, val)
	}
}

func runAmpUpgradeChain(t *core.T) error {
	cfg := FromConfig(t.Options)
	u := &upgradeChainRun{cfg: chainConfigFrom(t.Options), baselines: map[string]string{}}
	if err := u.checkConfig(); err != nil {
		return err
	}
	if reason, ok := gateUpgradeBacking(t.Context, t.Env); !ok {
		t.Skip(reason)
		return nil
	}
	if reason, ok := gateNoDoubleProvision(t.Env); !ok {
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
	f.reg = openRunReapRegistry(f.logf)
	u.f = f

	// The chain is resolved before any infrastructure is stood up: an HTTP read
	// that finds no releases must not cost a universe first.
	var chainErr error
	_ = t.Step("resolve_chain", func(h *core.StepHandle) error {
		u.step = h
		chainErr = u.resolveChain(t.Context)
		return chainErr
	})
	if chainErr != nil {
		return chainErr
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
		{"install_base_release", u.installBase},
		{"seed", u.seed},
		{"upgrade_chain", u.runChain},
		{"new_endpoint_uses_new_image", u.newEndpointUsesNewImage},
		{"chain_result", u.chainResult},
	}
	return runLifecycle(t.Context, step, f.td, phases, f.logf, f.collectDiagnostics, f.onTeardownDone)
}

// checkConfig rejects a configuration that would run and prove nothing.
func (u *upgradeChainRun) checkConfig() error {
	if u.cfg.hops < 1 {
		return fmt.Errorf("functional.upgrade_chain.hops is %d: a chain with no upgrade in it would install a release, verify its own seed and pass, so at least 1 hop is required", u.cfg.hops)
	}
	if u.cfg.rows < 1 || u.cfg.wideRows < 1 {
		return fmt.Errorf("functional.upgrade_chain.rows=%d wide_rows=%d: an empty table verifies clean after any upgrade, so both must be at least 1", u.cfg.rows, u.cfg.wideRows)
	}
	return nil
}

// resolveChain picks the releases to install and upgrade through, oldest first.
//
// The explicit `versions` knob wins (a bisect, or a chain over releases the
// portal has forgotten). Otherwise the portal's newest hops+1 releases are
// taken, so a run always ends on the newest release there is.
func (u *upgradeChainRun) resolveChain(ctx context.Context) error {
	want := u.cfg.hops + 1
	if len(u.cfg.versions) > 0 {
		chain, err := chainFromTags(u.cfg.versions)
		if err != nil {
			return err
		}
		u.chain = chain
	} else {
		fctx, cancel := context.WithTimeout(ctx, chainReleaseFetchTimeout)
		defer cancel()
		list, err := releases.New(u.cfg.portalURL).WithLogf(u.f.logf).List(fctx, want)
		if err != nil {
			return core.AsEnvFailure(fmt.Errorf("read the real release list from the report portal %q: %w. The chain must be built from releases amp-build actually cut; set functional.upgrade_chain.versions to name them explicitly when the portal cannot be reached", u.cfg.portalURL, err))
		}
		// The portal answers newest first; a chain runs oldest to newest.
		for i := len(list) - 1; i >= 0; i-- {
			u.chain = append(u.chain, list[i])
		}
	}
	if len(u.chain) < 2 {
		return core.AsEnvFailure(fmt.Errorf("the release list holds %d release(s), so there is nothing to upgrade FROM and TO; a chain needs at least two", len(u.chain)))
	}
	if err := checkChainDistinct(u.chain); err != nil {
		return err
	}
	if len(u.chain) < want {
		// Fewer releases than asked for is normal early in a release line. Run
		// the hops that exist and say so, rather than skipping the coverage.
		u.f.logf("upgrade chain: portal lists %d releases, %d were asked for; running %d hops", len(u.chain), want, len(u.chain)-1)
	}
	tags := make([]string, 0, len(u.chain))
	for _, r := range u.chain {
		tags = append(tags, r.Tag)
	}
	u.set("chain", strings.Join(tags, " -> "))
	u.set("hops", len(u.chain)-1)
	u.set("base_release", u.chain[0].Tag)
	u.set("final_release", u.chain[len(u.chain)-1].Tag)
	return nil
}

// chainFromTags turns explicitly configured tags into the chain, oldest first.
func chainFromTags(tags []string) ([]releases.Release, error) {
	out := make([]releases.Release, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		version, sha, ok := releases.ParseTag(tag)
		if !ok {
			return nil, fmt.Errorf("functional.upgrade_chain.versions names %q, which is not a release tag (MAJOR.YYMMDD.PATCH-<sha>): a chain of branch builds proves nothing about the upgrades customers take", tag)
		}
		out = append(out, releases.Release{Version: version, Tag: tag, Sha: sha})
	}
	return out, nil
}

// checkChainDistinct rejects a repeated release. Re-applying the manifest a
// release is already running returns from the rollout at once, and every
// assertion after it would pass against a stack that never upgraded.
func checkChainDistinct(chain []releases.Release) error {
	for i := 1; i < len(chain); i++ {
		if chain[i].Tag == chain[i-1].Tag {
			return fmt.Errorf("the chain upgrades %s to itself at hop %d: re-applying a manifest the stack already runs completes immediately, so the hop would pass without upgrading anything", chain[i].Tag, i)
		}
	}
	return nil
}

// installBase stands the control plane up on the OLDEST release in the chain.
func (u *upgradeChainRun) installBase(ctx context.Context) error {
	return u.install(ctx, u.chain[0])
}

// install deploys a control plane at one release, in its own namespace and its
// own registry database, and registers its teardown.
//
// Each install gets a fresh database: a reinstall that inherited the previous
// install's registry rows would be an upgrade wearing a reinstall's clothes,
// and the hops after it would no longer be starting from a clean release.
func (u *upgradeChainRun) install(ctx context.Context, rel releases.Release) error {
	f := u.f
	acfg := ampstack.FromConfig(f.t.Options)
	if acfg.ControllerManifest != "" {
		return fmt.Errorf("this test upgrades by re-rendering the controller chart, but ampstack is in raw-manifest mode (env.ampstack controller manifest set), where the image fields are ignored: run it without a raw controller manifest")
	}
	u.installs++
	acfg.Namespace = u.installNamespace()
	acfg.Backing = ybbacking.ExternalBacking(f.store)
	// The database name has to go on the BACKING, not on acfg.DBName: in
	// external-backing mode ampstack reads Backing.DBName and ignores the other
	// (ampstack.storageDBName), so setting acfg.DBName here would silently leave
	// every install sharing yb_storage and a reinstall inheriting the registry
	// rows of the install it replaced.
	acfg.Backing.DBName = fmt.Sprintf("yb_storage_chain_%s_%d", strings.ReplaceAll(f.suffix(), "-", "_"), u.installs)
	acfg.ExtraNamespaceLabels = map[string]string{"ampqa-foundation": f.suffix()}
	acfg.ControllerImage = rel.ControllerImage()
	acfg.EndpointImage = rel.EndpointImage()

	if u.installs > 1 {
		// Before the deploy, not after: the namespace is created in the deploy's
		// first step, so a deploy that fails halfway leaves one behind. The plan
		// recorded at provision time named only the first namespace, and reaping
		// a namespace that was never created is a no-op.
		f.addAmpNamespaceToReapPlan(acfg.Namespace)
	}
	app := ampstack.New(acfg)
	f.ampApp = app
	info, derr := app.Deploy(ctx)
	f.register("close amp port-forward "+acfg.Namespace, func(ctx context.Context) error { return app.Close(ctx) })
	f.register("teardown amp namespace "+acfg.Namespace, func(ctx context.Context) error { return app.Teardown(ctx) })
	if derr != nil {
		return core.AsEnvFailure(fmt.Errorf("deploy the control plane at release %s (controller %s, endpoint %s): %w. Standing a released version up is environment work; check the release's images exist in the configured registry",
			rel.Version, acfg.ControllerImage, acfg.EndpointImage, derr))
	}
	u.set("install", u.installs)
	u.set("registry_db", acfg.Backing.DBName)
	u.set("installed_release", rel.Tag)
	u.set("installed_namespace", acfg.Namespace)
	u.set("controller_image", info.Images["controller"])
	u.set("endpoint_image", info.Images["endpoint"])
	return nil
}

// installNamespace names the namespace of the current install. The first keeps
// the flow's namespace, which is the one the crash-recovery reap plan recorded.
func (u *upgradeChainRun) installNamespace() string {
	if u.installs <= 1 {
		return u.f.namespace()
	}
	return fmt.Sprintf("%s-i%d", u.f.namespace(), u.installs)
}

// seed creates the cluster, endpoint and tables, writes every data shape, and
// captures the baseline checksums the whole chain is judged against.
func (u *upgradeChainRun) seed(ctx context.Context) error {
	f := u.f
	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("upgc-%s-%d", f.suffix(), u.installs)
	// The cluster-delete cascade is deliberately NOT registered: this test tears
	// the whole amp namespace down (and a reinstall replaces the control plane
	// that owns these rows), so a registered delete would run against a
	// controller that is already gone and fail teardown for no reason.
	proj, err := setupAmpCluster(ctx, cl, name, f.cfg.PgVersion, func(string, func(context.Context) error) {})
	if err != nil {
		return err
	}
	u.proj = proj
	// Seed on the cluster's own branch: a child branch cannot be forked yet,
	// because the parent carries no endpoint and so has no fork point.
	u.seedBranchID = proj.branchID

	ep, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, u.seedBranchID)
	if err != nil {
		return fmt.Errorf("create the endpoint the chain seeds through: %w", err)
	}
	u.ep = ep

	conn, err := f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect the endpoint %s at %s: %w", ep.ID, ep.Address, err)
	}
	defer conn.Close()

	if err := seedChainData(ctx, conn, u.cfg.rows, u.cfg.wideRows); err != nil {
		return err
	}
	u.hopRows = 0
	if err := verifyChainData(ctx, conn, u.cfg.rows, u.cfg.wideRows, u.hopRows, "the endpoint that seeded it"); err != nil {
		return err
	}
	base, err := chainChecksums(ctx, conn)
	if err != nil {
		return err
	}
	u.baselines = base
	u.seedLSN, err = currentLSN(ctx, conn)
	if err != nil {
		return err
	}
	u.set("seeded_rows", u.cfg.rows)
	u.set("seeded_wide_rows", u.cfg.wideRows)
	u.set("seed_lsn", u.seedLSN)
	for table, sum := range base {
		u.set("baseline_"+table, sum)
	}
	return nil
}

// runChain walks the chain, one hop per step. A hop's own step goes red on
// failure and the chain continues from a reinstall, so one broken release costs
// one hop of coverage rather than every hop after it.
func (u *upgradeChainRun) runChain(ctx context.Context) error {
	for i := 1; i < len(u.chain); i++ {
		from, to := u.chain[i-1], u.chain[i]
		hopErr := u.f.t.Step(fmt.Sprintf("upgrade_hop_%d", i), func(h *core.StepHandle) error {
			u.step = h
			h.Set("from", from.Tag)
			h.Set("to", to.Tag)
			return u.hop(ctx, to)
		})
		out := hopOutcome{From: from.Tag, To: to.Tag}
		if hopErr != nil {
			out.Err = hopErr.Error()
			u.f.logf("upgrade chain: hop %d (%s -> %s) failed: %v", i, from.Tag, to.Tag, hopErr)
			if !u.cfg.reinstall {
				u.outcomes = append(u.outcomes, out)
				return hopErr
			}
			if err := u.recover(ctx, to, i); err != nil {
				u.outcomes = append(u.outcomes, out)
				return fmt.Errorf("hop %d (%s -> %s) failed (%v) and the recovery install at %s failed too, so the rest of the chain cannot run: %w",
					i, from.Tag, to.Tag, hopErr, to.Tag, err)
			}
			out.Reinstalled = true
		}
		u.outcomes = append(u.outcomes, out)
	}
	return nil
}

// hop upgrades the control plane one release and verifies what an upgrade must
// leave standing: a controller running the new image and answering, the seeded
// data byte-for-byte intact through a fresh connection, a working write path,
// and a control-plane task over rows an older release wrote.
func (u *upgradeChainRun) hop(ctx context.Context, to releases.Release) error {
	f := u.f
	before, verr := u.controllerVersion(ctx)
	if verr != nil {
		u.set("version_before", "unknown: "+verr.Error())
	} else {
		u.set("version_before", before)
	}

	if err := f.ampApp.UpgradeImages(ctx, to.ControllerImage(), to.EndpointImage()); err != nil {
		return fmt.Errorf("the in-place upgrade to %s never completed: %w. The new controller image must roll out over the running one with the previous release's registry database in place", to.Tag, err)
	}
	if err := u.controllerRunsImage(ctx, to.ControllerImage()); err != nil {
		return err
	}

	cl, err := u.waitControllerServes(ctx, to)
	if err != nil {
		return err
	}
	after, err := u.controllerVersion(ctx)
	if err != nil {
		u.set("version_after", "unknown: "+err.Error())
	} else {
		u.set("version_after", after)
		if stampedVersion(before) && stampedVersion(after) && before == after {
			return fmt.Errorf("the controller still reports version %q after the upgrade to %s, so the rollout did not replace the running controller", after, to.Tag)
		}
	}

	conn, err := f.endpointSQL(ctx, u.ep)
	if err != nil {
		return fmt.Errorf("the endpoint %s created before the upgrade to %s no longer accepts connections at %s: %w", u.ep.ID, to.Tag, u.ep.Address, err)
	}
	defer conn.Close()

	if err := verifyChainData(ctx, conn, u.cfg.rows, u.cfg.wideRows, u.hopRows, "the endpoint after the upgrade to "+to.Tag); err != nil {
		return err
	}
	if err := u.verifyBaselines(ctx, conn, to.Tag); err != nil {
		return err
	}
	// New rows on the upgraded stack, which the NEXT hop then has to carry.
	if err := appendHopRows(ctx, conn, u.hopRows, chainHopRows, to.Tag); err != nil {
		return err
	}
	u.hopRows += chainHopRows
	u.set("hop_rows_total", u.hopRows)

	// A fork exercises the task pipeline against registry rows an older release
	// wrote — the migration path, not just the process.
	forkName := fmt.Sprintf("upgc-fork-%s-%d", f.suffix(), len(u.outcomes)+1)
	if _, err := cl.CreateBranchAndWait(ctx, u.proj.poolID, u.proj.projID, controller.CreateBranchRequest{
		Name:           forkName,
		ParentBranchID: u.seedBranchID,
		ForkPointLSN:   u.seedLSN,
	}); err != nil {
		return fmt.Errorf("the controller at %s could not fork branch %s at the pre-upgrade LSN %s: %w. Migrations and the task pipeline must work against registry rows written by an earlier release",
			to.Tag, u.seedBranchID, u.seedLSN, err)
	}
	return nil
}

// recover reinstalls the control plane at the release whose hop failed and
// re-seeds it, so the remaining hops still run. It is not a retry: the failed
// hop stays failed and the run stays red.
func (u *upgradeChainRun) recover(ctx context.Context, at releases.Release, hop int) error {
	u.f.logf("upgrade chain: reinstalling at %s after hop %d failed, so the remaining hops still run", at.Tag, hop)
	return u.f.t.Step(fmt.Sprintf("reinstall_after_hop_%d", hop), func(h *core.StepHandle) error {
		u.step = h
		h.Set("release", at.Tag)
		h.Set("reason", "the upgrade to this release failed; the chain continues from a clean install of it")
		// Collect before killing: the controller that refused the hop is in the
		// install about to be deleted, and the end-of-run diagnostics only see
		// whatever is standing then. A run that recovered used to end with no
		// trace of why the hop failed at all.
		u.collectFailedInstall(ctx, hop)
		// The broken install goes second. Its endpoint pod holds a whole node's
		// ephemeral storage (20GiB per vCPU, meko#602), so leaving it standing
		// until the run's teardown means the replacement endpoint has nowhere to
		// be scheduled — which is exactly how the third install of a run failed
		// with no compute workload at all. Best-effort: a namespace that will not
		// go must not hide the hop failure that led here.
		if err := u.f.ampApp.Teardown(ctx); err != nil {
			u.f.logf("upgrade chain: tearing the failed install down (best-effort): %v", err)
			h.Set("previous_install_teardown", "failed: "+err.Error())
		}
		if err := u.install(ctx, at); err != nil {
			return err
		}
		return u.seed(ctx)
	})
}

// waitControllerServes re-resolves the controller after the rollout and waits
// for it to answer.
//
// The rollout replaces the pod, so the address held from before the upgrade can
// point at a pod that no longer exists — and on the exec reach path the curl
// itself runs in the pod being replaced, which surfaced as a 120s connection
// timeout rather than a refusal. Reconnecting picks up the live target, and
// polling rides out the seconds between "rollout complete" and "the new process
// is listening".
func (u *upgradeChainRun) waitControllerServes(ctx context.Context, to releases.Release) (*controller.Client, error) {
	deadline := time.Now().Add(controllerServeTimeout)
	var last error
	for {
		cl, _, err := u.f.ampApp.ReconnectController(ctx)
		if err == nil {
			if last = cl.Readyz(ctx); last == nil {
				return cl, nil
			}
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the controller never answered /readyz within %s of the upgrade to %s: %w", controllerServeTimeout, to.Tag, last)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(controllerServePoll):
		}
	}
}

// collectFailedInstall attaches the logs of the install whose hop just failed,
// under a name that says which hop it belonged to so it cannot be confused with
// the install that finished the run.
func (u *upgradeChainRun) collectFailedInstall(ctx context.Context, hop int) {
	if u.f.ampApp == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, failedInstallCollectTimeout)
	defer cancel()
	sink := prefixedSink{
		to:     attachSink{t: u.f.t, logf: u.f.logf},
		prefix: fmt.Sprintf("Failed hop %d install %d/", hop, u.installs),
	}
	if err := u.f.ampApp.Collect(cctx, sink); err != nil {
		u.f.logf("upgrade chain: collect the failed install's logs (best-effort): %v", err)
	}
}

// controllerVersion is the version string the controller reports, or an error.
func (u *upgradeChainRun) controllerVersion(ctx context.Context) (string, error) {
	cl, err := u.f.controllerClient(ctx)
	if err != nil {
		return "", err
	}
	v, err := cl.Version(ctx)
	if err != nil {
		return "", err
	}
	return v.Version, nil
}

// kube is kubectl bound to the namespace of the CURRENT install. flow.ampKube
// is bound to the flow's one namespace, which stops being right the moment a
// failed hop forces a reinstall into a new one.
func (u *upgradeChainRun) kube() *k8s.Kubectl {
	if u.f.ampApp == nil {
		return nil
	}
	return u.f.ampApp.Kube()
}

// controllerRunsImage proves the controller was replaced without depending on
// any version string: the running pod must carry the image it was upgraded to.
func (u *upgradeChainRun) controllerRunsImage(ctx context.Context, want string) error {
	kube := u.kube()
	if kube == nil {
		return fmt.Errorf("no kubectl is available for the amp namespace, so the controller pod's image cannot be read after the upgrade, and nothing else proves the controller was replaced")
	}
	name := ampstack.FromConfig(u.f.t.Options).ControllerName
	pctx, cancel := context.WithTimeout(ctx, chainPodReadTimeout)
	defer cancel()
	images, err := kube.GetSelector(pctx, "pod", "app="+name, "{.items[*].spec.containers[*].image}")
	if err != nil {
		return fmt.Errorf("read the container images of the controller pods (selector app=%s) after the upgrade: %w", name, err)
	}
	images = strings.TrimSpace(images)
	u.set("controller_pod_images", images)
	if images == "" {
		return fmt.Errorf("no pod matches app=%s after the upgrade, so the upgraded controller is not running", name)
	}
	if !imageMatches(images, want) {
		return fmt.Errorf("the controller pods run %q after the upgrade, want %q: the rollout did not replace the running controller", images, want)
	}
	return nil
}

// verifyBaselines compares every unchanging table against the checksum captured
// at seed time. The per-row checks below catch a value that drifted; this
// catches anything else that moved — a dropped row, a re-encoded column, a
// changed collation in the aggregate.
func (u *upgradeChainRun) verifyBaselines(ctx context.Context, conn sqlConn, tag string) error {
	got, err := chainChecksums(ctx, conn)
	if err != nil {
		return err
	}
	for table, want := range u.baselines {
		if got[table] != want {
			return fmt.Errorf("%s hashes to %q after the upgrade to %s, want the %q it hashed to when it was seeded: the table's contents changed across the upgrade", table, got[table], tag, want)
		}
	}
	return nil
}

// newEndpointUsesNewImage asserts the last release's controller deploys
// endpoints from the image tag it was upgraded with. It runs once, at the end:
// an endpoint create costs minutes, and the answer cannot differ per hop.
func (u *upgradeChainRun) newEndpointUsesNewImage(ctx context.Context) error {
	f := u.f
	final := u.chain[len(u.chain)-1]
	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	forkName := "upgc-final-" + f.suffix()
	if _, err := cl.CreateBranchAndWait(ctx, u.proj.poolID, u.proj.projID, controller.CreateBranchRequest{
		Name:           forkName,
		ParentBranchID: u.seedBranchID,
		ForkPointLSN:   u.seedLSN,
	}); err != nil {
		return fmt.Errorf("fork the seeded branch on the final release %s: %w", final.Tag, err)
	}
	fork, err := findBranchCtl(ctx, cl, u.proj.poolID, u.proj.projID, forkName)
	if err != nil {
		return err
	}
	ep, err := createEndpoint(ctx, cl, u.proj.poolID, u.proj.projID, fork.BranchID)
	if err != nil {
		return fmt.Errorf("create an endpoint on the fork taken after the last upgrade: %w", err)
	}
	pod, images, err := endpointPodImages(ctx, u.kube(), ep.ID)
	if err != nil {
		return fmt.Errorf("read the pod of the endpoint created after the last upgrade: %w", err)
	}
	u.set("new_endpoint_pod", pod)
	u.set("new_endpoint_image", images)
	if !imageMatches(images, final.EndpointImage()) {
		return fmt.Errorf("the controller at %s is still deploying an older endpoint image: pod %s runs %q, want %q; the controller must deploy the endpoint image tag it was given",
			final.Tag, pod, images, final.EndpointImage())
	}

	// The fork is taken at the SEED LSN, on the last release in the chain. So it
	// must serve the seeded data exactly, and none of the rows the hops wrote
	// after that LSN: a fork carrying post-fork-point rows would mean the
	// branch point stopped being honoured somewhere across the upgrades.
	conn, err := f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect the endpoint on the post-chain fork: %w", err)
	}
	defer conn.Close()
	return verifyChainData(ctx, conn, u.cfg.rows, u.cfg.wideRows, 0,
		fmt.Sprintf("the endpoint on the fork taken at the seed LSN %s after the last upgrade (it must hold the seeded rows and none of the %d written after that LSN)", u.seedLSN, u.hopRows))
}

// chainResult fails the run when any hop failed, naming every one of them. The
// individual hops are already red steps; this is what makes the TEST red after
// a recovery let the chain continue.
func (u *upgradeChainRun) chainResult(context.Context) error {
	var failed []string
	for _, o := range u.outcomes {
		if o.Err != "" {
			failed = append(failed, fmt.Sprintf("%s -> %s: %s", o.From, o.To, o.Err))
		}
	}
	u.set("hops_run", len(u.outcomes))
	u.set("hops_failed", len(failed))
	u.set("installs", u.installs)
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("%d of %d upgrade hops failed:\n%s", len(failed), len(u.outcomes), strings.Join(failed, "\n"))
}

// --- data shapes ---

// seedChainData creates the tables and writes every shape the chain carries:
// inline values of each storage-relevant type, out-of-line (TOASTed) wide rows,
// and the indexes that have to keep answering after an upgrade.
func seedChainData(ctx context.Context, c sqlConn, rows, wideRows int) error {
	ddl := []string{
		"CREATE TABLE " + chainTypesTable + " (" +
			"id int PRIMARY KEY, v bigint, num numeric(20,6), pad text, blob bytea, " +
			"doc jsonb, ts timestamptz, uid uuid, flag boolean, tags text[])",
		"CREATE INDEX " + chainTypesTable + "_v_idx ON " + chainTypesTable + " (v)",
		"CREATE INDEX " + chainTypesTable + "_doc_idx ON " + chainTypesTable + " USING gin (doc jsonb_path_ops)",
		"CREATE TABLE " + chainWideTable + " (id int PRIMARY KEY, body text)",
		"CREATE TABLE " + chainHopsTable + " (id int PRIMARY KEY, release text NOT NULL, note text NOT NULL)",
	}
	for _, stmt := range ddl {
		if _, err := c.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("create the seed schema (%s): %w", stmt, err)
		}
	}
	if _, err := c.Exec(ctx,
		"INSERT INTO "+chainTypesTable+" (id, v, num, pad, blob, doc, ts, uid, flag, tags) "+
			"SELECT g, g::bigint * $1, (g * 1.5)::numeric(20,6), repeat('x', $2), decode(md5(g::text), 'hex'), "+
			"jsonb_build_object('id', g, 'sq', g * g), timestamptz 'epoch' + (g || ' seconds')::interval, "+
			"md5(g::text)::uuid, (g % 2 = 0), ARRAY['t' || g, 'u' || (g * 2)] "+
			"FROM generate_series(1, $3) g",
		chainVFactor, chainPadLen, rows); err != nil {
		return fmt.Errorf("seed %d typed rows: %w", rows, err)
	}
	if _, err := c.Exec(ctx,
		"INSERT INTO "+chainWideTable+" (id, body) SELECT g, repeat(md5(g::text), $1) FROM generate_series(1, $2) g",
		chainWideLen/32, wideRows); err != nil {
		return fmt.Errorf("seed %d wide rows: %w", wideRows, err)
	}
	return nil
}

// appendHopRows writes a batch of rows on the release just upgraded to. They are
// data an EARLIER release never saw, which every later hop must still serve.
func appendHopRows(ctx context.Context, c sqlConn, from, count int, release string) error {
	// generate_series is overloaded and a bare placeholder is `unknown`, which
	// resolves to no single overload: the casts are what make the call legal.
	if _, err := c.Exec(ctx,
		"INSERT INTO "+chainHopsTable+" (id, release, note) SELECT g, $1, md5(g::text) FROM generate_series($2::int, $3::int) g",
		release, from+1, from+count); err != nil {
		return fmt.Errorf("write %d rows on release %s: %w", count, release, err)
	}
	return nil
}

// verifyChainData asserts the seeded data is exactly as written: the row counts,
// and not one row of any type whose value drifted. Every predicate re-derives
// the value from the id, so a re-encoded column fails on its own column rather
// than as a diffuse checksum mismatch.
func verifyChainData(ctx context.Context, c sqlConn, rows, wideRows, hopRows int, where string) error {
	counts := []struct {
		table string
		want  int
	}{
		{chainTypesTable, rows},
		{chainWideTable, wideRows},
		{chainHopsTable, hopRows},
	}
	for _, cnt := range counts {
		got, err := c.QueryInt(ctx, "SELECT count(*) FROM "+cnt.table)
		if err != nil {
			return fmt.Errorf("count %s on %s: %w", cnt.table, where, err)
		}
		if got != int64(cnt.want) {
			return fmt.Errorf("%s serves %d rows of %s, want %d: rows written before this point did not survive", where, got, cnt.table, cnt.want)
		}
	}

	checks := []struct {
		what string
		pred string
	}{
		{"v = id * " + fmt.Sprint(chainVFactor), fmt.Sprintf("v <> id::bigint * %d", chainVFactor)},
		{"num = id * 1.5", "num <> (id * 1.5)::numeric(20,6)"},
		{fmt.Sprintf("pad is %d bytes", chainPadLen), fmt.Sprintf("length(pad) <> %d", chainPadLen)},
		{"blob = md5(id)", "blob <> decode(md5(id::text), 'hex')"},
		{"doc = {id, sq}", "doc <> jsonb_build_object('id', id, 'sq', id * id)"},
		{"ts = epoch + id seconds", "ts <> timestamptz 'epoch' + (id || ' seconds')::interval"},
		{"uid = md5(id)::uuid", "uid <> md5(id::text)::uuid"},
		{"flag = (id even)", "flag <> (id % 2 = 0)"},
		{"tags = {t<id>, u<2id>}", "tags <> ARRAY['t' || id, 'u' || (id * 2)]"},
	}
	for _, ch := range checks {
		bad, err := c.QueryInt(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE %s", chainTypesTable, ch.pred))
		if err != nil {
			return fmt.Errorf("check %s on %s: %w", ch.what, where, err)
		}
		if bad != 0 {
			return fmt.Errorf("%s serves %d of %d rows of %s where %s no longer holds, so that column did not survive intact", where, bad, rows, chainTypesTable, ch.what)
		}
	}

	// The index has to answer too: a sequential scan would pass the checks above
	// even with a corrupt index behind them.
	indexed, err := c.QueryInt(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE v = $1::bigint", chainTypesTable), int64(rows)*chainVFactor)
	if err != nil {
		return fmt.Errorf("read %s through its v index on %s: %w", chainTypesTable, where, err)
	}
	if indexed != 1 {
		return fmt.Errorf("%s finds %d rows with v = %d in %s, want exactly the last seeded row: the index no longer answers for data written before the upgrade",
			where, indexed, int64(rows)*chainVFactor, chainTypesTable)
	}
	jsonIndexed, err := c.QueryInt(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE doc @> jsonb_build_object('id', $1::int)", chainTypesTable), rows)
	if err != nil {
		return fmt.Errorf("read %s through its jsonb index on %s: %w", chainTypesTable, where, err)
	}
	if jsonIndexed != 1 {
		return fmt.Errorf("%s finds %d rows matching doc @> {\"id\": %d}, want exactly one: the GIN index no longer answers for data written before the upgrade", where, jsonIndexed, rows)
	}

	badWide, err := c.QueryInt(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE body <> repeat(md5(id::text), %d)", chainWideTable, chainWideLen/32))
	if err != nil {
		return fmt.Errorf("check the wide rows on %s: %w", where, err)
	}
	if badWide != 0 {
		return fmt.Errorf("%s serves %d of %d wide rows of %s that no longer match what was written, so out-of-line values did not survive intact", where, badWide, wideRows, chainWideTable)
	}

	if hopRows > 0 {
		badHop, err := c.QueryInt(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE note <> md5(id::text)", chainHopsTable))
		if err != nil {
			return fmt.Errorf("check the per-hop rows on %s: %w", where, err)
		}
		if badHop != 0 {
			return fmt.Errorf("%s serves %d of %d rows of %s that no longer match what the hop wrote, so data written between upgrades did not survive", where, badHop, hopRows, chainHopsTable)
		}
	}
	return nil
}

// chainChecksums hashes the tables whose contents must never change again after
// the seed. chainHopsTable is absent by design: it grows at every hop, so its
// invariants are checked per row instead.
func chainChecksums(ctx context.Context, c sqlConn) (map[string]string, error) {
	out := map[string]string{}
	for _, table := range []string{chainTypesTable, chainWideTable} {
		sum, err := c.QueryString(ctx, fmt.Sprintf("SELECT md5(string_agg(t::text, '|' ORDER BY t.id)) FROM %s t", table))
		if err != nil {
			return nil, fmt.Errorf("checksum %s: %w", table, err)
		}
		out[table] = sum
	}
	return out, nil
}
