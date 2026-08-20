package stress

import (
	"slices"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
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

// A prefix-only match would turn amp_nemesis into every concurrent test's
// nemesis, since all their endpoints share the namespace.
func TestFleetEndpointPodsScopesToOwnFleet(t *testing.T) {
	pods := []string{
		"yb-ep-aaaa1111-2222-3333-4444-555566667777-96b45f5cb-dk2j5", // ours
		"yb-ep-bbbb1111-2222-3333-4444-555566667777-85847c58d7-gkdl", // another test's
		"yb-ep-aaaa1111-2222-3333-4444-555566667777-96b45f5cb-x9z8q", // ours, second pod
		"amp-controller-5b9dcc4c44-f5hqm",
		"ampqa-http-relay",
	}
	tenants := []tenant{
		{Endpoint: controller.Endpoint{ID: "aaaa1111-2222-3333-4444-555566667777"}},
		{Endpoint: controller.Endpoint{}}, // endpoint never created: must match nothing
	}
	got := fleetEndpointPods(pods, tenants)
	want := []string{
		"yb-ep-aaaa1111-2222-3333-4444-555566667777-96b45f5cb-dk2j5",
		"yb-ep-aaaa1111-2222-3333-4444-555566667777-96b45f5cb-x9z8q",
	}
	if !slices.Equal(got, want) {
		t.Errorf("fleetEndpointPods = %v, want %v", got, want)
	}
	if got := fleetEndpointPods(pods, nil); len(got) != 0 {
		t.Errorf("no tenants must select no pods, got %v", got)
	}
}

// A foreign endpoint mid-churn must not fail us; shared pods still must.
func TestForeignEndpointPod(t *testing.T) {
	tenants := []tenant{{Endpoint: controller.Endpoint{ID: "aaaa1111-2222-3333-4444-555566667777"}}}
	for pod, foreign := range map[string]bool{
		"yb-ep-aaaa1111-2222-3333-4444-555566667777-96b45f5cb-dk2j5": false, // ours: asserted
		"yb-ep-bbbb1111-2222-3333-4444-555566667777-85847c58d7-gkdl": true,  // another fleet's
		"amp-controller-5b9dcc4c44-f5hqm":                            false, // shared: asserted
		"yb-amp-controller-logs-collector-4d6vt":                     false, // shared: asserted
	} {
		if got := foreignEndpointPod(pod, tenants); got != foreign {
			t.Errorf("foreignEndpointPod(%s) = %v, want %v", pod, got, foreign)
		}
	}
}
