package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/update"
)

// fakePresenter is a Presenter release's bin/meko_presenter, keeping its state in its folder in the data
// folder (MEKO_HOME, which Meko sets): version --json, install and start log to its log, status
// succeeds while it runs. With rollBackTo, start fails three times and then does what the launcher
// does on the fourth: points app/current back at that version and starts it.
func fakePresenter(version, rollBackTo string) string {
	start := `touch "$s/running"; echo "start ` + version + `" >> "$s/log"`
	if rollBackTo != "" {
		start = `n=$(( $(cat "$s/starts" 2>/dev/null || echo 0) + 1 )); echo $n > "$s/starts"
    [ $n -gt 3 ] || exit 1
    ln -sfn ` + rollBackTo + ` "$s/app/current"; touch "$s/running"; echo "start ` + rollBackTo + `" >> "$s/log"`
	}
	return `#!/bin/sh
s="$MEKO_HOME/extensions/presenter"
case "$1" in
  version) echo '{"version":"` + version + `"}' ;;
  install) echo "$* MEKO_HOME=$MEKO_HOME" >> "$s/log" ;;
  status) [ -f "$s/running" ] ;;
  start) ` + start + ` ;;
  stop) rm -f "$s/running" ;;
  uninstall) rm -rf "$s" ;;
esac
`
}

