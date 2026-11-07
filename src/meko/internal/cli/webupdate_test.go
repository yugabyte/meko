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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/update"
)

// fakeReleaseBinary is a release's bin/meko that only answers version --json, and logs its arguments
// to $FAKE_MEKO_ARGS when that's set.
func fakeReleaseBinary(version string) string {
	return fmt.Sprintf("#!/bin/sh\n[ -z \"$FAKE_MEKO_ARGS\" ] || echo \"$*\" >> \"$FAKE_MEKO_ARGS\"\n"+
		"[ \"$1 $2\" = 'version --json' ] && echo '{\"version\":\"%s\"}'\n", version)
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
	sums := hex.EncodeToString(h[:]) + "  " + name + "\n"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			_ = json.NewEncoder(w).Encode([]update.Release{{Tag: update.TagPrefix + next, Assets: []update.Asset{
				{Name: name, BrowserURL: srv.URL + "/tar"}, {Name: update.Sums, BrowserURL: srv.URL + "/sums"},
				{Name: update.SumsSig, BrowserURL: srv.URL + "/sig"}}}})
		case "/tar":
			_, _ = w.Write(tgz)
		case "/sums":
			_, _ = io.WriteString(w, sums)
		case "/sig":
			_, _ = io.WriteString(w, signSums(sums))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	oldClient := newReleaseClient
	newReleaseClient = func(string) *update.Client { return testClient(srv) }
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
	// Meko's one port, as serve reads it: MEKO_PORT (setup's).
	web := "http://127.0.0.1:" + os.Getenv("MEKO_PORT")
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

// serveUntilUp runs serve until its web listener answers, then stops it.
func serveUntilUp(t *testing.T, dir, port string) {
	t.Helper()
	cfg := config.Default()
	cfg.Server.Port, _ = strconv.Atoi(port)
	cfg.Secrets.Store = "file"
	cfg.Update.Channel = update.ChannelOff
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ServeOptions{Layout: paths.New(dir), Config: cfg}) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	web := "http://127.0.0.1:" + os.Getenv("MEKO_PORT")
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		resp, err := http.Get(web + "/api/v1/update") //nolint:noctx,gosec // G704: the test's own server
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the web listener didn't answer: %v", err)
		}
	}
}

// The extensions installed under their commands' old names move onto the new ones, like meko-cli,
// only once the release is past probation: a Meko rolled back after moving them would have a
// registry naming links its catalog doesn't know.
func TestServeRenamesExtensionsPastProbation(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	release := "0.20261021.0"
	app := releaseInstall(t, release, "0.20261022.0")
	if err := app.Switch("0.20261020.0", release, 1); err != nil {
		t.Fatal(err)
	}
	old := agentkFromBeforeTheRename(t, dir)
	serveUntilUp(t, dir, port)
	if target, err := os.Readlink(old); err != nil || filepath.Base(target) != "agentk" {
		t.Fatalf("on probation ~/.local/bin/agentk -> %q, %v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(old), "me_kode")); !os.IsNotExist(err) {
		t.Fatalf("~/.local/bin/me_kode on probation: %v", err)
	}
	finishUpdate(context.Background(), app, release, 0, paths.New(dir), slog.New(slog.DiscardHandler))
	wantRenamed(t, dir, old)
}

// A release on probation doesn't make meko-cli at its start, since one rolled back would leave
// meko-cli pointing at an older meko that runs as meko by that name. It makes it once the update is
// done, and a release past probation makes it at its start.
func TestServeMakesMekoCLIPastProbation(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	release := "0.20261021.0"
	app := releaseInstall(t, release, "0.20261022.0")
	home := os.Getenv("HOME")
	if _, err := app.LinkCommand(home); err != nil {
		t.Fatal(err)
	}
	if err := app.Switch("0.20261020.0", release, 1); err != nil {
		t.Fatal(err)
	}
	mekoCLI := filepath.Join(home, ".local", "bin", "meko-cli")
	serveUntilUp(t, dir, port)
	if _, err := os.Lstat(mekoCLI); !os.IsNotExist(err) { //nolint:gosec // G703: the test's home
		t.Fatalf("meko-cli on probation: %v", err)
	}
	finishUpdate(context.Background(), app, release, 0, paths.New(dir), slog.New(slog.DiscardHandler))
	if p, _ := app.ReadPending(); p != nil {
		t.Fatalf("the update isn't done: %+v", p)
	}
	if target, err := os.Readlink(mekoCLI); err != nil || target != app.CurrentExe() {
		t.Fatalf("meko-cli once the update is done -> %q, %v", target, err)
	}
	if err := os.Remove(mekoCLI); err != nil { //nolint:gosec // G703: the test's home
		t.Fatal(err)
	}
	serveUntilUp(t, dir, port)
	if target, err := os.Readlink(mekoCLI); err != nil || target != app.CurrentExe() {
		t.Fatalf("meko-cli past probation -> %q, %v", target, err)
	}
}
