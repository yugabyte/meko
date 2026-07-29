package registry

import (
	"encoding/json"
	"testing"
)

// These tests pin the JSON wire shapes the launcher's client expects. A field
// rename that silently broke the contract would fail here.

func TestAttachHookResponseWire(t *testing.T) {
	g := uint32(5)
	got := mustMarshal(t, AttachHookResponse{Generation: &g})
	if got != `{"gen":5}` {
		t.Fatalf("attach-hook response = %s, want {\"gen\":5}", got)
	}
	if got := mustMarshal(t, AttachHookResponse{}); got != `{"gen":null}` {
		t.Fatalf("detach response = %s, want {\"gen\":null}", got)
	}
}

func TestInspectResponseWire(t *testing.T) {
	g := uint32(7)
	if got := mustMarshal(t, InspectResponse{Generation: &g}); got != `{"generation":7}` {
		t.Fatalf("inspect response = %s, want {\"generation\":7}", got)
	}
	if got := mustMarshal(t, InspectResponse{}); got != `{"generation":null}` {
		t.Fatalf("unattached inspect = %s, want {\"generation\":null}", got)
	}
}

func TestAttachHookRequestParse(t *testing.T) {
	var req AttachHookRequest
	body := `{"branch_id":"abc","attach":true,"generation_override":null,"config":null}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.BranchID != "abc" || !req.Attach {
		t.Fatalf("parsed attach-hook request = %+v", req)
	}
	if req.GenerationOverride != nil {
		t.Fatalf("generation_override should be nil, got %v", *req.GenerationOverride)
	}
}

func TestBranchCreateResponseWire(t *testing.T) {
	got := mustMarshal(t, BranchCreateResponse{Shards: []BranchCreateResponseShard{
		{BranchID: "b", Generation: 4},
	}})
	want := `{"shards":[{"branch_id":"b","generation":4}]}`
	if got != want {
		t.Fatalf("branch-create response = %s, want %s", got, want)
	}
}

func TestBranchRecordWire(t *testing.T) {
	anc := "parent"
	got := mustMarshal(t, BranchRecord{
		BranchID: "b", BranchName: "main", PgVersion: 18, AncestorBranchID: &anc, StartLSN: "0/0",
	})
	want := `{"branch_id":"b","branch_name":"main","pg_version":18,"ancestor_branch_id":"parent","start_lsn":"0/0"}`
	if got != want {
		t.Fatalf("branch record = %s, want %s", got, want)
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
