package launch

import (
	"testing"

	"github.com/google/uuid"
)

// testBranchID is the branch id the package's tests place endpoints on.
var testBranchID = uuid.MustParse("9a054903-242f-46bc-a0c9-cc6b91a37e4c")

// The launcher is where a branch id leaves and re-enters the process as a
// string, so the two halves have to agree: what the API and the endpoint's
// BRANCH_ID carry is what `parseBranchID` reads back. One spelling now — the
// uuid's own — but the parser stays lenient, so an id in any uuid form resolves.
func TestBranchIDRoundTrip(t *testing.T) {
	want := uuid.MustParse("9753168c-7f0d-4c8f-9a3b-1e2d3c4b5a60")

	if got := want.String(); got != "9753168c-7f0d-4c8f-9a3b-1e2d3c4b5a60" {
		t.Errorf("branch id renders as %q, want the hyphenated spelling", got)
	}
	for _, spelling := range []string{
		"9753168c-7f0d-4c8f-9a3b-1e2d3c4b5a60", // what the wire carries
		"9753168c7f0d4c8f9a3b1e2d3c4b5a60",
	} {
		got, err := parseBranchID(spelling)
		if err != nil {
			t.Fatalf("parseBranchID(%q): %v", spelling, err)
		}
		if got != want {
			t.Errorf("parseBranchID(%q) = %v, want %v", spelling, got, want)
		}
	}
	if _, err := parseBranchID("not-a-branch-id"); err == nil {
		t.Error("parseBranchID(garbage) succeeded, want an error")
	}
}
