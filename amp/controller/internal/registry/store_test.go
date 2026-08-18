package registry

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestParseBranchID pins the spelling the wire uses. `yb_amp_local`, the API's
// branch ids and the endpoint's `BRANCH_ID` env var all carry the flat 32-hex
// form, while every `branch_id` column is `uuid` — so this parse is the only
// place the two meet, and nothing in the package renders the flat form to keep
// it honest by round trip.
func TestParseBranchID(t *testing.T) {
	want := uuid.MustParse("9753168c-7f0d-4c8f-9a3b-1e2d3c4b5a60")

	for _, spelling := range []string{
		"9753168c7f0d4c8f9a3b1e2d3c4b5a60", // flat: what the wire passes
		"9753168c-7f0d-4c8f-9a3b-1e2d3c4b5a60",
	} {
		got, err := ParseBranchID(spelling)
		if err != nil {
			t.Fatalf("ParseBranchID(%q): %v", spelling, err)
		}
		if got != want {
			t.Errorf("ParseBranchID(%q) = %v, want %v", spelling, got, want)
		}
	}

	// A malformed id is a caller error, not a branch that exists nowhere.
	if _, err := ParseBranchID("not-a-branch-id"); !errors.Is(err, ErrInvalid) {
		t.Errorf("ParseBranchID(garbage) error = %v, want ErrInvalid", err)
	}
}
