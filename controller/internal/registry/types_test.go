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

func TestPendingBranchInitWire(t *testing.T) {
	anc := "parent"
	got := mustMarshal(t, PendingBranchInit{
		BranchID: "b", BranchName: "main", PgVersion: 18, AncestorBranchID: &anc, StartLSN: "0/0",
	})
	want := `{"branch_id":"b","branch_name":"main","pg_version":18,"ancestor_branch_id":"parent","start_lsn":"0/0"}`
	if got != want {
		t.Fatalf("pending-init = %s, want %s", got, want)
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
