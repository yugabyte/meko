package launch

import (
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/local/config"
)

// The launcher is where the flat 32-hex spelling both leaves and re-enters the
// process, so the two halves have to agree: `hex32UUID` renders what the API and
// the endpoint's BRANCH_ID carry, `parseBranchID` reads it back into the uuid
// every `branch_id` column holds.
func TestBranchIDRoundTrip(t *testing.T) {
	want := uuid.MustParse("9753168c-7f0d-4c8f-9a3b-1e2d3c4b5a60")

	if got := hex32UUID(want); got != "9753168c7f0d4c8f9a3b1e2d3c4b5a60" {
		t.Errorf("hex32UUID = %q, want the flat spelling", got)
	}
	for _, spelling := range []config.BranchID{
		"9753168c7f0d4c8f9a3b1e2d3c4b5a60", // flat: what the wire carries
		"9753168c-7f0d-4c8f-9a3b-1e2d3c4b5a60",
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
