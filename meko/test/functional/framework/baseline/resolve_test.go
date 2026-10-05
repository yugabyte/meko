package baseline

import (
	"testing"
	"time"
)

// storeAt returns a store holding one baseline for perf/tpcc under each
// (version, load signature) pair given.
func storeAt(t *testing.T, entries map[string]Meta) *File {
	t.Helper()
	f := Empty()
	for v, m := range entries {
		f.Update(v, "perf/tpcc", dps(), time.Now())
		f.StampMeta(v, "perf/tpcc", m)
	}
	return f
}

// TestResolvePrefersTheRunsOwnVersion: with a baseline on this version's own
// line, nothing else is considered and the pick is not a fallback.
func TestResolvePrefersTheRunsOwnVersion(t *testing.T) {
	f := storeAt(t, map[string]Meta{
		"2025.2.1.0": {LoadSignature: "sigA"},
		"2025.2.2.0": {LoadSignature: "sigA"},
	})
	sel, ok := Resolve(f, "2025.2.2.0", "perf/tpcc", Meta{LoadSignature: "sigA"})
	if !ok || sel.Version != "2025.2.2.0" || sel.Fallback || sel.Match != LoadExact {
		t.Fatalf("Resolve = %+v ok=%v, want the run's own version, exact, no fallback", sel, ok)
	}
}

// TestResolveFallsBackToNearestPrecedingVersion is the version-bump gate: a run
// on a version with no baseline of its own is judged against the NEAREST
// PRECEDING version, not any other and not nothing.
func TestResolveFallsBackToNearestPrecedingVersion(t *testing.T) {
	f := storeAt(t, map[string]Meta{
		"2025.1.0.0": {LoadSignature: "sigA"},
		"2025.2.1.0": {LoadSignature: "sigA"},
		"2026.1.0.0": {LoadSignature: "sigA"},
	})
	sel, ok := Resolve(f, "2025.2.5.0", "perf/tpcc", Meta{LoadSignature: "sigA"})
	if !ok {
		t.Fatalf("Resolve found nothing, want the previous version's baseline")
	}
	if sel.Version != "2025.2.1.0" || !sel.Fallback {
		t.Errorf("Resolve = %+v, want 2025.2.1.0 as a fallback", sel)
	}
}

// TestResolveFallsForwardWhenNothingPrecedes: with only later versions stored,
// the nearest FOLLOWING one is picked rather than giving up.
func TestResolveFallsForwardWhenNothingPrecedes(t *testing.T) {
	f := storeAt(t, map[string]Meta{
		"2026.1.0.0": {LoadSignature: "sigA"},
		"2025.2.9.0": {LoadSignature: "sigA"},
	})
	sel, ok := Resolve(f, "2025.2.1.0", "perf/tpcc", Meta{LoadSignature: "sigA"})
	if !ok || sel.Version != "2025.2.9.0" {
		t.Fatalf("Resolve = %+v ok=%v, want the nearest following version 2025.2.9.0", sel, ok)
	}
}

// TestResolveGatesOnNearestMatchingSignature: a load-signature mismatch on this
// version's own baseline does not disable the gate, a stored baseline whose
// load matches is used instead, and the mismatch is visible as a fallback.
func TestResolveGatesOnNearestMatchingSignature(t *testing.T) {
	f := storeAt(t, map[string]Meta{
		"2025.2.2.0": {LoadSignature: "sigOther", LoadShape: "shapeOther"},
		"2025.2.1.0": {LoadSignature: "sigA", LoadShape: "shapeA"},
	})
	sel, ok := Resolve(f, "2025.2.2.0", "perf/tpcc", Meta{LoadSignature: "sigA", LoadShape: "shapeA"})
	if !ok || sel.Version != "2025.2.1.0" || !sel.Fallback || sel.Match != LoadExact {
		t.Fatalf("Resolve = %+v ok=%v, want the exact-load baseline at 2025.2.1.0", sel, ok)
	}
}

