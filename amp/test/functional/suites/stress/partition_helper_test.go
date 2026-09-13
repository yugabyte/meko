package stress

import (
	"context"
	"errors"
	"strings"
	"testing"

	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
)

func testNetnsHelper(sp *scriptedPod) *netnsHelper {
	return &netnsHelper{kube: sp.kube(), name: "ampqa-netns-t", uid: "u-1", node: "node-a", pod: "yb-ep-1-abc"}
}

// A liveness restart retires the pid while the namespace lives on.
func TestNetnsHelperResolvesPIDPerCall(t *testing.T) {
	sp := &scriptedPod{results: []execpkg.Result{
		{Stdout: "100078\n"}, {},
		{Stdout: "200341\n"}, {},
	}}
	h := testNetnsHelper(sp)
	ctx := context.Background()
	if err := h.iptables(ctx, "-I OUTPUT -d 10.0.0.1 -j DROP"); err != nil {
		t.Fatalf("iptables: %v", err)
	}
	if err := h.clearRules(ctx); err != nil {
		t.Fatalf("clearRules: %v", err)
	}
	if !strings.Contains(sp.cmds[1], "nsenter -t 100078 -n") {
		t.Errorf("iptables entered %q, want pid 100078", sp.cmds[1])
	}
	if !strings.Contains(sp.cmds[3], "nsenter -t 200341 -n") {
		t.Errorf("clearRules entered %q, want the re-resolved pid 200341", sp.cmds[3])
	}
}

func TestClearRulesTreatsAVanishedPodAsCleared(t *testing.T) {
	sp := &scriptedPod{results: []execpkg.Result{{Stdout: noPodPIDSentinel + "\n"}}}
	if err := testNetnsHelper(sp).clearRules(context.Background()); err != nil {
		t.Fatalf("clearRules on a vanished pod = %v, want nil", err)
	}
	if len(sp.cmds) != 1 {
		t.Errorf("ran %d command(s), want only the /proc scan", len(sp.cmds))
	}
}

// The blocker: kubectl's own exit 1 must not read as "the rules went away".
func TestClearRulesFailsWhenTheScanNeverRan(t *testing.T) {
	sp := &scriptedPod{results: []execpkg.Result{{ExitCode: 1, Stderr: `pods "ampqa-netns-t" not found`}}}
	err := testNetnsHelper(sp).clearRules(context.Background())
	if err == nil {
		t.Fatal("clearRules with a dead helper pod = nil, want an error")
	}
	if errors.Is(err, errNoPodProcess) {
		t.Errorf("clearRules reported %v as a vanished target", err)
	}
}

func TestIptablesFailsWhenThePodHasNoProcess(t *testing.T) {
	sp := &scriptedPod{results: []execpkg.Result{{Stdout: noPodPIDSentinel + "\n"}}}
	if err := testNetnsHelper(sp).iptables(context.Background(), "-S"); err == nil {
		t.Error("iptables on a vanished pod succeeded, want an error")
	}
}
