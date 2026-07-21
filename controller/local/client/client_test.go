package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yugabyte/amp/controller/internal/registry"
)

// capture records what the test server saw for one request.
type capture struct {
	method string
	path   string
	query  string
	body   []byte
	auth   string
}

// recordingServer returns an httptest.Server that records the last request into
// *cap and replies with the given status and (optionally) JSON body.
func recordingServer(t *testing.T, cap *capture, status int, respBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.method = r.Method
		cap.path = r.URL.Path
		cap.query = r.URL.RawQuery
		cap.body = body
		cap.auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if respBody != "" {
			_, _ = io.WriteString(w, respBody)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ptrStr(s string) *string { return &s }
func ptrI64(v int64) *int64   { return &v }

func TestControllerAttachHook(t *testing.T) {
	var cap capture
	srv := recordingServer(t, &cap, http.StatusOK, `{"gen":7}`)
	c := NewController(srv.URL, nil)

	resp, err := c.AttachHook(context.Background(), registry.AttachHookRequest{
		BranchID: "0102030405060708090a0b0c0d0e0f10",
		NodeID:   ptrI64(3),
	})
	if err != nil {
		t.Fatalf("AttachHook: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != "/debug/v1/attach-hook" {
		t.Fatalf("got %s %s", cap.method, cap.path)
	}

	var sent registry.AttachHookRequest
	if err := json.Unmarshal(cap.body, &sent); err != nil {
		t.Fatalf("request body not valid AttachHookRequest: %v", err)
	}
	if sent.BranchID != "0102030405060708090a0b0c0d0e0f10" || sent.NodeID == nil || *sent.NodeID != 3 {
		t.Fatalf("unexpected request body: %+v", sent)
	}
	if resp.Generation == nil || *resp.Generation != 7 {
		t.Fatalf("unexpected generation: %+v", resp.Generation)
	}
}

func TestControllerNodeRegisterAndList(t *testing.T) {
	var cap capture
	srv := recordingServer(t, &cap, http.StatusOK, `null`)
	c := NewController(srv.URL, nil)

	err := c.NodeRegister(context.Background(), registry.NodeRegisterRequest{
		NodeID:         1,
		ListenPgAddr:   "127.0.0.1",
		ListenPgPort:   6400,
		ListenHTTPAddr: "127.0.0.1",
		ListenHTTPPort: 9898,
	})
	if err != nil {
		t.Fatalf("NodeRegister: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != "/control/v1/node" {
		t.Fatalf("register hit %s %s", cap.method, cap.path)
	}
	var sent registry.NodeRegisterRequest
	if err := json.Unmarshal(cap.body, &sent); err != nil {
		t.Fatalf("bad register body: %v", err)
	}
	if sent.NodeID != 1 || sent.ListenHTTPPort != 9898 {
		t.Fatalf("unexpected register body: %+v", sent)
	}

	list := recordingServer(t, &cap, http.StatusOK,
		`[{"id":1,"availability":"Active","listen_http_addr":"127.0.0.1","listen_http_port":9898,"listen_pg_addr":"127.0.0.1","listen_pg_port":6400}]`)
	c2 := NewController(list.URL, nil)
	nodes, err := c2.NodeList(context.Background())
	if err != nil {
		t.Fatalf("NodeList: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != "/control/v1/node" {
		t.Fatalf("list hit %s %s", cap.method, cap.path)
	}
	if len(nodes) != 1 || nodes[0].ID != 1 || nodes[0].Availability != "Active" || nodes[0].ListenHTTPPort != 9898 {
		t.Fatalf("unexpected nodes: %+v", nodes)
	}
}

func TestControllerPendingInit(t *testing.T) {
	var cap capture

	// Record.
	rec := recordingServer(t, &cap, http.StatusOK, `null`)
	c := NewController(rec.URL, nil)
	err := c.RecordPendingInit(context.Background(), "branch-xyz", registry.PendingBranchInitRequest{
		BranchName: "main",
		PgVersion:  180000,
		StartLSN:   "0/0",
	})
	if err != nil {
		t.Fatalf("RecordPendingInit: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != "/control/v1/branch/branch-xyz/pending_init" {
		t.Fatalf("record hit %s %s", cap.method, cap.path)
	}
	var sent registry.PendingBranchInitRequest
	if err := json.Unmarshal(cap.body, &sent); err != nil {
		t.Fatalf("bad pending body: %v", err)
	}
	if sent.BranchName != "main" || sent.PgVersion != 180000 || sent.StartLSN != "0/0" {
		t.Fatalf("unexpected pending body: %+v", sent)
	}

	// List.
	lst := recordingServer(t, &cap, http.StatusOK,
		`[{"branch_id":"branch-xyz","branch_name":"main","pg_version":180000,"ancestor_branch_id":null,"start_lsn":"0/0"}]`)
	c2 := NewController(lst.URL, nil)
	inits, err := c2.ListPendingInits(context.Background())
	if err != nil {
		t.Fatalf("ListPendingInits: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != "/control/v1/pending_timeline_inits" {
		t.Fatalf("list hit %s %s", cap.method, cap.path)
	}
	if len(inits) != 1 || inits[0].BranchID != "branch-xyz" || inits[0].PgVersion != 180000 {
		t.Fatalf("unexpected inits: %+v", inits)
	}

	// Delete.
	del := recordingServer(t, &cap, http.StatusOK, `null`)
	c3 := NewController(del.URL, nil)
	if err := c3.DeletePendingInit(context.Background(), "branch-xyz"); err != nil {
		t.Fatalf("DeletePendingInit: %v", err)
	}
	if cap.method != http.MethodDelete || cap.path != "/control/v1/branch/branch-xyz/pending_init" {
		t.Fatalf("delete hit %s %s", cap.method, cap.path)
	}
}

func TestControllerEndpointCreateAndList(t *testing.T) {
	var cap capture
	rec := recordingServer(t, &cap, http.StatusOK, `null`)
	c := NewController(rec.URL, nil)

	err := c.EndpointCreate(context.Background(), registry.ComputeEndpoint{
		EndpointID: "ep-1",
		BranchID:   "branch-1",
		Spec:       json.RawMessage(`{"k":"v"}`),
	})
	if err != nil {
		t.Fatalf("EndpointCreate: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != "/control/v1/endpoint" {
		t.Fatalf("create hit %s %s", cap.method, cap.path)
	}
	var sent registry.ComputeEndpoint
	if err := json.Unmarshal(cap.body, &sent); err != nil {
		t.Fatalf("bad endpoint body: %v", err)
	}
	if sent.EndpointID != "ep-1" || sent.BranchID != "branch-1" || string(sent.Spec) != `{"k":"v"}` {
		t.Fatalf("unexpected endpoint body: %+v", sent)
	}

	lst := recordingServer(t, &cap, http.StatusOK,
		`[{"endpoint_id":"ep-1","branch_id":"branch-1","spec":{"k":"v"}}]`)
	c2 := NewController(lst.URL, nil)
	eps, err := c2.EndpointList(context.Background())
	if err != nil {
		t.Fatalf("EndpointList: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != "/control/v1/endpoint" {
		t.Fatalf("list hit %s %s", cap.method, cap.path)
	}
	if len(eps) != 1 || eps[0].EndpointID != "ep-1" || string(eps[0].Spec) != `{"k":"v"}` {
		t.Fatalf("unexpected endpoints: %+v", eps)
	}

	del := recordingServer(t, &cap, http.StatusOK, `null`)
	c3 := NewController(del.URL, nil)
	if err := c3.EndpointDelete(context.Background(), "ep-1"); err != nil {
		t.Fatalf("EndpointDelete: %v", err)
	}
	if cap.method != http.MethodDelete || cap.path != "/control/v1/endpoint/ep-1" {
		t.Fatalf("delete hit %s %s", cap.method, cap.path)
	}
}

func TestControllerBranchRegisterAndNames(t *testing.T) {
	var cap capture
	rec := recordingServer(t, &cap, http.StatusOK, `null`)
	c := NewController(rec.URL, nil)
	if err := c.BranchRegister(context.Background(), "b1", registry.BranchRegisterRequest{
		StartLSN:   "0/16B3E10",
		BranchName: ptrStr("main"),
	}); err != nil {
		t.Fatalf("BranchRegister: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != "/control/v1/branch/b1/register" {
		t.Fatalf("register hit %s %s", cap.method, cap.path)
	}

	names := recordingServer(t, &cap, http.StatusOK, `[{"branch_id":"b1","branch_name":"main"}]`)
	c2 := NewController(names.URL, nil)
	got, err := c2.ListBranchNames(context.Background())
	if err != nil {
		t.Fatalf("ListBranchNames: %v", err)
	}
	if len(got) != 1 || got[0].BranchID != "b1" || got[0].BranchName != "main" {
		t.Fatalf("unexpected names: %+v", got)
	}
}

func TestControllerReady(t *testing.T) {
	var cap capture
	srv := recordingServer(t, &cap, http.StatusOK, `null`)
	c := NewController(srv.URL, nil)
	if err := c.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != "/ready" {
		t.Fatalf("ready hit %s %s", cap.method, cap.path)
	}
}

func TestControllerHTTPError(t *testing.T) {
	var cap capture
	srv := recordingServer(t, &cap, http.StatusConflict, `{"message":"nope"}`)
	c := NewController(srv.URL, nil)
	_, err := c.NodeList(context.Background())
	if err == nil {
		t.Fatal("expected error on 409")
	}
	he, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if he.Status != http.StatusConflict {
		t.Fatalf("unexpected status: %d", he.Status)
	}
}

func TestPageserverTimelineInfo(t *testing.T) {
	var cap capture
	srv := recordingServer(t, &cap, http.StatusOK,
		`{"branch_id":"b1","last_record_lsn":"0/16B3E10","disk_consistent_lsn":"0/16B3D00","remote_consistent_lsn":"0/0"}`)
	p := NewPageserver(srv.URL, nil)

	info, err := p.TimelineInfo(context.Background(), "b1")
	if err != nil {
		t.Fatalf("TimelineInfo: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != "/v1/branch/b1" {
		t.Fatalf("hit %s %s", cap.method, cap.path)
	}
	if info.BranchID != "b1" || info.LastRecordLSN != "0/16B3E10" || info.DiskConsistentLSN != "0/16B3D00" {
		t.Fatalf("unexpected info: %+v", info)
	}
}

func TestPageserverStatusAndFlush(t *testing.T) {
	var cap capture
	srv := recordingServer(t, &cap, http.StatusOK, `null`)
	p := NewPageserver(srv.URL, nil)

	if err := p.Status(context.Background()); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if cap.path != "/v1/status" {
		t.Fatalf("status hit %s", cap.path)
	}

	if err := p.BranchFlush(context.Background(), "b1", "0/16B3E10"); err != nil {
		t.Fatalf("BranchFlush: %v", err)
	}
	if cap.method != http.MethodPut || cap.path != "/v1/branch/b1/flush" {
		t.Fatalf("flush hit %s %s", cap.method, cap.path)
	}
	if cap.query != "lsn=0%2F16B3E10" {
		t.Fatalf("unexpected flush query: %q", cap.query)
	}

	if err := p.BranchFlush(context.Background(), "b1", ""); err != nil {
		t.Fatalf("BranchFlush no-wait: %v", err)
	}
	if cap.query != "" {
		t.Fatalf("expected no query, got %q", cap.query)
	}
}

func TestSafekeeperCreateTimelineAndStatus(t *testing.T) {
	var cap capture
	srv := recordingServer(t, &cap, http.StatusOK, `null`)
	s := NewSafekeeper(srv.URL, nil)

	err := s.CreateTimeline(context.Background(), SafekeeperTimelineCreateRequest{
		BranchID:  "b1",
		PgVersion: 180000,
		StartLSN:  "0/16B3E10",
	})
	if err != nil {
		t.Fatalf("CreateTimeline: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != "/v1/branch" {
		t.Fatalf("create hit %s %s", cap.method, cap.path)
	}
	// pg_version must be a bare number and the optional fields present as null.
	var raw map[string]any
	if err := json.Unmarshal(cap.body, &raw); err != nil {
		t.Fatalf("bad create body: %v", err)
	}
	if raw["pg_version"] != float64(180000) {
		t.Fatalf("pg_version not a number: %v", raw["pg_version"])
	}
	if raw["start_lsn"] != "0/16B3E10" {
		t.Fatalf("start_lsn wrong: %v", raw["start_lsn"])
	}
	if _, ok := raw["system_id"]; !ok {
		t.Fatal("system_id field missing")
	}

	tst := recordingServer(t, &cap, http.StatusOK, `{"branch_id":"b1","flush_lsn":"0/16B3E10","commit_lsn":"0/16B3E10"}`)
	s2 := NewSafekeeper(tst.URL, nil)
	got, err := s2.TimelineStatus(context.Background(), "b1")
	if err != nil {
		t.Fatalf("TimelineStatus: %v", err)
	}
	if cap.path != "/v1/branch/b1" {
		t.Fatalf("status hit %s", cap.path)
	}
	if got.FlushLSN != "0/16B3E10" {
		t.Fatalf("unexpected flush_lsn: %q", got.FlushLSN)
	}

	stt := recordingServer(t, &cap, http.StatusOK, `{}`)
	s3 := NewSafekeeper(stt.URL, nil)
	_, err = s3.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
}

func TestComputeCtlStatusAndTerminate(t *testing.T) {
	var cap capture
	srv := recordingServer(t, &cap, http.StatusOK, `{"status":"running","error":null}`)
	cc := NewComputeCtl(srv.URL, "tok-123", nil)

	st, err := cc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if cap.path != "/status" || cap.auth != "Bearer tok-123" {
		t.Fatalf("status hit %s auth=%q", cap.path, cap.auth)
	}
	if st.Status != "running" || st.Error != nil {
		t.Fatalf("unexpected status: %+v", st)
	}

	term := recordingServer(t, &cap, http.StatusOK, `{"lsn":"0/16B3E10"}`)
	cc2 := NewComputeCtl(term.URL, "tok-123", nil)
	resp, err := cc2.Terminate(context.Background(), "immediate")
	if err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != "/terminate" || cap.query != "mode=immediate" {
		t.Fatalf("terminate hit %s %s?%s", cap.method, cap.path, cap.query)
	}
	if resp.LSN == nil || *resp.LSN != "0/16B3E10" {
		t.Fatalf("unexpected terminate lsn: %+v", resp.LSN)
	}
}
