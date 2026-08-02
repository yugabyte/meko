package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/registry"
)

// The storage-layer control surface is project-scoped: the branch and endpoint
// rows it addresses are the control plane's own, and those belong to a project.
// These pin the exact scoped paths the launcher must hit, so a prefix change that
// silently broke the contract fails here.
var (
	testProjectID   = uuid.MustParse("2f3a1c00-0000-0000-0000-000000000001")
	testProjectBase = "/control/v1/projects/" + testProjectID.String()
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

func TestControllerEndpointCreateAndList(t *testing.T) {
	var cap capture
	rec := recordingServer(t, &cap, http.StatusOK, `null`)
	c := NewController(rec.URL, testProjectID, nil)

	err := c.EndpointCreate(context.Background(), registry.ComputeEndpoint{
		EndpointID: "ep-1",
		BranchID:   "branch-1",
		Spec:       json.RawMessage(`{"k":"v"}`),
	})
	if err != nil {
		t.Fatalf("EndpointCreate: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != testProjectBase+"/endpoint" {
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
	c2 := NewController(lst.URL, testProjectID, nil)
	eps, err := c2.EndpointList(context.Background())
	if err != nil {
		t.Fatalf("EndpointList: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != testProjectBase+"/endpoint" {
		t.Fatalf("list hit %s %s", cap.method, cap.path)
	}
	if len(eps) != 1 || eps[0].EndpointID != "ep-1" || string(eps[0].Spec) != `{"k":"v"}` {
		t.Fatalf("unexpected endpoints: %+v", eps)
	}

	del := recordingServer(t, &cap, http.StatusOK, `null`)
	c3 := NewController(del.URL, testProjectID, nil)
	if err := c3.EndpointDelete(context.Background(), "ep-1"); err != nil {
		t.Fatalf("EndpointDelete: %v", err)
	}
	if cap.method != http.MethodDelete || cap.path != testProjectBase+"/endpoint/ep-1" {
		t.Fatalf("delete hit %s %s", cap.method, cap.path)
	}
}

func TestControllerReady(t *testing.T) {
	var cap capture
	srv := recordingServer(t, &cap, http.StatusOK, `null`)
	c := NewController(srv.URL, testProjectID, nil)
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
	c := NewController(srv.URL, testProjectID, nil)
	_, err := c.EndpointList(context.Background())
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
