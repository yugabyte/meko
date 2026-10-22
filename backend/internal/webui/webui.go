// Package webui serves the built web UI (backend/web_ui) at /: its files, and index.html for any
// other path so the app's own routes load on a refresh.
package webui

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// FindDir picks the built web UI: dir if set, else <bin>/../web_ui/dist (the source tree's
// layout, as the memory engine's). It must hold index.html.
func FindDir(dir string) (string, error) {
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(filepath.Dir(exe), "..", "web_ui", "dist")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		return "", fmt.Errorf("the web UI isn't built in %s (run make web_ui), or set [web_ui] dir in "+
			"config.toml or MEKO_WEB_UI_DIR", dir)
	}
	return dir, nil
}

// Handler serves the files in dir, and index.html for a path that names none. Hashed assets are
// cached for good; index.html never is, so a rebuilt UI loads at once.
func Handler(dir string) http.Handler {
	root := os.DirFS(dir)
	files := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secure(w)
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if fi, err := fs.Stat(root, name); name == "" || name == "index.html" || err != nil || fi.IsDir() {
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFileFS(w, r, root, "index.html")
			return
		}
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}

// Missing answers every UI path when the UI isn't built, saying why.
func Missing(reason error) http.Handler {
	if reason == nil {
		reason = errors.New("the web UI isn't built")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secure(w)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, "Meko's web UI isn't available: %v\n", reason)
	})
}

// secure keeps other sites from framing the UI, which holds the client token.
func secure(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
		"frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
}
