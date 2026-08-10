package kubemeta

import "testing"

// Every ampqa-created object carries the same identifying label, whichever app
// created it — a janitor selecting on ampqa-managed-by has to enumerate the apps
// and silently misses any new one.
func TestTestLabelsAlwaysIdentify(t *testing.T) {
	got := TestLabels(nil)
	if got[TestLabel] != TestLabelValue {
		t.Errorf("%s = %q, want %q", TestLabel, got[TestLabel], TestLabelValue)
	}
	if got["ampqa"] != "1" {
		t.Errorf("the pre-existing ampqa=1 label was dropped: %v", got)
	}

	withExtra := TestLabels(map[string]string{"suite": "perf"})
	if withExtra["suite"] != "perf" {
		t.Error("caller extras were not carried through")
	}

	// A caller must not be able to hide an object from the janitor.
	hijacked := TestLabels(map[string]string{TestLabel: "false", "ampqa": "0"})
	if hijacked[TestLabel] != TestLabelValue || hijacked["ampqa"] != "1" {
		t.Errorf("extras overrode the identifying labels: %v — an object the janitor cannot see is one nobody reclaims", hijacked)
	}
}
