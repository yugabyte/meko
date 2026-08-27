package stress

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// Starvation is the thing this number exists to catch: even tenants score 1, a
// tenant left behind drags it toward 0.
func TestFairnessOf(t *testing.T) {
	if got := fairnessOf([]tenant{{Written: 500}, {Written: 500}}); got != 1 {
		t.Errorf("fairness of an even pair = %v, want 1", got)
	}
	if got := fairnessOf([]tenant{{Written: 1000}, {Written: 100}}); math.Abs(got-0.1) > 1e-9 {
		t.Errorf("fairness with a starved tenant = %v, want 0.1", got)
	}
	if got := fairnessOf(nil); got != 0 {
		t.Errorf("fairness of no tenants = %v, want 0", got)
	}
	// A fleet that wrote nothing at all is 0, not a division by zero.
	if got := fairnessOf([]tenant{{Written: 0}, {Written: 0}}); got != 0 {
		t.Errorf("fairness with no writes = %v, want 0", got)
	}
}

func TestTotalWritten(t *testing.T) {
	if got := totalWritten([]tenant{{Written: 3}, {Written: 4}}); got != 7 {
		t.Errorf("totalWritten = %d, want 7", got)
	}
	if got := totalWritten(nil); got != 0 {
		t.Errorf("totalWritten(nil) = %d, want 0", got)
	}
}

func TestMultitenantLoadParamsDefaults(t *testing.T) {
	p := multitenantLoadParamsFromConfig(core.NewConfig())
	if p.Tenants < 2 {
		t.Errorf("default tenants = %d, want at least 2 for multitenancy", p.Tenants)
	}
	if p.Duration <= 0 || p.PayloadBytes <= 0 {
		t.Errorf("non-positive default in %+v", p)
	}
	if p.MinFairness <= 0 || p.MinFairness >= 1 {
		t.Errorf("default min_fairness = %v, want a fraction below 1", p.MinFairness)
	}
}

func TestAmpNemesisParamsDefaults(t *testing.T) {
	p := ampNemesisParamsFromConfig(core.NewConfig())
	if p.Tenants < 1 {
		t.Errorf("default tenants = %d", p.Tenants)
	}
	if p.Duration <= 0 || p.PayloadBytes <= 0 {
		t.Errorf("non-positive default in %+v", p)
	}
	// Recovery must be allowed to take longer than the fault phase, or the test
	// fails runs that were about to settle.
	if p.SettleTimeout <= p.Duration {
		t.Errorf("settle timeout %v is not longer than the chaos duration %v", p.SettleTimeout, p.Duration)
	}
}

// A tenant's id range must not overlap its neighbour's, or the isolation check
// would be asserting nothing.
func TestTenantIDRangesDoNotOverlap(t *testing.T) {
	const span = int64(tenantIDSpan)
	for i := 0; i < 8; i++ {
		base := int64(i) * span
		next := int64(i+1) * span
		if base+span > next {
			t.Fatalf("tenant %d's range [%d,%d) overlaps tenant %d's start %d", i, base, base+span, i+1, next)
		}
	}
}

// The writers are given a deadline, not a count: an over-long phase would blow
// the test's own timeout budget.
func TestLoadDurationsAreBounded(t *testing.T) {
	load := multitenantLoadParamsFromConfig(core.NewConfig())
	nem := ampNemesisParamsFromConfig(core.NewConfig())
	for name, d := range map[string]time.Duration{"multitenant_load": load.Duration, "amp_nemesis": nem.Duration} {
		if d > 30*time.Minute {
			t.Errorf("%s default duration %v is too long for a nightly default", name, d)
		}
	}
}

// The writer's cursor must persist across load phases. When it did not, the
// second phase reissued the first phase's ids, every re-insert was a no-op the
// writer still counted, and the verification reported data loss that never
// happened — which is worse than a missed bug, because it accuses the product.
func TestWriterCursorSurvivesPhases(t *testing.T) {
	ten := tenant{Index: 2}
	base := int64(ten.Index) * 1_000_000_000

	firstPhase := []int64{}
	for i := 0; i < 5; i++ {
		firstPhase = append(firstPhase, base+ten.nextID)
		ten.nextID++
		ten.Attempted++
		ten.Written++
	}
	secondPhaseStart := base + ten.nextID
	for _, id := range firstPhase {
		if id == secondPhaseStart {
			t.Fatalf("the second phase restarts at id %d, which the first phase already wrote", id)
		}
	}
	if secondPhaseStart != base+5 {
		t.Errorf("second phase starts at %d, want %d", secondPhaseStart, base+5)
	}
}

// Acknowledged can never exceed attempted; the verification's bounds depend on
// that ordering holding.
func TestAckedNeverExceedsAttempted(t *testing.T) {
	ten := tenant{Attempted: 10, Written: 10}
	if ten.Written > ten.Attempted {
		t.Fatal("acknowledged exceeds attempted")
	}
	ten.Attempted++ // an attempt that errored: attempted moves, acknowledged does not
	if ten.Written >= ten.Attempted {
		t.Errorf("an in-doubt attempt did not widen the gap: attempted=%d acked=%d", ten.Attempted, ten.Written)
	}
}

// A tenant that committed nothing must be reported as a broken client, with the
// evidence, rather than left for the fairness gate to call starvation.
func TestSilentTenantIsNamedAsABrokenClient(t *testing.T) {
	healthy := []tenant{{Written: 10}, {Written: 8}}
	if why := silentTenant(healthy); why != "" {
		t.Errorf("silentTenant on a healthy fleet = %q, want empty", why)
	}
	broken := []tenant{{Index: 0, Name: "a", Written: 10}, {Index: 1, Name: "b", Written: 0, WriteErrors: 42, Reconnects: 3, Attempted: 45}}
	why := silentTenant(broken)
	if why == "" {
		t.Fatal("a tenant with zero writes was not reported")
	}
	for _, want := range []string{"tenant 1", "42", "3", "unusable"} {
		if !strings.Contains(why, want) {
			t.Errorf("message %q is missing %q — the evidence is what separates this from starvation", why, want)
		}
	}
}
