package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// Every resource the launcher addresses hangs off a pool, a project and a
// branch, so its routes are nested all the way down. These pin the exact paths,
// so a nesting change that silently broke the contract fails here.
var (
	testPoolID     = uuid.MustParse("2f3a1c00-0000-0000-0000-000000000000")
	testProjectID  = uuid.MustParse("2f3a1c00-0000-0000-0000-000000000001")
	testBranchID   = uuid.MustParse("2f3a1c00-0000-0000-0000-000000000002")
	testBranchBase = "/api/v1/storage-pools/" + testPoolID.String() +
		"/projects/" + testProjectID.String() +
		"/branches/" + testBranchID.String()
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

func TestAPIEndpointCreateAndDelete(t *testing.T) {
	var cap capture
	epID := uuid.MustParse("2f3a1c00-0000-0000-0000-0000000000ee")

	rec := recordingServer(t, &cap, http.StatusAccepted,
		`{"id":"3f3a1c00-0000-0000-0000-000000000001","kind":"create_endpoint","status":"pending",`+
			`"target_ref":"`+epID.String()+`","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`)
	c := NewAPI(rec.URL, nil)

	task, err := c.CreateEndpoint(context.Background(), testPoolID, testProjectID, testBranchID,
		EndpointCreate{
			Name:       "ep_b1",
			Extended:   json.RawMessage(`{"pg_port":55432}`),
			PgSettings: map[string]string{"work_mem": "64MB"},
		})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != testBranchBase+"/endpoints" {
		t.Fatalf("create hit %s %s", cap.method, cap.path)
	}
	// The launcher's whole per-endpoint state rides in this body: the handle as
	// the row's name, the local run config as extended_config, and the caller's
	// --pg-setting overrides as pg_settings. A field silently dropped here is a
	// launcher that cannot find its own endpoint again.
	var sent struct {
		Name           *string           `json:"name"`
		ExtendedConfig map[string]any    `json:"extended_config"`
		PgSettings     map[string]string `json:"pg_settings"`
	}
	if err := json.Unmarshal(cap.body, &sent); err != nil {
		t.Fatalf("bad create body: %v", err)
	}
	if sent.Name == nil || *sent.Name != "ep_b1" {
		t.Errorf("name = %v, want ep_b1", sent.Name)
	}
	if sent.ExtendedConfig["pg_port"] != float64(55432) {
		t.Errorf("extended_config = %v, want pg_port 55432", sent.ExtendedConfig)
	}
	if sent.PgSettings["work_mem"] != "64MB" {
		t.Errorf("pg_settings = %v, want work_mem 64MB", sent.PgSettings)
	}
	// The controller reports the id it assigned as the task's target, which is how
	// the launcher learns it without waiting for the task to finish.
	if task.TargetRef == nil || *task.TargetRef != epID.String() {
		t.Fatalf("create task target_ref = %v, want %s", task.TargetRef, epID)
	}

	del := recordingServer(t, &cap, http.StatusAccepted,
		`{"id":"3f3a1c00-0000-0000-0000-000000000002","kind":"delete_endpoint","status":"pending",`+
			`"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`)
	if _, err := NewAPI(del.URL, nil).DeleteEndpoint(
		context.Background(), testPoolID, testProjectID, testBranchID, epID); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	if cap.method != http.MethodDelete || cap.path != testBranchBase+"/endpoints/"+epID.String() {
		t.Fatalf("delete hit %s %s", cap.method, cap.path)
	}
}

func TestAPIReady(t *testing.T) {
	var cap capture
	srv := recordingServer(t, &cap, http.StatusOK, `{"status":"ready"}`)
	if err := NewAPI(srv.URL, nil).Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != "/readyz" {
		t.Fatalf("ready hit %s %s", cap.method, cap.path)
	}
}

func TestAPIHTTPError(t *testing.T) {
	var cap capture
	srv := recordingServer(t, &cap, http.StatusConflict, `{"message":"nope"}`)
	_, err := NewAPI(srv.URL, nil).CreateEndpoint(
		context.Background(), testPoolID, testProjectID, testBranchID, EndpointCreate{})
	if err == nil {
		t.Fatal("expected error on 409")
	}
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if he.Status != http.StatusConflict {
		t.Fatalf("unexpected status: %d", he.Status)
	}
	if IsNotFound(err) {
		t.Error("a 409 must not read as not-found")
	}
}

// IsNotFound is what makes the launcher's teardown idempotent: `endpoint stop
// --destroy` on a record the controller already dropped has to succeed.
func TestAPIIsNotFound(t *testing.T) {
	var cap capture
	srv := recordingServer(t, &cap, http.StatusNotFound, `{"message":"gone"}`)
	_, err := NewAPI(srv.URL, nil).DeleteEndpoint(
		context.Background(), testPoolID, testProjectID, testBranchID, uuid.New())
	if !IsNotFound(err) {
		t.Fatalf("IsNotFound(%v) = false, want true", err)
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
