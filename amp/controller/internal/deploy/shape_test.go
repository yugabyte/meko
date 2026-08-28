package deploy

import (
	"strings"
	"testing"
)

// The floor is the whole point: a core without the memory to feed it produces an
// endpoint that starts, serves, and then evicts its own cache. These are the
// shapes that decide whether a request is accepted.
func TestResolveEndpointShape(t *testing.T) {
	for _, tc := range []struct {
		name           string
		cpu, memory    string
		ratio          int
		wantCPU, wantM string
		wantErr        bool
	}{
		{name: "nothing named gets the default cores and their memory",
			ratio: 8, wantCPU: "2", wantM: "16Gi"},
		{name: "memory follows cpu when only cpu is named",
			cpu: "8", ratio: 8, wantCPU: "8", wantM: "64Gi"},
		{name: "a fractional core still gets whole GiB, rounded up",
			cpu: "500m", ratio: 8, wantCPU: "500m", wantM: "4Gi"},
		{name: "exactly on the floor is accepted",
			cpu: "4", memory: "32Gi", ratio: 8, wantCPU: "4", wantM: "32Gi"},
		// The requirement for 0.1 cores is 858993459.2 bytes, which no byte count
		// can equal. 858993459 is the nearest a caller can ask for and is 0.2
		// bytes short, so a strict comparison refuses it; the tolerance is what
		// makes a request that is as close as the units allow acceptable. A
		// whole-number core count would not catch this — 1.5 × 8 GiB is exact in
		// float64, so strict and tolerant agree there.
		{name: "a request as close to the floor as its units allow is accepted",
			cpu: "0.1", memory: "858993459", ratio: 8, wantCPU: "0.1", wantM: "858993459"},
		{name: "more memory per core than the floor is a legitimate ask",
			cpu: "2", memory: "64Gi", ratio: 8, wantCPU: "2", wantM: "64Gi"},
		{name: "below the floor is refused",
			cpu: "4", memory: "16Gi", ratio: 8, wantErr: true},
		{name: "one GiB under is still under",
			cpu: "1", memory: "7Gi", ratio: 8, wantErr: true},
		{name: "the old default shape no longer passes",
			cpu: "2", memory: "4Gi", ratio: 8, wantErr: true},
		{name: "a different configured ratio moves the floor",
			cpu: "4", memory: "16Gi", ratio: 4, wantCPU: "4", wantM: "16Gi"},
		{name: "and moves what the default fills in",
			cpu: "2", ratio: 16, wantCPU: "2", wantM: "32Gi"},
		{name: "zero cpu is refused rather than dividing by it",
			cpu: "0", memory: "8Gi", ratio: 8, wantErr: true},
		{name: "an unreadable memory quantity is refused",
			cpu: "2", memory: "sixteen", ratio: 8, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cpu, mem, errMsg := ResolveEndpointShape(tc.cpu, tc.memory, tc.ratio)
			if tc.wantErr {
				if errMsg == "" {
					t.Fatalf("got (%q, %q), want a refusal", cpu, mem)
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("refused: %s", errMsg)
			}
			if cpu != tc.wantCPU || mem != tc.wantM {
				t.Errorf("got (%q, %q), want (%q, %q)", cpu, mem, tc.wantCPU, tc.wantM)
			}
		})
	}
}

// The refusal has to say what to ask for instead. An operator who is told only
// "too small" has to work out the floor themselves, and the number they need is
// the one this function already computed.
func TestResolveEndpointShapeSaysWhatWouldBeAccepted(t *testing.T) {
	_, _, errMsg := ResolveEndpointShape("4", "16Gi", 8)
	if errMsg == "" {
		t.Fatal("want a refusal")
	}
	for _, want := range []string{"16Gi", "8 GiB per vCPU", "32Gi"} {
		if !strings.Contains(errMsg, want) {
			t.Errorf("the refusal does not mention %q: %s", want, errMsg)
		}
	}
}

func TestNoWorseThan(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		fromCPU, fromM, toCPU, toM string
		want                       bool
	}{
		// The grandfather case: 2/4Gi is what everything created before the floor
		// sits at, and it has to stay resizable.
		{"same shape", "2", "4Gi", "2", "4Gi", true},
		{"shrink towards the floor", "2", "4Gi", "1", "4Gi", true},
		{"grow memory", "2", "4Gi", "2", "8Gi", true},
		{"grow cores alone starves further", "2", "4Gi", "4", "4Gi", false},
		{"shrink memory alone", "2", "4Gi", "2", "2Gi", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := NoWorseThan(tc.fromCPU, tc.fromM, tc.toCPU, tc.toM)
			if got != tc.want {
				t.Fatalf("NoWorseThan(%q,%q -> %q,%q) = %v (%s), want %v",
					tc.fromCPU, tc.fromM, tc.toCPU, tc.toM, got, msg, tc.want)
			}
			if !got && msg == "" {
				t.Error("a refusal with no message reaches the caller as an empty 400")
			}
		})
	}
}

// A dimension the request leaves out falls back to the endpoint's stored value,
// and nothing validates that. Reading it as "not worse" would hand a value the
// parser rejects to the deploy as the resolved shape, so it is refused instead.
func TestNoWorseThanRefusesWhatItCannotRead(t *testing.T) {
	for _, tc := range []struct{ name, fromCPU, fromM string }{
		{"unparseable memory", "2", "not-a-quantity"},
		{"unparseable cpu", "two", "4Gi"},
		{"empty pair", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, msg := NoWorseThan(tc.fromCPU, tc.fromM, "4", "32Gi")
			if ok {
				t.Fatal("an unreadable stored shape must not read as 'not worse'")
			}
			if !strings.Contains(msg, "cannot be read") {
				t.Errorf("the refusal does not say why: %s", msg)
			}
		})
	}
}
