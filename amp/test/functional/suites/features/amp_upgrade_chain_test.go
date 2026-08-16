package features

import (
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/releases"
)

// Every knob the chain reads must be declared in default.yaml, and the declared
// values must be the ones that resolve. An undeclared knob is invisible to
// anyone reading the config for something to override.
func TestUpgradeChainKnobsAreDeclared(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load shipped defaults: %v", err)
	}
	for _, key := range []string{
		"functional.upgrade_chain.hops",
		"functional.upgrade_chain.rows",
		"functional.upgrade_chain.wide_rows",
		"functional.upgrade_chain.versions",
		"functional.upgrade_chain.portal_url",
		"functional.upgrade_chain.reinstall_on_failure",
	} {
		if !cfg.Has(key) {
			t.Errorf("%s is read by the test but not declared in configs/default.yaml", key)
		}
	}
	c := chainConfigFrom(cfg)
	if c.hops != DefaultChainHops {
		t.Errorf("hops = %d, want the declared %d", c.hops, DefaultChainHops)
	}
	if c.rows != DefaultChainRows || c.wideRows != DefaultChainWideRows {
		t.Errorf("rows = %d, wide_rows = %d, want %d and %d", c.rows, c.wideRows, DefaultChainRows, DefaultChainWideRows)
	}
	if !c.reinstall {
		t.Error("reinstall_on_failure defaults off; a single broken hop would then cost every hop after it")
	}
}

// A chain with no upgrade in it, or with no data in it, would install a release,
// verify its own seed and pass.
func TestUpgradeChainRejectsAVacuousConfig(t *testing.T) {
	for _, c := range []chainConfig{
		{hops: 0, rows: 10, wideRows: 10},
		{hops: 2, rows: 0, wideRows: 10},
		{hops: 2, rows: 10, wideRows: 0},
	} {
		u := &upgradeChainRun{cfg: c}
		if err := u.checkConfig(); err == nil {
			t.Errorf("%+v was accepted; it would pass while proving nothing", c)
		}
	}
	u := &upgradeChainRun{cfg: chainConfig{hops: 5, rows: 10, wideRows: 5}}
	if err := u.checkConfig(); err != nil {
		t.Errorf("a real config was rejected: %v", err)
	}
}

// Re-applying the manifest a stack already runs returns from the rollout at
// once, so a repeated release is a hop that upgrades nothing.
func TestChainMustNotRepeatARelease(t *testing.T) {
	same := []releases.Release{
		{Version: "0.260811.0", Tag: "0.260811.0-c06368f8d"},
		{Version: "0.260812.0", Tag: "0.260812.0-5fb27dca5"},
		{Version: "0.260812.0", Tag: "0.260812.0-5fb27dca5"},
	}
	if err := checkChainDistinct(same); err == nil {
		t.Error("a chain that upgrades a release to itself was accepted")
	}
	distinct := same[:2]
	if err := checkChainDistinct(distinct); err != nil {
		t.Errorf("a real chain was rejected: %v", err)
	}
}

// The explicit version list is the escape hatch, not a way to smuggle branch
// builds into a test about releases.
func TestChainFromTagsRejectsBranchBuilds(t *testing.T) {
	if _, err := chainFromTags([]string{"0.260811.0-c06368f8d", "custom-9373f3e70"}); err == nil {
		t.Error("custom-<sha> was accepted as a chain entry")
	}
	got, err := chainFromTags([]string{"0.260811.0-c06368f8d", "0.260812.0-5fb27dca5"})
	if err != nil {
		t.Fatalf("chainFromTags: %v", err)
	}
	if len(got) != 2 || got[0].Version != "0.260811.0" || got[1].Tag != "0.260812.0-5fb27dca5" {
		t.Errorf("got %+v, want the two releases in the order given (oldest first)", got)
	}
}

