package stress

import (
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func TestDiskFullRestartSwitchReachesTheGate(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load shipped defaults: %v", err)
	}
	if !cfg.Has(diskFullRestartIsFailKey) {
		t.Fatalf("%s is read by the test but not declared in configs/default.yaml", diskFullRestartIsFailKey)
	}
	if g := (&settleGate{}).asProductFailure(cfg, diskFullRestartIsFailKey); !g.p.RestartIsFail {
		t.Error("shipped defaults: a pod going down on a full backing reads as env damage, want a product failure")
	}

	off, err := core.LoadConfig(nil, nil, []string{diskFullRestartIsFailKey + "=false"})
	if err != nil {
		t.Fatalf("load with override: %v", err)
	}
	if g := (&settleGate{}).asProductFailure(off, diskFullRestartIsFailKey); g.p.RestartIsFail {
		t.Errorf("%s=false did not reach the gate", diskFullRestartIsFailKey)
	}
}
