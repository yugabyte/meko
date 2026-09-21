package stress

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/netshape"
	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
)

// Entering the pod's network namespace is apps/netshape's job and is tested
// there; what belongs here is the iptables vocabulary layered on top.
func testHelper(sp *scriptedPod) *netshape.Helper {
	return netshape.Adopt(sp.kube(), "ampqa-netns-t", "u-1", "node-a", "yb-ep-1-abc")
}

func TestIptablesInRunsThePickedFlavour(t *testing.T) {
	sp := &scriptedPod{results: []execpkg.Result{{Stdout: "100078\n"}, {}}}
	if err := iptablesIn(context.Background(), testHelper(sp), "-I OUTPUT -d 10.0.0.1 -j DROP"); err != nil {
		t.Fatalf("iptablesIn: %v", err)
	}
	cmd := sp.cmds[1]
	for _, want := range []string{"nsenter -t 100078 -n", "iptables-legacy", "-I OUTPUT -d 10.0.0.1 -j DROP"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command %q lacks %q", cmd, want)
		}
	}
}

func TestIptablesInSurfacesANonZeroExit(t *testing.T) {
	sp := &scriptedPod{results: []execpkg.Result{{Stdout: "100078\n"}, {ExitCode: 2, Stderr: "Bad rule"}}}
	err := iptablesIn(context.Background(), testHelper(sp), "-I OUTPUT -j DROP")
	if err == nil || !strings.Contains(err.Error(), "Bad rule") {
		t.Errorf("iptablesIn on a rejected rule = %v, want the stderr surfaced", err)
	}
}

// A pod with no process left has no namespace, so its rules are already gone.
func TestClearTaggedRulesTreatsAVanishedPodAsCleared(t *testing.T) {
	sp := &scriptedPod{results: []execpkg.Result{{Stdout: "ampqa-no-pid\n"}}}
	if err := clearTaggedRules(context.Background(), testHelper(sp)); err != nil {
		t.Fatalf("clearTaggedRules on a vanished pod = %v, want nil", err)
	}
}

// The blocker this pins: kubectl's own failure must never read as "the rules
// went away", or a partition is reported healed while it is still applied.
func TestClearTaggedRulesFailsWhenTheScanNeverRan(t *testing.T) {
	sp := &scriptedPod{results: []execpkg.Result{{ExitCode: 1, Stderr: `pods "ampqa-netns-t" not found`}}}
	err := clearTaggedRules(context.Background(), testHelper(sp))
	if err == nil {
		t.Fatal("clearTaggedRules with a dead helper pod = nil, want an error")
	}
	if errors.Is(err, netshape.ErrNoPodProcess) {
		t.Errorf("clearTaggedRules reported %v as a vanished target", err)
	}
}

func TestClearTaggedRulesDeletesOnlyTheTaggedOnes(t *testing.T) {
	sp := &scriptedPod{results: []execpkg.Result{{Stdout: "100078\n"}, {}}}
	if err := clearTaggedRules(context.Background(), testHelper(sp)); err != nil {
		t.Fatalf("clearTaggedRules: %v", err)
	}
	if !strings.Contains(sp.cmds[1], "--comment "+iptablesComment) {
		t.Errorf("clear command %q must select only this engine's rules", sp.cmds[1])
	}
}

func TestDefaultDevReadsTheRoute(t *testing.T) {
	sp := &scriptedPod{results: []execpkg.Result{{Stdout: "100078\n"}, {Stdout: "default via 10.21.128.1 dev eth0 \n"}}}
	dev, err := defaultDev(context.Background(), testHelper(sp))
	if err != nil || dev != "eth0" {
		t.Fatalf("defaultDev = %q, %v; want eth0", dev, err)
	}
	sp = &scriptedPod{results: []execpkg.Result{{Stdout: "100078\n"}, {Stdout: "\n"}}}
	if _, err := defaultDev(context.Background(), testHelper(sp)); err == nil {
		t.Error("defaultDev with no default route = nil, want an error")
	}
}