// A recovered hop must still fail the run. The whole point of reinstalling is
// to keep measuring the remaining hops, not to forgive the broken one.
func TestChainResultFailsOnAnyFailedHop(t *testing.T) {
	u := &upgradeChainRun{outcomes: []hopOutcome{
		{From: "a", To: "b"},
		{From: "b", To: "c", Err: "rollout never completed", Reinstalled: true},
		{From: "c", To: "d"},
	}}
	err := u.chainResult(nil)
	if err == nil {
		t.Fatal("a hop failed and was reinstalled, and the run passed")
	}
	if !strings.Contains(err.Error(), "b -> c") || !strings.Contains(err.Error(), "rollout never completed") {
		t.Errorf("the failure does not name the hop and its error: %v", err)
	}
	if err := (&upgradeChainRun{outcomes: []hopOutcome{{From: "a", To: "b"}}}).chainResult(nil); err != nil {
		t.Errorf("a clean chain failed: %v", err)
	}
}

// Each install must get its own registry database. A reinstall that inherited
// the previous one's rows would be an upgrade wearing a reinstall's clothes.
func TestReinstallGetsAFreshNamespace(t *testing.T) {
	body := readSource(t, "amp_upgrade_chain.go")
	i := strings.Index(body, "func (u *upgradeChainRun) install(")
	if i < 0 {
		t.Fatal("install is gone")
	}
	fn := body[i : i+2600]
	// The name must land on the BACKING: external-backing mode reads
	// Backing.DBName and ignores acfg.DBName, so the obvious spelling is a
	// silent no-op that leaves every install on yb_storage.
	if !strings.Contains(fn, "acfg.Backing.DBName = ") {
		t.Error("an install no longer names its own registry database on the backing, so a reinstall would inherit the previous install's rows")
	}
	if strings.Contains(fn, "\tacfg.DBName = ") {
		t.Error("acfg.DBName is ignored in external-backing mode; setting it reads as isolation the run does not have")
	}
	if !strings.Contains(fn, "u.installNamespace()") {
		t.Error("an install no longer names its own namespace")
	}
}

// Every column the seed writes must be checked back. A type added to the seed
// and forgotten in the verification is a shape the chain carries but never
// looks at.
func TestChainVerifiesEveryTypeItSeeds(t *testing.T) {
	body := readSource(t, "amp_upgrade_chain.go")
	i := strings.Index(body, "func verifyChainData(")
	if i < 0 {
		t.Fatal("verifyChainData is gone")
	}
	fn := body[i:]
	if j := strings.Index(fn[1:], "\nfunc "); j >= 0 {
		fn = fn[:j+1]
	}
	for _, col := range []string{"v <>", "num <>", "length(pad)", "blob <>", "doc <>", "ts <>", "uid <>", "flag <>", "tags <>"} {
		if !strings.Contains(fn, col) {
			t.Errorf("the seeded column checked by %q is no longer verified after an upgrade", col)
		}
	}
	if !strings.Contains(fn, "jsonb index") || !strings.Contains(fn, "v index") {
		t.Error("the indexes are no longer read back; a sequential scan would pass every value check with a corrupt index behind it")
	}
	if !strings.Contains(fn, "chainWideTable") {
		t.Error("the out-of-line (TOASTed) rows are no longer verified")
	}
}

// A reinstall moves the control plane into a new namespace. Reading pods
// through the flow's namespace-pinned kubectl would then look at the namespace
// the previous install was torn out of, and report the new controller missing.
func TestChainReadsPodsInTheCurrentInstallsNamespace(t *testing.T) {
	body := readSource(t, "amp_upgrade_chain.go")
	if strings.Contains(body, "u.f.ampKube()") {
		t.Error("the chain reads pods through the flow's namespace, which is only the FIRST install's")
	}
	if !strings.Contains(body, "return u.f.ampApp.Kube()") {
		t.Error("the chain's kubectl is no longer taken from the deployed stack, so it can drift from the namespace that stack lives in")
	}
}

// The chain's own portal knob is declared empty in default.yaml, so a present
// empty string is what GetString returns and a default argument would never
// fire. The run's portal has to be consulted explicitly, or every pipeline run
// resolves no releases at all.
func TestChainPortalURLFallsBackToTheRunsPortal(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, []string{"report.portal.url=http://portal.example"})
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	if got := chainPortalURL(cfg); got != "http://portal.example" {
		t.Errorf("portal URL = %q, want the run's portal: the declared-empty knob shadowed it", got)
	}
	own, err := core.LoadConfig(nil, nil, []string{
		"report.portal.url=http://portal.example",
		"functional.upgrade_chain.portal_url=http://other.example",
	})
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	if got := chainPortalURL(own); got != "http://other.example" {
		t.Errorf("portal URL = %q, want the test's own knob to win when set", got)
	}
}

