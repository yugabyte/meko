package perf

import (
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/baseline"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const gateTest = "perf/tpcc"

func gateCfg() perfConfig {
	return perfConfig{BaselinePath: "baselines/perf.json", ThroughputDropPct: 20, LatencyRisePct: 35}
}

// gateRun is the identity of a run at one load.
func gateRun(sig, shape string) Identity {
	return Identity{LoadSignature: sig, LoadShape: shape, YBImage: "yb:" + sig, Commit: "abc1234"}
}

// seedBaseline stores a baseline of `tps` for gateTest under version, stamped
// with the given load identity.
func seedBaseline(version string, tps float64, m baseline.Meta) *baseline.File {
	f := baseline.Empty()
	f.Update(version, gateTest, []core.Datapoint{
		{Name: "tpcc.throughput", Value: tps, Unit: "req/s", Direction: core.HigherIsBetter},
	}, time.Now())
	f.StampMeta(version, gateTest, m)
	return f
}

// measured is one run's throughput datapoint.
func measured(tps float64) []core.Datapoint {
	return []core.Datapoint{{Name: "tpcc.throughput", Value: tps, Unit: "req/s", Direction: core.HigherIsBetter}}
}

// TestGateVersionBumpUsesThePreviousVersionsBaseline is green-path 1: a run on a
// version with no baseline of its own is GATED against the previous version's
// instead of silently establishing one, so a regression landing at a version
// bump fails. It still writes its own line for the next run.
func TestGateVersionBumpUsesThePreviousVersionsBaseline(t *testing.T) {
	id := gateRun("sigA", "shapeA")
	f := seedBaseline("2025.2.1.0", 1000, id.meta())

	out := gatePerf(f, "2025.2.2.0", gateTest, measured(700), gateCfg(), id.meta())

	if !out.Regressed {
		t.Fatalf("a 30%% drop at a version bump did not regress: %+v", out)
	}
	if out.Established {
		t.Errorf("the run self-established instead of gating: %+v", out)
	}
	if out.Selection.Version != "2025.2.1.0" || !out.Selection.Fallback {
		t.Errorf("Selection = %+v, want the previous version as a fallback", out.Selection)
	}
	// The new version still gets a line of its own.
	if !out.Wrote {
		t.Errorf("Wrote = false, want the new version's baseline written")
	}
	if _, ok := f.StoredMeta("2025.2.2.0", gateTest); !ok {
		t.Errorf("no baseline written under the run's own version")
	}
}

// TestGateVersionBumpPassesWithinThresholds: the same fallback path passes a run
// that is within tolerance of the previous version's baseline.
func TestGateVersionBumpPassesWithinThresholds(t *testing.T) {
	id := gateRun("sigA", "shapeA")
	f := seedBaseline("2025.2.1.0", 1000, id.meta())

	out := gatePerf(f, "2025.2.2.0", gateTest, measured(950), gateCfg(), id.meta())
	if out.Regressed || !out.Gated {
		t.Fatalf("a 5%% drop should pass, gated: %+v", out)
	}
}

// TestGateFirstRunEverEstablishesAndWarns: with nothing in the store at all
// (the very first run, or a brand new metric) there is nothing to fall back to,
// so establishing and warning stays the behaviour.
func TestGateFirstRunEverEstablishesAndWarns(t *testing.T) {
	id := gateRun("sigA", "shapeA")
	f := baseline.Empty()

	out := gatePerf(f, "2025.2.2.0", gateTest, measured(700), gateCfg(), id.meta())
	if !out.Established || out.Gated || out.Regressed {
		t.Fatalf("out = %+v, want established and not gated", out)
	}
	if !out.Wrote {
		t.Errorf("Wrote = false, want the first baseline written")
	}
	if got := gateSummary("2025.2.2.0", gateTest, gateCfg(), out); !strings.Contains(got, "WARN") {
		t.Errorf("establish log = %q, want a WARN", got)
	}

	// A brand new metric on a version that already has a baseline for other
	// metrics also establishes rather than failing.
	f = seedBaseline("2025.2.2.0", 1000, id.meta())
	out = gatePerf(f, "2025.2.2.0", gateTest,
		[]core.Datapoint{{Name: "tpcc.new_metric", Value: 5, Direction: core.LowerIsBetter}}, gateCfg(), id.meta())
	if out.Gated || out.Regressed {
		t.Errorf("a new metric name must not be gated: %+v", out)
	}
}

// TestGateSignatureMismatchGatesAgainstNearestMatch is green-path 2: a load
// signature that does not match this version's stored baseline no longer skips
// the gate — a stored baseline with the SAME load is used instead.
func TestGateSignatureMismatchGatesAgainstNearestMatch(t *testing.T) {
	id := gateRun("sigA", "shapeA")
	f := seedBaseline("2025.2.1.0", 1000, id.meta())
	// This version's own baseline was measured at a different load entirely.
	f.Update("2025.2.2.0", gateTest, measured(400), time.Now())
	f.StampMeta("2025.2.2.0", gateTest, baseline.Meta{LoadSignature: "sigOther", LoadShape: "shapeOther"})

	out := gatePerf(f, "2025.2.2.0", gateTest, measured(700), gateCfg(), id.meta())
	if !out.Gated || !out.Regressed {
		t.Fatalf("out = %+v, want gated against the matching load and regressed", out)
	}
	if out.Selection.Version != "2025.2.1.0" || out.Selection.Match != baseline.LoadExact {
		t.Errorf("Selection = %+v, want the exact-load baseline at 2025.2.1.0", out.Selection)
	}
	// The incomparable same-version baseline is left intact.
	if m, _ := f.StoredMeta("2025.2.2.0", gateTest); m.LoadSignature != "sigOther" {
		t.Errorf("the same-version baseline was overwritten: %+v", m)
	}
}

// TestGateShapeMatchIsGatedAndWarned: only the harness cadence differs, so the
// comparison still runs, is marked as a shape match, and is WARNed.
func TestGateShapeMatchIsGatedAndWarned(t *testing.T) {
	f := seedBaseline("2025.2.2.0", 1000, baseline.Meta{LoadSignature: "sigLong", LoadShape: "shapeA"})
	id := gateRun("sigShort", "shapeA")

	out := gatePerf(f, "2025.2.2.0", gateTest, measured(700), gateCfg(), id.meta())
	if !out.Gated || !out.Regressed {
		t.Fatalf("out = %+v, want a gated, regressed shape match", out)
	}
	if out.Selection.Match != baseline.LoadShape {
		t.Errorf("Match = %q, want a shape match", out.Selection.Match)
	}
	if got := gateSummary("2025.2.2.0", gateTest, gateCfg(), out); !strings.Contains(got, "WARN") {
		t.Errorf("shape-match log = %q, want a WARN", got)
	}
}

// TestGateIncomparableLoadIsLoudAndUngated: a mismatch on the load SHAPE itself
// is not comparable to anything, so the run is not gated — but it says so, and
// leaves the stored baseline alone.
func TestGateIncomparableLoadIsLoudAndUngated(t *testing.T) {
	f := seedBaseline("2025.2.2.0", 1000, baseline.Meta{LoadSignature: "sigOther", LoadShape: "shapeOther"})
	id := gateRun("sigA", "shapeA")

	out := gatePerf(f, "2025.2.2.0", gateTest, measured(100), gateCfg(), id.meta())
	if !out.Ungated || out.Gated || out.Regressed || out.Wrote {
		t.Fatalf("out = %+v, want ungated, nothing written", out)
	}
	got := gateSummary("2025.2.2.0", gateTest, gateCfg(), out)
	if !strings.Contains(got, "WARN") || !strings.Contains(got, "UNGATED") {
		t.Errorf("incomparable log = %q, want a loud UNGATED warning", got)
	}
	prov := baselineProvenance("2025.2.2.0", gateTest, gateCfg(), id, out)
	if !strings.Contains(prov, "gated against: nothing") || !strings.Contains(prov, "outcome: not gated") {
		t.Errorf("provenance = %q, want it to record that nothing was gated", prov)
	}
}

// TestGateUpdateBaselineStillRefreshes: --update-baseline keeps refreshing this
// version's line without gating.
func TestGateUpdateBaselineStillRefreshes(t *testing.T) {
	cfg := gateCfg()
	cfg.UpdateBaseline = true
	id := gateRun("sigA", "shapeA")
	f := seedBaseline("2025.2.1.0", 1000, id.meta())

	out := gatePerf(f, "2025.2.2.0", gateTest, measured(100), cfg, id.meta())
	if !out.Updated || out.Regressed || !out.Wrote {
		t.Fatalf("out = %+v, want a refresh with no gate", out)
	}
}

// TestBaselineProvenanceNamesTheBaselineUsed: the recorded evidence says which
// stored baseline the verdict came from, that it was a fallback, and why.
func TestBaselineProvenanceNamesTheBaselineUsed(t *testing.T) {
	id := gateRun("sigA", "shapeA")
	f := seedBaseline("2025.2.1.0", 1000, id.meta())
	out := gatePerf(f, "2025.2.2.0", gateTest, measured(700), gateCfg(), id.meta())

	prov := baselineProvenance("2025.2.2.0", gateTest, gateCfg(), id, out)
	for _, want := range []string{
		"test: perf/tpcc",
		"run version: 2025.2.2.0",
		"run load: signature=sigA shape=shapeA",
		"gated against: version 2025.2.1.0",
		"fallback: true",
		"load match: exact",
		"baseline load: signature=sigA",
		"baseline written: true",
		"outcome: REGRESSION",
	} {
		if !strings.Contains(prov, want) {
			t.Errorf("provenance missing %q:\n%s", want, prov)
		}
	}
	if got := gateSummary("2025.2.2.0", gateTest, gateCfg(), out); !strings.Contains(got, "2025.2.1.0") {
		t.Errorf("gate log = %q, want the baseline it used named", got)
	}
}

// TestLoadShapeSignatureIgnoresCadenceOnly: the shape signature drops the
// harness cadence and nothing else — a real load change still moves it.
func TestLoadShapeSignatureIgnoresCadenceOnly(t *testing.T) {
	cfg := perfConfig{RF: 3, Warehouses: 4}
	short := BenchConfig{Iterations: 3, Warmup: 30 * time.Second, MeasurementDuration: 60 * time.Second}
	long := BenchConfig{Iterations: 5, Warmup: 60 * time.Second, MeasurementDuration: 600 * time.Second}

	if LoadShapeSignature(loadFieldsTPCC(short, cfg)) != LoadShapeSignature(loadFieldsTPCC(long, cfg)) {
		t.Error("a different cadence must not change the load shape")
	}
	if LoadSignature(loadFieldsTPCC(short, cfg)) == LoadSignature(loadFieldsTPCC(long, cfg)) {
		t.Error("a different cadence must still change the full load signature")
	}
	heavier := perfConfig{RF: 3, Warehouses: 8}
	if LoadShapeSignature(loadFieldsTPCC(short, cfg)) == LoadShapeSignature(loadFieldsTPCC(short, heavier)) {
		t.Error("a different warehouse count must change the load shape")
	}
	params := pgbenchParams{Scale: 10, Clients: 8, Jobs: 2}
	heavyParams := pgbenchParams{Scale: 10, Clients: 64, Jobs: 2}
	if LoadShapeSignature(loadFieldsPgbench(short, cfg, params)) == LoadShapeSignature(loadFieldsPgbench(short, cfg, heavyParams)) {
		t.Error("a different client count must change the load shape")
	}
}
