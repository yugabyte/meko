package registry

import (
	"encoding/json"
	"testing"
)

// These tests pin the JSON wire shapes to what the launcher's Rust client
// (control_plane) and pageserver_api::controller_api serialize/expect. A field
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
	att := [2]uint64{3, 7}
	if got := mustMarshal(t, InspectResponse{Attachment: &att}); got != `{"attachment":[3,7]}` {
		t.Fatalf("inspect response = %s, want {\"attachment\":[3,7]}", got)
	}
	if got := mustMarshal(t, InspectResponse{}); got != `{"attachment":null}` {
		t.Fatalf("unattached inspect = %s, want {\"attachment\":null}", got)
	}
}

func TestAttachHookRequestParse(t *testing.T) {
	var req AttachHookRequest
	body := `{"branch_id":"abc","node_id":1,"generation_override":null,"config":null}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.BranchID != "abc" || req.NodeID == nil || *req.NodeID != 1 {
		t.Fatalf("parsed attach-hook request = %+v", req)
	}
	if req.GenerationOverride != nil {
		t.Fatalf("generation_override should be nil, got %v", *req.GenerationOverride)
	}
}

func TestBranchCreateResponseWire(t *testing.T) {
	got := mustMarshal(t, BranchCreateResponse{Shards: []BranchCreateResponseShard{
		{BranchID: "b", NodeID: 2, Generation: 4},
	}})
	want := `{"shards":[{"branch_id":"b","node_id":2,"generation":4}]}`
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