// The rollout replaces the controller pod, so an address held from before the
// upgrade can point at a pod that no longer exists. A single Readyz shot right
// after the rollout timed out for 120s and failed a hop that was fine.
func TestChainWaitsForTheUpgradedControllerToServe(t *testing.T) {
	body := readSource(t, "amp_upgrade_chain.go")
	i := strings.Index(body, "func (u *upgradeChainRun) waitControllerServes(")
	if i < 0 {
		t.Fatal("waitControllerServes is gone; a single Readyz call after a rollout is a race")
	}
	fn := body[i:]
	if j := strings.Index(fn[1:], "\nfunc "); j >= 0 {
		fn = fn[:j+1]
	}
	if !strings.Contains(fn, "ReconnectController(") {
		t.Error("the controller is no longer re-resolved after the rollout, so the pre-upgrade address can outlive its pod")
	}
	if !strings.Contains(fn, "controllerServeTimeout") {
		t.Error("the wait is no longer bounded by a deadline")
	}
}

// Each endpoint pod holds a whole node's ephemeral storage. Leaving the broken
// install standing until the run's teardown left the replacement endpoint with
// nowhere to be scheduled.
func TestReinstallTearsTheFailedInstallDownFirst(t *testing.T) {
	body := readSource(t, "amp_upgrade_chain.go")
	i := strings.Index(body, "func (u *upgradeChainRun) recover(")
	if i < 0 {
		t.Fatal("recover is gone")
	}
	fn := body[i:]
	if j := strings.Index(fn[1:], "\nfunc "); j >= 0 {
		fn = fn[:j+1]
	}
	col, td, ins := strings.Index(fn, "collectFailedInstall("), strings.Index(fn, "ampApp.Teardown("), strings.Index(fn, "u.install(")
	if col < 0 || col > td {
		t.Error("the failed install's logs are not collected before it is deleted; a recovered run then ends with no trace of why the hop failed")
	}
	if td < 0 {
		t.Fatal("the failed install is no longer torn down, so its endpoint keeps a node the replacement needs")
	}
	if ins < 0 || td > ins {
		t.Error("the teardown must come before the new install")
	}
}

// The final fork is taken at the SEED LSN, so it must NOT carry the rows the
// hops wrote afterwards. Verifying it against the running hop-row total asserted
// the opposite of the branch point.
func TestFinalForkIsVerifiedAtItsForkPoint(t *testing.T) {
	body := readSource(t, "amp_upgrade_chain.go")
	i := strings.Index(body, "func (u *upgradeChainRun) newEndpointUsesNewImage(")
	if i < 0 {
		t.Fatal("newEndpointUsesNewImage is gone")
	}
	fn := body[i:]
	if j := strings.Index(fn[1:], "\nfunc "); j >= 0 {
		fn = fn[:j+1]
	}
	if strings.Contains(fn, "u.cfg.wideRows, u.hopRows") {
		t.Error("the fork is verified against the post-fork hop rows; a fork at the seed LSN cannot hold them")
	}
	if !strings.Contains(fn, "u.cfg.wideRows, 0") {
		t.Error("the fork no longer asserts that rows written after its fork point are absent")
	}
}

// A reinstall stands the control plane up in a namespace the reap plan recorded
// at provision time cannot know about. A run killed after one leaks it.
func TestReinstallRecordsItsNamespaceForReaping(t *testing.T) {
	body := readSource(t, "amp_upgrade_chain.go")
	i := strings.Index(body, "func (u *upgradeChainRun) install(")
	if i < 0 {
		t.Fatal("install is gone")
	}
	fn := body[i:]
	if j := strings.Index(fn[1:], "\nfunc "); j >= 0 {
		fn = fn[:j+1]
	}
	reap, deploy := strings.Index(fn, "addAmpNamespaceToReapPlan("), strings.Index(fn, "app.Deploy(")
	if reap < 0 {
		t.Fatal("a reinstall's namespace is no longer added to the crash-recovery reap plan, so a killed run leaks its control plane")
	}
	if deploy >= 0 && reap > deploy {
		t.Error("the namespace is recorded after the deploy; a deploy that fails halfway created it already and it would leak unrecorded")
	}
}
