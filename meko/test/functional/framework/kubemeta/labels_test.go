package kubemeta

import (
	"strings"
	"testing"
)

// Every mekoqa-created object carries the same identifying label, whichever app
// created it, a janitor selecting on mekoqa-managed-by has to enumerate the apps
// and silently misses any new one.
func TestTestLabelsAlwaysIdentify(t *testing.T) {
	got := TestLabels(nil)
	if got[TestLabel] != TestLabelValue {
		t.Errorf("%s = %q, want %q", TestLabel, got[TestLabel], TestLabelValue)
	}
	if got["mekoqa"] != "1" {
		t.Errorf("the pre-existing mekoqa=1 label was dropped: %v", got)
	}

	withExtra := TestLabels(map[string]string{"suite": "perf"})
	if withExtra["suite"] != "perf" {
		t.Error("caller extras were not carried through")
	}

	// A caller must not be able to hide an object from the janitor.
	hijacked := TestLabels(map[string]string{TestLabel: "false", "mekoqa": "0"})
	if hijacked[TestLabel] != TestLabelValue || hijacked["mekoqa"] != "1" {
		t.Errorf("extras overrode the identifying labels: %v, an object the janitor cannot see is one nobody reclaims", hijacked)
	}
}

func TestLabelValueSanitises(t *testing.T) {
	cases := map[string]string{
		"spilshchikov":          "spilshchikov",
		"a b@yugabyte.com":      "a-b-yugabyte.com",
		"--x--":                 "x",
		"":                      "",
		strings.Repeat("a", 80): strings.Repeat("a", 63),
	}
	for in, want := range cases {
		if got := LabelValue(in); got != want {
			t.Errorf("LabelValue(%q) = %q, want %q", in, got, want)
		}
	}
}