// TestResolveAcceptsAShapeMatch: when no stored baseline has this run's exact
// load signature, one measured on the same load SHAPE (different harness
// cadence) is used and reported as a shape match.
func TestResolveAcceptsAShapeMatch(t *testing.T) {
	f := storeAt(t, map[string]Meta{
		"2025.2.2.0": {LoadSignature: "sigLongRun", LoadShape: "shapeA"},
	})
	sel, ok := Resolve(f, "2025.2.2.0", "perf/tpcc", Meta{LoadSignature: "sigShortRun", LoadShape: "shapeA"})
	if !ok || sel.Match != LoadShape || sel.Version != "2025.2.2.0" {
		t.Fatalf("Resolve = %+v ok=%v, want a shape match on this version", sel, ok)
	}
}

// TestResolvePrefersExactLoadOverVersionProximity: an identical load on another
// version beats a same-shape one on this version, the store's comparability
// rule is the load, not the version.
func TestResolvePrefersExactLoadOverVersionProximity(t *testing.T) {
	f := storeAt(t, map[string]Meta{
		"2025.2.2.0": {LoadSignature: "sigOther", LoadShape: "shapeA"},
		"2025.2.1.0": {LoadSignature: "sigA", LoadShape: "shapeA"},
	})
	sel, _ := Resolve(f, "2025.2.2.0", "perf/tpcc", Meta{LoadSignature: "sigA", LoadShape: "shapeA"})
	if sel.Version != "2025.2.1.0" || sel.Match != LoadExact {
		t.Errorf("Resolve = %+v, want the exact-load baseline at 2025.2.1.0", sel)
	}
}

// TestResolveRefusesAnIncomparableLoad: neither signature nor shape matching
// means there is nothing honest to gate against.
func TestResolveRefusesAnIncomparableLoad(t *testing.T) {
	f := storeAt(t, map[string]Meta{"2025.2.1.0": {LoadSignature: "sigA", LoadShape: "shapeA"}})
	if sel, ok := Resolve(f, "2025.2.2.0", "perf/tpcc", Meta{LoadSignature: "sigB", LoadShape: "shapeB"}); ok {
		t.Errorf("Resolve = %+v, want no comparable baseline", sel)
	}
	// A pre-shape entry (no stored load_shape) can only ever match exactly.
	f = storeAt(t, map[string]Meta{"2025.2.1.0": {LoadSignature: "sigA"}})
	if sel, ok := Resolve(f, "2025.2.2.0", "perf/tpcc", Meta{LoadSignature: "sigB", LoadShape: ""}); ok {
		t.Errorf("Resolve = %+v, want no match against an unshaped entry", sel)
	}
}

// TestResolveSkipsUnorderableVersions: a bucket keyed by something that is not a
// version (a custom build tag) is never guessed at, but the run's own bucket and
// the version-agnostic one still resolve.
func TestResolveSkipsUnorderableVersions(t *testing.T) {
	f := storeAt(t, map[string]Meta{"custom-abc123": {LoadSignature: "sigA"}})
	if sel, ok := Resolve(f, "2025.2.2.0", "perf/tpcc", Meta{LoadSignature: "sigA"}); ok {
		t.Errorf("Resolve = %+v, want an unorderable bucket to be skipped", sel)
	}
	if sel, ok := Resolve(f, "custom-abc123", "perf/tpcc", Meta{LoadSignature: "sigA"}); !ok || sel.Fallback {
		t.Errorf("Resolve = %+v ok=%v, want the run's own custom bucket", sel, ok)
	}
}

// TestResolveIsDeterministic: the pick never depends on map iteration order.
func TestResolveIsDeterministic(t *testing.T) {
	want := Meta{LoadSignature: "sigA", LoadShape: "shapeA"}
	for i := 0; i < 50; i++ {
		f := storeAt(t, map[string]Meta{
			"2025.1.0.0": {LoadSignature: "sigA"},
			"2025.2.1.0": {LoadSignature: "sigA"},
			"2025.2.4.0": {LoadSignature: "sigOther", LoadShape: "shapeA"},
			"":           {LoadSignature: "sigOther", LoadShape: "shapeA"},
		})
		sel, ok := Resolve(f, "2025.2.5.0", "perf/tpcc", want)
		if !ok || sel.Version != "2025.2.1.0" {
			t.Fatalf("iteration %d: Resolve = %+v ok=%v, want 2025.2.1.0 every time", i, sel, ok)
		}
	}
}
