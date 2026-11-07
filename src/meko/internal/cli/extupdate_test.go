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

	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/update"
)

// fakePresent is a Present release's bin/me_present, keeping its state in its folder in the data
// folder (MEKO_HOME, which Meko sets): version --json, install and start log to its log, status
// succeeds while it runs, its handoff names web.sock there, and it's busy while busy is there. With
// rollBackTo, start fails three
// times and then does what the launcher does on the fourth: points app/current back at that version
// and starts it, which fails too when old-fails is there.
func fakePresent(version, rollBackTo string) string {
	start := `touch "$s/running"; echo "start ` + version + `" >> "$s/log"`
	if rollBackTo != "" {
		start = `n=$(( $(cat "$s/starts" 2>/dev/null || echo 0) + 1 )); echo $n > "$s/starts"
    [ $n -gt 3 ] || exit 1
    ln -sfn ` + rollBackTo + ` "$s/app/current"; [ ! -f "$s/old-fails" ] || exit 1
    touch "$s/running"; echo "start ` + rollBackTo + `" >> "$s/log"`
	}
	return `#!/bin/sh
s="$MEKO_HOME/extensions/present"
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

// presentTarball is present-<version>-<os>-<arch>.tar.gz holding bin/me_present.
func presentTarball(t *testing.T, version, rollBackTo string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := fakePresent(version, rollBackTo)
	if err := tw.WriteHeader(&tar.Header{Name: "present-" + version + "/bin/me_present", Mode: 0o755,
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

// presentReleases serves a GitHub API with Kode's and Meko's releases and Present's release of
// version for this platform, and makes the CLI use it.
func presentReleases(t *testing.T, version string, tgz []byte) {
	t.Helper()
	goos, arch := update.Platform()
	name := update.PackageTarball("present", version, goos, arch)
	h := sha256.Sum256(tgz)
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(h[:]), name)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			_ = json.NewEncoder(w).Encode([]update.Release{
				{Tag: "meko-v0.20261031.0", Assets: []update.Asset{{Name: "install.sh"}}},
				{Tag: "agentk-v0.20261030.0", Assets: []update.Asset{{Name: "install.sh"}}},
				{Tag: "present-v" + version, Assets: []update.Asset{
					{Name: name, BrowserURL: srv.URL + "/tar"}, {Name: update.Sums, BrowserURL: srv.URL + "/sums"},
					{Name: update.SumsSig, BrowserURL: srv.URL + "/sig"},
				}},
			})
		case "/tar":
			_, _ = w.Write(tgz)
		case "/sums":
			_, _ = fmt.Fprint(w, sums)
		case "/sig":
			_, _ = fmt.Fprint(w, signSums(sums))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := newReleaseClient
	newReleaseClient = func(string) *update.Client { return testClient(srv) }
	t.Cleanup(func() { newReleaseClient = old })
}

// installedVersion is the Present release installedPresent installs.
const installedVersion = "0.20261024.0"

// installedPresent is a release install of Present's installedVersion, as an older extension
// install left it: app/<version>, app/current, ~/.local/bin/me_present, and the registry naming that
// link.
func installedPresent(t *testing.T, dir string) update.App {
	t.Helper()
	version := installedVersion
	app := extensions.Known["present"].App(dir, "present")
	if err := os.MkdirAll(filepath.Join(app.Dir(version), "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G306: a test's fake program
	if err := os.WriteFile(app.Exe(version), []byte(fakePresent(version, "")), 0o755); err != nil {
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
	reg.Entries["present"] = extensions.Entry{Command: link, InstalledByMeko: true}
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
	app := installedPresent(t, dir)
	presentReleases(t, next, presentTarball(t, next, ""))
	state := filepath.Dir(app.Root)
	if err := os.WriteFile(filepath.Join(state, "running"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errs := run("extension", "update", "present", "--check", "--data-dir", dir)
	want(t, "--check", code, 0, out+errs, "Present "+next+" is available (this is "+old+")")
	if app.Current() != old {
		t.Fatalf("--check changed app/current to %s", app.Current())
	}

	code, out, errs = run("extension", "update", "present", "--data-dir", dir)
	want(t, "extension update", code, 0, out+errs, "Present is running "+next)
	want(t, "extension update", code, 0, out,
		"  ✓ Release         Present "+next+", downloaded and checked\n")
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
	if v := extensions.Known["present"].Release(dir, "present", reg.Entries["present"].Command); v != next {
		t.Errorf("Release = %q", v)
	}

	code, out, errs = run("extension", "update", "present", "--data-dir", dir)
	want(t, "a second extension update", code, 0, out+errs, "Present "+next+" is the newest release")
}

// The launcher rolls back a version that keeps failing to start; the update says so and fails.
func TestUpdateExtensionReportsARollBack(t *testing.T) {
	const old, next = installedVersion, "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	app := installedPresent(t, dir)
	presentReleases(t, next, presentTarball(t, next, old))
	if err := os.WriteFile(filepath.Join(filepath.Dir(app.Root), "running"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run("extension", "update", "present", "--data-dir", dir)
	want(t, "extension update", code, 1, errs, "Present "+next+" failed to start 3 times, so it was rolled back to "+old)
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
	app := installedPresent(t, dir)
	presentReleases(t, next, presentTarball(t, next, ""))
	code, out, errs := run("extension", "update", "present", "--data-dir", dir)
	want(t, "extension update", code, 0, out+errs, "Installed Present "+next+"; it runs at its next start")
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
	code, _, errs := run("extension", "update", "kode", "--data-dir", dir)
	want(t, "Kode", code, 1, errs, "meko extension install kode installs it")
	code, _, errs = run("extension", "update", "present", "--data-dir", dir)
	want(t, "not installed", code, 1, errs, "meko extension install present installs it")
	// A build from source on PATH: used as it is, never updated.
	src := filepath.Join(t.TempDir(), "present")
	if err := os.WriteFile(src, []byte(fakePresent("0.0.0-dev", "")), 0o700); err != nil { //nolint:gosec // G306: test
		t.Fatal(err)
	}
	reg, _ := extensions.Load(dir)
	reg.Entries["present"] = extensions.Entry{Command: src, InstalledByMeko: true}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	code, _, errs = run("extension", "update", "present", "--data-dir", dir)
	want(t, "a build from source", code, 1, errs, "isn't a release Meko installed")
	code, _, errs = run("extension", "update", "--data-dir", dir)
	want(t, "no name", code, exitUsage, errs, "name the extension (kode, present)")
	code, _, errs = run("extension", "update", "present", "--all", "--data-dir", dir)
	want(t, "--all and a name", code, exitUsage, errs, "--all takes no name")
	lock := update.NewLock(dir)
	if !lock.TryLock() {
		t.Fatal("lock")
	}
	code, _, errs = run("extension", "update", "present", "--data-dir", dir)
	lock.Unlock()
	want(t, "another update running", code, 1, errs, "another update is running")
}

// extension install present, with no present here, installs Present's newest release itself:
// no installer script, the tarball unpacked into its folder's app/, no link on PATH (it's run as
// meko present), then set up with me_present install as one already there.
func TestInstallExtensionInstallsPresentsRelease(t *testing.T) {
	const version = "0.20261024.0"
	dir, port := setup(t)
	noRealExtensions(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	presentReleases(t, version, presentTarball(t, version, ""))
	code, out, errs = run("extension", "install", "present", "--data-dir", dir)
	want(t, "extension install", code, 0, out+errs,
		"  ✓ Release         Present "+version+", downloaded and checked")
	app := extensions.Known["present"].App(dir, "present")
	if app.Current() != version {
		t.Errorf("app/current is %q", app.Current())
	}
	home, _ := os.UserHomeDir()
	reg, _ := extensions.Load(dir)
	if reg.Entries["present"].Command != app.CurrentExe() {
		t.Errorf("registry %+v", reg.Entries)
	}
	if _, err := os.Lstat(filepath.Join(home, ".local", "bin", "me_present")); !os.IsNotExist(err) {
		t.Errorf("~/.local/bin/me_present was made: %v", err)
	}
	got := readLog(t, filepath.Join(filepath.Dir(app.Root), "log"))
	if !strings.HasPrefix(got, "install --meko-url http://127.0.0.1:"+port+"/mcp --meko-key ") ||
		!strings.HasSuffix(got, " MEKO_HOME="+dir+"\n") {
		t.Errorf("me_present install got %q", got)
	}
}

// destroy --with-extensions runs Present's uninstall, which removes its folder, releases and all,
// and then the link Meko made in ~/.local/bin goes too.
func TestDestroyRemovesTheLinkToPresentsRelease(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	installedPresent(t, dir)
	home, _ := os.UserHomeDir()
	link := filepath.Join(home, ".local", "bin", "me_present")
	code, out, errs := run("destroy", "--data-dir", dir, "--force", "--with-extensions")
	want(t, "destroy", code, 0, out+errs, "Uninstalling Present")
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("the link is left: %v", err)
	}
}

// addVersion puts another release of Present beside the installed one, as an earlier update left it.
func addVersion(t *testing.T, app update.App, version string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(app.Dir(version), "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G306: a test's fake program
	if err := os.WriteFile(app.Exe(version), []byte(fakePresent(version, "")), 0o755); err != nil {
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
	app := installedPresent(t, dir)
	addVersion(t, app, proven)
	addVersion(t, app, "0.20261022.0") // older still: pruned
	if err := app.Switch(proven, installedVersion, 0); err != nil {
		t.Fatal(err)
	}
	presentReleases(t, next, presentTarball(t, next, ""))
	if err := os.WriteFile(filepath.Join(filepath.Dir(app.Root), "running"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("extension", "update", "present", "--data-dir", dir)
	want(t, "extension update", code, 0, out+errs, "Present "+installedVersion+" hasn't served 5 minutes yet, so "+
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
	app := installedPresent(t, dir)
	presentReleases(t, next, []byte("not a tarball"))
	code, out, _ := run("extension", "update", "present", "--data-dir", dir)
	want(t, "extension update", code, 1, out,
		"  ✗ Release         installing Present "+next+"\n")
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
	app := installedPresent(t, dir)
	presentReleases(t, next, presentTarball(t, next, ""))
	state := filepath.Dir(app.Root)
	if err := os.WriteFile(filepath.Join(state, "running"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A folder where the update's record goes: it can't be written.
	if err := os.Mkdir(filepath.Join(app.Root, "update-pending.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run("extension", "update", "present", "--data-dir", dir)
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
	app := installedPresent(t, dir)
	presentReleases(t, next, presentTarball(t, next, installedVersion))
	state := filepath.Dir(app.Root)
	for _, f := range []string{"running", "old-fails"} {
		if err := os.WriteFile(filepath.Join(state, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	code, _, errs := run("extension", "update", "present", "--data-dir", dir)
	want(t, "extension update", code, 1, errs, "so it was rolled back to "+installedVersion+", which didn't start either")
}

// With every release for this platform rolled back before, there's nothing to update to, and it says
// so rather than calling the installed one the newest.
func TestUpdateExtensionSaysWhenNoReleaseIsFound(t *testing.T) {
	const next = "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	app := installedPresent(t, dir)
	presentReleases(t, next, presentTarball(t, next, ""))
	if err := os.WriteFile(filepath.Join(app.Root, "bad-versions"), []byte(next+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("extension", "update", "present", "--data-dir", dir)
	want(t, "extension update", code, 0, out+errs, "no release of it for")
	if strings.Contains(out, "newest release") {
		t.Errorf("called the installed one the newest: %q", out)
	}
}

// The Extensions page's Update: it waits for the socket when the update leaves Present running,
// returns at once when it leaves it stopped, and the page learns the version from ExtensionUpdates.
func TestRunExtensionUpdates(t *testing.T) {
	const next = "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	app := installedPresent(t, dir)
	presentReleases(t, next, presentTarball(t, next, ""))
	w := testListener(t, dir)
	if u := w.ExtensionUpdates("present"); u.Version != installedVersion || !u.Updatable || !u.AutoUpdate {
		t.Errorf("ExtensionUpdates(present) = %+v", u)
	}
	if u := w.ExtensionUpdates("kode"); u.Version != "" || u.Updatable {
		t.Errorf("ExtensionUpdates(kode) = %+v", u)
	}
	old := extensions.ListenTimeout
	extensions.ListenTimeout = 300 * time.Millisecond
	t.Cleanup(func() { extensions.ListenTimeout = old })

	// Stopped: switched, left stopped, and no wait for a socket nothing serves. The job's output
	// names the release before its download starts, so the page has a line to show meanwhile.
	var out bytes.Buffer
	if err := w.RunExtension(context.Background(), "present", "update", &out); err != nil ||
		!strings.Contains(out.String(), "it runs at its next start") ||
		!strings.HasPrefix(out.String(), "Installing Present "+next+"\n") {
		t.Fatalf("stopped: %v %q", err, out.String())
	}

	// Running: started again, and it returns once the socket answers.
	const later = "0.20261026.0"
	presentReleases(t, later, presentTarball(t, later, ""))
	state := filepath.Dir(app.Root)
	if err := os.WriteFile(filepath.Join(state, "running"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(state, "web.sock")
	listenAfter(t, sock, 0)
	out.Reset()
	if err := w.RunExtension(context.Background(), "present", "update", &out); err != nil ||
		!strings.Contains(out.String(), "Present is running "+later) {
		t.Fatalf("running: %v %q", err, out.String())
	}
	if w.sockets["present"] != sock {
		t.Errorf("socket %q", w.sockets["present"])
	}
	if u := w.ExtensionUpdates("present"); u.Version != later || !u.Updatable {
		t.Errorf("ExtensionUpdates(present) = %+v", u)
	}
}

// With ~/.local/bin/me_present (an older Meko's link) running another data folder's release,
// extension install present installs this one's own and leaves that link alone.
func TestInstallExtensionLeavesAnotherDataFoldersLink(t *testing.T) {
	const version = "0.20261025.0"
	dir, port := setup(t)
	noRealExtensions(t)
	other := installedPresent(t, shortDir(t))
	home, _ := os.UserHomeDir()
	link := filepath.Join(home, ".local", "bin", "me_present")
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	presentReleases(t, version, presentTarball(t, version, ""))
	code, out, errs = run("extension", "install", "present", "--data-dir", dir)
	want(t, "extension install", code, 0, out+errs, "  ✓ Present is installed\n")
	app := extensions.Known["present"].App(dir, "present")
	reg, _ := extensions.Load(dir)
	if reg.Entries["present"].Command != app.CurrentExe() {
		t.Errorf("registered %q, want %q", reg.Entries["present"].Command, app.CurrentExe())
	}
	if target, _ := os.Readlink(link); target != other.CurrentExe() {
		t.Errorf("the other data folder's link now points at %q", target)
	}
}
