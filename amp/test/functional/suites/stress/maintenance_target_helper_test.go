package stress

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/yba"
	"github.com/yugabyte/amp/test/functional/framework/env/sharedk8s"
	"github.com/yugabyte/amp/test/functional/framework/env/universe"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// fakeYBAMaintainer records what a target asked YBA to do.
type fakeYBAMaintainer struct {
	restarts      int
	gflags        []map[string]string
	options       []yba.UpgradeOption
	upgraded      []string
	universe      yba.Universe
	getErr        error
	actionErr     error
	appVersion    string
	appVersionErr error
	ensured       []string
	ensureErr     error
}

func (f *fakeYBAMaintainer) RestartUniverseAndWait(_ context.Context, _ string, opt yba.UpgradeOption, _ time.Duration) error {
	f.restarts++
	f.options = append(f.options, opt)
	return f.actionErr
}

func (f *fakeYBAMaintainer) UpgradeGflagsAndWait(_ context.Context, _ string, _, tserver map[string]string, opt yba.UpgradeOption, _ time.Duration) error {
	f.gflags = append(f.gflags, tserver)
	f.options = append(f.options, opt)
	return f.actionErr
}

func (f *fakeYBAMaintainer) UpgradeSoftwareAndWait(_ context.Context, _, version string, opt yba.UpgradeOption, _ time.Duration) error {
	f.upgraded = append(f.upgraded, version)
	f.options = append(f.options, opt)
	return f.actionErr
}

func (f *fakeYBAMaintainer) GetUniverse(context.Context, string) (yba.Universe, error) {
	return f.universe, f.getErr
}

func (f *fakeYBAMaintainer) AppVersion(context.Context) (string, error) {
	return f.appVersion, f.appVersionErr
}

func (f *fakeYBAMaintainer) EnsureReleaseAvailable(_ context.Context, version, arch string, k8s bool) error {
	kind := arch
	if k8s {
		kind = "helm"
	}
	f.ensured = append(f.ensured, version+"/"+kind)
	return f.ensureErr
}

func readyUniverse(version string) yba.Universe {
	u := yba.Universe{UniverseUUID: "u1", Name: "uni"}
	u.UniverseDetails.UpdateSucceeded = true
	u.UniverseDetails.Clusters = []yba.Cluster{{UserIntent: yba.UserIntent{YBSoftwareVersion: version}}}
	return u
}

func TestYBATargetDrivesEveryOperationRolling(t *testing.T) {
	f := &fakeYBAMaintainer{universe: readyUniverse("2026.1.1.0-b40")}
	tgt := &ybaTarget{client: f, universeID: "u1", restartSleep: time.Second, logf: func(string, ...any) {}}
	ctx := context.Background()

	if err := tgt.RollingRestart(ctx); err != nil {
		t.Fatalf("RollingRestart: %v", err)
	}
	if err := tgt.SetTServerGflag(ctx, "log_min_seconds_to_retain", "1800"); err != nil {
		t.Fatalf("SetTServerGflag: %v", err)
	}
	if err := tgt.UpgradeSoftware(ctx, "2026.1.2.0-b67"); err != nil {
		t.Fatalf("UpgradeSoftware: %v", err)
	}

	if f.restarts != 1 {
		t.Errorf("restarts = %d, want 1", f.restarts)
	}
	if len(f.gflags) != 1 || f.gflags[0]["log_min_seconds_to_retain"] != "1800" {
		t.Errorf("gflags = %v", f.gflags)
	}
	if len(f.upgraded) != 1 || f.upgraded[0] != "2026.1.2.0-b67" {
		t.Errorf("upgraded = %v", f.upgraded)
	}
	// Any other option takes the universe down, and the scenario asserts writes keep flowing throughout.
	for i, opt := range f.options {
		if opt != yba.UpgradeRolling {
			t.Errorf("operation %d used %q, want Rolling", i, opt)
		}
	}
}

func TestYBATargetHealthRejectsAnUnsettledUniverse(t *testing.T) {
	f := &fakeYBAMaintainer{universe: yba.Universe{UniverseUUID: "u1"}} // UpdateSucceeded false
	tgt := &ybaTarget{client: f, universeID: "u1", logf: func(string, ...any) {}}
	if err := tgt.Health(context.Background()); err == nil {
		t.Fatal("a universe whose last update did not succeed must fail the health check")
	}
}

