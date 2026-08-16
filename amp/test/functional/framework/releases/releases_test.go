package releases

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseTagRejectsBranchBuilds(t *testing.T) {
	for _, tag := range []string{"custom-d820a557d", "br-topic-3d12ec14d", "latest", "0.260812.0-zzzzzzzzz", "main"} {
		if _, _, ok := ParseTag(tag); ok {
			t.Errorf("%q parsed as a release; a chain built from branch builds proves nothing about releases", tag)
		}
	}
	version, sha, ok := ParseTag("0.260812.1-9373f3e70")
	if !ok || version != "0.260812.1" || sha != "9373f3e70" {
		t.Errorf("ParseTag = (%q, %q, %v), want (0.260812.1, 9373f3e70, true)", version, sha, ok)
	}
}

func TestCompareIsNumeric(t *testing.T) {
	if Compare("0.260812.10", "0.260812.2") <= 0 {
		t.Error("patch 10 must order above patch 2")
	}
	if Compare("0.260812.0", "0.260812.0") != 0 {
		t.Error("equal versions must compare equal")
	}
}

func TestImageRefsAreBare(t *testing.T) {
	r := Release{Tag: "0.260812.1-9373f3e70"}
	if got := r.ControllerImage(); got != "amp-controller:0.260812.1-9373f3e70" {
		t.Errorf("controller image %q: ampstack's registry knob resolves bare refs, a qualified one would bypass it", got)
	}
	if got := r.EndpointImage(); got != "amp-endpoint:0.260812.1-9373f3e70" {
		t.Errorf("endpoint image %q", got)
	}
}

func TestListReadsReleasesEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/releases" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if q := r.URL.Query().Get("require"); q != "amp-controller,amp-endpoint" {
			t.Errorf("require=%q: a release missing either image cannot be deployed", q)
		}
		_, _ = w.Write([]byte(`{"items":[{"version":"0.260812.1","tag":"0.260812.1-9373f3e70"},{"version":"0.260812.0","tag":"0.260812.0-5fb27dca5"}]}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL).List(context.Background(), 5)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].Version != "0.260812.1" {
		t.Fatalf("got %+v, want two releases newest first", got)
	}
}

// A portal that predates /releases must not take the chain down with it: the
// same list is derivable from the builds the portal recorded.
func TestListFallsBackToBuilds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/releases" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"items":[
			{"build_tag":"custom-d820a557d","branch":"topic","commit_sha":"d820a557d"},
			{"build_tag":"0.260811.0-c06368f8d","branch":"main","seen_at":"2026-08-11T05:13:11Z"},
			{"build_tag":"0.260812.1-9373f3e70","branch":"main","seen_at":"2026-08-13T05:15:31Z"}]}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL).List(context.Background(), 5)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v, want the two releases without the branch build", got)
	}
	if got[0].Version != "0.260812.1" || got[1].Version != "0.260811.0" {
		t.Errorf("got %+v, want newest first", got)
	}
}

func TestListReportsAnEmptyPortal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()
	if _, err := New(srv.URL).List(context.Background(), 5); err == nil {
		t.Error("an empty release list must be an error, not an empty chain that silently tests nothing")
	}
}
