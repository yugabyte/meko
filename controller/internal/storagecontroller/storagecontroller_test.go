package storagecontroller

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/yugabyte/yb-amp/controller/internal/config"
)

func TestSpecBuildsArgs(t *testing.T) {
	spec := Spec(config.StorageControllerConfig{
		BinPath:     "/opt/bin/storage_controller",
		ListenAddr:  "127.0.0.1:50051",
		DatabaseURL: "postgres://u@h/db",
		Dev:         true,
		ExtraArgs:   []string{"--timeline-safekeeper-count", "1"},
	})

	if spec.Name != "storage_controller" {
		t.Errorf("Name = %q", spec.Name)
	}
	if spec.Path != "/opt/bin/storage_controller" {
		t.Errorf("Path = %q", spec.Path)
	}
	want := []string{
		"--listen", "127.0.0.1:50051",
		"--database-url", "postgres://u@h/db",
		"--dev",
		"--timeline-safekeeper-count", "1",
	}
	if !slices.Equal(spec.Args, want) {
		t.Errorf("Args = %v, want %v", spec.Args, want)
	}
	if spec.Ready == nil {
		t.Error("Ready probe not set")
	}
}

func TestSpecOmitsDatabaseAndDev(t *testing.T) {
	spec := Spec(config.StorageControllerConfig{
		BinPath:    "storage_controller",
		ListenAddr: ":50051",
	})
	want := []string{"--listen", ":50051"}
	if !slices.Equal(spec.Args, want) {
		t.Errorf("Args = %v, want %v", spec.Args, want)
	}
}

// clientForServer returns a client pointed at srv, using a bare ":port" address
// so the test also exercises NewClient's loopback normalization.
func clientForServer(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	return NewClient(":" + port)
}

func TestClientReady(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := clientForServer(t, srv).Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if gotPath != "/ready" {
		t.Errorf("path = %q, want /ready", gotPath)
	}
}

func TestClientReadyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "warming up", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if err := clientForServer(t, srv).Ready(context.Background()); err == nil {
		t.Fatal("expected error on 503")
	}
}

func TestClientRecordPendingTimelineInit(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ancestor := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	err := clientForServer(t, srv).RecordPendingTimelineInit(context.Background(), "branch-1",
		PendingTimelineInit{
			BranchName:       "feature",
			PgVersion:        17,
			AncestorBranchID: &ancestor,
			StartLSN:         "0/16ABCD0",
		})
	if err != nil {
		t.Fatalf("RecordPendingTimelineInit: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/control/v1/branch/branch-1/pending_init" {
		t.Errorf("path = %q", gotPath)
	}
	// The field names must match the Rust serde shape (PendingTimelineInitRequest).
	if gotBody["branch_name"] != "feature" {
		t.Errorf("branch_name = %v", gotBody["branch_name"])
	}
	if gotBody["ancestor_branch_id"] != ancestor {
		t.Errorf("ancestor_branch_id = %v, want %q", gotBody["ancestor_branch_id"], ancestor)
	}
	if gotBody["start_lsn"] != "0/16ABCD0" {
		t.Errorf("start_lsn = %v", gotBody["start_lsn"])
	}
}

func TestClientDeletePendingTimelineInit(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := clientForServer(t, srv).DeletePendingTimelineInit(context.Background(), "branch-1"); err != nil {
		t.Fatalf("DeletePendingTimelineInit: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/control/v1/branch/branch-1/pending_init" {
		t.Errorf("path = %q", gotPath)
	}
}

func TestClientDeletePendingTimelineInitTolerates404(t *testing.T) {
	// A missing recipe (already materialized) is treated as success.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such recipe", http.StatusNotFound)
	}))
	defer srv.Close()

	if err := clientForServer(t, srv).DeletePendingTimelineInit(context.Background(), "branch-x"); err != nil {
		t.Fatalf("DeletePendingTimelineInit should tolerate 404, got: %v", err)
	}
}

func TestClientListPendingTimelineInits(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"branch_id":"b1","branch_name":"main","pg_version":17,"ancestor_branch_id":null,"start_lsn":"0/0"},`+
			`{"branch_id":"b2","branch_name":"feature","pg_version":17,"ancestor_branch_id":"b1","start_lsn":"0/16ABCD0"}]`)
	}))
	defer srv.Close()

	inits, err := clientForServer(t, srv).ListPendingTimelineInits(context.Background())
	if err != nil {
		t.Fatalf("ListPendingTimelineInits: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/control/v1/pending_timeline_inits" {
		t.Errorf("path = %q", gotPath)
	}
	if len(inits) != 2 {
		t.Fatalf("got %d inits, want 2", len(inits))
	}
	if inits[0].BranchID != "b1" || inits[0].AncestorBranchID != nil {
		t.Errorf("inits[0] = %+v", inits[0])
	}
	if inits[1].BranchID != "b2" || inits[1].AncestorBranchID == nil || *inits[1].AncestorBranchID != "b1" {
		t.Errorf("inits[1] = %+v", inits[1])
	}
}

func TestClientErrorIncludesStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such branch", http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := clientForServer(t, srv).RecordPendingTimelineInit(context.Background(), "branch-x",
		PendingTimelineInit{BranchName: "x", PgVersion: 17, StartLSN: "0/0"})
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "no such branch") {
		t.Errorf("error = %q, want it to carry status and body", err)
	}
}