func TestYBATargetHealthAcceptsAReadyUniverse(t *testing.T) {
	f := &fakeYBAMaintainer{universe: readyUniverse("2026.1.1.0-b40")}
	tgt := &ybaTarget{client: f, universeID: "u1", logf: func(string, ...any) {}}
	if err := tgt.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestYBATargetVersionReadsTheUniverse(t *testing.T) {
	f := &fakeYBAMaintainer{universe: readyUniverse("2026.1.2.0-b67")}
	tgt := &ybaTarget{client: f, universeID: "u1", logf: func(string, ...any) {}}
	got, err := tgt.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != "2026.1.2.0-b67" {
		t.Errorf("Version = %q, want the universe's own", got)
	}
}

func TestYBATargetSurfacesAFailedOperation(t *testing.T) {
	f := &fakeYBAMaintainer{universe: readyUniverse("x"), actionErr: errors.New("task failed")}
	tgt := &ybaTarget{client: f, universeID: "u1", logf: func(string, ...any) {}}
	if err := tgt.UpgradeSoftware(context.Background(), "y"); err == nil {
		t.Fatal("a failed YBA upgrade must fail the operation")
	}
}

// yb-admin needs somewhere in-cluster to run; a VM universe has no such place and must say so, not silently skip.
func TestYBATargetDataMoveNeedsAnInClusterPlaceToRunYBAdmin(t *testing.T) {
	vm := &ybaTarget{}
	if vm.SupportsDataMove() {
		t.Error("a VM universe has no kube handle, so it cannot run yb-admin")
	}
	if err := vm.DataMove(context.Background(), nil, 3, func(int) int { return 0 }, time.Second, func(string, ...any) {}); err == nil {
		t.Error("DataMove on a VM universe must fail with a reason, not proceed")
	}
	k8sUni := &ybaTarget{kube: k8s.New(k8s.Config{Namespace: "yb-uni"})}
	if !k8sUni.SupportsDataMove() {
		t.Error("a kubernetes universe can run yb-admin in a tserver pod")
	}
}

// maintRun builds a run wired to a fake YBA at the given universe version.
func maintRun(t *testing.T, f *fakeYBAMaintainer, cfgUpgradeTo string) *maintenanceRun {
	t.Helper()
	tgt := &ybaTarget{client: f, universeID: "u1", arch: "x86_64", logf: func(string, ...any) {}}
	return &maintenanceRun{
		cfg: maintenanceConfig{
			UpgradeTo:   cfgUpgradeTo,
			UpgradeArch: "x86_64",
			Operations:  []string{OpSoftwareUpgrade},
		},
		target: tgt,
		logf:   func(string, ...any) {},
	}
}

// Unset, the target is the YBA's own version: the universe is created a train behind and brought up to it.
func TestUpgradeTargetDefaultsToTheYBAsOwnVersion(t *testing.T) {
	f := &fakeYBAMaintainer{universe: readyUniverse("2025.2.4.0-b12"), appVersion: "2026.1.0.0-b1"}
	r := maintRun(t, f, "")
	if err := r.resolveUpgradeTarget(context.Background()); err != nil {
		t.Fatalf("resolveUpgradeTarget: %v", err)
	}
	if r.cfg.UpgradeTo != "2026.1.0.0-b1" {
		t.Errorf("target = %q, want the YBA's own version", r.cfg.UpgradeTo)
	}
}

func TestUpgradeTargetHonoursAnExplicitVersion(t *testing.T) {
	f := &fakeYBAMaintainer{universe: readyUniverse("2025.2.4.0-b12"), appVersion: "2026.1.0.0-b1"}
	r := maintRun(t, f, "2026.1.2.0-b67")
	if err := r.resolveUpgradeTarget(context.Background()); err != nil {
		t.Fatalf("resolveUpgradeTarget: %v", err)
	}
	if r.cfg.UpgradeTo != "2026.1.2.0-b67" {
		t.Errorf("target = %q, want the configured version", r.cfg.UpgradeTo)
	}
}

// A universe already on the target would otherwise "pass" having proved nothing.
func TestUpgradeTargetRejectsAUniverseAlreadyAtTheTarget(t *testing.T) {
	f := &fakeYBAMaintainer{universe: readyUniverse("2026.1.0.0-b1"), appVersion: "2026.1.0.0-b1"}
	r := maintRun(t, f, "")
	err := r.resolveUpgradeTarget(context.Background())
	if err == nil {
		t.Fatal("upgrading a universe to the version it already runs must be rejected")
	}
	if !strings.Contains(err.Error(), "2025.2") {
		t.Errorf("error = %v, want it to name the train the universe should be created on", err)
	}
}

func TestUpgradeTargetRejectsAUniverseNewerThanTheTarget(t *testing.T) {
	f := &fakeYBAMaintainer{universe: readyUniverse("2026.1.2.0-b67"), appVersion: "2026.1.0.0-b1"}
	r := maintRun(t, f, "")
	if err := r.resolveUpgradeTarget(context.Background()); err == nil {
		t.Fatal("a universe newer than the YBA cannot be upgraded to it")
	}
}

func TestUpgradeTargetAcceptsAnOlderUniverse(t *testing.T) {
	f := &fakeYBAMaintainer{universe: readyUniverse("2025.2.4.0-b12"), appVersion: "2026.1.0.0-b1"}
	r := maintRun(t, f, "")
	if err := r.resolveUpgradeTarget(context.Background()); err != nil {
		t.Fatalf("an older universe is exactly the case that should run: %v", err)
	}
}

// A YBA that has never seen the target release fails the upgrade with its own vaguer error, so the target imports it first.
func TestYBATargetEnsuresTheReleaseBeforeUpgrading(t *testing.T) {
	f := &fakeYBAMaintainer{universe: readyUniverse("2025.2.4.0-b12")}
	tgt := &ybaTarget{client: f, universeID: "u1", arch: "x86_64", logf: func(string, ...any) {}}
	if err := tgt.UpgradeSoftware(context.Background(), "2026.1.0.0-b1"); err != nil {
		t.Fatalf("UpgradeSoftware: %v", err)
	}
	if len(f.ensured) != 1 || f.ensured[0] != "2026.1.0.0-b1/x86_64" {
		t.Errorf("ensured = %v, want the target release ensured for the configured arch", f.ensured)
	}
	if len(f.upgraded) != 1 {
		t.Errorf("upgraded = %v, want the upgrade to follow the release import", f.upgraded)
	}
}

func TestYBATargetDoesNotUpgradeWhenTheReleaseCannotBeMade(t *testing.T) {
	f := &fakeYBAMaintainer{universe: readyUniverse("2025.2.4.0-b12"), ensureErr: errors.New("import failed")}
	tgt := &ybaTarget{client: f, universeID: "u1", arch: "x86_64", logf: func(string, ...any) {}}
	if err := tgt.UpgradeSoftware(context.Background(), "2026.1.0.0-b1"); err == nil {
		t.Fatal("an unavailable release must fail before the upgrade is attempted")
	}
	if len(f.upgraded) != 0 {
		t.Error("the upgrade was attempted despite the release being unavailable")
	}
}

// The universe reaches this test either as the env itself (env/universe) or as the backing under
// an amp control plane (env/sharedk8s). A rename on either side must fail here, not mid-run.
func TestBothEnvLayersSatisfyTheYBACapability(t *testing.T) {
	var _ ybaBackedEnv = (*universe.Env)(nil)
	var _ kubeUniverseEnv = (*universe.Env)(nil)
	var _ ybaBackedEnv = (*sharedk8s.Env)(nil)
	var _ kubeUniverseEnv = (*sharedk8s.Env)(nil)
	// Tenants are written through the control plane, so the env must expose one too.
	var _ ampTenantEnv = (*sharedk8s.Env)(nil)
}

// Hardcoding one naming style finds no pods on the other's cluster, which is how a YBA-backed
// run failed at seed_datasets with "array index out of bounds".
func TestTserverSelectorsCoverBothChartNamingStyles(t *testing.T) {
	want := map[string]bool{"app=yb-tserver": false, "app.kubernetes.io/name=yb-tserver": false}
	for _, sel := range tserverSelectors {
		if _, ok := want[sel]; !ok {
			t.Errorf("unexpected selector %q", sel)
			continue
		}
		want[sel] = true
	}
	for sel, seen := range want {
		if !seen {
			t.Errorf("selector %q is not tried", sel)
		}
	}
}
