package node

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTimelineFlushLSN(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		// The safekeeper's TimelineStatus carries many fields; only flush_lsn is read.
		io.WriteString(w, `{"flush_lsn":"0/16ABCD0","commit_lsn":"0/16ABCD0","timeline_id":"tl-1"}`)
	}))
	defer srv.Close()

	lsn, err := NewClient().TimelineFlushLSN(context.Background(), srv.URL, "br-1")
	if err != nil {
		t.Fatalf("TimelineFlushLSN: %v", err)
	}
	if lsn != "0/16ABCD0" {
		t.Errorf("lsn = %q, want 0/16ABCD0", lsn)
	}
	if want := "/v1/branch/br-1"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

func TestTimelineFlushLSNTrailingSlashBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"flush_lsn":"0/10"}`)
	}))
	defer srv.Close()

	// A base URL with a trailing slash must not double the separator.
	if _, err := NewClient().TimelineFlushLSN(context.Background(), srv.URL+"/", "br"); err != nil {
		t.Fatalf("TimelineFlushLSN: %v", err)
	}
}

func TestTimelineFlushLSNMissing(t *testing.T) {
	// No flush_lsn in the body (e.g. a timeline the safekeeper does not serve) is
	// an error, not an empty LSN, so the caller surfaces a clear failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"commit_lsn":"0/10"}`)
	}))
	defer srv.Close()

	if _, err := NewClient().TimelineFlushLSN(context.Background(), srv.URL, "br"); err == nil {
		t.Fatal("expected an error when the status has no flush_lsn")
	}
}

func TestTimelineFlushLSNNotFound(t *testing.T) {
	// A non-2xx (the safekeeper does not know this timeline) is surfaced as an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "Timeline t/tl was not found")
	}))
	defer srv.Close()

	if _, err := NewClient().TimelineFlushLSN(context.Background(), srv.URL, "br"); err == nil {
		t.Fatal("expected an error on a 404 from the safekeeper")
	}
}
