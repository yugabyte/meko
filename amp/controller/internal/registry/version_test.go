package registry

import "testing"

// TestFullPgVersionID pins the PG major -> PgVersionId mapping RecordBranch
// stamps onto a branch's amp_storage.branch_state row.
//
// It is worth pinning because a wrong value is not discovered here: the branch
// is created successfully and the mismatch only surfaces when the pageserver
// attaches, by which point the branch exists and the endpoint is failing to
// start. Zero is the case that matters most in practice — an unset field would
// otherwise seed pg_version 0 silently.
func TestFullPgVersionID(t *testing.T) {
	got, err := FullPgVersionID(18)
	if err != nil {
		t.Fatalf("FullPgVersionID(18) returned error: %v", err)
	}
	// PG_VERSION_NUM of the shipped 18.2 build.
	if got != 180002 {
		t.Errorf("FullPgVersionID(18) = %d, want 180002", got)
	}
	// The value must round-trip to the major the pageserver boots.
	if got/10000 != 18 {
		t.Errorf("FullPgVersionID(18)/10000 = %d, want 18", got/10000)
	}

	// Anything else is a caller bug: amp ships PG 18 only, so there is no
	// version to guess at.
	for _, major := range []int{0, 15, 17, 19} {
		if _, err := FullPgVersionID(major); err == nil {
			t.Errorf("FullPgVersionID(%d) = nil error, want an error", major)
		}
	}
}
