package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
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
	"time"

	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/update"
)

// fakePresenter is a Presenter release's bin/meko_presenter, keeping its state in its folder in the data
// folder (MEKO_HOME, which Meko sets): version --json, install and start log to its log, status
// succeeds while it runs, its handoff names web.sock there, and it's busy while busy is there. With
// rollBackTo, start fails three
// times and then does what the launcher does on the fourth: points app/current back at that version
// and starts it, which fails too when old-fails is there.
func fakePresenter(version, rollBackTo string) string {
	start := `touch "$s/running"; echo "start ` + version + `" >> "$s/log"`
	if rollBackTo != "" {
		start = `n=$(( $(cat "$s/starts" 2>/dev/null || echo 0) + 1 )); echo $n > "$s/starts"
    [ $n -gt 3 ] || exit 1
    ln -sfn ` + rollBackTo + ` "$s/app/current"; [ ! -f "$s/old-fails" ] || exit 1
    touch "$s/running"; echo "start ` + rollBackTo + `" >> "$s/log"`
	}
	return `#!/bin/sh
s="$MEKO_HOME/extensions/presenter"
case "$1" in
  version) echo '{"version":"` + version + `"}' ;;
  install) echo "$* MEKO_HOME=$MEKO_HOME" >> "$s/log" ;;
  status) [ -f "$s/running" ] ;;
  start) ` + start + ` ;;
  stop) rm -f "$s/running" ;;
  _handoff) printf '{"socket": "%s"}\n' "$s/web.sock" ;;
  _busy) [ -f "$s/busy" ] ;;
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

// installedPresenter is a release install of Presenter's installedVersion, as extension install
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

	code, out, errs := run("extension", "update", "presenter", "--check", "--data-dir", dir)
	want(t, "--check", code, 0, out+errs, "Presenter "+next+" is available (this is "+old+")")
	if app.Current() != old {
		t.Fatalf("--check changed app/current to %s", app.Current())
	}

	code, out, errs = run("extension", "update", "presenter", "--data-dir", dir)
	want(t, "extension update", code, 0, out+errs, "Presenter is running "+next)
	want(t, "extension update", code, 0, out,
		"  ✓ Release         Presenter "+next+" (presenter-v"+next+"), downloaded and checked\n")
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

	code, out, errs = run("extension", "update", "presenter", "--data-dir", dir)
	want(t, "a second extension update", code, 0, out+errs, "Presenter "+next+" is the newest release")
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
	code, _, errs := run("extension", "update", "presenter", "--data-dir", dir)
	want(t, "extension update", code, 1, errs, "Presenter "+next+" failed to start 3 times, so it was rolled back to "+old)
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
	code, out, errs := run("extension", "update", "presenter", "--data-dir", dir)
	want(t, "extension update", code, 0, out+errs, "Installed Presenter "+next+"; it runs at its next start")
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
	code, _, errs := run("extension", "update", "agentk", "--data-dir", dir)
	want(t, "AgentK", code, 1, errs, "meko extension install agentk installs it")
	code, _, errs = run("extension", "update", "presenter", "--data-dir", dir)
	want(t, "not installed", code, 1, errs, "meko extension install presenter installs it")
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
	code, _, errs = run("extension", "update", "presenter", "--data-dir", dir)
	want(t, "a build from source", code, 1, errs, "isn't a release Meko installed")
	code, _, errs = run("extension", "update", "--data-dir", dir)
	want(t, "no name", code, exitUsage, errs, "name the extension (agentk, presenter)")
	code, _, errs = run("extension", "update", "presenter", "--all", "--data-dir", dir)
	want(t, "--all and a name", code, exitUsage, errs, "--all takes no name")
	lock := update.NewLock(dir)
	if !lock.TryLock() {
		t.Fatal("lock")
	}
	code, _, errs = run("extension", "update", "presenter", "--data-dir", dir)
	lock.Unlock()
	want(t, "another update running", code, 1, errs, "another update is running")
}

// extension install presenter, with no presenter here, installs Presenter's newest release itself:
// no installer script, the tarball unpacked into its folder's app/, linked from ~/.local/bin, then
// set up with meko_presenter install as one already there.
func TestInstallExtensionInstallsPresentersRelease(t *testing.T) {
	const version = "0.20261024.0"
	dir, port := setup(t)
	noRealExtensions(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	presenterReleases(t, version, presenterTarball(t, version, ""))
	code, out, errs = run("extension", "install", "presenter", "--data-dir", dir)
	want(t, "extension install", code, 0, out+errs,
		"  ✓ Release         Presenter "+version+" (presenter-v"+version+"), downloaded and checked")
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

// addVersion puts another release of Presenter beside the installed one, as an earlier update left it.
func addVersion(t *testing.T, app update.App, version string) {
	t.Helper()
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
}

// An update of a version still on probation keeps the version before it, the last one known to
// work, as the one to roll back to.
func TestUpdateExtensionKeepsTheLastProvenVersion(t *testing.T) {
	const proven, next = "0.20261023.0", "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	app := installedPresenter(t, dir)
	addVersion(t, app, proven)
	addVersion(t, app, "0.20261022.0") // older still: pruned
	if err := app.Switch(proven, installedVersion, 0); err != nil {
		t.Fatal(err)
	}
	presenterReleases(t, next, presenterTarball(t, next, ""))
	if err := os.WriteFile(filepath.Join(filepath.Dir(app.Root), "running"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("extension", "update", "presenter", "--data-dir", dir)
	want(t, "extension update", code, 0, out+errs, "Presenter "+installedVersion+" hasn't served 5 minutes yet, so "+
		proven+" stays the version to roll back to")
	if p, _ := app.ReadPending(); p == nil || p.From != proven || p.To != next {
		t.Errorf("pending %+v", p)
	}
	if got := strings.Join(app.Versions(), " "); got != proven+" "+installedVersion+" "+next {
		t.Errorf("versions %s", got)
	}
}

// A release that doesn't unpack fails the update with the ✗ row, and the installed version stays.
func TestUpdateExtensionSaysWhenTheReleaseFails(t *testing.T) {
	const next = "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	app := installedPresenter(t, dir)
	presenterReleases(t, next, []byte("not a tarball"))
	code, out, _ := run("extension", "update", "presenter", "--data-dir", dir)
	want(t, "extension update", code, 1, out,
		"  ✗ Release         installing Presenter "+next+" (presenter-v"+next+")\n")
	if app.Current() != installedVersion {
		t.Errorf("app/current is %s", app.Current())
	}
}

// A switch that fails leaves app/current on the old version, which is started again.
func TestUpdateExtensionRestartsTheOldVersionWhenTheSwitchFails(t *testing.T) {
	const next = "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	app := installedPresenter(t, dir)
	presenterReleases(t, next, presenterTarball(t, next, ""))
	state := filepath.Dir(app.Root)
	if err := os.WriteFile(filepath.Join(state, "running"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A folder where the update's record goes: it can't be written.
	if err := os.Mkdir(filepath.Join(app.Root, "update-pending.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run("extension", "update", "presenter", "--data-dir", dir)
	want(t, "extension update", code, 1, errs, "can't switch to "+next)
	if app.Current() != installedVersion {
		t.Errorf("app/current is %s", app.Current())
	}
	if got := readLog(t, filepath.Join(state, "log")); got != "start "+installedVersion+"\n" {
		t.Errorf("log %q", got)
	}
}

// When the version rolled back to doesn't start either, the update says so, with its error.
func TestUpdateExtensionReportsARollBackThatDoesntStart(t *testing.T) {
	const next = "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	app := installedPresenter(t, dir)
	presenterReleases(t, next, presenterTarball(t, next, installedVersion))
	state := filepath.Dir(app.Root)
	for _, f := range []string{"running", "old-fails"} {
		if err := os.WriteFile(filepath.Join(state, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	code, _, errs := run("extension", "update", "presenter", "--data-dir", dir)
	want(t, "extension update", code, 1, errs, "so it was rolled back to "+installedVersion+", which didn't start either")
}

// With every release for this platform rolled back before, there's nothing to update to, and it says
// so rather than calling the installed one the newest.
func TestUpdateExtensionSaysWhenNoReleaseIsFound(t *testing.T) {
	const next = "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	app := installedPresenter(t, dir)
	presenterReleases(t, next, presenterTarball(t, next, ""))
	if err := os.WriteFile(filepath.Join(app.Root, "bad-versions"), []byte(next+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("extension", "update", "presenter", "--data-dir", dir)
	want(t, "extension update", code, 0, out+errs, "no release of it for")
	if strings.Contains(out, "newest release") {
		t.Errorf("called the installed one the newest: %q", out)
	}
}

// The Extensions page's Update: it waits for the socket when the update leaves Presenter running,
// returns at once when it leaves it stopped, and the page learns the version from ExtensionUpdates.
func TestRunExtensionUpdates(t *testing.T) {
	const next = "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	app := installedPresenter(t, dir)
	presenterReleases(t, next, presenterTarball(t, next, ""))
	w := testListener(t, dir)
	if u := w.ExtensionUpdates("presenter"); u.Version != installedVersion || !u.Updatable || !u.AutoUpdate {
		t.Errorf("ExtensionUpdates(presenter) = %+v", u)
	}
	if u := w.ExtensionUpdates("agentk"); u.Version != "" || u.Updatable {
		t.Errorf("ExtensionUpdates(agentk) = %+v", u)
	}
	old := extensions.ListenTimeout
	extensions.ListenTimeout = 300 * time.Millisecond
	t.Cleanup(func() { extensions.ListenTimeout = old })

	// Stopped: switched, left stopped, and no wait for a socket nothing serves. The job's output
	// names the release before its download starts, so the page has a line to show meanwhile.
	var out bytes.Buffer
	if err := w.RunExtension(context.Background(), "presenter", "update", &out); err != nil ||
		!strings.Contains(out.String(), "it runs at its next start") ||
		!strings.HasPrefix(out.String(), "Installing Presenter "+next+" (presenter-v"+next+")\n") {
		t.Fatalf("stopped: %v %q", err, out.String())
	}

	// Running: started again, and it returns once the socket answers.
	const later = "0.20261026.0"
	presenterReleases(t, later, presenterTarball(t, later, ""))
	state := filepath.Dir(app.Root)
	if err := os.WriteFile(filepath.Join(state, "running"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(state, "web.sock")
	listenAfter(t, sock, 0)
	out.Reset()
	if err := w.RunExtension(context.Background(), "presenter", "update", &out); err != nil ||
		!strings.Contains(out.String(), "Presenter is running "+later) {
		t.Fatalf("running: %v %q", err, out.String())
	}
	if w.sockets["presenter"] != sock {
		t.Errorf("socket %q", w.sockets["presenter"])
	}
	if u := w.ExtensionUpdates("presenter"); u.Version != later || !u.Updatable {
		t.Errorf("ExtensionUpdates(presenter) = %+v", u)
	}
}

// With ~/.local/bin/meko_presenter running another data folder's release, extension install
// presenter installs this one's own and leaves that link alone.
func TestInstallExtensionLeavesAnotherDataFoldersLink(t *testing.T) {
	const version = "0.20261025.0"
	dir, port := setup(t)
	noRealExtensions(t)
	other := installedPresenter(t, shortDir(t))
	home, _ := os.UserHomeDir()
	link := filepath.Join(home, ".local", "bin", "meko_presenter")
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	presenterReleases(t, version, presenterTarball(t, version, ""))
	code, out, errs = run("extension", "install", "presenter", "--data-dir", dir)
	want(t, "extension install", code, 0, out+errs, "runs another Meko data folder's Presenter, so it stays")
	app := extensions.Known["presenter"].App(dir, "presenter")
	reg, _ := extensions.Load(dir)
	if reg.Entries["presenter"].Command != app.CurrentExe() {
		t.Errorf("registered %q, want %q", reg.Entries["presenter"].Command, app.CurrentExe())
	}
	if target, _ := os.Readlink(link); target != other.CurrentExe() {
		t.Errorf("the other data folder's link now points at %q", target)
	}
}
