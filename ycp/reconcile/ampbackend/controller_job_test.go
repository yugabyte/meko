package ampbackend

import (
	"context"
	"errors"
	"strings"
	"testing"

	"meko/ycp/reconcile"
)

// fakeUpgrader records the runs it is asked for and applies each to the fake
// helm, so the next tick reads the upgrade back the way it would once a Job ran.
type fakeUpgrader struct {
	helm    *fakeHelm
	starts  []upgradeStart
	latest  UpgradeRun
	removed []string
}

type upgradeStart struct {
	name   string
	args   []string
	values []byte
}

func (u *fakeUpgrader) Start(_ context.Context, name string, args []string, values []byte) error {
	u.starts = append(u.starts, upgradeStart{name, args, values})
	u.helm.applyUpgrade(args, values)
	u.latest = UpgradeRun{Name: name, State: UpgradeRunning}
	return nil
}

func (u *fakeUpgrader) Latest(context.Context) (UpgradeRun, error) { return u.latest, nil }

func (u *fakeUpgrader) Remove(_ context.Context, name string) error {
	u.removed = append(u.removed, name)
	if u.latest.Name == name {
		u.latest = UpgradeRun{}
	}
	return nil
}

func upgraderServer(version string) (*ctlServer, *fakeUpgrader) {
	helm := deployedAt(version)
	up := &fakeUpgrader{helm: helm}
	return &ctlServer{helm: helm, ctlVersion: version, upgrades: up}, up
}

// With an Upgrader the agent never runs `helm upgrade` itself: its own pod is
// replaced by the upgrade, and a helm client killed while it waits on the
// release's hooks leaves the release pending-upgrade for good.
func TestControllerUpgradeRunsThroughTheUpgrader(t *testing.T) {
	f, up := upgraderServer("0.260801.0-old")
	b := f.start(t)

	mustPending(t, b.Update(t.Context(), ctlRes(1, ctlSpec(ctlVersionTarget))))

	if got := f.helm.upgrades(); len(got) != 0 {
		t.Errorf("helm ran %v in-process, want the upgrade handed to the Upgrader", got)
	}
	if len(up.starts) != 1 {
		t.Fatalf("runs started = %d, want 1", len(up.starts))
	}
	st := up.starts[0]
	if got, want := strings.Join(st.args, " "), "upgrade "+ctlRelease+" "+ctlChart+" --namespace "+ctlNamespace+" --version "+ctlChartVersion; got != want {
		t.Errorf("args = %q, want %q (the values file is the Upgrader's to attach)", got, want)
	}
	if !strings.Contains(string(st.values), `"tag":"`+ctlVersionTarget+`"`) {
		t.Errorf("values = %s, want the image tags in the document", st.values)
	}
	if !strings.HasPrefix(st.name, ctlRelease+"-upgrade-") || len(st.name) > 63 {
		t.Errorf("run name = %q, want %s-upgrade-<hash> within 63 characters", st.name, ctlRelease)
	}
}

// A run still going is waited on — the replacement pod's first tick lands here —
// and nothing new is started over it.
func TestControllerWaitsOnARunningUpgrade(t *testing.T) {
	f, up := upgraderServer("0.260801.0-old")
	up.latest = UpgradeRun{Name: "yb-amp-controller-upgrade-1", State: UpgradeRunning}
	b := f.start(t)

	mustPending(t, b.Update(t.Context(), ctlRes(1, ctlSpec(ctlVersionTarget))))
	if len(up.starts) != 0 {
		t.Errorf("started %d runs over a running one, want none", len(up.starts))
	}
}

// A failed run is reported with helm's own error, once: removing it lets the next
// tick decide from the release itself, and retry.
func TestControllerReportsAFailedUpgradeOnce(t *testing.T) {
	f, up := upgraderServer("0.260801.0-old")
	up.latest = UpgradeRun{Name: "yb-amp-controller-upgrade-1", State: UpgradeFailed,
		Message: "Error: UPGRADE FAILED: post-upgrade hooks failed"}
	b := f.start(t)

	err := b.Update(t.Context(), ctlRes(1, ctlSpec(ctlVersionTarget)))
	var pending *reconcile.PendingError
	if err == nil || errors.As(err, &pending) || !strings.Contains(err.Error(), "post-upgrade hooks failed") {
		t.Fatalf("Update after a failed run = %v, want helm's error, not Pending", err)
	}
	if len(up.removed) != 1 || up.removed[0] != "yb-amp-controller-upgrade-1" {
		t.Errorf("removed = %v, want the failed run", up.removed)
	}
	mustPending(t, b.Update(t.Context(), ctlRes(1, ctlSpec(ctlVersionTarget))))
	if len(up.starts) != 1 {
		t.Errorf("runs started after the failure was reported = %d, want 1 retry", len(up.starts))
	}
}

func TestUpgradeRunNameIsStable(t *testing.T) {
	args := []string{"upgrade", "r", "chart"}
	a := upgradeRunName("r", args, []byte(`{"a":1}`))
	if b := upgradeRunName("r", args, []byte(`{"a":1}`)); a != b {
		t.Errorf("the same run is named %q and %q", a, b)
	}
	if b := upgradeRunName("r", args, []byte(`{"a":2}`)); a == b {
		t.Errorf("different values share the name %q", a)
	}
	if long := upgradeRunName(strings.Repeat("x", 80), args, nil); len(long) > 63 {
		t.Errorf("name %q is %d characters, want at most 63", long, len(long))
	}
}
