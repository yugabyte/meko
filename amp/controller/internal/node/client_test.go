package node

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// testBranchID is a branch id in the only spelling one ever has: the uuid's own.
var testBranchID = uuid.MustParse("92331eed-afb2-40ef-bb2d-724d649bee78")

func TestTimelineFlushLSN(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		// The safekeeper's TimelineStatus carries many fields; only flush_lsn is read.
		io.WriteString(w, `{"flush_lsn":"0/16ABCD0","commit_lsn":"0/16ABCD0","timeline_id":"tl-1"}`)
	}))
	defer srv.Close()

	lsn, err := NewClient().TimelineFlushLSN(context.Background(), srv.URL, testBranchID)
	if err != nil {
		t.Fatalf("TimelineFlushLSN: %v", err)
	}
	if lsn != "0/16ABCD0" {
		t.Errorf("lsn = %q, want 0/16ABCD0", lsn)
	}
	if want := "/v1/branch/" + testBranchID.String(); gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

func TestTimelineFlushLSNTrailingSlashBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"flush_lsn":"0/10"}`)
	}))
	defer srv.Close()

	// A base URL with a trailing slash must not double the separator.
	if _, err := NewClient().TimelineFlushLSN(context.Background(), srv.URL+"/", testBranchID); err != nil {
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

	if _, err := NewClient().TimelineFlushLSN(context.Background(), srv.URL, testBranchID); err == nil {
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

	if _, err := NewClient().TimelineFlushLSN(context.Background(), srv.URL, testBranchID); err == nil {
		t.Fatal("expected an error on a 404 from the safekeeper")
	}
}

// A node that never answered is ErrUnreachable — the signal callers retry on —
// while a node that answered badly is not, however unwelcome the answer.
func TestTimelineFlushLSNUnreachableIsDistinguished(t *testing.T) {
	// Port 1 needs root to bind, so nothing is listening: the dial is refused.
	_, err := NewClient().TimelineFlushLSN(context.Background(), "http://127.0.0.1:1", testBranchID)
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("dial refused: err = %v, want ErrUnreachable", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := NewClient().TimelineFlushLSN(context.Background(), srv.URL, testBranchID); errors.Is(err, ErrUnreachable) {
		t.Errorf("404 from the safekeeper: err = %v, want a non-ErrUnreachable error", err)
	}
}
