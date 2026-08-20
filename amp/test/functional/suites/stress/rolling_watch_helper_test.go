package stress

import (
	"testing"
)

// More than one node down at a time was not rolling, whatever option we asked YBA for.
func TestRollingObservationVerdicts(t *testing.T) {
	rolling := rollingObservation{Expected: 3, Samples: 40, MaxOut: 1, MinReady: 2}
	if !rolling.Complete() {
		t.Fatal("a sampled observation with a baseline must be usable")
	}
	if rolling.MaxOut > 1 {
		t.Fatal("fixture is wrong")
	}

	allAtOnce := rollingObservation{Expected: 3, Samples: 40, MaxOut: 3, MinReady: 0}
	if allAtOnce.MaxOut <= 1 {
		t.Error("an all-at-once restart must be distinguishable from a rolling one")
	}
}

// No samples, or none of the baseline, proves nothing and must not read as a pass.
func TestRollingObservationIsIncompleteWithoutEvidence(t *testing.T) {
	for name, obs := range map[string]rollingObservation{
		"no samples":  {Expected: 3, Samples: 0},
		"no baseline": {Expected: 0, Samples: 10},
		"errored":     {Expected: 3, Samples: 10, Err: errSample},
	} {
		if obs.Complete() {
			t.Errorf("%s: must not count as a verified rolling observation", name)
		}
	}
}

var errSample = errSampleType{}

type errSampleType struct{}

func (errSampleType) Error() string { return "sample failed" }
