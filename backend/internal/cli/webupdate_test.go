package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/update"
)

// fakeReleaseBinary is a release's bin/meko that only answers version --json.
func fakeReleaseBinary(version string) string {
	return fmt.Sprintf("#!/bin/sh\n[ \"$1 $2\" = 'version --json' ] && echo '{\"version\":\"%s\"}'\n", version)
}

// releaseInstall makes an app folder with old installed and current, has serve take it for its own,
// and serves a GitHub API offering next.
func releaseInstall(t *testing.T, old, next string) update.App {
	t.Helper()
	app := update.App{Root: filepath.Join(t.TempDir(), "app")}
	bin := filepath.Join(app.Dir(old), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(app.Exe(old), []byte(fakeReleaseBinary(old)), 0o755); err != nil { //nolint:gosec // G306: test
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.Dir(old), "VERSION"), []byte(old+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.Repoint(old); err != nil {
		t.Fatal(err)
	}
	oldRunning := runningRelease
	runningRelease = func() (update.App, string, bool) { return app, old, true }
	t.Cleanup(func() { runningRelease = oldRunning })

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := fakeReleaseBinary(next)
	if err := tw.WriteHeader(&tar.Header{Name: "meko-" + next + "/bin/meko", Mode: 0o755, Size: int64(len(body)),
		Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = gz.Close()
	serveMekoRelease(t, next, buf.Bytes())
	return app
}

// serveMekoRelease serves a GitHub API whose one Meko release, next, has tgz as this platform's
// tarball (with its checksum), and makes the CLI use it.
func serveMekoRelease(t *testing.T, next string, tgz []byte) {
	t.Helper()
	h := sha256.Sum256(tgz)
	goos, arch := update.Platform()
	name := update.TarballName(next, goos, arch)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			_ = json.NewEncoder(w).Encode([]update.Release{{Tag: update.TagPrefix + next, Assets: []update.Asset{
				{Name: name, BrowserURL: srv.URL + "/tar"}, {Name: update.Sums, BrowserURL: srv.URL + "/sums"}}}})
		case "/tar":
			_, _ = w.Write(tgz)
		case "/sums":
			_, _ = io.WriteString(w, hex.EncodeToString(h[:])+"  "+name+"\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	oldClient := newReleaseClient
	newReleaseClient = func(string) *update.Client {
		return &update.Client{Repo: "o/r", API: srv.URL, HTTP: srv.Client()}
	}
	t.Cleanup(func() { newReleaseClient = oldClient })
}

// serveAndUpdate runs serve as a release install, installs next from the web listener's route and
// returns what serve returned.
func serveAndUpdate(t *testing.T, old, next string) (update.App, paths.Layout, error) {
	t.Helper()
	dir, port := setup(t)
	app := releaseInstall(t, old, next)
	l := paths.New(dir)
	cfg := config.Default()
	cfg.Server.Port, _ = strconv.Atoi(port)
	cfg.Secrets.Store = "file"
	cfg.Update.Channel = update.ChannelOff // the page installs even with scheduled checks off
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ServeOptions{Layout: l, Config: cfg}) }()
	stopped := false
	// Before setup's meko stop, which would signal this process: serve runs in it.
	t.Cleanup(func() {
		cancel()
		if !stopped {
			<-done
		}
	})
	// The web listener reads its port as serve does, from MEKO_WEB_PORT (setup's).
	web := "http://127.0.0.1:" + os.Getenv("MEKO_WEB_PORT")
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		resp, err := http.Get(web + "/api/v1/update") //nolint:noctx,gosec // G704: the test's own server
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the web listener didn't answer: %v", err)
		}
	}
	resp, err := http.Post(web+"/api/v1/update/apply", "application/json", nil) //nolint:noctx,gosec // as above
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("apply: %d", resp.StatusCode)
	}
	select {
	case err = <-done:
		stopped = true
	case <-time.After(60 * time.Second):
		t.Fatal("serve didn't stop to restart into the update")
	}
	if app.Current() != next {
		t.Errorf("current is %q, want %s", app.Current(), next)
	}
	if p, _ := app.ReadPending(); p == nil || p.From != old || p.To != next {
		t.Errorf("pending %+v", p)
	}
	return app, l, err
}

// Started by hand, serve stops and asks Run to exec the new version in its place.
func TestWebUpdateWithoutAServiceExecsTheNewVersion(t *testing.T) {
	t.Setenv("MEKO_SERVICE", "")
	app, l, err := serveAndUpdate(t, "0.20261021.0", "0.20261022.0")
	var ex *execError
	if !errors.As(err, &ex) || ex.exe != app.CurrentExe() {
		t.Fatalf("serve returned %v, want an exec of %s", err, app.CurrentExe())
	}
	// A successful update isn't logged as a failure.
	b, err := os.ReadFile(l.Log)
	if err != nil || !bytes.Contains(b, []byte("restarting into the update")) ||
		bytes.Contains(b, []byte("server stopped with an error")) {
		t.Errorf("the log (%v):\n%s", err, b)
	}
}

// Under the service, serve exits cleanly for the service manager to start the new version.
func TestWebUpdateUnderTheServiceExits(t *testing.T) {
	t.Setenv("MEKO_SERVICE", "systemd")
	if _, _, err := serveAndUpdate(t, "0.20261021.0", "0.20261022.0"); err != nil {
		t.Fatalf("serve returned %v", err)
	}
}
