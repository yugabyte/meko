package webui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func built(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"index.html": "<html>app</html>", "assets/app-1.js": "js", "meko-logo.svg": "<svg/>"}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func serve(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func TestHandler(t *testing.T) {
	h := Handler(built(t))
	const app = "<html>app</html>"
	cases := []struct {
		path  string
		code  int
		body  string
		cache string
	}{
		{"/", 200, app, "no-cache"},
		{"/index.html", http.StatusMovedPermanently, "", "no-cache"},           // to /, as net/http does
		{"/datapacks/x/memories", 200, app, "no-cache"},                        // the app's own routes
		{"/assets", 200, app, "no-cache"},                                      // a folder is no file
		{"/../../etc/passwd", http.StatusBadRequest, "invalid URL path\n", ""}, // http.Error drops the header
		{"/assets/app-1.js", 200, "js", "public, max-age=31536000, immutable"},
		{"/meko-logo.svg", 200, "<svg/>", ""},
	}
	for _, tc := range cases {
		w := serve(h, tc.path)
		if w.Code != tc.code || w.Body.String() != tc.body {
			t.Errorf("%s: %d %q, want %d %q", tc.path, w.Code, w.Body.String(), tc.code, tc.body)
		}
		if got := w.Header().Get("Cache-Control"); got != tc.cache {
			t.Errorf("%s: Cache-Control %q, want %q", tc.path, got, tc.cache)
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s: no frame-ancestors in the CSP", tc.path)
		}
	}
}

func TestFindDir(t *testing.T) {
	dir := built(t)
	if got, err := FindDir(dir); err != nil || got != dir {
		t.Errorf("FindDir(built) = %q, %v", got, err)
	}
	if _, err := FindDir(t.TempDir()); err == nil || !strings.Contains(err.Error(), "make web_ui") {
		t.Errorf("FindDir(empty) = %v, want an error naming make web_ui", err)
	}
}

func TestMissing(t *testing.T) {
	w := serve(Missing(errors.New("not built here")), "/")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "not built here") {
		t.Errorf("Missing: %d %q", w.Code, w.Body.String())
	}
}
