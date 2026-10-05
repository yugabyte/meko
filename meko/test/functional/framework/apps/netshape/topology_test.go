package netshape

import (
	"context"
	"strings"
	"testing"
	"time"

	execpkg "github.com/yugabyte/meko/meko/test/functional/framework/exec"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

func TestParseTopologyReadsSitesAndEndpoint(t *testing.T) {
	tp, err := ParseTopology("multi_region: 0-1=12 2-0=47 1-2=60.5")
	if err != nil {
		t.Fatalf("ParseTopology: %v", err)
	}
	if tp.Name != "multi_region" || tp.EndpointSite != 0 {
		t.Errorf("name/site = %q/%d", tp.Name, tp.EndpointSite)
	}
	if got := tp.RoundTrip(2, 0); got != 47*time.Millisecond {
		t.Errorf("RoundTrip(2,0) = %v, want 47ms", got)
	}
	if got := tp.RoundTrip(1, 2); got != 60500*time.Microsecond {
		t.Errorf("RoundTrip(1,2) = %v, want 60.5ms", got)
	}
	if got := tp.RoundTrip(1, 1); got != 0 {
		t.Errorf("RoundTrip(1,1) = %v, want 0", got)
	}
	if tp.MaxSite() != 2 {
		t.Errorf("MaxSite = %d, want 2", tp.MaxSite())
	}

	tp, err = ParseTopology("remote_site@3: 0-3=12 1-3=12 2-3=12")
	if err != nil {
		t.Fatalf("ParseTopology: %v", err)
	}
	if tp.EndpointSite != 3 || tp.MaxSite() != 3 || tp.RoundTrip(0, 1) != 0 {
		t.Errorf("endpoint site %d, max %d, RoundTrip(0,1) %v", tp.EndpointSite, tp.MaxSite(), tp.RoundTrip(0, 1))
	}
}

func TestParseTopologyRejectsMalformedEntries(t *testing.T) {
	for _, bad := range []string{"nocolon 0-1=1", "x@-1: 0-1=1", "x: 0-0=1", "x: 0-1=0", "x: 0-1", "x:", ": 0-1=1"} {
		if _, err := ParseTopology(bad); err == nil {
			t.Errorf("ParseTopology(%q) = nil error", bad)
		}
	}
	if _, err := ParseTopologies([]string{"a: 0-1=1", "a: 0-2=1"}); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate names = %v", err)
	}
}

