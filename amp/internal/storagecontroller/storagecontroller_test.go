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

	"github.com/yugabyte/yb-light/amp/internal/config"
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

func TestClientCreateTimeline(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"timeline_id":"tl-1"}`)
	}))
	defer srv.Close()

	out, err := clientForServer(t, srv).CreateTimeline(
		context.Background(), "tenant-1", json.RawMessage(`{"new_timeline_id":"tl-1"}`))
	if err != nil {
		t.Fatalf("CreateTimeline: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/tenant/tenant-1/timeline" {
		t.Errorf("path = %q", gotPath)
	}
	if string(gotBody) != `{"new_timeline_id":"tl-1"}` {
		t.Errorf("body = %q", gotBody)
	}
	var resp struct {
		TimelineID string `json:"timeline_id"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.TimelineID != "tl-1" {
		t.Errorf("timeline_id = %q, want tl-1", resp.TimelineID)
	}
}

func TestClientDeleteTimeline(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := clientForServer(t, srv).DeleteTimeline(context.Background(), "tenant-1", "tl-1"); err != nil {
		t.Fatalf("DeleteTimeline: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/v1/tenant/tenant-1/timeline/tl-1" {
		t.Errorf("path = %q", gotPath)
	}
}

func TestClientGetTimeline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/control/v1/tenant/tenant-1/timeline/tl-1" {
			t.Errorf("path = %q", r.URL.Path)
		}
		io.WriteString(w, `{"timeline_id":"tl-1"}`)
	}))
	defer srv.Close()

	out, err := clientForServer(t, srv).GetTimeline(context.Background(), "tenant-1", "tl-1")
	if err != nil {
		t.Fatalf("GetTimeline: %v", err)
	}
	if string(out) != `{"timeline_id":"tl-1"}` {
		t.Errorf("out = %q", out)
	}
}

func TestClientErrorsOnEmptyBodyWhenResultExpected(t *testing.T) {
	// 2xx with no body: a method that expects a JSON result must surface the
	// empty response as an error, not return a silently-unpopulated value.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := clientForServer(t, srv).GetTimeline(context.Background(), "tenant-1", "tl-1"); err == nil {
		t.Fatal("expected an error decoding an empty body into a requested result")
	}
}

func TestClientErrorIncludesStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such tenant", http.StatusNotFound)
	}))
	defer srv.Close()

	err := clientForServer(t, srv).DeleteTimeline(context.Background(), "tenant-x", "tl-x")
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "no such tenant") {
		t.Errorf("error = %q, want it to carry status and body", err)
	}
}
