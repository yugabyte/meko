package stress

import (
	"testing"

	"context"
	"errors"
	"github.com/yugabyte/amp/test/functional/framework/apps/yba"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"net/url"
	"time"
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

func TestGrowYBAVolumesBlamesOnlyAnUnansweredYBA(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		env  bool
	}{
		{"no answer", &url.Error{Op: "Get", URL: "http://127.0.0.1:39065/api/v1/universes/u1", Err: context.DeadlineExceeded}, true},
		{"api error", &yba.APIError{StatusCode: 400, Message: "bad volume size"}, false},
		{"task failed", errors.New("task 7 (grow volumes) ended Failure"), false},
	} {
		_, err := growYBAVolumes(context.Background(), &fakeYBAMaintainer{actionErr: tc.err}, "u1", 50, time.Minute)
		if err == nil {
			t.Fatalf("%s: want an error", tc.name)
		}
		if got := core.IsEnvFailure(err); got != tc.env {
			t.Errorf("%s: env failure = %v, want %v: %v", tc.name, got, tc.env, err)
		}
	}
}