// The bug this pins: a 1 ms floor on the slack put the lower bound at zero for
// a 1 ms ask, so an unshaped path passed and the two 1 ms topologies verified
// nothing. Every bound is now relative to the pair's native cost.
func TestWithinJudgesAgainstTheNativeCost(t *testing.T) {
	const native = 430 * time.Microsecond
	for _, tc := range []struct {
		name string
		got  time.Duration
		want time.Duration
		ok   bool
	}{
		{"1ms shaped", 1500 * time.Microsecond, time.Millisecond, true},
		{"1ms unshaped", native, time.Millisecond, false},
		// A 1 ms ask carrying only one of its two halves measures ~930µs, half
		// a jitter floor from the 930µs lower bound, so it is NOT separable
		// here and no case pins it. Fleet.Apply writes both ends from one
		// loop, so a half-shaped pair is not a failure mode this can produce.
		{"2ms shaped", 2130 * time.Microsecond, 2 * time.Millisecond, true},
		{"2ms unshaped", native, 2 * time.Millisecond, false},
		{"12ms shaped", 12400 * time.Microsecond, 12 * time.Millisecond, true},
		{"12ms unshaped", native, 12 * time.Millisecond, false},
		{"47ms shaped", 47540 * time.Microsecond, 47 * time.Millisecond, true},
		{"47ms one direction only", 24 * time.Millisecond, 47 * time.Millisecond, false},
		{"47ms far above", 90 * time.Millisecond, 47 * time.Millisecond, false},
		{"cleared", native, 0, true},
		{"not cleared", 12 * time.Millisecond, 0, false},
	} {
		err := Within(tc.got, native, tc.want, 0.3)
		if tc.ok && err != nil {
			t.Errorf("%s: Within(%v, %v, %v) = %v, want nil", tc.name, tc.got, native, tc.want, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: Within(%v, %v, %v) = nil, want an error", tc.name, tc.got, native, tc.want)
		}
	}
}

func TestCheckSitesRejectsAnUnoccupiedSite(t *testing.T) {
	occupied := map[int]bool{0: true, 1: true, 2: true}
	tp, err := ParseTopology("ok: 0-1=12 0-2=47")
	if err != nil {
		t.Fatalf("ParseTopology: %v", err)
	}
	if err := tp.CheckSites(occupied); err != nil {
		t.Errorf("CheckSites on an occupied matrix = %v", err)
	}
	// A site with no pod: the delay is never applied and never missed, so the
	// run would benchmark an unshaped stack under this topology's name.
	tp, err = ParseTopology("gap: 0-3=47 0-1=12")
	if err != nil {
		t.Fatalf("ParseTopology: %v", err)
	}
	err = tp.CheckSites(occupied)
	if err == nil || !strings.Contains(err.Error(), "[3]") {
		t.Errorf("CheckSites with site 3 empty = %v, want it named", err)
	}
	// The endpoint's own site counts as occupied.
	tp, _ = ParseTopology("ep@3: 0-3=12 1-3=12 2-3=12")
	if err := tp.CheckSites(map[int]bool{0: true, 1: true, 2: true, 3: true}); err != nil {
		t.Errorf("CheckSites with the endpoint in site 3 = %v", err)
	}
}

func TestSignatureTracksTheMatrix(t *testing.T) {
	a, _ := ParseTopologies([]string{"m: 0-1=12 0-2=47"})
	b, _ := ParseTopologies([]string{"m: 0-1=12 0-2=48"})
	c, _ := ParseTopologies([]string{"m: 0-2=47 0-1=12"})
	if Signature(a) == Signature(b) {
		t.Error("a changed round trip kept the signature")
	}
	if Signature(a) != Signature(c) {
		t.Error("pair order changed the signature")
	}
}

// fleetOf builds a two-member fleet over one scripted executor, so a test can
// assert the ORDER of the commands the fleet issues.
func fleetOf(s *scripted) *Fleet {
	kube := k8s.New(k8s.Config{Namespace: "mekoqa-t"}).WithRunner(s.runner())
	return &Fleet{
		Members: []*Member{
			{Label: "a", Pod: "pod-a", IP: "10.0.0.1", Site: 0, Helper: Adopt(kube, "h-a", "u-a", "node-a", "pod-a")},
			{Label: "b", Pod: "pod-b", IP: "10.0.0.2", Site: 1, Helper: Adopt(kube, "h-b", "u-b", "node-b", "pod-b")},
		},
		Device: "eth0", Pings: 3, Tolerance: 0.3,
	}
}

// The bug this pins: the stress caller shaped without measuring the baseline,
// so Verify had nothing to judge against and refused. Apply measures it.
func TestApplyMeasuresTheBaselineTheCallerSkipped(t *testing.T) {
	s := &scripted{results: []execpkg.Result{
		{Stdout: "11\n"}, {Stdout: "rtt min/avg/max/mdev = 0.3/0.4/0.5/0.1 ms\n"}, // baseline ping a->b
		{Stdout: "11\n"}, {Stdout: ""}, // tc on a
		{Stdout: "22\n"}, {Stdout: ""}, // tc on b
	}}
	f := fleetOf(s)
	tp, err := ParseTopology("t: 0-1=12")
	if err != nil {
		t.Fatalf("ParseTopology: %v", err)
	}
	if err := f.Apply(context.Background(), tp); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	native, ok := f.Native("a", "b")
	if !ok || native != 400*time.Microsecond {
		t.Fatalf("Native after Apply = %v, %v; want 400µs measured", native, ok)
	}
	var firstPing, firstTC = -1, -1
	for i, c := range s.cmds {
		if firstPing < 0 && strings.Contains(c, "ping ") {
			firstPing = i
		}
		if firstTC < 0 && strings.Contains(c, "tc qdisc") {
			firstTC = i
		}
	}
	if firstPing < 0 || firstTC < 0 || firstPing > firstTC {
		t.Errorf("the baseline ping must precede any tc command (ping at %d, tc at %d)", firstPing, firstTC)
	}
}

// Re-measuring after shaping would move the bar every later check uses.
func TestBaselineIsMeasuredOnce(t *testing.T) {
	s := &scripted{results: []execpkg.Result{
		{Stdout: "11\n"}, {Stdout: "rtt min/avg/max/mdev = 0.3/0.4/0.5/0.1 ms\n"},
	}}
	f := fleetOf(s)
	first, err := f.Baseline(context.Background())
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	before := len(s.cmds)
	again, err := f.Baseline(context.Background())
	if err != nil {
		t.Fatalf("second Baseline: %v", err)
	}
	if len(s.cmds) != before {
		t.Errorf("the second Baseline issued %d more command(s), want none", len(s.cmds)-before)
	}
	if len(again) != len(first) || again[0].Got != first[0].Got {
		t.Errorf("the second Baseline returned %v, want the first's %v", again, first)
	}
}
