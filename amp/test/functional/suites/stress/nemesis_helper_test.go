package stress

import (
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// The selector list is what makes backing chaos work against EVERY backing
// this framework deploys. Each shape is load-bearing and was read off live
// pods: ybk8s labels tservers app=yb-tserver, a YBA-provisioned universe
// labels them app.kubernetes.io/name=yb-tserver, and ampstack's built-in
// yugabyted is app=yugabyted (one pod, both roles).
func TestBackingPodSelectorsCoverEveryBacking(t *testing.T) {
	want := map[string]bool{
		"app=yb-tserver":                    false,
		"app.kubernetes.io/name=yb-tserver": false,
		"app=yugabyted":                     false,
	}
	for _, sel := range backingPodSelectors {
		if _, ok := want[sel]; !ok {
			t.Errorf("unexpected selector %q — masters and non-serving pods must not be targeted", sel)
		}
		want[sel] = true
	}
	for sel, seen := range want {
		if !seen {
			t.Errorf("selector %q missing: that backing's pods would never be targeted", sel)
		}
	}
}

func TestBackingNemesisParamsDeclaredInDefaults(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load shipped defaults: %v", err)
	}
	for _, key := range []string{
		"stress.backing_nemesis.tenants",
		"stress.backing_nemesis.duration",
		"stress.backing_nemesis.settle_timeout",
		"stress.backing_nemesis.payload_bytes",
		"stress.backing_nemesis.resume_duration",
		"stress.amp_nemesis.tenants",
		"stress.amp_nemesis.duration",
	} {
		if !cfg.Has(key) {
			t.Errorf("%s is read by the test but not declared in configs/default.yaml", key)
		}
	}
	p := backingNemesisParamsFromConfig(cfg)
	if p.Tenants < 2 {
		t.Errorf("default tenants = %d: with one tenant, isolation and fairness mean nothing", p.Tenants)
	}
	if p.ResumeDuration <= 0 {
		t.Error("resume_duration must be positive — the post-chaos write probe is the point of the test (meko#727)")
	}
}