// presenterTarball is presenter-<version>-<os>-<arch>.tar.gz holding bin/meko_presenter.
func presenterTarball(t *testing.T, version, rollBackTo string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := fakePresenter(version, rollBackTo)
	if err := tw.WriteHeader(&tar.Header{Name: "presenter-" + version + "/bin/meko_presenter", Mode: 0o755,
		Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// presenterReleases serves a GitHub API with AgentK's and Meko's releases and Presenter's release of
// version for this platform, and makes the CLI use it.
func presenterReleases(t *testing.T, version string, tgz []byte) {
	t.Helper()
	goos, arch := update.Platform()
	name := update.PackageTarball("presenter", version, goos, arch)
	h := sha256.Sum256(tgz)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			_ = json.NewEncoder(w).Encode([]update.Release{
				{Tag: "meko-v0.20261031.0", Assets: []update.Asset{{Name: "install.sh"}}},
				{Tag: "agentk-v0.20261030.0", Assets: []update.Asset{{Name: "install.sh"}}},
				{Tag: "presenter-v" + version, Assets: []update.Asset{
					{Name: name, BrowserURL: srv.URL + "/tar"}, {Name: update.Sums, BrowserURL: srv.URL + "/sums"},
				}},
			})
		case "/tar":
			_, _ = w.Write(tgz)
		case "/sums":
			_, _ = fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(h[:]), name)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := newReleaseClient
	newReleaseClient = func(string) *update.Client { return &update.Client{Repo: "o/r", API: srv.URL, HTTP: srv.Client()} }
	t.Cleanup(func() { newReleaseClient = old })
}

// installedVersion is the Presenter release installedPresenter installs.
const installedVersion = "0.20261024.0"

// installedPresenter is a release install of Presenter's installedVersion, as install_extension
// leaves it: app/<version>, app/current, ~/.local/bin/meko_presenter, and the registry naming that
// link.
func installedPresenter(t *testing.T, dir string) update.App {
	t.Helper()
	version := installedVersion
	app := extensions.Known["presenter"].App(dir, "presenter")
	if err := os.MkdirAll(filepath.Join(app.Dir(version), "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G306: a test's fake program
	if err := os.WriteFile(app.Exe(version), []byte(fakePresenter(version, "")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.Dir(version), "VERSION"), []byte(version+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.Repoint(version); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	link, err := app.LinkCommand(home)
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := extensions.Load(dir)
	reg.Entries["presenter"] = extensions.Entry{Command: link, InstalledByMeko: true}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestUpdateExtensionInstallsTheNewestReleaseAndRestartsIt(t *testing.T) {
	const old, next = installedVersion, "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	app := installedPresenter(t, dir)
	presenterReleases(t, next, presenterTarball(t, next, ""))
	state := filepath.Dir(app.Root)
	if err := os.WriteFile(filepath.Join(state, "running"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errs := run("update_extension", "presenter", "--check", "--data-dir", dir)
	want(t, "--check", code, 0, out+errs, "Presenter "+next+" is available (this is "+old+")")
	if app.Current() != old {
		t.Fatalf("--check changed app/current to %s", app.Current())
	}

	code, out, errs = run("update_extension", "presenter", "--data-dir", dir)
	want(t, "update_extension", code, 0, out+errs, "Presenter is running "+next)
	if app.Current() != next {
		t.Errorf("app/current is %s", app.Current())
	}
	if got := readLog(t, filepath.Join(state, "log")); got != "start "+next+"\n" {
		t.Errorf("log %q", got)
	}
	// On probation until the new version clears it, for its launcher to roll back.
	if p, _ := app.ReadPending(); p == nil || p.From != old || p.To != next {
		t.Errorf("pending %+v", p)
	}
	reg, _ := extensions.Load(dir)
	if v := extensions.Known["presenter"].Release(dir, "presenter", reg.Entries["presenter"].Command); v != next {
		t.Errorf("Release = %q", v)
	}

	code, out, errs = run("update_extension", "presenter", "--data-dir", dir)
	want(t, "a second update_extension", code, 0, out+errs, "Presenter "+next+" is the newest release")
}

// The launcher rolls back a version that keeps failing to start; the update says so and fails.
func TestUpdateExtensionReportsARollBack(t *testing.T) {
	const old, next = installedVersion, "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	app := installedPresenter(t, dir)
	presenterReleases(t, next, presenterTarball(t, next, old))
	if err := os.WriteFile(filepath.Join(filepath.Dir(app.Root), "running"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run("update_extension", "presenter", "--data-dir", dir)
	want(t, "update_extension", code, 1, errs, "Presenter "+next+" failed to start 3 times, so it was rolled back to "+old)
	if app.Current() != old {
		t.Errorf("app/current is %s", app.Current())
	}
}

// A stopped one is switched and left stopped: its next start runs the new version.
func TestUpdateExtensionLeavesAStoppedOneStopped(t *testing.T) {
	const old, next = installedVersion, "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	app := installedPresenter(t, dir)
	presenterReleases(t, next, presenterTarball(t, next, ""))
	code, out, errs := run("update_extension", "presenter", "--data-dir", dir)
	want(t, "update_extension", code, 0, out+errs, "Installed Presenter "+next+"; it runs at its next start")
	if app.Current() != next {
		t.Errorf("app/current is %s", app.Current())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(app.Root), "log")); !os.IsNotExist(err) {
		t.Error("it was started")
	}
}

func TestUpdateExtensionRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	code, _, errs := run("update_extension", "agentk", "--data-dir", dir)
	want(t, "AgentK", code, 1, errs, "AgentK updates itself")
	code, _, errs = run("update_extension", "presenter", "--data-dir", dir)
	want(t, "not installed", code, 1, errs, "meko install_extension presenter installs it")
	// A build from source on PATH: used as it is, never updated.
	src := filepath.Join(t.TempDir(), "presenter")
	if err := os.WriteFile(src, []byte(fakePresenter("0.0.0-dev", "")), 0o700); err != nil { //nolint:gosec // G306: test
		t.Fatal(err)
	}
	reg, _ := extensions.Load(dir)
	reg.Entries["presenter"] = extensions.Entry{Command: src, InstalledByMeko: true}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	code, _, errs = run("update_extension", "presenter", "--data-dir", dir)
	want(t, "a build from source", code, 1, errs, "isn't a release Meko installed")
	code, _, errs = run("update_extension", "--data-dir", dir)
	want(t, "no name", code, exitUsage, errs, "name the extension (agentk, presenter)")
}

// install_extension presenter, with no presenter here, installs Presenter's newest release itself:
// no installer script, the tarball unpacked into its folder's app/, linked from ~/.local/bin, then
// set up with meko_presenter install as one already there.
func TestInstallExtensionInstallsPresentersRelease(t *testing.T) {
	const version = "0.20261024.0"
	dir, port := setup(t)
	noRealExtensions(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	presenterReleases(t, version, presenterTarball(t, version, ""))
	code, out, errs = run("install_extension", "presenter", "--data-dir", dir)
	want(t, "install_extension", code, 0, out+errs, "Installing Presenter "+version+" (presenter-v"+version+")")
	app := extensions.Known["presenter"].App(dir, "presenter")
	if app.Current() != version {
		t.Errorf("app/current is %q", app.Current())
	}
	home, _ := os.UserHomeDir()
	reg, _ := extensions.Load(dir)
	if reg.Entries["presenter"].Command != filepath.Join(home, ".local", "bin", "meko_presenter") {
		t.Errorf("registry %+v", reg.Entries)
	}
	got := readLog(t, filepath.Join(filepath.Dir(app.Root), "log"))
	if !strings.HasPrefix(got, "install --meko-url http://127.0.0.1:"+port+"/mcp --meko-key ") ||
		!strings.HasSuffix(got, " MEKO_HOME="+dir+"\n") {
		t.Errorf("meko_presenter install got %q", got)
	}
}

// destroy --with-extensions runs Presenter's uninstall, which removes its folder, releases and all,
// and then the link Meko made in ~/.local/bin goes too.
func TestDestroyRemovesTheLinkToPresentersRelease(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	installedPresenter(t, dir)
	home, _ := os.UserHomeDir()
	link := filepath.Join(home, ".local", "bin", "meko_presenter")
	code, out, errs := run("destroy", "--data-dir", dir, "--force", "--with-extensions")
	want(t, "destroy", code, 0, out+errs, "Uninstalling Presenter")
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("the link is left: %v", err)
	}
}
