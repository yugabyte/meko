package registry

import (
	"encoding/json"
	"testing"
)

// These tests pin the JSON wire shapes the launcher's client expects. A field
// rename that silently broke the contract would fail here.

// TestBranchCreateRecordWire pins the body of
// POST /control/v1/projects/{p}/branch/{id}/record — still the in-process seam the
// controller's record_branch_recipe step drives, and the launcher's client type.
func TestBranchCreateRecordWire(t *testing.T) {
	anc := "parent"
	got := mustMarshal(t, BranchCreateRecord{
		BranchName: "main", PgVersion: 18, AncestorBranchID: &anc, StartLSN: "0/0",
	})
	want := `{"branch_name":"main","pg_version":18,"ancestor_branch_id":"parent","start_lsn":"0/0"}`
	if got != want {
		t.Fatalf("branch create record = %s, want %s", got, want)
	}
}

// TestComputeEndpointWire pins the launcher's endpoint record, whose `spec` is
// opaque JSON the controller round-trips without interpreting.
func TestComputeEndpointWire(t *testing.T) {
	got := mustMarshal(t, ComputeEndpoint{
		EndpointID: "ep-main", BranchID: "b", Spec: json.RawMessage(`{"k":"v"}`),
	})
	want := `{"endpoint_id":"ep-main","branch_id":"b","spec":{"k":"v"}}`
	if got != want {
		t.Fatalf("compute endpoint = %s, want %s", got, want)
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// FullPgVersionID is the only writer of `amp_storage.branch_state.pg_version`,
// and the pageserver derives the major it boots by dividing that column by
// 10000. A wrong mapping would therefore be discovered at attach, on a branch
// that already exists — so pin both the value and the refusal.
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
	// version to guess at. Zero is the one that matters in practice — an unset
	// field would otherwise have seeded pg_version 0.
	for _, major := range []int{0, 15, 17, 19} {
		if _, err := FullPgVersionID(major); err == nil {
			t.Errorf("FullPgVersionID(%d) = nil error, want an error", major)
		}
	}
}
