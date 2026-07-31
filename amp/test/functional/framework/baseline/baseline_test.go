package baseline

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func dps() []core.Datapoint {
	return []core.Datapoint{
		{Name: "tpcc.throughput", Value: 1000, Unit: "req/s", Direction: core.HigherIsBetter},
		{Name: "tpcc.latency_p99", Value: 50, Unit: "ms", Direction: core.LowerIsBetter},
	}
}

// TestCompareNoBaselineWarns: with an empty store nothing regresses and no
// baseline was found — the caller establishes one and warns rather than failing.
func TestCompareNoBaselineWarns(t *testing.T) {
	rep := Compare(Empty(), "2025.2.1.0", "perf/tpcc", dps(), Thresholds{DropPct: 10, RisePct: 20})
	if rep.HadBaseline {
		t.Errorf("HadBaseline = true on an empty store, want false")
	}
	if rep.Regressed {
		t.Errorf("Regressed = true with no baseline, want false")
	}
	for _, dp := range rep.Datapoints {
		if dp.Baseline != nil {
			t.Errorf("%s got a baseline comparison against an empty store: %+v", dp.Name, dp.Baseline)
		}
	}
}

// TestComparePasses: values within tolerance of the baseline do not regress and
// each datapoint carries its comparison.
func TestComparePasses(t *testing.T) {
	f := Empty()
	f.Update("2025.2.1.0", "perf/tpcc", dps(), time.Now())
	// A run 5% slower throughput / 10% higher latency — both within tolerance.
	run := []core.Datapoint{
		{Name: "tpcc.throughput", Value: 950, Direction: core.HigherIsBetter},
		{Name: "tpcc.latency_p99", Value: 55, Direction: core.LowerIsBetter},
	}
	rep := Compare(f, "2025.2.1.0", "perf/tpcc", run, Thresholds{DropPct: 10, RisePct: 20})
	if !rep.HadBaseline {
		t.Fatalf("HadBaseline = false, want true")
	}
	if rep.Regressed {
		t.Errorf("Regressed = true within tolerance, want false")
	}
	if b := rep.Datapoints[0].Baseline; b == nil || b.Regressed {
		t.Errorf("throughput comparison = %+v, want present + not regressed", b)
	}
}

// TestCompareThroughputDropRegresses: a throughput drop beyond DropPct regresses.
func TestCompareThroughputDropRegresses(t *testing.T) {
	f := Empty()
	f.Update("2025.2.1.0", "perf/tpcc", dps(), time.Now())
	run := []core.Datapoint{
		{Name: "tpcc.throughput", Value: 800, Direction: core.HigherIsBetter}, // -20% > 10% tol
	}
	rep := Compare(f, "2025.2.1.0", "perf/tpcc", run, Thresholds{DropPct: 10, RisePct: 20})
	if !rep.Regressed {
		t.Fatalf("Regressed = false on a 20%% throughput drop, want true")
	}
	b := rep.Datapoints[0].Baseline
	if b == nil || !b.Regressed || b.DeltaPct >= 0 {
		t.Errorf("throughput comparison = %+v, want regressed with negative delta", b)
	}
}

// TestCompareLatencyRiseRegresses: a latency rise beyond RisePct regresses.
func TestCompareLatencyRiseRegresses(t *testing.T) {
	f := Empty()
	f.Update("2025.2.1.0", "perf/tpcc", dps(), time.Now())
	run := []core.Datapoint{
		{Name: "tpcc.latency_p99", Value: 70, Direction: core.LowerIsBetter}, // +40% > 20% tol
	}
	rep := Compare(f, "2025.2.1.0", "perf/tpcc", run, Thresholds{DropPct: 10, RisePct: 20})
	if !rep.Regressed {
		t.Fatalf("Regressed = false on a 40%% latency rise, want true")
	}
}

// TestVersionFallback: a version with no bucket falls back to the "" bucket.
func TestVersionFallback(t *testing.T) {
	f := Empty()
	f.Update("", "perf/tpcc", dps(), time.Now()) // version-agnostic default
	rep := Compare(f, "2099.9.9.9", "perf/tpcc", dps(), Thresholds{DropPct: 10, RisePct: 20})
	if !rep.HadBaseline {
		t.Errorf("HadBaseline = false, want true (fell back to the version-agnostic bucket)")
	}
}

// TestUpdateThenLoadRoundTrips: Update + Save + Load reproduces the stored
// values, and a missing file loads as an empty store (not an error).
func TestUpdateThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "perf.json")

	// A missing file loads empty, no error.
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if f.Baselines == nil {
		t.Fatalf("Load missing returned nil Baselines")
	}

	f.Update("2025.2.1.0", "perf/tpcc", dps(), time.Now())
	if err := Save(path, f); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e, ok := got.lookup("2025.2.1.0", "perf/tpcc", "tpcc.throughput")
	if !ok {
		t.Fatalf("throughput entry missing after round-trip")
	}
	if e.Value != 1000 || e.Unit != "req/s" || e.Direction != string(core.HigherIsBetter) {
		t.Errorf("round-tripped entry = %+v, want {1000 req/s higher_is_better}", e)
	}
	if e.UpdatedAt == "" {
		t.Errorf("UpdatedAt not stamped")
	}
}

// TestStampAndStoredMetaRoundTrip: after Update establishes a baseline, StampMeta
// records the run identity onto every entry and StoredMeta reads it back (with
// the version-agnostic "" fallback). A test with no entries reports none.
func TestStampAndStoredMetaRoundTrip(t *testing.T) {
	f := Empty()
	f.Update("2025.2.1.0", "perf/tpcc", dps(), time.Now())
	f.StampMeta("2025.2.1.0", "perf/tpcc", Meta{LoadSignature: "sigABC", YBImage: "yb:img", Commit: "abc1234"})

	m, ok := f.StoredMeta("2025.2.1.0", "perf/tpcc")
	if !ok {
		t.Fatalf("StoredMeta not found after Stamp")
	}
	if m.LoadSignature != "sigABC" || m.YBImage != "yb:img" || m.Commit != "abc1234" {
		t.Errorf("StoredMeta = %+v, want the stamped identity", m)
	}
	// Every entry carries the stamp (Compare reads any of them).
	for name, e := range f.Baselines["2025.2.1.0"]["perf/tpcc"] {
		if e.LoadSignature != "sigABC" {
			t.Errorf("entry %q load_signature = %q, want sigABC", name, e.LoadSignature)
		}
	}
	// A version-specific baseline is visible via the "" fallback lookup only when
	// stored under "". Here it is under the explicit version, so an unknown
	// version with the "" fallback finds nothing.
	if _, ok := f.StoredMeta("does-not-exist", "perf/tpcc"); ok {
		t.Errorf("StoredMeta found a baseline for an unrelated version + no '' bucket")
	}
	if _, ok := f.StoredMeta("2025.2.1.0", "perf/other"); ok {
		t.Errorf("StoredMeta found meta for a test with no entries")
	}
}

// TestStoredMetaVersionAgnosticFallback: an entry stored under the "" bucket is
// found for any version (the same fallback Compare uses).
func TestStoredMetaVersionAgnosticFallback(t *testing.T) {
	f := Empty()
	f.Update("", "perf/tpcc", dps(), time.Now())
	f.StampMeta("", "perf/tpcc", Meta{LoadSignature: "sigZ"})
	m, ok := f.StoredMeta("2025.2.1.0", "perf/tpcc")
	if !ok || m.LoadSignature != "sigZ" {
		t.Errorf("StoredMeta via '' fallback = %+v ok=%v, want sigZ", m, ok)
	}
}
